package storage

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/opsolete/opsolete/internal/runnerapi"
	"github.com/opsolete/opsolete/internal/storage/db"
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
// in a workspace, and whether it's a pull request's speculative plan; ErrNotFound if none is.
func (s *Store) WorkingRun(ctx context.Context, workspaceID uuid.UUID) (run uuid.UUID, status string, pullRequest bool, err error) {
	r, err := s.q.WorkingRun(ctx, workspaceID)
	return r.ID, r.Status, r.PullRequest, notFound(err)
}

// StateVersions lists a workspace's state versions, newest first.
func (s *Store) StateVersions(ctx context.Context, workspaceID uuid.UUID, limit int) ([]StateVersion, error) {
	return s.StateVersionsUpTo(ctx, workspaceID, math.MaxInt64, limit)
}

// StateVersionsUpTo lists a workspace's state versions with id up to and including upTo, newest first.
func (s *Store) StateVersionsUpTo(ctx context.Context, workspaceID uuid.UUID, upTo int64, limit int) ([]StateVersion, error) {
	rows, err := s.q.StateVersions(ctx, db.StateVersionsParams{WorkspaceID: workspaceID, UpTo: upTo, Lim: int32(limit)})
	out := make([]StateVersion, 0, len(rows))
	for _, v := range rows {
		out = append(out, StateVersion{ID: v.ID, Serial: v.Serial, Lineage: v.Lineage, BlobKey: v.BlobKey, MD5: v.Md5, Size: v.Size,
			Indexed: v.Indexed, Resources: int(v.Resources), CopyOf: v.CopyOf, CreatedAt: v.CreatedAt})
	}
	return out, err
}

// ErrNotCloudLocked: a cloud API state upload whose user no longer holds the state lock.
var ErrNotCloudLocked = errors.New("the workspace must be locked by you to save state")

// ErrLockLost: a rollback's state lock was released before it recorded (it ran past
// STALE_LOCK_TIMEOUT, or someone force-unlocked): it changed nothing.
var ErrLockLost = errors.New("the rollback's state lock was released meanwhile: nothing was changed, try again")

// AddStateVersion records a state file written to blob storage as the workspace's current state.
// runID is the run whose Terraform wrote it (uuid.Nil for the CLI and rollbacks): if that run no longer
// applies with a lease (force-canceled, reaped), nothing is recorded and ErrLeaseLost returned.
// cloudUser is the user uploading through the cloud API (else ""): if they no longer hold the state
// lock, nothing is recorded and ErrNotCloudLocked returned. FOR SHARE makes both checks hold until
// commit: a force-cancel or force-unlock waits for the insert, or the insert sees its result.
func (s *Store) AddStateVersion(ctx context.Context, id, runID uuid.UUID, cloudUser string, v StateVersion) error {
	return addStateVersion(ctx, s.q, id, runID, cloudUser, "", v)
}

func addStateVersion(ctx context.Context, q *db.Queries, id, runID uuid.UUID, cloudUser, rollbackLock string, v StateVersion) error {
	var out []byte
	if v.Outputs != nil {
		var err error
		if out, err = json.Marshal(v.Outputs); err != nil {
			return err
		}
	}
	n, err := q.AddStateVersion(ctx, db.AddStateVersionParams{WorkspaceID: id, Serial: v.Serial, Lineage: v.Lineage, BlobKey: v.BlobKey,
		Md5: v.MD5, Size: v.Size, Outputs: out, RunID: optID(runID), CloudUser: cloudUser, CloudPrefix: CloudLockPrefix, RollbackLock: rollbackLock})
	switch {
	case err != nil:
		return err
	case n == 0 && cloudUser != "":
		return ErrNotCloudLocked
	case n == 0 && rollbackLock != "":
		return ErrLockLost
	case n == 0:
		return ErrLeaseLost
	}
	return nil
}

