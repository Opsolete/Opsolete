package app

// Terraform state, one per workspace: the http state backend (runs), the cloud API (the CLI), the
// UI (locks, rollbacks, state pages). State files hold secrets in plain text: their contents need
// operate, and sensitive output values never leave the server (versions store masked copies).

import (
	"cmp"
	"context"
	"crypto/hmac"
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	tfjson "github.com/hashicorp/terraform-json"

	"github.com/opsolete/opsolete/internal/mask"
	"github.com/opsolete/opsolete/internal/runnerapi"
	"github.com/opsolete/opsolete/internal/storage"
)

// MaxStateSize is the largest state file accepted.
const MaxStateSize = 64 << 20

var (
	errNoWorkingRun    = reason{ErrForbidden, "no run is working on this workspace"}
	errBadStateToken   = reason{ErrForbidden, "invalid state credentials"}
	errForeignState    = reason{ErrForbidden, "only this workspace's runs may change its state"}
	errPlanWrite       = reason{ErrForbidden, "only an apply may write the state"}
	errPullRequestRead = reason{ErrForbidden, "a pull request's speculative plan can't read other workspaces' state"}
	errNotShared       = reason{ErrForbidden, "this workspace does not share its state with yours (Settings > General > Remote state sharing)"}
	errNotRunLocked    = reason{ErrStateConflict, "the state must be locked by this run"}
	errAlreadyLocked   = reason{storage.ErrStateLocked, "the workspace is already locked"}
	errOperatorsOnly   = reason{ErrForbidden, "state values are shown to operators only"}
)

// saveState stores body as the workspace's new current state version, unless it's the same as the
// current one: it writes the file, then record records the version (with its writer's checks); if
// that fails the file is deleted again. The version's outputs are stored masked: Terraform's
// sensitive ones and those containing a sensitive variable or input value of the workspace (mask).
// The caller holds the state lock. ValidationError: not a
// state file; ErrStateConflict: another lineage, an older serial, or the same serial with other content.
func (a *App) saveState(ctx context.Context, workspaceID uuid.UUID, body []byte, record func(storage.StateVersion) error) error {
	sum := md5.Sum(body)
	// Only the fields needed to version the file and show its outputs; the rest is Terraform's business.
	var head struct {
		Serial  *int64 `json:"serial"`
		Lineage string `json:"lineage"`
		Outputs map[string]struct {
			Value     any  `json:"value"`
			Sensitive bool `json:"sensitive"`
		} `json:"outputs"`
	}
	if err := json.Unmarshal(body, &head); err != nil || head.Serial == nil || head.Lineage == "" {
		return &ValidationError{Message: "not a Terraform state file"}
	}
	v := storage.StateVersion{Serial: *head.Serial, Lineage: head.Lineage, MD5: hex.EncodeToString(sum[:]), Size: int64(len(body)),
		Outputs: map[string]any{}}
	secrets, err := a.workspaceMasker(ctx, workspaceID)
	if err != nil { // the state must still be saved (a lost write orphans resources): hide every output instead
		a.Log.Error("open workspace secrets to mask outputs; all outputs hidden", "workspace", workspaceID, "err", err)
	}
	for k, o := range head.Outputs {
		v.Outputs[k] = secrets.Value(o.Value) // an output echoing a sensitive variable or input
		if o.Sensitive || err != nil {        // sensitive values never leave the state file
			v.Outputs[k] = mask.Sensitive
		}
	}
	cur, err := a.Store.StateVersions(ctx, workspaceID, 1)
	if err != nil {
		return err
	}
	if len(cur) > 0 {
		switch {
		case cur[0].Lineage != v.Lineage:
			return reason{ErrStateConflict, "state lineage differs from the workspace's state"}
		case v.Serial < cur[0].Serial:
			return reason{ErrStateConflict, "state serial is older than the workspace's state"}
		case v.MD5 == cur[0].MD5:
			return nil // unchanged: no new version
		case v.Serial == cur[0].Serial: // Terraform bumps the serial on every change: same serial, other content is a fork
			return reason{ErrStateConflict, "state serial equals the workspace's state, but its content differs"}
		}
	}
	v.BlobKey = fmt.Sprintf("workspaces/%s/%d-%s.tfstate", workspaceID, v.Serial, rand.Text())
	if err := a.WriteBlob(ctx, v.BlobKey, body); err != nil {
		return err
	}
	if err := record(v); err != nil {
		// WithoutCancel: a dropped request must not stop the cleanup and leave the file behind.
		if derr := a.Blobs.Delete(context.WithoutCancel(ctx), v.BlobKey); derr != nil {
			a.Log.Warn("delete unrecorded state file", "key", v.BlobKey, "err", derr)
		}
		return err
	}
	a.Log.Info("state saved", "workspace", workspaceID, "serial", v.Serial, "bytes", v.Size)
	return nil
}

