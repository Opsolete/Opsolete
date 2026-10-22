package storage

import (
	"context"
	"crypto/rand"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rudesome/form2stack/internal/runnerapi"
)

func testStore(t *testing.T) *Store {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	s, err := Open(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.DB.Close() })
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Leftovers from earlier runs would be claimed first.
	if _, err := s.DB.Exec(`UPDATE runs SET status = 'errored'
		WHERE status IN ('plan_queued', 'planning', 'needs_confirmation', 'apply_queued', 'applying')`); err != nil {
		t.Fatal(err)
	}
	return s
}

func newWorkspace(t *testing.T, s *Store, autoApply bool) (Workspace, Run) {
	ctx := context.Background()
	name := "t-" + strings.ToLower(rand.Text()[:10]) // the test DB is reused
	w, run, err := s.CreateWorkspace(ctx, name, "job-test", "git::x", "v1", map[string]any{"n": 1.0}, autoApply, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.DB.Exec(`DELETE FROM workspaces WHERE id=$1`, w.ID) })
	return w, run
}

func claim(t *testing.T, s *Store, phase string) Job {
	t.Helper()
	j, ok, err := s.ClaimJob(context.Background(), "r", time.Minute)
	if err != nil || !ok || j.Phase != phase {
		t.Fatalf("claim %s: %+v %v %v", phase, j, ok, err)
	}
	return j
}

func status(t *testing.T, s *Store, runID string) string {
	t.Helper()
	r, _, err := s.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	return r.Status
}

func TestRunLifecycle(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	w, run := newWorkspace(t, s, false)

	// Many runners race for one job: exactly one wins.
	var mu sync.Mutex
	var won []Job
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			j, ok, err := s.ClaimJob(ctx, "r", time.Minute)
			if err != nil {
				t.Error(err)
			}
			if ok {
				mu.Lock()
				won = append(won, j)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(won) != 1 || won[0].RunID != run.ID || won[0].WorkspaceID != w.ID || won[0].Phase != "plan" ||
		won[0].ModuleSource != "git::x" || won[0].Inputs["n"] != 1.0 {
		t.Fatalf("claims: %+v", won)
	}
	plan := won[0]
	if working, err := s.WorkingRun(ctx, w.ID); err != nil || working != run.ID {
		t.Fatalf("the planning run must hold the workspace's state: %q %v", working, err)
	}

	if err := s.Heartbeat(ctx, run.ID, "not-the-lease", time.Minute); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("foreign lease must be rejected, got %v", err)
	}
	for _, chunk := range []string{"0123456789", "abcdefghij", "dropped"} { // the 2nd crosses the limit
		if err := s.AppendLog(ctx, run.ID, plan.LeaseID, chunk, time.Minute, 15); err != nil {
			t.Fatal(err)
		}
	}
	logs, err := s.Logs(ctx, run.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 2 || logs[0].Chunk != "0123456789" || !strings.Contains(logs[1].Chunk, "truncated") {
		t.Fatalf("logs: %+v", logs)
	}
	if more, _ := s.Logs(ctx, run.ID, logs[0].Seq, 100); len(more) != 1 {
		t.Fatalf("cursor: %+v", more)
	}

	// Plan with changes: waits for a human; nothing else may run on the workspace meanwhile.
	sum := &runnerapi.Plan{Add: 2, Changes: []runnerapi.Change{{Address: "a.b", Action: "create"}, {Address: "a.c", Action: "create"}}}
	if st, err := s.FinishJob(ctx, run.ID, plan.LeaseID, "succeeded", "", nil, sum, nil); err != nil || st != NeedsConfirmation {
		t.Fatalf("finish plan: %q %v", st, err)
	}
	if _, ok, _ := s.ClaimJob(ctx, "r", time.Minute); ok {
		t.Fatal("a plan waiting for confirmation must not be claimed")
	}
	if _, err := s.CreateRun(ctx, w.ID, "apply", ""); !errors.Is(err, ErrRunActive) {
		t.Fatalf("second run while one is active: %v", err)
	}
	if err := s.ConfirmRun(ctx, run.ID, "", "alice@example.com"); err != nil {
		t.Fatal(err)
	}
	if err := s.ConfirmRun(ctx, run.ID, "", "alice@example.com"); !errors.Is(err, ErrNotConfirmable) {
		t.Fatalf("confirming twice: %v", err)
	}

	// Apply phase: a new lease; the plan phase's lease is dead.
	apply := claim(t, s, "apply")
	if apply.RunID != run.ID || apply.LeaseID == plan.LeaseID {
		t.Fatalf("apply job: %+v", apply)
	}
	if err := s.Heartbeat(ctx, run.ID, plan.LeaseID, time.Minute); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("the plan phase's lease must be dead: %v", err)
	}
	res := []runnerapi.Resource{
		{Address: "azurerm_storage_account.this", Type: "azurerm_storage_account", Name: "this", Provider: "registry.terraform.io/hashicorp/azurerm"},
		{Address: "module.net.azurerm_subnet.a[0]", Type: "azurerm_subnet", Name: "a", Provider: "registry.terraform.io/hashicorp/azurerm", Module: "module.net"},
	}
	if err := s.AddStateVersion(ctx, w.ID, StateVersion{Serial: 3, Lineage: "lin", BlobKey: "k/" + w.ID, MD5: "m", Size: 1}); err != nil {
		t.Fatal(err)
	}
	if st, err := s.FinishJob(ctx, run.ID, apply.LeaseID, "succeeded", "", map[string]any{"id": "abc"}, nil, res); err != nil || st != Applied {
		t.Fatalf("finish apply: %q %v", st, err)
	}
	if _, err := s.FinishJob(ctx, run.ID, apply.LeaseID, "failed", "", nil, nil, nil); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("finishing twice must fail, got %v", err)
	}
	got, hist, err := s.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Plan == nil || got.Plan.Add != 2 || got.Outputs["id"] != "abc" || hist[len(hist)-1].Status != Applied {
		t.Fatalf("applied run: %+v %+v", got, hist)
	}
	if out, destroyed, err := s.CurrentOutputs(ctx, w.ID); err != nil || out["id"] != "abc" || destroyed {
		t.Fatalf("workspace outputs: %v %v %v", out, destroyed, err)
	}
	if got, err := s.Resources(ctx, "", w.ID); err != nil || len(got) != 2 || got[1].Module != "module.net" || got[0].Workspace != w.Name || got[0].Serial != 3 {
		t.Fatalf("inventory: %+v %v", got, err)
	}
	if got, _ := s.Resources(ctx, "SUBNET", w.ID); len(got) != 1 || got[0].Type != "azurerm_subnet" {
		t.Fatalf("filter: %+v", got)
	}

	// A plan without changes ends the run right away.
	again, err := s.CreateRun(ctx, w.ID, "apply", "")
	if err != nil {
		t.Fatal(err)
	}
	j := claim(t, s, "plan")
	if st, err := s.FinishJob(ctx, again.ID, j.LeaseID, "succeeded", "", nil, nil, nil); err != nil || st != NoChanges {
		t.Fatalf("no-op plan: %q %v", st, err)
	}
	if _, ok, _ := s.ClaimJob(ctx, "r", time.Minute); ok {
		t.Fatal("nothing should be left to claim")
	}
}

