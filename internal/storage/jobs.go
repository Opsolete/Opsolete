package storage

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/opsolete/opsolete/internal/runnerapi"
)

// ErrLeaseLost means the caller no longer holds the job: it expired, finished or was never claimed.
var ErrLeaseLost = errors.New("job lease lost")

// Job is one phase of a run, as handed to a runner.
type Job struct {
	RunID       string
	WorkspaceID string
	Phase       string // plan | apply
	Action      string // apply | destroy | refresh | assess | plan
	Replace     []string
	Target      []string // -target: module.<name> of a destroy run that removes that template
	LeaseID     string
	VCS         bool                // the root configuration comes from the workspace's VCS branch
	VCSCommit   string              // commit pinned when the run was queued (VCS-triggered runs), or by an earlier phase
	Modules     []WorkspaceTemplate // templates added to the workspace
}

// ClaimJob hands the longest-waiting queued phase (plan or confirmed apply) to runner, or returns
// ok=false if there is none. SKIP LOCKED lets any number of runners claim without handing out a
// job twice. Runs of a locked workspace (a lock from the UI, the CLI or a rollback) wait until it
// is unlocked; speculative plans and drift checks, never applied, go ahead
// without taking the state lock.
func (s *Store) ClaimJob(ctx context.Context, runner string, lease time.Duration) (Job, bool, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Job{}, false, err
	}
	defer tx.Rollback()
	j := Job{LeaseID: rand.Text()}
	var status string
	err = tx.QueryRowContext(ctx, `SELECT id, status FROM runs r WHERE status IN ('plan_queued', 'apply_queued')
		AND (r.action IN ('plan', 'assess') OR NOT EXISTS (SELECT 1 FROM workspaces w WHERE w.id = r.workspace_id AND w.state_lock IS NOT NULL))
		ORDER BY updated_at FOR UPDATE OF r SKIP LOCKED LIMIT 1`).Scan(&j.RunID, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, false, nil
	}
	if err != nil {
		return Job{}, false, err
	}
	j.Phase, status = "plan", map[string]string{PlanQueued: Planning, ApplyQueued: Applying}[status]
	if status == Applying {
		j.Phase = "apply"
	}
	if _, err := setStatus(ctx, tx, j.RunID, status, j.Phase+" claimed by runner "+runner); err != nil {
		return Job{}, false, err
	}
	var replace []byte
	var removeTemplate string
	if err := tx.QueryRowContext(ctx, `UPDATE runs SET runner = $2, lease_id = $3, lease_until = now() + make_interval(secs => $4)
		WHERE id = $1::uuid RETURNING workspace_id, action, replace, COALESCE(vcs_commit, ''), COALESCE(remove_template, '')`,
		j.RunID, runner, j.LeaseID, lease.Seconds()).Scan(&j.WorkspaceID, &j.Action, &replace, &j.VCSCommit, &removeTemplate); err != nil {
		return Job{}, false, err
	}
	if replace != nil {
		if err := json.Unmarshal(replace, &j.Replace); err != nil {
			return Job{}, false, err
		}
	}
	if removeTemplate != "" {
		j.Target = []string{"module." + removeTemplate}
	}
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM workspace_vcs WHERE workspace_id = $1)`,
		j.WorkspaceID).Scan(&j.VCS); err != nil {
		return Job{}, false, err
	}
	if j.Modules, err = workspaceTemplates(ctx, tx, j.WorkspaceID); err != nil {
		return Job{}, false, err
	}
	return j, true, tx.Commit()
}

// Heartbeat extends the lease of a running job. cancel reports that a user asked to cancel it.
func (s *Store) Heartbeat(ctx context.Context, runID, lease string, ttl time.Duration) (cancel bool, err error) {
	err = s.DB.QueryRowContext(ctx, `UPDATE runs SET lease_until = now() + make_interval(secs => $3)
		WHERE id = ops_uuid($1) AND lease_id = $2 AND `+working+` RETURNING cancel_requested`, runID, lease, ttl.Seconds()).Scan(&cancel)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrLeaseLost
	}
	return cancel, err
}

// HoldsLease reports whether lease is the current lease of a run in the given phase status.
func (s *Store) HoldsLease(ctx context.Context, runID, lease, status string) (bool, error) {
	var ok bool
	err := s.DB.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM runs WHERE id = ops_uuid($1) AND lease_id = $2 AND status = $3)`,
		runID, lease, status).Scan(&ok)
	return ok, err
}