// StateRun is a run authenticated to the http state backend for one workspace's state: its own
// workspace's (read, lock, and write while applying), or one shared with it (read only).
type StateRun struct {
	a            *App
	Workspace    uuid.UUID // the state's
	Run          uuid.UUID
	runWorkspace uuid.UUID
	status       string // planning or applying
	pullRequest  bool
}

// StateRun authenticates a run to the state of workspace: username is the run's workspace id,
// password its StateToken, valid only while a runner works on the run (planning or applying).
// ErrForbidden otherwise.
func (a *App) StateRun(ctx context.Context, workspace uuid.UUID, username, password string) (StateRun, error) {
	user, err := uuid.Parse(username)
	if err != nil {
		return StateRun{}, errNoWorkingRun // not an id: no run works on it, as for an unknown one
	}
	run, status, pullRequest, err := a.Store.WorkingRun(ctx, user)
	if errors.Is(err, storage.ErrNotFound) {
		return StateRun{}, errNoWorkingRun
	}
	if err != nil {
		return StateRun{}, err
	}
	if !hmac.Equal([]byte(password), []byte(a.StateToken(user, run))) {
		return StateRun{}, errBadStateToken
	}
	return StateRun{a: a, Workspace: workspace, Run: run, runWorkspace: user, status: status, pullRequest: pullRequest}, nil
}

// own: terraform_remote_state of another workspace's run reads the state, never writes or locks it.
func (s StateRun) own() error {
	if s.runWorkspace != s.Workspace {
		return errForeignState
	}
	return nil
}

// Read returns the current state file, nil if there is none yet. A run of another workspace reads
// it only if this one shares its state with that one, and never a pull request's speculative plan:
// that is unreviewed code (SetWorkspaceVCS keeps variable sets from these plans for the same reason).
func (s StateRun) Read(ctx context.Context) ([]byte, error) {
	if s.own() != nil {
		if s.pullRequest {
			return nil, errPullRequestRead
		}
		shared, err := s.a.Store.SharesState(ctx, s.Workspace, s.runWorkspace)
		if err != nil {
			return nil, err
		}
		if !shared {
			return nil, errNotShared
		}
	}
	vs, err := s.a.Store.StateVersions(ctx, s.Workspace, 1)
	if err != nil || len(vs) == 0 {
		return nil, err
	}
	return s.a.ReadBlob(ctx, vs[0].BlobKey)
}

// Write saves body (StateRun's workspace's) as the new current state; only an apply writes state
// (plans, also speculative ones and drift checks, lock but never write), under the lock lockID it took.
// storage.ErrLeaseLost: the run was canceled or lost its runner meanwhile.
func (s StateRun) Write(ctx context.Context, lockID string, body []byte) error {
	ctx = context.WithoutCancel(ctx) // a lost state write orphans the resources the apply just created
	if err := s.own(); err != nil {
		return err
	}
	if s.status != storage.Applying {
		return errPlanWrite
	}
	// the lock this run took, not just its ID: a 423 shows other locks' IDs to any run
	if locked, err := s.a.Store.StateLockedBy(ctx, s.Workspace, lockID, s.Run); err != nil {
		return err
	} else if !locked {
		return errNotRunLocked
	}
	err := s.a.saveState(ctx, s.Workspace, body, func(v storage.StateVersion) error {
		return s.a.Store.AddStateVersion(ctx, s.Workspace, s.Run, "", v)
	})
	if errors.Is(err, storage.ErrLeaseLost) {
		return reason{storage.ErrLeaseLost, "the run was canceled or lost its runner: state not saved"}
	}
	return err
}

