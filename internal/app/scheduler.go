package app

import (
	"context"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// Background work runs in one Scheduler per server replica. Its jobs run only on the leader: the
// replica holding a session advisory lock on its own connection. Postgres releases the lock when that
// session ends (crash, lost connection, shutdown), and another replica takes over at its next try.
// Local jobs keep per-replica state (the in-memory template catalog) and run on every replica.

// schedulerLock is the advisory lock id of the leader ("opsolete" in ASCII).
const schedulerLock int64 = 0x6f70736f6c657465

// Job is a named piece of background work, run every Every; a run never overlaps the previous one.
type Job struct {
	Name  string
	Every time.Duration
	Local bool // run on every replica, not only the leader
	Run   func(context.Context) error
}

type Scheduler struct {
	Jobs   []Job
	DB     *pgx.ConnConfig // for the lock's own connection: a session lock belongs to one connection, not the pool
	Retry  time.Duration   // how often a follower tries to lead and the leader checks its lock connection
	Log    *slog.Logger
	OnLead func(leading bool) // optional: called on every leadership change
}

// Start runs the jobs until ctx ends; wait returns once every job has stopped and the lock is released.
func (s *Scheduler) Start(ctx context.Context) (wait func()) {
	var local, led []Job
	for _, j := range s.Jobs {
		if j.Local {
			local = append(local, j)
		} else {
			led = append(led, j)
		}
	}
	var wg sync.WaitGroup
	wg.Go(func() { runJobs(ctx, s.Log, local) })
	if len(led) > 0 {
		wg.Go(func() { s.lead(ctx, led) })
	}
	return wg.Wait
}

// lead tries to take the lock every Retry; while it holds it, it runs jobs and pings the lock's
// connection every Retry. A failed ping cancels the jobs' context, waits for them and follows again.
func (s *Scheduler) lead(ctx context.Context, jobs []Job) {
	var conn *pgx.Conn
	defer func() { // ends the session, which releases the lock
		if conn != nil {
			conn.Close(context.Background())
		}
	}()
	leading := false
	setLeading := func(on bool) {
		leading = on
		if s.OnLead != nil {
			s.OnLead(on)
		}
	}
	var follow func() // as leader: cancels the jobs and waits for them
	t := time.NewTicker(s.Retry)
	defer t.Stop()
	for {
		holds, err := s.hold(ctx, &conn, leading)
		if err != nil && ctx.Err() == nil {
			s.Log.Warn("scheduler: lock connection", "err", err)
		}
		switch {
		case holds && !leading:
			s.Log.Info("scheduler: this replica leads, running background jobs")
			jctx, stop := context.WithCancel(ctx)
			done := make(chan struct{})
			go func() { defer close(done); runJobs(jctx, s.Log, jobs) }()
			follow = func() { stop(); <-done; setLeading(false) }
			setLeading(true)
		case !holds && leading:
			s.Log.Warn("scheduler: lost leadership, background jobs stopped")
			follow()
		}
		select {
		case <-ctx.Done():
			if leading {
				follow()
			}
			return
		case <-t.C:
		}
	}
}

// hold reports whether this replica holds the lock: as leader, whether its connection still works;
// as follower, whether it took the lock. A connection error closes the connection (and so the lock).
func (s *Scheduler) hold(ctx context.Context, conn **pgx.Conn, leading bool) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, s.Retry)
	defer cancel()
	if *conn == nil {
		cfg := s.DB.Copy()
		cfg.RuntimeParams["application_name"] = "opsolete-scheduler"
		// Postgres notices a vanished leader (node lost, no FIN) in ~25 s instead of the OS default of hours.
		cfg.RuntimeParams["tcp_keepalives_idle"], cfg.RuntimeParams["tcp_keepalives_interval"], cfg.RuntimeParams["tcp_keepalives_count"] = "10", "5", "3"
		c, err := pgx.ConnectConfig(ctx, cfg)
		if err != nil {
			return false, err
		}
		*conn = c
	}
	var err error
	ok := leading
	if leading {
		err = (*conn).Ping(ctx)
	} else {
		err = (*conn).QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", schedulerLock).Scan(&ok)
	}
	if err != nil {
		(*conn).Close(context.Background())
		*conn = nil
		return false, err
	}
	return ok, nil
}

// runJobs runs each job now and then every Every, until ctx ends, and returns when all have stopped.
func runJobs(ctx context.Context, log *slog.Logger, jobs []Job) {
	var wg sync.WaitGroup
	for _, j := range jobs {
		wg.Go(func() {
			t := time.NewTicker(j.Every)
			defer t.Stop()
			for ctx.Err() == nil {
				runJob(ctx, log, j)
				select {
				case <-ctx.Done():
				case <-t.C:
				}
			}
		})
	}
	wg.Wait()
}

func runJob(ctx context.Context, log *slog.Logger, j Job) {
	defer func() {
		if r := recover(); r != nil {
			log.Error("background job panicked", "job", j.Name, "panic", r, "stack", string(debug.Stack()))
		}
	}()
	if err := j.Run(ctx); err != nil && ctx.Err() == nil {
		log.Error("background job failed", "job", j.Name, "err", err)
	}
}
