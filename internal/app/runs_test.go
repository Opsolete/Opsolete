package app

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/opsolete/opsolete/internal/registry"
	"github.com/opsolete/opsolete/internal/runnerapi"
	"github.com/opsolete/opsolete/internal/storage"
)

var admin = Actor{Email: "admin@example.com", Admin: true}

// configured is a workspace with a template added (its plan applied: ending it otherwise would
// remove the template again) and a queued run.
func configured(t *testing.T, a *App) (storage.Workspace, storage.Run) {
	t.Helper()
	ctx := context.Background()
	ws := emptyWorkspace(t, a, admin)
	added, err := a.Store.SetWorkspaceTemplate(ctx, ws.ID, storage.WorkspaceTemplate{Name: "m", TemplateID: "x",
		ModuleSource: "example.com/m", ModuleRef: "1.0.0", Inputs: map[string]any{}}, true, uuid.Nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Store.Pool.Exec(ctx, `UPDATE runs SET status = 'applied' WHERE id = $1`, added.ID); err != nil {
		t.Fatal(err)
	}
	ws, run, err := a.StartRun(ctx, admin, ws.Name, NewRun{Action: "apply"})
	if err != nil {
		t.Fatal(err)
	}
	return ws, run
}

// leased puts a queued run in planning, held by lease.
// leased makes run runID's plan phase claimed and acknowledged by runner "r" with lease.
func leased(t *testing.T, a *App, runID uuid.UUID, lease string) {
	t.Helper()
	if _, err := a.Store.Pool.Exec(context.Background(), `UPDATE runs SET status = 'planning', runner = 'r', lease_id = $2,
		lease_until = now() + interval '1 minute', lease_acked_at = now() WHERE id = $1`, runID, lease); err != nil {
		t.Fatal(err)
	}
}

// A claim nothing acknowledged can't be used (heartbeat, log, artifacts, finish, state) and goes
// back to the queue once its ack window passes; the ack makes it usable.
func TestUnacknowledgedClaim(t *testing.T) {
	a, ctx := testApp(t), context.Background()
	a.Registry = &registry.Registry{Key: []byte("ack-test")}
	ws, run := configured(t, a)
	leased(t, a, run.ID, "l")
	if _, err := a.Store.Pool.Exec(ctx, `UPDATE runs SET lease_acked_at = NULL WHERE id = $1`, run.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Heartbeat(ctx, run.ID, "l"); !errors.Is(err, storage.ErrLeaseLost) {
		t.Errorf("heartbeat before the ack: %v", err)
	}
	if _, err := a.AppendLog(ctx, run.ID, "l", []byte("x")); !errors.Is(err, storage.ErrLeaseLost) {
		t.Errorf("log before the ack: %v", err)
	}
	if err := a.PutArtifact(ctx, run.ID, "l", runnerapi.ArtifactPlan, []byte("x")); !errors.Is(err, storage.ErrLeaseLost) {
		t.Errorf("artifact before the ack: %v", err)
	}
	if _, err := a.FinishJob(ctx, run.ID, "l", runnerapi.FinishRequest{Status: "failed"}); !errors.Is(err, storage.ErrLeaseLost) {
		t.Errorf("finish before the ack: %v", err)
	}
	if _, err := a.StateRun(ctx, ws.ID, ws.ID.String(), a.StateToken(ws.ID, run.ID)); !errors.Is(err, ErrForbidden) {
		t.Errorf("state credential before the ack: %v", err)
	}

	// within the window nothing happens; past it the job is queued again
	a.ReapLeases(ctx)
	if r, _ := a.Store.RunStatus(ctx, run.ID); r.Status != storage.Planning {
		t.Fatalf("unclaimed within the window: %s", r.Status)
	}
	a.Store.Pool.Exec(ctx, `UPDATE runs SET lease_until = now() - interval '1 second' WHERE id = $1`, run.ID)
	a.ReapLeases(ctx)
	if r, _ := a.Store.RunStatus(ctx, run.ID); r.Status != storage.PlanQueued {
		t.Fatalf("after the window: %s, want plan_queued", r.Status)
	}
	if err := a.AckJob(ctx, run.ID, "l"); !errors.Is(err, storage.ErrLeaseLost) {
		t.Errorf("a late ack: %v", err)
	}

	leased(t, a, run.ID, "l2") // acknowledged: usable
	if _, err := a.Heartbeat(ctx, run.ID, "l2"); err != nil {
		t.Errorf("heartbeat after the ack: %v", err)
	}
	if _, err := a.StateRun(ctx, ws.ID, ws.ID.String(), a.StateToken(ws.ID, run.ID)); err != nil {
		t.Errorf("state credential after the ack: %v", err)
	}
}

func TestStartRun(t *testing.T) {
	a, ctx := testApp(t), context.Background()
	group := uniq("grp-")
	reader := user(t, a, group)
	ws, queued := configured(t, a)
	if err := a.SetGrant(ctx, admin, ws.Name, group, "read"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.StartRun(ctx, reader, ws.Name, NewRun{Action: "apply"}); !errors.Is(err, ErrForbidden) {
		t.Errorf("a reader starts a run: %v", err)
	}
	if _, _, err := a.StartRun(ctx, admin, ws.Name, NewRun{Action: "apply"}); !IsConflict(err) || !errors.Is(err, storage.ErrRunActive) {
		t.Errorf("with an active run: %v", err)
	}
	if _, err := a.CancelRun(ctx, admin, ws.Name, queued.ID, false); err != nil {
		t.Fatal(err)
	}
	for _, in := range []NewRun{{Action: "bogus"}, {Action: "destroy", Replace: []string{"a.b"}}, {Action: "apply", Replace: []string{"a.b"}}} {
		if _, _, err := a.StartRun(ctx, admin, ws.Name, in); !isValidation(err) {
			t.Errorf("%+v: %v", in, err)
		}
	}
	if _, err := a.SaveGeneralSettings(ctx, admin, ws.Name, GeneralSettings{Name: ws.Name, ExecutionMode: "local"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.StartRun(ctx, admin, ws.Name, NewRun{Action: "apply"}); !IsConflict(err) || !errors.Is(err, storage.ErrLocalExecution) {
		t.Errorf("local execution: %v", err)
	}
	if _, err := a.SaveGeneralSettings(ctx, admin, ws.Name, GeneralSettings{Name: ws.Name, ExecutionMode: "remote"}); err != nil {
		t.Fatal(err)
	}
	_, run, err := a.StartRun(ctx, admin, ws.Name, NewRun{Action: "apply"})
	if err != nil || run.Status != storage.PlanQueued {
		t.Fatalf("start: %+v %v", run, err)
	}
	if _, _, err := a.StartRun(ctx, admin, emptyWorkspace(t, a, admin).Name, NewRun{Action: "apply"}); !IsConflict(err) || !errors.Is(err, ErrNoConfig) {
		t.Errorf("no configuration: %v", err)
	}
	other, _ := configured(t, a)
	if _, err := a.Run(ctx, admin, other.Name, run.ID, LevelRead); !errors.Is(err, ErrNotFound) {
		t.Errorf("a run through another workspace: %v", err)
	}
}

// Every way a run ends deletes its artifacts; one that goes on keeps them.
func TestRunEndDeletesArtifacts(t *testing.T) {
	a, ctx := testApp(t), context.Background()
	put := func(runID uuid.UUID) {
		t.Helper()
		for _, n := range []string{runnerapi.ArtifactPlan, runnerapi.ArtifactLockfile} {
			if err := a.WriteBlob(ctx, ArtifactKey(runID, n), []byte("x")); err != nil {
				t.Fatal(err)
			}
		}
	}
	left := func(path string, runID uuid.UUID, want bool) {
		t.Helper()
		for _, n := range []string{runnerapi.ArtifactPlan, runnerapi.ArtifactLockfile} {
			if ok, _ := a.Blobs.Exists(ctx, ArtifactKey(runID, n)); ok != want {
				t.Errorf("%s: %s left %v, want %v", path, n, ok, want)
			}
		}
	}

	// finish: a failed plan
	_, run := configured(t, a)
	leased(t, a, run.ID, "l1")
	put(run.ID)
	if st, err := a.FinishJob(ctx, run.ID, "l1", runnerapi.FinishRequest{Status: "failed"}); err != nil || st != storage.Errored {
		t.Fatalf("finish: %s %v", st, err)
	}
	left("finish", run.ID, false)

	// a plan with changes goes on (kept), then discard ends it
	ws, run := configured(t, a)
	leased(t, a, run.ID, "l2")
	put(run.ID)
	if st, err := a.FinishJob(ctx, run.ID, "l2", runnerapi.FinishRequest{Status: "succeeded", Plan: &runnerapi.Plan{}}); err != nil || st != storage.NeedsConfirmation {
		t.Fatalf("plan: %s %v", st, err)
	}
	left("needs confirmation", run.ID, true)
	if err := a.DiscardRun(ctx, admin, ws.Name, run.ID); err != nil {
		t.Fatal(err)
	}
	left("discard", run.ID, false)

	// cancel: a queued run
	ws, run = configured(t, a)
	put(run.ID)
	if st, err := a.CancelRun(ctx, admin, ws.Name, run.ID, false); err != nil || st != storage.Canceled {
		t.Fatalf("cancel: %s %v", st, err)
	}
	left("cancel", run.ID, false)

	// reap: a runner that stopped heartbeating
	_, run = configured(t, a)
	leased(t, a, run.ID, "l3")
	a.Store.Pool.Exec(ctx, `UPDATE runs SET lease_until = now() - interval '1 second' WHERE id = $1`, run.ID)
	put(run.ID)
	a.ReapLeases(ctx)
	if r, _ := a.Store.RunStatus(ctx, run.ID); r.Status != storage.Errored {
		t.Fatalf("reaped run: %s", r.Status)
	}
	left("reap", run.ID, false)
}

// An artifact that lands after its run ended (canceled between the lease check and the write) is
// deleted, not left behind; the planning run's own upload stays.
func TestPutArtifactRace(t *testing.T) {
	a, ctx := testApp(t), context.Background()
	ws, run := configured(t, a)
	leased(t, a, run.ID, "l")
	if err := a.PutArtifact(ctx, run.ID, "other", runnerapi.ArtifactPlan, []byte("x")); !errors.Is(err, storage.ErrLeaseLost) {
		t.Errorf("another lease: %v", err)
	}
	if err := a.PutArtifact(ctx, run.ID, "l", "evil", []byte("x")); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown artifact: %v", err)
	}
	if err := a.PutArtifact(ctx, run.ID, "l", runnerapi.ArtifactPlan, []byte("plan")); err != nil {
		t.Fatal(err)
	}
	if b, err := a.ReadBlob(ctx, ArtifactKey(run.ID, runnerapi.ArtifactPlan)); err != nil || string(b) != "plan" {
		t.Fatalf("stored: %q %v", b, err)
	}
	if _, err := a.CancelRun(ctx, admin, ws.Name, run.ID, true); err != nil {
		t.Fatal(err)
	}
	// the write of an upload whose lease check passed before the cancel
	if err := a.writeArtifact(ctx, run.ID, runnerapi.ArtifactLockfile, []byte("late")); !errors.Is(err, storage.ErrLeaseLost) {
		t.Errorf("late write: %v", err)
	}
	for _, n := range []string{runnerapi.ArtifactPlan, runnerapi.ArtifactLockfile} {
		if ok, _ := a.Blobs.Exists(ctx, ArtifactKey(run.ID, n)); ok {
			t.Errorf("%s left behind", n)
		}
	}
}