// Lock takes the state lock for Terraform's LockInfo JSON info; ok=false and the holder's LockInfo
// if another lock is held. A run's lock is released when the run ends. ValidationError: info.
func (s StateRun) Lock(ctx context.Context, info []byte) (held []byte, ok bool, err error) {
	if err := s.own(); err != nil {
		return nil, false, err
	}
	id, err := lockInfoID(info)
	if err != nil {
		return nil, false, err
	}
	return s.a.Store.LockState(ctx, s.Workspace, id, s.Run, info)
}

// Unlock releases the lock of LockInfo info if this run holds it.
func (s StateRun) Unlock(ctx context.Context, info []byte) error {
	if err := s.own(); err != nil {
		return err
	}
	id, err := lockInfoID(info)
	if err != nil {
		return err
	}
	return s.a.Store.UnlockState(ctx, s.Workspace, id, s.Run)
}

func lockInfoID(info []byte) (string, error) {
	var l struct{ ID string }
	if json.Unmarshal(info, &l) != nil || l.ID == "" {
		return "", &ValidationError{Message: "body must be Terraform lock info with an ID"}
	}
	return l.ID, nil
}

// State is a workspace's state as an actor uses it, opened by State or StateByID with the actor's
// Level there. Anyone who sees the workspace reads versions, resources, outputs (masked) and the
// lock; the files, diffs and every change need operate.
type State struct {
	a         *App
	actor     Actor
	Workspace storage.Workspace
	Level     Level
}

// State opens the state of the workspace if the actor has at least need there (Workspace).
func (a *App) State(ctx context.Context, actor Actor, workspace string, need Level) (State, error) {
	ws, level, err := a.Workspace(ctx, actor, workspace, need)
	return State{a, actor, ws, level}, err
}

// StateByID is State by workspace id.
func (a *App) StateByID(ctx context.Context, actor Actor, id uuid.UUID, need Level) (State, error) {
	ws, level, err := a.WorkspaceByID(ctx, actor, id, need)
	return State{a, actor, ws, level}, err
}

func (s State) operate() error {
	if s.Level < LevelOperate {
		return errReadOnly
	}
	return nil
}

// Versions lists the newest state versions, newest first.
func (s State) Versions(ctx context.Context, limit int) ([]storage.StateVersion, error) {
	return s.a.Store.StateVersions(ctx, s.Workspace.ID, limit)
}

// Resources lists the resources of the current state.
func (s State) Resources(ctx context.Context) ([]storage.Resource, error) {
	return s.a.Store.Resources(ctx, s.Workspace.ID)
}

// Outputs are the current state's outputs, sensitive values masked; destroyed: the state is empty.
func (s State) Outputs(ctx context.Context) (outputs map[string]any, destroyed bool, err error) {
	return s.a.Store.CurrentOutputs(ctx, s.Workspace.ID)
}

// CurrentLock is the state lock, nil if unlocked.
func (s State) CurrentLock(ctx context.Context) (*storage.StateLock, error) {
	return s.a.Store.GetStateLock(ctx, s.Workspace.ID)
}

// The workspace's pages read the rest of it through its State too: access is checked once.

// Runs lists the workspace's newest runs, newest first.
func (s State) Runs(ctx context.Context, limit int) ([]storage.Run, error) {
	return s.a.Store.Runs(ctx, s.Workspace.ID, limit)
}

// LatestAssessment is the workspace's latest drift check; nil if none.
func (s State) LatestAssessment(ctx context.Context) (*storage.Run, error) {
	r, err := s.a.Store.LatestAssessment(ctx, s.Workspace.ID)
	if errors.Is(err, storage.ErrNotFound) {
		return nil, nil
	}
	return &r, err
}

