package storage

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/rudesome/form2stack/internal/runnerapi"
)

// ErrLeaseLost means the caller no longer holds the job: it expired, finished or was never claimed.
var ErrLeaseLost = errors.New("job lease lost")

// Job is one phase of a run, as handed to a runner.
type Job struct {
	RunID        string
	WorkspaceID  string
	Phase        string // plan | apply
	Action       string // apply | destroy
	LeaseID      string
	ModuleSource string
	ModuleRef    string
	Inputs       map[string]any
}

// ClaimJob hands the longest-waiting queued phase (plan or confirmed apply) to runner, or returns
// ok=false if there is none. SKIP LOCKED lets any number of runners claim without handing out a job twice.
func (s *Store) ClaimJob(ctx context.Context, runner string, lease time.Duration) (Job, bool, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Job{}, false, err
	}
	defer tx.Rollback()
	j := Job{LeaseID: rand.Text()}
	var status string
	err = tx.QueryRowContext(ctx, `
		UPDATE runs SET status = CASE status WHEN 'plan_queued' THEN 'planning' ELSE 'applying' END,
			runner = $1, lease_id = $2, lease_until = now() + make_interval(secs => $3), updated_at = now()
		WHERE id = (SELECT id FROM runs WHERE status IN ('plan_queued', 'apply_queued')
			ORDER BY updated_at FOR UPDATE SKIP LOCKED LIMIT 1)
		RETURNING id, workspace_id, action, status`,
		runner, j.LeaseID, lease.Seconds()).Scan(&j.RunID, &j.WorkspaceID, &j.Action, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, false, nil
	}
	if err != nil {
		return Job{}, false, err
	}
	j.Phase = "plan"
	if status == Applying {
		j.Phase = "apply"
	}
	var raw []byte
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(module_source, ''), module_ref, inputs FROM workspaces WHERE id = $1`,
		j.WorkspaceID).Scan(&j.ModuleSource, &j.ModuleRef, &raw); err != nil {
		return Job{}, false, err
	}
	if err := json.Unmarshal(raw, &j.Inputs); err != nil {
		return Job{}, false, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO run_status_history (run_id, status, message)
		VALUES ($1, $2, $3 || ' claimed by runner ' || $4)`, j.RunID, status, j.Phase, runner); err != nil {
		return Job{}, false, err
	}
	return j, true, tx.Commit()
}

const working = `status IN ('planning', 'applying')`

// Heartbeat extends the lease of a running job.
func (s *Store) Heartbeat(ctx context.Context, runID, lease string, ttl time.Duration) error {
	res, err := s.DB.ExecContext(ctx, `UPDATE runs SET lease_until = now() + make_interval(secs => $3)
		WHERE id::text = $1 AND lease_id = $2 AND `+working, runID, lease, ttl.Seconds())
	return leaseResult(res, err)
}

// HoldsLease reports whether lease is the current lease of a run in the given phase status.
func (s *Store) HoldsLease(ctx context.Context, runID, lease, status string) (bool, error) {
	var ok bool
	err := s.DB.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM runs WHERE id::text = $1 AND lease_id = $2 AND status = $3)`,
		runID, lease, status).Scan(&ok)
	return ok, err
}

// AppendLog stores a chunk of job output and extends the lease. Past maxBytes the log is
// truncated (with one marker line) but the job keeps running.
func (s *Store) AppendLog(ctx context.Context, runID, lease, chunk string, ttl time.Duration, maxBytes int64) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var total int64
	err = tx.QueryRowContext(ctx, `UPDATE runs SET lease_until = now() + make_interval(secs => $3),
		log_bytes = log_bytes + $4 WHERE id::text = $1 AND lease_id = $2 AND `+working+` RETURNING log_bytes`,
		runID, lease, ttl.Seconds(), len(chunk)).Scan(&total)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrLeaseLost
	}
	if err != nil {
		return err
	}
	before := total - int64(len(chunk))
	switch {
	case total <= maxBytes:
	case before <= maxBytes:
		chunk = "\n[form2stack: log truncated, limit reached]\n"
	default:
		return tx.Commit()
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO run_logs (run_id, chunk) VALUES ($1, $2)`, runID, chunk); err != nil {
		return err
	}
	return tx.Commit()
}

// FinishJob records the result of a phase, releases the lease and the workspace's state lock, and
// returns the run's new status:
//   - plan with changes: needs_confirmation, or apply_queued for apply runs of auto-apply workspaces
//     (destroys always wait for a human); without changes: no_changes
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
	var autoApply bool
	err = tx.QueryRowContext(ctx, `SELECT r.status, r.action, r.workspace_id, w.auto_apply FROM runs r
		JOIN workspaces w ON w.id = r.workspace_id
		WHERE r.id::text = $1 AND r.lease_id = $2 AND r.`+working+` FOR UPDATE OF r`, runID, lease).
		Scan(&status, &action, &workspaceID, &autoApply)
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
	case autoApply && action == "apply":
		final = ApplyQueued
	default:
		final = NeedsConfirmation
	}
	var planJSON, outJSON []byte
	if plan != nil {
		if planJSON, err = json.Marshal(plan); err != nil {
			return "", err
		}
	}
	if final == Applied && action == "apply" {
		if outJSON, err = json.Marshal(outputs); err != nil {
			return "", err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE runs SET status = $2, plan = COALESCE($3::jsonb, plan),
		outputs = COALESCE($4::jsonb, outputs), lease_id = NULL, lease_until = NULL, updated_at = now() WHERE id::text = $1`,
		runID, final, nullJSON(planJSON), nullJSON(outJSON)); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE workspaces SET state_lock = NULL WHERE id = $1`, workspaceID); err != nil {
		return "", err
	}
	if final == ApplyQueued && message == "" {
		message = "auto-apply: waiting for a runner"
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO run_status_history (run_id, status, message)
		VALUES ($1::uuid, $2, NULLIF($3, ''))`, runID, final, message); err != nil {
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

// ReapExpiredLeases errors runs whose runner stopped heartbeating, and releases their state locks.
// Runs are not retried: a half-applied terraform run needs a human to look at it.
func (s *Store) ReapExpiredLeases(ctx context.Context) ([]string, error) {
	rows, err := s.DB.QueryContext(ctx, `
		WITH r AS (
			UPDATE runs SET status = 'errored', lease_id = NULL, lease_until = NULL, updated_at = now()
			WHERE `+working+` AND lease_id IS NOT NULL AND lease_until < now() RETURNING id, workspace_id
		), unlocked AS (
			UPDATE workspaces SET state_lock = NULL WHERE id IN (SELECT workspace_id FROM r)
		)
		INSERT INTO run_status_history (run_id, status, message)
		SELECT id, 'errored', 'runner stopped responding (lease expired)' FROM r RETURNING run_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

type LogChunk struct {
	Seq   int64
	Chunk string
}

// Logs returns up to limit chunks of a run's log after seq.
func (s *Store) Logs(ctx context.Context, runID string, after int64, limit int) ([]LogChunk, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT seq, chunk FROM run_logs
		WHERE run_id::text = $1 AND seq > $2 ORDER BY seq LIMIT $3`, runID, after, limit)
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

func leaseResult(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrLeaseLost
	}
	return nil
}