// DropIntermediateStates deletes the state versions a run saved before its last one: Terraform's
// intermediate snapshots, superseded once the run ended (as HCP Terraform does). It returns their
// blob keys, for the caller to delete.
func (s *Store) DropIntermediateStates(ctx context.Context, runID uuid.UUID) ([]string, error) {
	return s.q.DropIntermediateStates(ctx, runID)
}

// StateVersionBlob returns the blob key of one of a workspace's state versions; ErrNotFound if unknown.
func (s *Store) StateVersionBlob(ctx context.Context, workspaceID uuid.UUID, id int64) (string, error) {
	key, err := s.q.StateVersionBlob(ctx, db.StateVersionBlobParams{WorkspaceID: workspaceID, ID: id})
	return key, notFound(err)
}

// AddRollbackVersion records v, a copy of version from written to blob storage, as the workspace's
// current state with from's resource index, and as a copy of from: a rollback, all or nothing. Only
// while lockID is still the workspace's state lock: ErrLockLost otherwise.
func (s *Store) AddRollbackVersion(ctx context.Context, workspaceID uuid.UUID, from int64, lockID string, v StateVersion) error {
	return s.tx(ctx, func(q *db.Queries) error {
		if err := addStateVersion(ctx, q, workspaceID, uuid.Nil, "", lockID, v); err != nil {
			return err
		}
		return q.CopyStateIndex(ctx, db.CopyStateIndexParams{WorkspaceID: workspaceID, FromID: from})
	})
}

// LockState takes the workspace's state lock for the Terraform LockInfo in info. If another lock
// is held, it returns that lock's info and ok=false. Taking a lock you already hold succeeds.
// runID is the run whose Terraform takes it (uuid.Nil for rollbacks and the cloud API): when the run
// ends, only a lock recorded with its id is released. A held lock's ID is no credential (a 423
// shows it to the run that tried): re-taking it also needs the same run (or none).
func (s *Store) LockState(ctx context.Context, id uuid.UUID, lockID string, runID uuid.UUID, info []byte) (held []byte, ok bool, err error) {
	r, err := s.q.LockState(ctx, db.LockStateParams{ID: id, Info: info, RunID: optID(runID), LockID: lockID})
	if err != nil {
		return nil, false, err
	}
	if !r.Taken && r.Held == "" { // lost a race: the statement's snapshot predates the winner's commit
		r.Held, err = s.q.CurrentStateLock(ctx, id)
	}
	if r.Held == "" {
		return nil, r.Taken, err
	}
	return []byte(r.Held), r.Taken, err
}

// ReleaseOrphanedLocks drops state locks nobody can release anymore and returns the workspace ids
// with the released LockInfo:
//   - a run's lock once that run no longer works with a lease (it took the lock just as it was
//     reaped or force-canceled); the run is fenced off from the state, so this is safe at once
//   - a rollback's lock older than timeout (its server died mid-rollback; a rollback takes seconds)
//
// Cloud API and UI locks (CloudLockPrefix) are never touched: they are deliberate, and
// force-unlock releases them.
func (s *Store) ReleaseOrphanedLocks(ctx context.Context, timeout time.Duration) (map[uuid.UUID]string, error) {
	rows, err := s.q.ReleaseOrphanedLocks(ctx, db.ReleaseOrphanedLocksParams{CloudPrefix: CloudLockPrefix, TimeoutSecs: timeout.Seconds()})
	if err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID]string, len(rows))
	for _, r := range rows {
		out[r.ID] = r.Info
	}
	return out, nil
}

// UnlockState releases the lock if lockID holds it and runID (uuid.Nil none) took it.
func (s *Store) UnlockState(ctx context.Context, id uuid.UUID, lockID string, runID uuid.UUID) error {
	return s.q.UnlockState(ctx, db.UnlockStateParams{ID: id, LockID: lockID, RunID: optID(runID)})
}

// StateLockedBy reports whether lockID holds the workspace's state lock, taken by runID (uuid.Nil none).
func (s *Store) StateLockedBy(ctx context.Context, id uuid.UUID, lockID string, runID uuid.UUID) (bool, error) {
	return s.q.StateLockedBy(ctx, db.StateLockedByParams{ID: id, LockID: lockID, RunID: optID(runID)})
}