// Templates lists the templates added to the workspace.
func (s State) Templates(ctx context.Context) ([]storage.WorkspaceTemplate, error) {
	return s.a.Store.WorkspaceTemplates(ctx, s.Workspace.ID)
}

// RunMetrics are the workspace's run statistics.
func (s State) RunMetrics(ctx context.Context) (storage.RunMetrics, error) {
	return s.a.Store.RunMetrics(ctx, s.Workspace.ID)
}

// RemoteStateConsumers lists the workspaces the workspace shares its state with, also those the
// actor can't see. Needs operate.
func (s State) RemoteStateConsumers(ctx context.Context) ([]storage.Workspace, error) {
	if err := s.operate(); err != nil {
		return nil, err
	}
	return s.a.Store.RemoteStateConsumers(ctx, s.Workspace.ID)
}

// File reads a state version's file. Needs operate; ErrNotFound for a version of another workspace.
func (s State) File(ctx context.Context, version int64) ([]byte, error) {
	if err := s.operate(); err != nil {
		return nil, err
	}
	key, err := s.a.Store.StateVersionBlob(ctx, s.Workspace.ID, version)
	if err != nil {
		return nil, err
	}
	return s.a.ReadBlob(ctx, key)
}

// VersionDiff is what a state version changed, and what rolling back to it would change.
type VersionDiff struct {
	Version   storage.StateVersion
	Prev      *storage.StateVersion // the version before it; nil for the first
	Changes   []StateChange         // from Prev to Version
	IsCurrent bool
	Rollback  []StateChange // from the current version to Version; nil if it is the current one
}

// Diff diffs a state version with the one before it and with the current one. The diff shows
// attribute values, and the redaction misses unmarked secrets: like the files, operators only
// (ErrForbidden: readers see the version list anyway).
func (s State) Diff(ctx context.Context, version int64) (VersionDiff, error) {
	var d VersionDiff
	if s.Level < LevelOperate {
		return d, errOperatorsOnly
	}
	pair, err := s.a.Store.StateVersionsUpTo(ctx, s.Workspace.ID, version, 2) // the version and the one before it
	if err != nil {
		return d, err
	}
	if len(pair) == 0 || pair[0].ID != version {
		return d, ErrNotFound
	}
	d.Version = pair[0]
	if len(pair) > 1 {
		d.Prev = &pair[1]
	}
	secrets, err := s.a.workspaceMasker(ctx, s.Workspace.ID)
	if err != nil {
		return d, err
	}
	if d.Changes, err = s.a.diffVersions(ctx, secrets, d.Prev, &d.Version); err != nil {
		return d, err
	}
	cur, err := s.Versions(ctx, 1)
	if err != nil {
		return d, err
	}
	if d.IsCurrent = cur[0].ID == version; !d.IsCurrent {
		d.Rollback, err = s.a.diffVersions(ctx, secrets, &cur[0], &d.Version)
	}
	return d, err
}

// diffVersions diffs two state versions; a nil version is no state. Values containing a secret are hidden.
func (a *App) diffVersions(ctx context.Context, secrets mask.Masker, from, to *storage.StateVersion) ([]StateChange, error) {
	var files [2][]byte
	for i, v := range []*storage.StateVersion{from, to} {
		if v == nil {
			continue
		}
		var err error
		if files[i], err = a.ReadBlob(ctx, v.BlobKey); err != nil {
			return nil, err
		}
	}
	return diffStates(secrets, files[0], files[1])
}

// Lock locks the workspace from the UI, with an optional reason (at most 300 characters). It's a
// state lock as the CLI takes it (LockCLI), so the CLI, terraform force-unlock and the cloud API see
// it too; runs wait until it's released, and it never expires. Needs operate.
func (s State) Lock(ctx context.Context, why string) error {
	if utf8.RuneCountInString(why) > 300 {
		return invalid("reason", "the reason is at most 300 characters")
	}
	return s.userLock(ctx, "locked in Opsolete", why)
}

