package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/opsolete/opsolete/internal/testdb"
)

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

func TestRunJobs(t *testing.T) {
	var ticks, fails, panics, slow, running, overlap atomic.Int32
	jobs := []Job{
		{Name: "tick", Every: 20 * time.Millisecond, Run: func(context.Context) error { ticks.Add(1); return nil }},
		{Name: "fail", Every: 5 * time.Millisecond, Run: func(context.Context) error { fails.Add(1); return errors.New("boom") }},
		{Name: "panic", Every: 5 * time.Millisecond, Run: func(context.Context) error { panics.Add(1); panic("boom") }},
		{Name: "slow", Every: time.Millisecond, Run: func(context.Context) error {
			if running.Add(1) > 1 {
				overlap.Add(1)
			}
			defer running.Add(-1)
			slow.Add(1)
			time.Sleep(30 * time.Millisecond)
			return nil
		}},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	runJobs(ctx, discard, jobs) // returns once ctx ends: cancel stops every job
	if n := ticks.Load(); n < 5 || n > 17 {
		t.Errorf("a 20ms job ran %d times in 300ms", n)
	}
	if fails.Load() < 2 || panics.Load() < 2 {
		t.Errorf("failing and panicking jobs must keep running: %d, %d", fails.Load(), panics.Load())
	}
	if overlap.Load() != 0 || slow.Load() > 11 {
		t.Errorf("a job overlapped itself %d times (%d runs of 30ms in 300ms)", overlap.Load(), slow.Load())
	}
	n := ticks.Load()
	time.Sleep(50 * time.Millisecond)
	if ticks.Load() != n {
		t.Error("a job ran after the context ended")
	}
}

func TestSchedulerLeader(t *testing.T) {
	url := testdb.URL(t)
	cfg, err := pgx.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	const retry = 50 * time.Millisecond
	type replica struct {
		ran, canceled, locals atomic.Int32
		leading               atomic.Bool
		stop                  context.CancelFunc
		wait                  func()
	}
	start := func() *replica {
		r := &replica{}
		s := &Scheduler{DB: cfg, Retry: retry, Log: discard, OnLead: r.leading.Store, Jobs: []Job{
			// in flight as long as it leads: it returns only when its context is cancelled
			{Name: "block", Every: 10 * time.Millisecond, Run: func(ctx context.Context) error {
				r.ran.Add(1)
				<-ctx.Done()
				r.canceled.Add(1)
				return ctx.Err()
			}},
			{Name: "local", Every: 10 * time.Millisecond, Local: true, Run: func(context.Context) error { r.locals.Add(1); return nil }},
		}}
		ctx, stop := context.WithCancel(context.Background())
		r.stop, r.wait = stop, s.Start(ctx)
		t.Cleanup(func() { stop(); r.wait() })
		return r
	}
	eventually := func(what string, within time.Duration, ok func() bool) {
		t.Helper()
		for deadline := time.Now().Add(within); !ok(); time.Sleep(5 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("not within %v: %s", within, what)
			}
		}
	}
	a, b := start(), start()
	eventually("one replica leads", time.Second, func() bool { return a.leading.Load() || b.leading.Load() })
	leader, follower := a, b
	if b.leading.Load() {
		leader, follower = b, a
	}
	time.Sleep(5 * retry)
	if !leader.leading.Load() || follower.leading.Load() || follower.ran.Load() != 0 || leader.ran.Load() != 1 {
		t.Fatalf("leader %v ran %d, follower %v ran %d", leader.leading.Load(), leader.ran.Load(), follower.leading.Load(), follower.ran.Load())
	}
	if a.locals.Load() < 2 || b.locals.Load() < 2 {
		t.Fatalf("local jobs run on every replica: %d, %d", a.locals.Load(), b.locals.Load())
	}

	// The leader's lock session dies: its in-flight job is cancelled and a replica leads again.
	conn, err := pgx.Connect(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	locks := func() (n int) {
		conn.QueryRow(context.Background(), `SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND granted
			AND database = (SELECT oid FROM pg_database WHERE datname = current_database())`).Scan(&n)
		return n
	}
	var killed int
	if err := conn.QueryRow(context.Background(), `SELECT count(pg_terminate_backend(pid)) FROM pg_locks WHERE locktype = 'advisory'
		AND database = (SELECT oid FROM pg_database WHERE datname = current_database())`).Scan(&killed); err != nil || killed != 1 {
		t.Fatalf("terminate the leader's lock session: %d, %v", killed, err)
	}
	eventually("the old leader's job is cancelled", 10*retry, func() bool { return leader.canceled.Load() == 1 })
	eventually("a replica leads again", 10*retry, func() bool { return a.ran.Load()+b.ran.Load() == 2 && locks() == 1 })

	// The leader shuts down: the other replica takes over within the retry interval (and a bit).
	leader, follower = a, b
	if b.leading.Load() {
		leader, follower = b, a
	}
	ranBefore := follower.ran.Load()
	leader.stop()
	leader.wait()
	if leader.leading.Load() || leader.canceled.Load() != leader.ran.Load() {
		t.Fatalf("after shutdown: leading %v, %d of %d runs cancelled", leader.leading.Load(), leader.canceled.Load(), leader.ran.Load())
	}
	eventually("the other replica takes over", 4*retry, func() bool { return follower.leading.Load() && follower.ran.Load() == ranBefore+1 })
	follower.stop()
	follower.wait()
	if n := locks(); n != 0 {
		t.Fatalf("the lock must be released on shutdown: %d held", n)
	}
}