// StateLockID returns the ID of the workspace's current state lock, or "" if unlocked; ErrNotFound
// for an unknown workspace.
func (s *Store) StateLockID(ctx context.Context, id uuid.UUID) (string, error) {
	lock, err := s.q.StateLockID(ctx, id)
	return lock, notFound(err)
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

// GetStateLock returns the workspace's state lock, or nil if it is unlocked; ErrNotFound for an
// unknown workspace.
func (s *Store) GetStateLock(ctx context.Context, id uuid.UUID) (*StateLock, error) {
	raw, err := s.q.GetStateLock(ctx, id)
	if err != nil || raw == nil {
		return nil, notFound(err)
	}
	var l StateLock
	return &l, json.Unmarshal(raw, &l)
}

// CloudLockPrefix starts the ID of state locks taken through the cloud API (terraform with a cloud
// block); their lock info also carries the UserID of whoever took them.
const CloudLockPrefix = "cloud-"

// UnlockCloudState releases a lock taken through the cloud API by userID, or by anyone if userID
// is "" (force unlock). Locks of runs and rollbacks are never released here.
func (s *Store) UnlockCloudState(ctx context.Context, id uuid.UUID, userID string) error {
	return s.q.UnlockCloudState(ctx, db.UnlockCloudStateParams{ID: id, UserID: userID, CloudPrefix: CloudLockPrefix})
}

// CloudLocked reports whether userID holds the workspace's state lock through the cloud API.
func (s *Store) CloudLocked(ctx context.Context, id uuid.UUID, userID string) (bool, error) {
	ok, err := s.q.CloudLocked(ctx, db.CloudLockedParams{ID: id, UserID: userID, CloudPrefix: CloudLockPrefix})
	return ok, notFound(err)
}

// IndexState stores resources as the index of the workspace's current state version.
func (s *Store) IndexState(ctx context.Context, id uuid.UUID, resources []runnerapi.Resource) error {
	return s.tx(ctx, func(q *db.Queries) error { return indexState(ctx, q, id, resources) })
}

// indexState stores resources as the index of the workspace's current state version, which is
// what the runner read them from at the end of its run.
func indexState(ctx context.Context, q *db.Queries, id uuid.UUID, resources []runnerapi.Resource) error {
	version, err := q.CurrentStateVersion(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // nothing was ever saved, so there is nothing to index
	}
	if err != nil {
		return err
	}
	if err := q.ClearStateIndex(ctx, version); err != nil {
		return err
	}
	raw, err := json.Marshal(resources)
	if err != nil {
		return err
	}
	if err := q.InsertStateIndex(ctx, db.InsertStateIndexParams{ID: version, Resources: raw}); err != nil {
		return err
	}
	return q.MarkIndexed(ctx, version)
}

// Resource is one resource in a workspace's current state, with the workspace it belongs to.
type Resource struct {
	runnerapi.Resource
	Workspace string // name
	Serial    int64  // state version the index comes from
}

// UnknownResource returns an address that isn't in the workspace's current state, and false if all
// are. Unlike Resources it isn't capped.
func (s *Store) UnknownResource(ctx context.Context, workspaceID uuid.UUID, addresses []string) (string, bool, error) {
	a, err := s.q.UnknownResource(ctx, db.UnknownResourceParams{WorkspaceID: workspaceID, Addresses: addresses})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return a, err == nil, err
}

// Resources lists the resources in a workspace's current state version (at most 2000).
func (s *Store) Resources(ctx context.Context, workspaceID uuid.UUID) ([]Resource, error) {
	rows, err := s.q.Resources(ctx, workspaceID)
	out := make([]Resource, 0, len(rows))
	for _, r := range rows {
		out = append(out, Resource{Resource: runnerapi.Resource{Address: r.Address, Type: r.Type, Name: r.Name, Provider: r.Provider,
			Module: r.Module}, Workspace: r.Workspace, Serial: r.Serial})
	}
	return out, err
}
