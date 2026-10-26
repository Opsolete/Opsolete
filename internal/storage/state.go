package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"time"

	"github.com/opsolete/opsolete/internal/runnerapi"
)

// StateVersion is one saved Terraform state of a workspace; the file itself is in blob storage.
type StateVersion struct {
	ID        int64
	Serial    int64
	Lineage   string
	BlobKey   string
	MD5       string
	Size      int64
	Indexed   bool
	Resources int            // index size
	CopyOf    int64          // a rollback: serial of the version this is a copy of; 0 otherwise
	Outputs   map[string]any // from the state file, sensitive values masked (not read by StateVersions)
	CreatedAt time.Time
}

// WorkingRun returns the id and status (planning or applying) of the run a runner is working on
// in a workspace, or sql.ErrNoRows if none is.
func (s *Store) WorkingRun(ctx context.Context, workspaceID string) (run, status string, err error) {
	err = s.DB.QueryRowContext(ctx, `SELECT id, status FROM runs
		WHERE workspace_id = ops_uuid($1) AND `+working+` AND lease_id IS NOT NULL`, workspaceID).Scan(&run, &status)
	return run, status, err
}

// StateVersions lists a workspace's state versions, newest first.
func (s *Store) StateVersions(ctx context.Context, workspaceID string, limit int) ([]StateVersion, error) {
	return s.StateVersionsUpTo(ctx, workspaceID, math.MaxInt64, limit)
}

