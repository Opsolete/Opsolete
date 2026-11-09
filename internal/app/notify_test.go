package app

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/opsolete/opsolete/internal/storage"
)

// listening starts a Notifier on a's database and returns once it delivers notifications.
func listening(t *testing.T, a *App, fallback time.Duration) *Notifier {
	t.Helper()
	n := &Notifier{DB: a.Store.Pool.Config().ConnConfig, Log: discard, Fallback: fallback}
	ctx, stop := context.WithCancel(context.Background())
	wait := n.Start(ctx)
	t.Cleanup(func() { stop(); wait() })
	delivers(t, a, n)
	return n
}

// delivers waits (up to 10 s: a reconnect backs off) until a jobs notification reaches n's waiters.
func delivers(t *testing.T, a *App, n *Notifier) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; {
		w := n.Jobs()
		if _, err := a.Store.Pool.Exec(context.Background(), "SELECT pg_notify($1, '')", storage.JobsChannel); err != nil {
			t.Fatal(err)
		}
		ok := signalled(w, 100*time.Millisecond)
		w.Close()
		if ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the notifier delivers nothing")
		}
	}
}

// signalled reports whether w gets a signal within d (the fallback timer doesn't count).
func signalled(w *Waiter, d time.Duration) bool {
	select {
	case <-w.c:
		return true
	case <-time.After(d):
		return false
	}
}

func TestNotifyDelivery(t *testing.T) {
	a, ctx := testApp(t), context.Background()
	n := listening(t, a, time.Hour)

	t.Run("queueing a run wakes a claimer", func(t *testing.T) {
		jobs := n.Jobs()
		defer jobs.Close()
		configured(t, a)
		if !signalled(jobs, time.Second) {
			t.Fatal("not woken")
		}
	})

	t.Run("a run's log and status wake its own subscribers only", func(t *testing.T) {
		_, runA := configured(t, a)
		_, runB := configured(t, a)
		leased(t, a, runA.ID, "lease-a")
		wa, wb := n.Run(runA.ID), n.Run(runB.ID)
		defer wa.Close()
		defer wb.Close()
		if _, err := a.AppendLog(ctx, runA.ID, "lease-a", []byte("line\n")); err != nil {
			t.Fatal(err)
		}
		if !signalled(wa, time.Second) || signalled(wb, 200*time.Millisecond) {
			t.Fatal("a log chunk wakes its run's subscriber, and only that one")
		}
		if _, err := a.Store.CancelRun(ctx, runB.ID, "x@example.com", false); err != nil {
			t.Fatal(err)
		}
		if !signalled(wb, time.Second) || signalled(wa, 200*time.Millisecond) {
			t.Fatal("a status change wakes its run's subscriber, and only that one")
		}
	})

	t.Run("a released state lock wakes a claimer", func(t *testing.T) {
		ws := emptyWorkspace(t, a, admin)
		if _, ok, err := a.Store.LockState(ctx, ws.ID, "l1", uuid.Nil, []byte(`{"ID":"l1"}`)); err != nil || !ok {
			t.Fatal(ok, err)
		}
		jobs := n.Jobs()
		defer jobs.Close()
		if err := a.Store.UnlockState(ctx, ws.ID, "l1", uuid.Nil); err != nil {
			t.Fatal(err)
		}
		if !signalled(jobs, time.Second) {
			t.Fatal("not woken")
		}
	})

	t.Run("a rolled back notification is not delivered", func(t *testing.T) {
		id := uuid.New()
		jobs, other := n.Jobs(), n.Run(id)
		defer jobs.Close()
		defer other.Close()
		tx, err := a.Store.Pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		tx.Exec(ctx, "SELECT pg_notify($1, '')", storage.JobsChannel)
		tx.Rollback(ctx)
		// delivered in commit order: once this one arrives, the rolled-back one would have too
		if _, err := a.Store.Pool.Exec(ctx, "SELECT pg_notify($1, $2)", storage.RunsChannel, id.String()); err != nil {
			t.Fatal(err)
		}
		if !signalled(other, time.Second) {
			t.Fatal("the committed notification didn't arrive")
		}
		if signalled(jobs, 0) {
			t.Fatal("a rolled-back notification was delivered")
		}
	})
}

func TestNotifyReconnect(t *testing.T) {
	a, ctx := testApp(t), context.Background()
	n := listening(t, a, 100*time.Millisecond)
	var killed int
	if err := a.Store.Pool.QueryRow(ctx, `SELECT count(pg_terminate_backend(pid)) FROM pg_stat_activity
		WHERE application_name = 'opsolete-notify' AND datname = current_database()`).Scan(&killed); err != nil || killed != 1 {
		t.Fatalf("terminate the listener: %d, %v", killed, err)
	}
	// meanwhile waiters still look again after the fallback interval
	w := n.Jobs()
	start := time.Now()
	if !w.Wait(ctx) || time.Since(start) > time.Second {
		t.Fatalf("fallback: waited %v", time.Since(start))
	}
	w.Close()
	delivers(t, a, n) // it reconnected
	jobs := n.Jobs()
	defer jobs.Close()
	configured(t, a)
	if !signalled(jobs, time.Second) {
		t.Fatal("no notification after the reconnect")
	}
}
