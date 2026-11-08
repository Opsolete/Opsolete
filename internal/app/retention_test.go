package app

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

// oldRun inserts a run of ws that ended (or, active, was last updated) daysAgo, created a minute
// before that, with one log chunk. pr 0: no pull request.
func oldRun(t *testing.T, a *App, ws uuid.UUID, action, status string, pr, daysAgo int) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := a.Store.Pool.QueryRow(context.Background(), `INSERT INTO runs (workspace_id, action, status, vcs_pull_request, log_bytes, created_at, updated_at)
		VALUES ($1, $2, $3, NULLIF($4, 0), 4, now() - make_interval(days => $5) - interval '1 minute', now() - make_interval(days => $5)) RETURNING id`,
		ws, action, status, pr, daysAgo).Scan(&id)
	if err == nil {
		_, err = a.Store.Pool.Exec(context.Background(), `INSERT INTO run_logs (run_id, chunk) VALUES ($1, 'line')`, id)
	}
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// runLeft reports whether the run still exists, and whether its log does and log_removed_at is set.
func runLeft(t *testing.T, a *App, id uuid.UUID) (exists, logged, marked bool) {
	t.Helper()
	err := a.Store.Pool.QueryRow(context.Background(), `SELECT EXISTS (SELECT 1 FROM runs WHERE id = $1),
		EXISTS (SELECT 1 FROM run_logs WHERE run_id = $1), EXISTS (SELECT 1 FROM runs WHERE id = $1 AND log_removed_at IS NOT NULL)`, id).
		Scan(&exists, &logged, &marked)
	if err != nil {
		t.Fatal(err)
	}
	return
}

func TestRetention(t *testing.T) {
	a, ctx := testApp(t), context.Background()
	ws, other := emptyWorkspace(t, a, user(t, a)).ID, emptyWorkspace(t, a, user(t, a)).ID
	applied91 := oldRun(t, a, ws, "apply", "applied", 0, 91)
	applied89 := oldRun(t, a, ws, "apply", "errored", 0, 89)
	destroyed := oldRun(t, a, ws, "destroy", "applied", 0, 400)
	refreshed := oldRun(t, a, ws, "refresh", "no_changes", 0, 400)
	active := oldRun(t, a, ws, "apply", "needs_confirmation", 0, 400)
	activeAssess := oldRun(t, a, other, "assess", "plan_queued", 0, 400)
	// drift checks: the oldest goes; the latest with a result (drift flag) and the latest (Health page) stay
	assess100 := oldRun(t, a, ws, "assess", "drifted", 0, 100)
	assess60 := oldRun(t, a, ws, "assess", "no_changes", 0, 60)
	assess40 := oldRun(t, a, ws, "assess", "errored", 0, 40)
	assessOther := oldRun(t, a, other, "assess", "drifted", 0, 300) // newer than the active (old) one: its latest
	// speculative plans: a pull request's latest stays, plans run by hand go
	pr7old := oldRun(t, a, ws, "plan", "planned", 7, 50)
	pr7 := oldRun(t, a, ws, "plan", "planned", 7, 40)
	pr8 := oldRun(t, a, ws, "plan", "no_changes", 8, 40)
	pr8other := oldRun(t, a, other, "plan", "planned", 8, 40) // another workspace's pull request 8
	manual := oldRun(t, a, ws, "plan", "planned", 0, 31)
	manualYoung := oldRun(t, a, ws, "plan", "planned", 0, 29)
	// canceled while queued: it never had a log, so none was removed either
	unlogged := oldRun(t, a, ws, "refresh", "canceled", 0, 200)
	for _, q := range []string{`DELETE FROM run_logs WHERE run_id = $1`, `UPDATE runs SET log_bytes = 0 WHERE id = $1`} {
		if _, err := a.Store.Pool.Exec(ctx, q, unlogged); err != nil {
			t.Fatal(err)
		}
	}

	// RUN_LOG_RETENTION_DAYS=0: no log is removed, speculative runs are pruned anyway
	p, err := a.Retention(ctx)
	if err != nil || p.Logs != 0 || p.Runs != 3 {
		t.Fatalf("retention without log retention: %+v %v", p, err)
	}
	if _, logged, marked := runLeft(t, a, applied91); !logged || marked {
		t.Fatal("RUN_LOG_RETENTION_DAYS=0 must keep every log")
	}
	for id, want := range map[uuid.UUID]bool{assess100: false, assess60: true, assess40: true, assessOther: true, activeAssess: true,
		pr7old: false, pr7: true, pr8: true, pr8other: true, manual: false, manualYoung: true} {
		if exists, _, _ := runLeft(t, a, id); exists != want {
			t.Errorf("run %s kept %v, want %v", id, exists, want)
		}
	}
	if l, err := a.Store.LatestAssessment(ctx, ws); err != nil || l.ID != assess40 {
		t.Errorf("latest assessment: %v %v", l.ID, err)
	}
	if w, err := a.Store.GetWorkspaceByID(ctx, other); err != nil || !w.Drifted {
		t.Errorf("the drift flag must survive pruning: %+v %v", w.Drifted, err)
	}

	a.RunLogRetention = 90 * 24 * time.Hour
	if p, err := a.Retention(ctx); err != nil || p.Runs != 0 || p.Logs != 4 {
		t.Fatalf("retention: %+v %v", p, err)
	}
	for id, want := range map[uuid.UUID]bool{applied91: false, applied89: true, destroyed: false, refreshed: false, active: true, activeAssess: true} {
		exists, logged, marked := runLeft(t, a, id)
		if !exists || logged != want || marked == want {
			t.Errorf("run %s: exists %v, log kept %v (want %v), marked %v", id, exists, logged, want, marked)
		}
	}
	r, _, err := a.Store.GetRun(ctx, applied91)
	if err != nil || r.LogRemoved() != "The log was removed 91 days after the run ended (RUN_LOG_RETENTION_DAYS)." {
		t.Fatalf("log removed line: %q %v", r.LogRemoved(), err)
	}
	if r, _, err := a.Store.GetRun(ctx, unlogged); err != nil || r.LogRemoved() != "" {
		t.Fatalf("a run that never had a log must not say its log was removed: %q %v", r.LogRemoved(), err)
	}
	if p, err := a.Retention(ctx); err != nil || p != (Pruned{}) {
		t.Fatalf("a second pass has nothing to do: %+v %v", p, err)
	}
}

// More runs than one statement handles: the batches go on until one is short.
func TestRetentionBatches(t *testing.T) {
	a, ctx := testApp(t), context.Background()
	ws := emptyWorkspace(t, a, user(t, a)).ID
	if _, err := a.Store.Pool.Exec(ctx, `WITH r AS (INSERT INTO runs (workspace_id, action, status, log_bytes, created_at, updated_at)
		SELECT $1, a, 'no_changes', 4, now() - interval '100 days' + g * interval '1 second', now() - interval '100 days'
		FROM generate_series(1, 1200) g, unnest(ARRAY['apply', 'assess']) a
		RETURNING id) INSERT INTO run_logs (run_id, chunk) SELECT id, 'line' FROM r`, ws); err != nil {
		t.Fatal(err)
	}
	a.RunLogRetention = 90 * 24 * time.Hour
	if p, err := a.Retention(ctx); err != nil || p.Logs != 2400 || p.Runs != 1199 {
		t.Fatalf("batched retention: %+v %v", p, err)
	}
	var logs int
	a.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM run_logs`).Scan(&logs)
	if logs != 0 {
		t.Fatalf("%d log chunks left", logs)
	}
}