// AppendLog stores a chunk of job output and extends the lease. Past maxBytes the log is
// truncated (with one marker line) but the job keeps running. cancel is as for Heartbeat.
func (s *Store) AppendLog(ctx context.Context, runID, lease, chunk string, ttl time.Duration, maxBytes int64) (cancel bool, err error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var total int64
	err = tx.QueryRowContext(ctx, `UPDATE runs SET lease_until = now() + make_interval(secs => $3),
		log_bytes = log_bytes + $4 WHERE id = ops_uuid($1) AND lease_id = $2 AND `+working+` RETURNING log_bytes, cancel_requested`,
		runID, lease, ttl.Seconds(), len(chunk)).Scan(&total, &cancel)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrLeaseLost
	}
	if err != nil {
		return false, err
	}
	before := total - int64(len(chunk))
	switch {
	case total <= maxBytes:
	case before <= maxBytes:
		chunk = "\n[opsolete: log truncated, limit reached]\n"
	default:
		return cancel, tx.Commit()
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO run_logs (run_id, chunk) VALUES ($1, $2)`, runID, chunk); err != nil {
		return false, err
	}
	return cancel, tx.Commit()
}

// CancelRun cancels a queued run at once. A run a runner works on is asked to stop: the runner
// interrupts Terraform, which still saves state, and reports it canceled. force cancels it right
// away instead and fences the runner off (its state password dies with the run), for runners
// that don't react; resources being created at that moment may then be missing from state.
// It returns the run's new status.
func (s *Store) CancelRun(ctx context.Context, id, email string, force bool) (string, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var status string
	err = tx.QueryRowContext(ctx, `SELECT status FROM runs WHERE id = ops_uuid($1) FOR UPDATE`, id).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotCancelable
	}
	if err != nil {
		return "", err
	}
	busy := status == Planning || status == Applying
	if busy && !force {
		if _, err := tx.ExecContext(ctx, `UPDATE runs SET cancel_requested = true WHERE id = ops_uuid($1)`, id); err != nil {
			return "", err
		}
		// a note, not a transition: the run ends when its runner reports it canceled
		if err := addHistory(ctx, tx, id, status, "cancel requested by "+email+": waiting for Terraform to stop and save state"); err != nil {
			return "", err
		}
		return status, tx.Commit()
	}
	msg := "canceled by " + email
	if busy {
		msg = "force-canceled by " + email + "; the runner was cut off, check the state for resources it may have missed"
	}
	if _, err := setStatus(ctx, tx, id, Canceled, msg); errors.Is(err, ErrTransition) {
		return "", ErrNotCancelable
	} else if err != nil {
		return "", err
	}
	return Canceled, tx.Commit()
}

// FinishJob records the result of a phase, releases the lease and the workspace's state lock, and
// returns the run's new status:
//   - plan with changes: needs_confirmation, or apply_queued for apply runs of auto-apply workspaces
//     (destroys, refresh-only runs and template rollouts always wait for a human), or drifted for assess runs and
//     planned for speculative plans, which are never applied; without changes: no_changes
//   - apply: applied
//   - failed / canceled: errored / canceled
//
// resources is the index of the workspace's current state version (apply phase only).
func (s *Store) FinishJob(ctx context.Context, runID, lease, result, message string, outputs map[string]any,
	plan *runnerapi.Plan, resources []runnerapi.Resource) (string, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var status, action, workspaceID string
	var autoApply, confirm, cancelRequested bool
	// the lease is checked here; setStatus then checks the transition
	err = tx.QueryRowContext(ctx, `SELECT r.status, r.action, r.workspace_id, w.auto_apply, r.rollout_id IS NOT NULL, r.cancel_requested FROM runs r
		JOIN workspaces w ON w.id = r.workspace_id
		WHERE r.id = ops_uuid($1) AND r.lease_id = $2 AND r.`+working+` FOR UPDATE OF r`, runID, lease).
		Scan(&status, &action, &workspaceID, &autoApply, &confirm, &cancelRequested)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrLeaseLost
	}
	if err != nil {
		return "", err
	}
	final := map[string]string{"failed": Errored, "canceled": Canceled}[result]
	switch {
	case final != "":
	case status == Applying:
		final = Applied
	case plan == nil:
		final = NoChanges
	case action == "assess":
		final = Drifted
	case action == "plan":
		final = Planned
	case autoApply && action == "apply" && !confirm: // confirm: a template rollout's run (see template_rollouts)
		final = ApplyQueued
	default:
		final = NeedsConfirmation
	}
	// The plan finished before a cancel reached Terraform: end the run here, or the request still
	// standing would interrupt its apply.
	if cancelRequested && (final == NeedsConfirmation || final == ApplyQueued) {
		final, message = Canceled, "canceled: the plan finished before the cancel request reached the runner"
	}
	var planJSON, outJSON []byte
	if plan != nil {
		if planJSON, err = json.Marshal(plan); err != nil {
			return "", err
		}
	}
	if final == Applied && action != "destroy" {
		if outJSON, err = json.Marshal(outputs); err != nil {
			return "", err
		}
	}
	if final == ApplyQueued && message == "" {
		message = "auto-apply: waiting for a runner"
	}
	if _, err := setStatus(ctx, tx, runID, final, message); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE runs SET plan = COALESCE($2::jsonb, plan), outputs = COALESCE($3::jsonb, outputs)
		WHERE id = $1::uuid`, runID, nullJSON(planJSON), nullJSON(outJSON)); err != nil {
		return "", err
	}
	if resources != nil {
		if err := indexState(ctx, tx, workspaceID, resources); err != nil {
			return "", err
		}
	}
	return final, tx.Commit()
}

