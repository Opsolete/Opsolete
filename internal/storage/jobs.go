package storage

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// ErrLeaseLost means the caller no longer holds the job: it expired, finished or was never claimed.
var ErrLeaseLost = errors.New("job lease lost")

// Job is what a runner needs to execute one deployment.
type Job struct {
	DeploymentID string
	LeaseID      string
	ModuleSource string
	ModuleRef    string
	StateKey     string
	Inputs       map[string]any
}

// ClaimJob hands the oldest queued runner job to runner, or returns ok=false if there is none.
// SKIP LOCKED lets any number of runners claim concurrently without handing out a job twice.
func (s *Store) ClaimJob(ctx context.Context, runner string, lease time.Duration) (Job, bool, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Job{}, false, err
	}
	defer tx.Rollback()
	j := Job{LeaseID: rand.Text()}
	var raw []byte
	err = tx.QueryRowContext(ctx, `
		UPDATE deployments SET status = 'running', runner = $1, lease_id = $2,
			lease_until = now() + make_interval(secs => $3), updated_at = now()
		WHERE id = (SELECT id FROM deployments WHERE status = 'queued'
			ORDER BY created_at FOR UPDATE SKIP LOCKED LIMIT 1)
		RETURNING id, COALESCE(module_source, ''), module_ref, state_key, inputs`,
		runner, j.LeaseID, lease.Seconds()).Scan(&j.DeploymentID, &j.ModuleSource, &j.ModuleRef, &j.StateKey, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, false, nil
	}
	if err != nil {
		return Job{}, false, err
	}
	if err := json.Unmarshal(raw, &j.Inputs); err != nil {
		return Job{}, false, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO deployment_status_history (deployment_id, status, message)
		VALUES ($1, 'running', 'claimed by runner ' || $2)`, j.DeploymentID, runner); err != nil {
		return Job{}, false, err
	}
	return j, true, tx.Commit()
}

// Heartbeat extends the lease of a running job.
func (s *Store) Heartbeat(ctx context.Context, id, lease string, ttl time.Duration) error {
	res, err := s.DB.ExecContext(ctx, `UPDATE deployments SET lease_until = now() + make_interval(secs => $3)
		WHERE id::text = $1 AND lease_id = $2 AND status = 'running'`, id, lease, ttl.Seconds())
	return leaseResult(res, err)
}

// AppendLog stores a chunk of job output and extends the lease. Past maxBytes the log is
// truncated (with one marker line) but the job keeps running.
func (s *Store) AppendLog(ctx context.Context, id, lease, chunk string, ttl time.Duration, maxBytes int64) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var total int64
	err = tx.QueryRowContext(ctx, `UPDATE deployments SET lease_until = now() + make_interval(secs => $3),
		log_bytes = log_bytes + $4 WHERE id::text = $1 AND lease_id = $2 AND status = 'running' RETURNING log_bytes`,
		id, lease, ttl.Seconds(), len(chunk)).Scan(&total)
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
	if _, err := tx.ExecContext(ctx, `INSERT INTO deployment_logs (deployment_id, chunk) VALUES ($1, $2)`, id, chunk); err != nil {
		return err
	}
	return tx.Commit()
}

// FinishJob records the result of a job and releases its lease.
func (s *Store) FinishJob(ctx context.Context, id, lease, status, message string, outputs map[string]any) error {
	out, err := json.Marshal(outputs)
	if err != nil {
		return err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE deployments SET status = $3, outputs = $4, lease_id = NULL, lease_until = NULL,
		updated_at = now() WHERE id::text = $1 AND lease_id = $2 AND status = 'running'`, id, lease, status, out)
	if err := leaseResult(res, err); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO deployment_status_history (deployment_id, status, message)
		VALUES ($1, $2, NULLIF($3, ''))`, id, status, message); err != nil {
		return err
	}
	return tx.Commit()
}

// ReapExpiredLeases fails running jobs whose runner stopped heartbeating. Jobs are not retried:
// a half-applied terraform run needs a human to look at it.
func (s *Store) ReapExpiredLeases(ctx context.Context) ([]string, error) {
	rows, err := s.DB.QueryContext(ctx, `
		WITH r AS (
			UPDATE deployments SET status = 'failed', lease_id = NULL, lease_until = NULL, updated_at = now()
			WHERE status = 'running' AND lease_id IS NOT NULL AND lease_until < now() RETURNING id
		)
		INSERT INTO deployment_status_history (deployment_id, status, message)
		SELECT id, 'failed', 'runner stopped responding (lease expired)' FROM r RETURNING deployment_id`)
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

// Logs returns up to limit chunks of a deployment's log after seq.
func (s *Store) Logs(ctx context.Context, id string, after int64, limit int) ([]LogChunk, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT seq, chunk FROM deployment_logs
		WHERE deployment_id::text = $1 AND seq > $2 ORDER BY seq LIMIT $3`, id, after, limit)
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