// StateVersionsUpTo lists a workspace's state versions with id up to and including upTo, newest first.
func (s *Store) StateVersionsUpTo(ctx context.Context, workspaceID string, upTo int64, limit int) ([]StateVersion, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT v.id, v.serial, v.lineage, v.blob_key, v.md5, v.size, v.indexed,
			(SELECT count(*) FROM state_version_resources r WHERE r.state_version_id = v.id),
			COALESCE((SELECT o.serial FROM state_versions o WHERE o.id = v.rollback_of), 0), v.created_at
		FROM state_versions v WHERE v.workspace_id = ops_uuid($1) AND v.id <= $3 ORDER BY v.id DESC LIMIT $2`, workspaceID, limit, upTo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StateVersion
	for rows.Next() {
		var v StateVersion
		if err := rows.Scan(&v.ID, &v.Serial, &v.Lineage, &v.BlobKey, &v.MD5, &v.Size, &v.Indexed, &v.Resources, &v.CopyOf, &v.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// AddStateVersion records a state file written to blob storage as the workspace's current state.
// runID is the run whose Terraform wrote it ("" for the CLI and rollbacks): if that run no longer
// applies with a lease (force-canceled, reaped), nothing is recorded and ErrLeaseLost returned.
func (s *Store) AddStateVersion(ctx context.Context, id, runID string, v StateVersion) error {
	var out []byte
	if v.Outputs != nil {
		var err error
		if out, err = json.Marshal(v.Outputs); err != nil {
			return err
		}
	}
	res, err := s.DB.ExecContext(ctx, `INSERT INTO state_versions (workspace_id, serial, lineage, blob_key, md5, size, outputs)
		SELECT $1::uuid, $2, $3, $4, $5, $6, $7::jsonb WHERE $8 = '' OR EXISTS (SELECT 1 FROM runs
			WHERE id = ops_uuid($8) AND status = 'applying' AND lease_id IS NOT NULL)`,
		id, v.Serial, v.Lineage, v.BlobKey, v.MD5, v.Size, nullJSON(out), runID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return ErrLeaseLost
	}
	return nil
}

// StateVersionBlob returns the blob key of one of a workspace's state versions; sql.ErrNoRows if unknown.
func (s *Store) StateVersionBlob(ctx context.Context, workspaceID string, id int64) (string, error) {
	var key string
	err := s.DB.QueryRowContext(ctx, `SELECT blob_key FROM state_versions WHERE workspace_id = ops_uuid($1) AND id = $2`,
		workspaceID, id).Scan(&key)
	return key, err
}

// CopyStateIndex gives the workspace's current state version the resource index of version from,
// and records it as a copy of from: a rollback.
func (s *Store) CopyStateIndex(ctx context.Context, workspaceID string, from int64) error {
	_, err := s.DB.ExecContext(ctx, `
		WITH cur AS (SELECT id FROM state_versions WHERE workspace_id = ops_uuid($1) ORDER BY id DESC LIMIT 1),
		ins AS (INSERT INTO state_version_resources (state_version_id, address, type, name, provider, module)
			SELECT cur.id, r.address, r.type, r.name, r.provider, r.module FROM cur, state_version_resources r
			WHERE r.state_version_id = $2 ON CONFLICT DO NOTHING)
		UPDATE state_versions SET indexed = (SELECT indexed FROM state_versions WHERE id = $2), rollback_of = $2
		WHERE id = (SELECT id FROM cur)`, workspaceID, from)
	return err
}

// LockState takes the workspace's state lock for the Terraform LockInfo in info. If another lock
// is held, it returns that lock's info and ok=false. Taking a lock you already hold succeeds.
// runID is the run whose Terraform takes it ("" for rollbacks and the cloud API): when the run
// ends, only a lock recorded with its id is released.
func (s *Store) LockState(ctx context.Context, id, lockID, runID string, info []byte) (held []byte, ok bool, err error) {
	err = s.DB.QueryRowContext(ctx, `
		WITH taken AS (
			UPDATE workspaces SET state_lock = ($2::jsonb - 'OpsoleteRun') || jsonb_strip_nulls(jsonb_build_object('OpsoleteRun', NULLIF($4, ''))),
				state_locked_at = CASE WHEN state_lock IS NULL THEN now() ELSE state_locked_at END
			WHERE id = ops_uuid($1) AND (state_lock IS NULL OR state_lock->>'ID' = $3) RETURNING 1
		)
		SELECT EXISTS (SELECT 1 FROM taken), COALESCE((SELECT state_lock::text FROM workspaces WHERE id = ops_uuid($1)), '')`,
		id, string(info), lockID, runID).Scan(&ok, &held)
	return held, ok, err
}

// releaseRunLock ($1 workspace id, $2 run id) drops the state lock a run's Terraform left behind,
// never a rollback's or a CLI's.
const releaseRunLock = `UPDATE workspaces SET state_lock = NULL WHERE id = $1 AND state_lock->>'OpsoleteRun' = $2::text`

// ReleaseOrphanedLocks drops state locks nobody can release anymore and returns the workspace ids
// with the released LockInfo:
//   - a run's lock once that run no longer works with a lease (it took the lock just as it was
//     reaped or force-canceled); the run is fenced off from the state, so this is safe at once
//   - a rollback's lock older than timeout (its server died mid-rollback; a rollback takes seconds)
//
// Cloud API and UI locks (CloudLockPrefix) are never touched: they are deliberate, and
// force-unlock releases them.
func (s *Store) ReleaseOrphanedLocks(ctx context.Context, timeout time.Duration) (map[string]string, error) {
	rows, err := s.DB.QueryContext(ctx, `
		WITH stale AS (
			SELECT w.id, w.state_lock FROM workspaces w WHERE w.state_lock IS NOT NULL AND CASE
				WHEN w.state_lock->>'OpsoleteRun' IS NOT NULL THEN NOT EXISTS (SELECT 1 FROM runs r
					WHERE r.id::text = w.state_lock->>'OpsoleteRun' AND r.`+working+` AND r.lease_id IS NOT NULL)
				ELSE NOT starts_with(w.state_lock->>'ID', $2)
					AND COALESCE(w.state_locked_at, '-infinity') < now() - make_interval(secs => $1)
			END FOR UPDATE
		)
		UPDATE workspaces w SET state_lock = NULL FROM stale WHERE w.id = stale.id RETURNING w.id, stale.state_lock::text`,
		timeout.Seconds(), CloudLockPrefix)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, info string
		if err := rows.Scan(&id, &info); err != nil {
			return nil, err
		}
		out[id] = info
	}
	return out, rows.Err()
}

// UnlockState releases the lock if lockID holds it.
func (s *Store) UnlockState(ctx context.Context, id, lockID string) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE workspaces SET state_lock = NULL
		WHERE id = ops_uuid($1) AND state_lock->>'ID' = $2`, id, lockID)
	return err
}

// StateLockID returns the ID of the workspace's current state lock, or "" if unlocked.
func (s *Store) StateLockID(ctx context.Context, id string) (string, error) {
	var lock sql.NullString
	err := s.DB.QueryRowContext(ctx, `SELECT state_lock->>'ID' FROM workspaces WHERE id = ops_uuid($1)`, id).Scan(&lock)
	return lock.String, err
}

// StateLock is the Terraform LockInfo of a workspace's state lock, plus Opsolete's own fields.
type StateLock struct {
	ID, Operation, Who, Created string
	Info                        string // the reason given when locking in the UI
	UserID                      string // cloud API and UI locks
	OpsoleteRun                 string // the run whose Terraform holds it
}