// LockCLI takes the state lock for the actor's Terraform (cloud block). Terraform then runs on the
// actor's machine: only for local-execution workspaces. Needs operate.
func (s State) LockCLI(ctx context.Context) error {
	if s.Workspace.ExecutionMode != "local" {
		return reason{ErrRemoteExecution, "workspace " + s.Workspace.Name + " uses remote execution: Opsolete runners plan and apply it. " +
			"Set its execution mode to local in the workspace's settings to run Terraform on your machine"}
	}
	return s.userLock(ctx, "terraform (cloud block)", "")
}

// userLock takes the state lock for the actor, as a CLI or UI lock (storage.CloudLockPrefix), which
// unlock (the actor) or force-unlock (anyone with operate) releases.
func (s State) userLock(ctx context.Context, operation, why string) error {
	if err := s.operate(); err != nil {
		return err
	}
	lockID := storage.CloudLockPrefix + rand.Text()
	info := map[string]string{"ID": lockID, "Operation": operation, "Who": s.actor.Email, "UserID": s.actor.UserID.String(),
		"Created": time.Now().UTC().Format(time.RFC3339)}
	if why != "" {
		info["Info"] = why
	}
	b, _ := json.Marshal(info)
	_, ok, err := s.a.Store.LockState(ctx, s.Workspace.ID, lockID, uuid.Nil, b)
	if err == nil && !ok {
		err = errAlreadyLocked
	}
	if err == nil {
		s.a.Log.Info("workspace lock", "workspace", s.Workspace.Name, "action", "lock", "user", s.actor.Email)
	}
	return err
}

// Unlock releases the actor's own CLI or UI lock, or with force anyone's. Locks of runs end with
// the run, a rollback's with the rollback: they are never released here. Needs operate.
func (s State) Unlock(ctx context.Context, force bool) error {
	if err := s.operate(); err != nil {
		return err
	}
	holder := s.actor.UserID.String()
	if force {
		holder = ""
	}
	if err := s.a.Store.UnlockCloudState(ctx, s.Workspace.ID, holder); err != nil {
		return err
	}
	s.a.Log.Info("workspace lock", "workspace", s.Workspace.Name, "action", "unlock", "force", force, "user", s.actor.Email)
	return nil
}

// SaveCLIState saves a state file uploaded by the actor's Terraform (cloud block) and indexes its
// resources from jsonState (Terraform's JSON form of it; left unindexed if missing or unreadable),
// so the workspace page shows them as after a run. Only while the actor holds the state lock
// (checked before writing the file, and atomically with the save) and only for local-execution
// workspaces: a UI lock passes the lock check too, but only runs write a remote workspace's state.
// Needs operate; it returns the current version.
func (s State) SaveCLIState(ctx context.Context, state, jsonState []byte) (storage.StateVersion, error) {
	if err := s.operate(); err != nil {
		return storage.StateVersion{}, err
	}
	if s.Workspace.ExecutionMode != "local" {
		return storage.StateVersion{}, reason{ErrRemoteExecution, "workspace " + s.Workspace.Name + " uses remote execution: only its runs save state"}
	}
	a, id, user := s.a, s.Workspace.ID, s.actor.UserID.String()
	if locked, err := a.Store.CloudLocked(ctx, id, user); err != nil || !locked {
		return storage.StateVersion{}, cmp.Or(err, storage.ErrNotCloudLocked)
	}
	if err := a.saveState(ctx, id, state, func(v storage.StateVersion) error { return a.Store.AddStateVersion(ctx, id, uuid.Nil, user, v) }); err != nil {
		return storage.StateVersion{}, err
	}
	var js tfjson.State
	if len(jsonState) > 0 && json.Unmarshal(jsonState, &js) == nil {
		if err := a.Store.IndexState(ctx, id, runnerapi.Inventory(&js)); err != nil {
			a.Log.Error("index state", "workspace", s.Workspace.Name, "err", err) // stays unindexed: deletion keeps refusing
		}
	}
	vs, err := s.Versions(ctx, 1)
	if err != nil || len(vs) == 0 {
		return storage.StateVersion{}, cmp.Or(err, ErrNotFound)
	}
	return vs[0], nil
}