func nullJSON(b []byte) any {
	if b == nil {
		return nil
	}
	return string(b)
}

// ReapExpiredLeases errors runs whose runner stopped heartbeating (setStatus releases their state
// locks) and returns their ids. Runs are not retried: a half-applied terraform run needs a human to
// look at it. SKIP LOCKED: a run being finished or canceled right now isn't waited for.
func (s *Store) ReapExpiredLeases(ctx context.Context) ([]string, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT id FROM runs WHERE `+working+` AND lease_id IS NOT NULL AND lease_until < now()
		FOR UPDATE SKIP LOCKED`)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, id := range ids {
		if _, err := setStatus(ctx, tx, id, Errored, "runner stopped responding (lease expired)"); err != nil {
			return nil, err
		}
	}
	return ids, tx.Commit()
}

type LogChunk struct {
	Seq   int64
	Chunk string
}

// Logs returns up to limit chunks of a run's log after seq.
func (s *Store) Logs(ctx context.Context, runID string, after int64, limit int) ([]LogChunk, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT seq, chunk FROM run_logs
		WHERE run_id = ops_uuid($1) AND seq > $2 ORDER BY seq LIMIT $3`, runID, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LogChunk
	for rows.Next() {
		var c LogChunk
		if err := rows.Scan(&c.Seq, &c.Chunk); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Stats is what the metrics endpoint reports from the database.
type Stats struct {
	Runs         map[string]int // runs by status, all time
	OldestQueued time.Duration  // age of the oldest queued job, 0 without one
	Workspaces   int
}

func (s *Store) Stats(ctx context.Context) (Stats, error) {
	st := Stats{Runs: map[string]int{}}
	var oldest float64
	if err := s.DB.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM workspaces),
		COALESCE((SELECT EXTRACT(EPOCH FROM now() - min(updated_at)) FROM runs WHERE status IN ('plan_queued', 'apply_queued')), 0)`).
		Scan(&st.Workspaces, &oldest); err != nil {
		return st, err
	}
	st.OldestQueued = time.Duration(oldest * float64(time.Second))
	rows, err := s.DB.QueryContext(ctx, `SELECT status, count(*) FROM runs GROUP BY status`)
	if err != nil {
		return st, err
	}
	defer rows.Close()
	for rows.Next() {
		var status string
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			return st, err
		}
		st.Runs[status] = n
	}
	return st, rows.Err()
}
