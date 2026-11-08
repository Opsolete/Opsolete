package app

import (
	"context"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/opsolete/opsolete/internal/storage"
)

// Every server replica runs one Notifier: a connection of its own LISTENing on storage's channels
// (sent in the transaction of the change, see storage.JobsChannel), fanned out in-process to runner
// claims waiting for a job and log streams following a run. Notifications sent while it reconnects
// are lost, so waiters also look again after Fallback; on reconnect it wakes every waiter.

// NotifyFallback is how long a waiter waits for a signal before looking anyway.
const NotifyFallback = 5 * time.Second

type Notifier struct {
	DB       *pgx.ConnConfig
	Log      *slog.Logger
	Fallback time.Duration // NotifyFallback if 0

	mu   sync.Mutex
	subs map[string]map[chan struct{}]struct{} // by channel + ":" + payload
}

// Waiter is a subscription. Its signals coalesce (buffer 1), so subscribe before reading what it
// signals: a change after the subscription is never missed.
type Waiter struct {
	n        *Notifier
	key      string
	c        chan struct{}
	fallback time.Duration
}

// Jobs: a job may have become claimable. A nil Notifier (none started) gives a waiter on Fallback only.
func (n *Notifier) Jobs() *Waiter { return n.subscribe(storage.JobsChannel + ":") }

// Run: run id's log or status changed.
func (n *Notifier) Run(id uuid.UUID) *Waiter {
	return n.subscribe(storage.RunsChannel + ":" + id.String())
}

func (n *Notifier) subscribe(key string) *Waiter {
	w := &Waiter{n: n, key: key, c: make(chan struct{}, 1), fallback: NotifyFallback}
	if n == nil {
		return w
	}
	if n.Fallback > 0 {
		w.fallback = n.Fallback
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.subs == nil {
		n.subs = map[string]map[chan struct{}]struct{}{}
	}
	if n.subs[key] == nil {
		n.subs[key] = map[chan struct{}]struct{}{}
	}
	n.subs[key][w.c] = struct{}{}
	return w
}

// Close ends the subscription.
func (w *Waiter) Close() {
	if w.n == nil {
		return
	}
	w.n.mu.Lock()
	defer w.n.mu.Unlock()
	delete(w.n.subs[w.key], w.c)
	if len(w.n.subs[w.key]) == 0 {
		delete(w.n.subs, w.key)
	}
}

// Wait returns true on a signal or after the fallback interval, false once ctx ends.
func (w *Waiter) Wait(ctx context.Context) bool {
	t := time.NewTimer(w.fallback)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-w.c:
	case <-t.C:
	}
	return true
}

// signal wakes the waiters of key, or every waiter if key is "".
func (n *Notifier) signal(key string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	wake := func(cs map[chan struct{}]struct{}) {
		for c := range cs {
			select {
			case c <- struct{}{}:
			default: // already signalled: coalesced
			}
		}
	}
	if key != "" {
		wake(n.subs[key])
		return
	}
	for _, cs := range n.subs {
		wake(cs)
	}
}

// Start listens until ctx ends, reconnecting with backoff (1 s doubling to 30 s); wait returns
// once it has stopped.
func (n *Notifier) Start(ctx context.Context) (wait func()) {
	var wg sync.WaitGroup
	wg.Go(func() {
		backoff := time.Second
		for {
			connected, err := n.listen(ctx)
			if ctx.Err() != nil {
				return
			}
			if connected {
				backoff = time.Second
			}
			n.Log.Warn("notify: listener connection lost, reconnecting", "err", err, "in", backoff)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = min(2*backoff, 30*time.Second)
		}
	})
	return wg.Wait
}

// listen connects, LISTENs and fans notifications out until the connection or ctx ends.
func (n *Notifier) listen(ctx context.Context) (connected bool, err error) {
	cfg := n.DB.Copy()
	cfg.RuntimeParams["application_name"] = "opsolete-notify"
	// it only reads: a vanished server (node lost, no FIN) shows as an error in ~25 s, not hours
	cfg.DialFunc = (&net.Dialer{Timeout: 10 * time.Second,
		KeepAliveConfig: net.KeepAliveConfig{Enable: true, Idle: 10 * time.Second, Interval: 5 * time.Second, Count: 3}}).DialContext
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return false, err
	}
	defer conn.Close(context.Background())
	for _, ch := range []string{storage.JobsChannel, storage.RunsChannel} {
		if _, err := conn.Exec(ctx, "LISTEN "+ch); err != nil {
			return false, err
		}
	}
	n.Log.Info("notify: listening", "channels", []string{storage.JobsChannel, storage.RunsChannel})
	n.signal("") // whatever happened while not listening
	for {
		m, err := conn.WaitForNotification(ctx)
		if err != nil {
			return true, err
		}
		n.signal(m.Channel + ":" + m.Payload)
	}
}