// Cloud: taken through the cloud API or the UI, so unlock and force-unlock release it.
func (l StateLock) Cloud() bool { return strings.HasPrefix(l.ID, CloudLockPrefix) }

// GetStateLock returns the workspace's state lock, or nil if it is unlocked.
func (s *Store) GetStateLock(ctx context.Context, id string) (*StateLock, error) {
	var raw []byte
	if err := s.DB.QueryRowContext(ctx, `SELECT state_lock FROM workspaces WHERE id = ops_uuid($1)`, id).Scan(&raw); err != nil || raw == nil {
		return nil, err
	}
	var l StateLock
	return &l, json.Unmarshal(raw, &l)
}

// CloudLockPrefix starts the ID of state locks taken through the cloud API (terraform with a cloud
// block); their lock info also carries the UserID of whoever took them.
const CloudLockPrefix = "cloud-"

// UnlockCloudState releases a lock taken through the cloud API by userID, or by anyone if userID
// is "" (force unlock). Locks of runs and rollbacks are never released here.
func (s *Store) UnlockCloudState(ctx context.Context, id, userID string) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE workspaces SET state_lock = NULL WHERE id = ops_uuid($1)
		AND starts_with(state_lock->>'ID', $3) AND ($2 = '' OR state_lock->>'UserID' = $2)`, id, userID, CloudLockPrefix)
	return err
}

// CloudLocked reports whether userID holds the workspace's state lock through the cloud API.
func (s *Store) CloudLocked(ctx context.Context, id, userID string) (bool, error) {
	var ok bool
	err := s.DB.QueryRowContext(ctx, `SELECT COALESCE(starts_with(state_lock->>'ID', $3) AND state_lock->>'UserID' = $2, false)
		FROM workspaces WHERE id = ops_uuid($1)`, id, userID, CloudLockPrefix).Scan(&ok)
	return ok, err
}

// IndexState stores resources as the index of the workspace's current state version.
func (s *Store) IndexState(ctx context.Context, id string, resources []runnerapi.Resource) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := indexState(ctx, tx, id, resources); err != nil {
		return err
	}
	return tx.Commit()
}

// indexState stores resources as the index of the workspace's current state version, which is
// what the runner read them from at the end of its run.
func indexState(ctx context.Context, tx *sql.Tx, id string, resources []runnerapi.Resource) error {
	var version int64
	err := tx.QueryRowContext(ctx, `SELECT id FROM state_versions WHERE workspace_id = ops_uuid($1) ORDER BY id DESC LIMIT 1`, id).Scan(&version)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // nothing was ever saved, so there is nothing to index
	}
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM state_version_resources WHERE state_version_id = $1`, version); err != nil {
		return err
	}
	raw, err := json.Marshal(resources)
	if err != nil {
		return err
	}
	// ON CONFLICT: a duplicate address must not lose the whole index.
	if _, err := tx.ExecContext(ctx, `INSERT INTO state_version_resources (state_version_id, address, type, name, provider, module)
		SELECT $1, address, type, name, provider, module FROM jsonb_to_recordset($2::jsonb)
			AS r(address text, type text, name text, provider text, module text)
		ON CONFLICT DO NOTHING`, version, string(raw)); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE state_versions SET indexed = true WHERE id = $1`, version)
	return err
}

// Resource is one resource in a workspace's current state, with the workspace it belongs to.
type Resource struct {
	runnerapi.Resource
	Workspace string // name
	Serial    int64  // state version the index comes from
}

// Resources lists the resources in a workspace's current state version.
// shortcut: no paging, capped at 2000 rows; add paging when inventories get that large.
func (s *Store) Resources(ctx context.Context, workspaceID string) ([]Resource, error) {
	rows, err := s.DB.QueryContext(ctx, `
		WITH cur AS (SELECT id, workspace_id, serial FROM state_versions
			WHERE workspace_id = ops_uuid($1) ORDER BY id DESC LIMIT 1)
		SELECT r.address, r.type, r.name, r.provider, r.module, w.name, cur.serial
		FROM cur JOIN state_version_resources r ON r.state_version_id = cur.id JOIN workspaces w ON w.id = cur.workspace_id
		ORDER BY r.type, r.address LIMIT 2000`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Resource
	for rows.Next() {
		var r Resource
		if err := rows.Scan(&r.Address, &r.Type, &r.Name, &r.Provider, &r.Module, &r.Workspace, &r.Serial); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