func TestAutoApplyAndDiscard(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	w, run := newWorkspace(t, s, true)
	j := claim(t, s, "plan")
	sum := &runnerapi.Plan{Add: 1}
	if st, _ := s.FinishJob(ctx, run.ID, j.LeaseID, "succeeded", "", nil, sum, nil); st != ApplyQueued {
		t.Fatalf("auto-apply plan: %q", st)
	}
	j = claim(t, s, "apply")
	if st, _ := s.FinishJob(ctx, run.ID, j.LeaseID, "succeeded", "", nil, nil, nil); st != Applied {
		t.Fatalf("auto-applied: %q", st)
	}

	// Destroys ask even with auto-apply; a discarded plan frees the workspace.
	d, err := s.CreateRun(ctx, w.ID, "destroy", "")
	if err != nil {
		t.Fatal(err)
	}
	j = claim(t, s, "plan")
	if j.Action != "destroy" {
		t.Fatalf("job: %+v", j)
	}
	if st, _ := s.FinishJob(ctx, d.ID, j.LeaseID, "succeeded", "", nil, sum, nil); st != NeedsConfirmation {
		t.Fatalf("destroy plan with auto-apply: %q", st)
	}
	if err := s.DiscardRun(ctx, d.ID, "bob@example.com"); err != nil || status(t, s, d.ID) != Discarded {
		t.Fatalf("discard: %v", err)
	}
	if _, err := s.CreateRun(ctx, w.ID, "apply", ""); err != nil {
		t.Fatalf("a discarded run must free the workspace: %v", err)
	}
}

func TestExpiredLeaseIsErrored(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	w, run := newWorkspace(t, s, false)
	j, ok, err := s.ClaimJob(ctx, "r", 10*time.Millisecond)
	if err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	if _, ok, err := s.LockState(ctx, w.ID, "l1", []byte(`{"ID":"l1"}`)); !ok || err != nil {
		t.Fatalf("lock: %v %v", ok, err)
	}
	time.Sleep(50 * time.Millisecond)
	ids, err := s.ReapExpiredLeases(ctx)
	if err != nil || len(ids) != 1 || ids[0] != run.ID {
		t.Fatalf("reap: %v %v", ids, err)
	}
	// The old runner is fenced off once its lease is gone.
	if err := s.AppendLog(ctx, run.ID, j.LeaseID, "late", time.Minute, 1<<20); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("late write must be rejected, got %v", err)
	}
	// No force-unlock is ever needed: a vanished runner's state lock goes with its lease.
	if lock, _ := s.StateLockID(ctx, w.ID); lock != "" {
		t.Fatalf("lock of a lost runner survived: %q", lock)
	}
	if st := status(t, s, run.ID); st != Errored {
		t.Fatalf("status %s", st)
	}
}