// Rollback makes a copy of an older state version the workspace's current state: same content,
// serial one above the current one, older versions kept, the copy's resource index the old one's.
// It holds a rollback lock throughout and refuses while a run is active, so no run writes state or
// applies a plan made against the replaced state meanwhile. The lock is released at the end, whatever
// happened, or after STALE_LOCK_TIMEOUT by ReleaseOrphanedLocks if this server dies meanwhile. It
// records only while it still holds that lock (storage.ErrLockLost otherwise, nothing changed): a
// rollback outliving STALE_LOCK_TIMEOUT, or force-unlocked, can't write over whoever locked next.
// Needs operate. Conflicts: the state is locked, a run is active, the version is the current one or
// of another lineage; ErrNotFound: no such version.
func (s State) Rollback(ctx context.Context, from int64) error {
	if err := s.operate(); err != nil {
		return err
	}
	ctx = context.WithoutCancel(ctx) // once locked, finish: a dropped request must not cut a rollback short
	a, id := s.a, s.Workspace.ID
	lockID := rand.Text()
	info, _ := json.Marshal(map[string]string{"ID": lockID, "Operation": "OperationTypeRollback", "Who": s.actor.Email})
	if _, ok, err := a.Store.LockState(ctx, id, lockID, uuid.Nil, info); err != nil {
		return err
	} else if !ok {
		return storage.ErrStateLocked
	}
	defer func() {
		if err := a.Store.UnlockState(ctx, id, lockID, uuid.Nil); err != nil {
			a.Log.Error("unlock state", "workspace", s.Workspace.Name, "err", err) // ReleaseOrphanedLocks releases it later
		}
	}()
	// After locking: a run that starts now can't touch the state until the rollback is done, and one
	// that is between phases (e.g. waiting for confirmation) would apply a plan made against the old state.
	runs, err := a.Store.Runs(ctx, id, 1)
	if err != nil {
		return err
	}
	if len(runs) > 0 && runs[0].Active() {
		return storage.ErrRunActive
	}
	key, err := a.Store.StateVersionBlob(ctx, id, from)
	if err != nil {
		return err
	}
	cur, err := s.Versions(ctx, 1)
	if err != nil {
		return err
	}
	if cur[0].ID == from {
		return reason{ErrStateConflict, "this is already the current state version"}
	}
	old, err := a.ReadBlob(ctx, key)
	if err != nil {
		return err
	}
	var state map[string]json.RawMessage
	if err := json.Unmarshal(old, &state); err != nil {
		return fmt.Errorf("parse state version %d: %w", from, err)
	}
	state["serial"] = json.RawMessage(strconv.FormatInt(cur[0].Serial+1, 10))
	body, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	// The version and its index are recorded in one transaction. Not atomic: the file is written
	// before; a failed record deletes it again, only a crash in between leaves an unreferenced file.
	if err := a.saveState(ctx, id, body, func(v storage.StateVersion) error { return a.Store.AddRollbackVersion(ctx, id, from, lockID, v) }); err != nil {
		return err
	}
	a.Log.Info("state rolled back", "workspace", s.Workspace.Name, "version", from, "user", s.actor.Email)
	return nil
}

// ReleaseOrphanedLocks releases the state locks nobody can release anymore: a run's once the run
// stopped working, a rollback's after staleLock (STALE_LOCK_TIMEOUT). CLI and UI locks never expire.
// For a background loop: errors are logged.
func (a *App) ReleaseOrphanedLocks(ctx context.Context, staleLock time.Duration) {
	released, err := a.Store.ReleaseOrphanedLocks(ctx, staleLock)
	if err != nil {
		a.Log.Error("release orphaned state locks", "err", err)
	}
	for ws, info := range released {
		a.Log.Warn("orphaned state lock released", "workspace", ws, "lock", info)
	}
}
