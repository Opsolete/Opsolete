package app

import (
	"context"
	"slices"

	"github.com/google/uuid"
	"gocloud.dev/gcerrors"

	"github.com/opsolete/opsolete/internal/runnerapi"
	"github.com/opsolete/opsolete/internal/storage"
)

// NewRun is a plan to queue: Action apply (re-plan with the workspace's inputs; Replace lists
// addresses of the current state to replace), destroy, refresh (refresh-only), assess (check for
// drift now) or plan (a speculative plan, never applied).
type NewRun struct {
	Action  string
	Replace []string
}

// StartRun queues a run on the workspace. Needs operate. ValidationError: the action, or a replace
// address that isn't one of the workspace's resources; conflicts: no configuration, local
// execution, an active run.
func (a *App) StartRun(ctx context.Context, actor Actor, workspace string, in NewRun) (storage.Workspace, storage.Run, error) {
	ws, _, err := a.Workspace(ctx, actor, workspace, LevelOperate)
	if err != nil {
		return ws, storage.Run{}, err
	}
	if !slices.Contains([]string{"apply", "destroy", "refresh", "assess", "plan"}, in.Action) {
		return ws, storage.Run{}, invalid("action", "action must be apply, destroy, refresh, assess or plan")
	}
	if !ws.HasConfig() {
		return ws, storage.Run{}, ErrNoConfig
	}
	if len(in.Replace) > 0 {
		if in.Action != "apply" {
			return ws, storage.Run{}, invalid("replace", "only apply runs can replace resources")
		}
		// Only addresses of the current state: they become Terraform arguments.
		addr, unknown, err := a.Store.UnknownResource(ctx, ws.ID, in.Replace)
		if err != nil {
			return ws, storage.Run{}, err
		}
		if unknown {
			return ws, storage.Run{}, invalid("replace", "not a resource of this workspace: "+addr)
		}
	}
	run, err := a.Store.CreateRun(ctx, ws.ID, in.Action, actor.UserID, in.Replace)
	if err != nil {
		return ws, run, err
	}
	a.Log.Info("run queued", "workspace", ws.Name, "run", run.ID, "action", in.Action, "user", actor.Email)
	return ws, run, nil
}

// RunDetail is a run with its status history (oldest first), its workspace and the actor's level there.
type RunDetail struct {
	Workspace storage.Workspace
	Level     Level
	Run       storage.Run
	History   []storage.StatusEntry
}

// Run loads run id of the workspace if the actor has at least need there; a run of another
// workspace is ErrNotFound.
func (a *App) Run(ctx context.Context, actor Actor, workspace string, id uuid.UUID, need Level) (RunDetail, error) {
	var d RunDetail
	var err error
	if d.Workspace, d.Level, err = a.Workspace(ctx, actor, workspace, need); err != nil {
		return d, err
	}
	d.Run, d.History, err = a.Store.GetRun(ctx, id)
	if err == nil && d.Run.WorkspaceID != d.Workspace.ID {
		err = ErrNotFound
	}
	return d, err
}

// ConfirmRun queues the apply of a plan that waits for confirmation. Needs operate.
func (a *App) ConfirmRun(ctx context.Context, actor Actor, workspace string, id uuid.UUID) error {
	d, err := a.Run(ctx, actor, workspace, id, LevelOperate)
	if err != nil {
		return err
	}
	if err := a.Store.ConfirmRun(ctx, id, actor.UserID, actor.Email); err != nil {
		return err
	}
	a.Log.Info("run decided", "workspace", d.Workspace.Name, "run", id, "confirmed", true, "user", actor.Email)
	return nil
}

// DiscardRun ends a run whose plan waits for confirmation, without applying it. Needs operate.
func (a *App) DiscardRun(ctx context.Context, actor Actor, workspace string, id uuid.UUID) error {
	d, err := a.Run(ctx, actor, workspace, id, LevelOperate)
	if err != nil {
		return err
	}
	if err := a.Store.DiscardRun(ctx, id, actor.Email); err != nil {
		return err
	}
	a.runEnded(ctx, id)
	a.Log.Info("run decided", "workspace", d.Workspace.Name, "run", id, "confirmed", false, "user", actor.Email)
	return nil
}

// CancelRun cancels a queued run, asks a running one to stop (Terraform still saves state), or with
// force cuts off a runner that doesn't react (storage.CancelRun). Needs operate. It returns the run's
// new status.
func (a *App) CancelRun(ctx context.Context, actor Actor, workspace string, id uuid.UUID, force bool) (string, error) {
	d, err := a.Run(ctx, actor, workspace, id, LevelOperate)
	if err != nil {
		return "", err
	}
	status, err := a.Store.CancelRun(ctx, id, actor.Email, force)
	if err != nil {
		return "", err
	}
	if storage.Ended(status) {
		a.runEnded(ctx, id)
	}
	a.Log.Info("run cancel", "workspace", d.Workspace.Name, "run", id, "status", status, "user", actor.Email)
	return status, nil
}

// RunLog reads one run's log; RunLog (the function) checked the actor may read it.
type RunLog struct {
	a  *App
	ID uuid.UUID
}

// RunLog opens the log of run id of the workspace for reading. Needs read.
func (a *App) RunLog(ctx context.Context, actor Actor, workspace string, id uuid.UUID) (RunLog, error) {
	if _, err := a.Run(ctx, actor, workspace, id, LevelRead); err != nil {
		return RunLog{}, err
	}
	return RunLog{a, id}, nil
}

// Chunks returns up to limit chunks of the log after seq.
func (l RunLog) Chunks(ctx context.Context, after int64, limit int) ([]storage.LogChunk, error) {
	return l.a.Store.Logs(ctx, l.ID, after, limit)
}

// Status is the run's current status (only Status is set).
func (l RunLog) Status(ctx context.Context) (storage.Run, error) {
	return l.a.Store.RunStatus(ctx, l.ID)
}

// ArtifactKey: saved plans and lock files live in the state store, as sensitive as state (a plan
// holds variable values). They exist from the plan phase until the run ends.
func ArtifactKey(runID uuid.UUID, name string) string { return "runs/" + runID.String() + "/" + name }

// runEnded is the one cleanup after a run ended (storage.Ended), by whatever ended it (finish,
// cancel, discard, the lease reaper, a claim that failed): its artifacts are deleted (the saved plan
// has been applied, or never will be) and its intermediate state versions dropped. Failures only
// leave stale files or listed versions behind; it outlives the caller's request.
func (a *App) runEnded(ctx context.Context, runID uuid.UUID) {
	ctx = context.WithoutCancel(ctx)
	for _, name := range []string{runnerapi.ArtifactPlan, runnerapi.ArtifactLockfile} {
		if err := a.Blobs.Delete(ctx, ArtifactKey(runID, name)); err != nil && gcerrors.Code(err) != gcerrors.NotFound {
			a.Log.Warn("delete artifact", "run", runID, "name", name, "err", err)
		}
	}
	keys, err := a.Store.DropIntermediateStates(ctx, runID)
	if err != nil {
		a.Log.Warn("drop intermediate states", "run", runID, "err", err)
	}
	for _, k := range keys {
		if err := a.Blobs.Delete(ctx, k); err != nil && gcerrors.Code(err) != gcerrors.NotFound {
			a.Log.Warn("delete intermediate state", "run", runID, "key", k, "err", err)
		}
	}
}
