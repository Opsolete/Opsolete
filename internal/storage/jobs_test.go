package storage

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opsolete/opsolete/internal/runnerapi"
	"github.com/opsolete/opsolete/internal/storage/db"
	"github.com/opsolete/opsolete/internal/testdb"
)

// testStore returns a migrated store on a database of t's own (testdb), or skips without TEST_DATABASE_URL.
func testStore(t *testing.T) *Store {
	s, err := Open(context.Background(), testdb.URL(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	s.AuditKey = []byte("test-audit-key")
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return s
}

// step runs one step of a scenario test as a subtest; a failed step ends the scenario, a filtered
// out one skips the rest: the steps after it need its state. (A copy of httpserver's.)
func step(t *testing.T, name string, f func(t *testing.T)) {
	t.Helper()
	ran := false
	if !t.Run(name, func(t *testing.T) { ran = true; f(t) }) {
		t.Fatalf("step %q failed: the steps after it need its state", name)
	}
	if !ran {
		t.Skipf("step %q was filtered out: the steps after it need its state", name)
	}
}

func newWorkspace(t *testing.T, s *Store, autoApply bool) (Workspace, Run) {
	ctx := context.Background()
	name := "t-" + strings.ToLower(rand.Text()[:10]) // several per test
	w, run, err := s.CreateWorkspace(ctx, uuid.Nil, name, "", testTemplate(map[string]any{"n": 1.0}), autoApply, uuid.Nil)
	if err != nil {
		t.Fatal(err)
	}
	return w, run
}

// testTemplate is template job-test as a workspace's added template "main".
func testTemplate(inputs map[string]any) WorkspaceTemplate {
	return WorkspaceTemplate{Name: "main", TemplateID: "job-test", ModuleSource: "git::x", ModuleRef: "v1", Inputs: inputs}
}

func claim(t *testing.T, s *Store, phase string) Job {
	t.Helper()
	j, ok, err := s.ClaimJob(context.Background(), Runner{Name: "r", Pool: DefaultPool}, time.Minute)
	if err != nil || !ok || j.Phase != phase {
		t.Fatalf("claim %s: %+v %v %v", phase, j, ok, err)
	}
	ack(t, s, j)
	return j
}

// ack acknowledges a claimed job, as its runner does before anything else.
func ack(t *testing.T, s *Store, j Job) {
	t.Helper()
	if err := s.AckJob(context.Background(), j.RunID, j.LeaseID, time.Minute); err != nil {
		t.Fatalf("ack %s: %v", j.RunID, err)
	}
}

func status(t *testing.T, s *Store, runID uuid.UUID) string {
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
			j, ok, err := s.ClaimJob(ctx, Runner{Name: "r", Pool: DefaultPool}, time.Minute)
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
		len(won[0].Modules) != 1 || won[0].Modules[0].ModuleSource != "git::x" || won[0].Modules[0].Inputs["n"] != 1.0 {
		t.Fatalf("claims: %+v", won)
	}
	plan := won[0]
	if _, err := s.Heartbeat(ctx, run.ID, plan.LeaseID, time.Minute); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("a heartbeat before the ack must be refused, got %v", err)
	}
	if _, err := s.FinishJob(ctx, run.ID, plan.LeaseID, "failed", "", nil, nil, nil, nil, nil); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("a finish before the ack must be refused, got %v", err)
	}
	if runs, err := s.WorkingRuns(ctx, w.ID); err != nil || len(runs) != 0 {
		t.Fatalf("state credentials before the ack: %v %v", runs, err)
	}
	ack(t, s, plan)
	ack(t, s, plan) // a retried ack is harmless
	if working, err := s.WorkingRuns(ctx, w.ID); err != nil || len(working) != 1 || working[0].ID != run.ID {
		t.Fatalf("the planning run must hold the workspace's state: %v %v", working, err)
	}

	if _, err := s.Heartbeat(ctx, run.ID, "not-the-lease", time.Minute); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("foreign lease must be rejected, got %v", err)
	}
	for _, chunk := range []string{"0123456789", "abcdefghij", "dropped"} { // the 2nd crosses the limit
		if _, err := s.AppendLog(ctx, run.ID, plan.LeaseID, chunk, time.Minute, 15); err != nil {
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
	if st, err := s.FinishJob(ctx, run.ID, plan.LeaseID, "succeeded", "", nil, sum, nil, nil, nil); err != nil || st != NeedsConfirmation {
		t.Fatalf("finish plan: %q %v", st, err)
	}
	if _, ok, _ := s.ClaimJob(ctx, Runner{Name: "r", Pool: DefaultPool}, time.Minute); ok {
		t.Fatal("a plan waiting for confirmation must not be claimed")
	}
	// a second run waits as pending: not claimable, and canceled it never ran
	if p, err := s.CreateRun(ctx, w.ID, "apply", uuid.Nil, nil); err != nil || p.Status != Pending {
		t.Fatalf("second run while one is active: %+v %v", p, err)
	} else if _, err := s.CancelRun(ctx, p.ID, "test", false); err != nil || status(t, s, p.ID) != Canceled {
		t.Fatalf("cancel a pending run: %v", err)
	}
	// a drift check runs beside it (it reads state, writes nothing), one at a time
	if a, err := s.CreateRun(ctx, w.ID, "assess", uuid.Nil, nil); err != nil || a.Status != PlanQueued {
		t.Fatalf("a drift check beside a run waiting for confirmation: %+v %v", a, err)
	} else if _, err := s.CreateRun(ctx, w.ID, "assess", uuid.Nil, nil); !errors.Is(err, ErrRunActive) {
		t.Fatalf("a second drift check: %v", err)
	} else if _, err := s.CancelRun(ctx, a.ID, "test", false); err != nil {
		t.Fatal(err)
	}
	if err := s.ConfirmRun(ctx, run.ID, uuid.Nil, "alice@example.com"); err != nil {
		t.Fatal(err)
	}
	if err := s.ConfirmRun(ctx, run.ID, uuid.Nil, "alice@example.com"); !errors.Is(err, ErrNotConfirmable) {
		t.Fatalf("confirming twice: %v", err)
	}

	// Apply phase: a new lease; the plan phase's lease is dead.
	apply := claim(t, s, "apply")
	if apply.RunID != run.ID || apply.LeaseID == plan.LeaseID {
		t.Fatalf("apply job: %+v", apply)
	}
	if _, err := s.Heartbeat(ctx, run.ID, plan.LeaseID, time.Minute); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("the plan phase's lease must be dead: %v", err)
	}
	res := []runnerapi.Resource{
		{Address: "azurerm_storage_account.this", Type: "azurerm_storage_account", Name: "this", Provider: "registry.terraform.io/hashicorp/azurerm"},
		{Address: "module.net.azurerm_subnet.a[0]", Type: "azurerm_subnet", Name: "a", Provider: "registry.terraform.io/hashicorp/azurerm", Module: "module.net"},
	}
	if err := s.AddStateVersion(ctx, w.ID, uuid.Nil, "", StateVersion{Serial: 3, Lineage: "lin", BlobKey: "k/" + w.ID.String(), MD5: "m", Size: 1, Outputs: map[string]any{"id": "abc"}}); err != nil {
		t.Fatal(err)
	}
	if st, err := s.FinishJob(ctx, run.ID, apply.LeaseID, "succeeded", "", map[string]any{"id": "abc"}, nil, res, nil, nil); err != nil || st != Applied {
		t.Fatalf("finish apply: %q %v", st, err)
	}
	if _, err := s.FinishJob(ctx, run.ID, apply.LeaseID, "failed", "", nil, nil, nil, nil, nil); !errors.Is(err, ErrLeaseLost) {
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
	if got, err := s.Resources(ctx, w.ID); err != nil || len(got) != 2 || got[1].Module != "module.net" || got[0].Workspace != w.Name || got[0].Serial != 3 {
		t.Fatalf("inventory: %+v %v", got, err)
	}

	// A plan without changes ends the run right away.
	again, err := s.CreateRun(ctx, w.ID, "apply", uuid.Nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	j := claim(t, s, "plan")
	if st, err := s.FinishJob(ctx, again.ID, j.LeaseID, "succeeded", "", nil, nil, nil, nil, nil); err != nil || st != NoChanges {
		t.Fatalf("no-op plan: %q %v", st, err)
	}
	if _, ok, _ := s.ClaimJob(ctx, Runner{Name: "r", Pool: DefaultPool}, time.Minute); ok {
		t.Fatal("nothing should be left to claim")
	}
}

func TestAutoApplyAndDiscard(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	w, run := newWorkspace(t, s, true)
	j := claim(t, s, "plan")
	sum := &runnerapi.Plan{Add: 1}
	if st, _ := s.FinishJob(ctx, run.ID, j.LeaseID, "succeeded", "", nil, sum, nil, nil, nil); st != ApplyQueued {
		t.Fatalf("auto-apply plan: %q", st)
	}
	j = claim(t, s, "apply")
	if st, _ := s.FinishJob(ctx, run.ID, j.LeaseID, "succeeded", "", nil, nil, nil, nil, nil); st != Applied {
		t.Fatalf("auto-applied: %q", st)
	}

	// Destroys ask even with auto-apply; a discarded plan frees the workspace.
	d, err := s.CreateRun(ctx, w.ID, "destroy", uuid.Nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	j = claim(t, s, "plan")
	if j.Action != "destroy" {
		t.Fatalf("job: %+v", j)
	}
	if st, _ := s.FinishJob(ctx, d.ID, j.LeaseID, "succeeded", "", nil, sum, nil, nil, nil); st != NeedsConfirmation {
		t.Fatalf("destroy plan with auto-apply: %q", st)
	}
	if err := s.DiscardRun(ctx, d.ID, "bob@example.com"); err != nil || status(t, s, d.ID) != Discarded {
		t.Fatalf("discard: %v", err)
	}
	if _, err := s.CreateRun(ctx, w.ID, "apply", uuid.Nil, nil); err != nil {
		t.Fatalf("a discarded run must free the workspace: %v", err)
	}
}

// A run ending releases only its own Terraform's lock, not a rollback's or a CLI's.
func TestFinishKeepsForeignStateLock(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	w, run := newWorkspace(t, s, false)
	j, ok, err := s.ClaimJob(ctx, Runner{Name: "r", Pool: DefaultPool}, time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	ack(t, s, j)
	if _, ok, err := s.LockState(ctx, w.ID, "rb", uuid.Nil, []byte(`{"ID":"rb","OpsoleteRun":"`+run.ID.String()+`"}`)); !ok || err != nil {
		t.Fatalf("lock: %v %v", ok, err)
	}
	// The run saw the lock's ID in a 423: that alone must not let it take over, use or release the lock.
	if _, ok, err := s.LockState(ctx, w.ID, "rb", run.ID, []byte(`{"ID":"rb"}`)); ok || err != nil {
		t.Fatalf("a run took over a rollback's lock: %v %v", ok, err)
	}
	if ok, err := s.StateLockedBy(ctx, w.ID, "rb", run.ID); ok || err != nil {
		t.Fatalf("a run may write under a rollback's lock: %v %v", ok, err)
	}
	if ok, err := s.StateLockedBy(ctx, w.ID, "rb", uuid.Nil); !ok || err != nil {
		t.Fatalf("the rollback's own lock: %v %v", ok, err)
	}
	if err := s.UnlockState(ctx, w.ID, "rb", run.ID); !errors.Is(err, ErrStateLocked) { // not its lock: Terraform hears so
		t.Fatalf("a run unlocking a rollback's lock: %v", err)
	}
	if lock, _ := s.StateLockID(ctx, w.ID); lock != "rb" {
		t.Fatalf("a run released a rollback's lock: %q", lock)
	}
	if _, err := s.FinishJob(ctx, run.ID, j.LeaseID, "failed", "423", nil, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if lock, _ := s.StateLockID(ctx, w.ID); lock != "rb" {
		t.Fatalf("a run's end released a lock it didn't take: %q", lock)
	}
}

// Orphaned locks are released: a run's once it has no lease, a rollback's after the timeout.
// A working run's lock, a fresh rollback's and CLI/UI locks stay.
func TestReleaseOrphanedLocks(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	working, run := newWorkspace(t, s, false)
	if _, ok, err := s.ClaimJob(ctx, Runner{Name: "r", Pool: DefaultPool}, time.Minute); err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	queued, queuedRun := newWorkspace(t, s, false) // its run has no lease
	oldRB, _ := newWorkspace(t, s, false)
	newRB, _ := newWorkspace(t, s, false)
	cli, _ := newWorkspace(t, s, false)
	for _, l := range []struct {
		ws  uuid.UUID
		id  string
		run uuid.UUID
	}{
		{working.ID, "l1", run.ID}, {queued.ID, "l2", queuedRun.ID}, {oldRB.ID, "rb1", uuid.Nil}, {newRB.ID, "rb2", uuid.Nil}, {cli.ID, CloudLockPrefix + "x", uuid.Nil},
	} {
		if _, ok, err := s.LockState(ctx, l.ws, l.id, l.run, []byte(`{"ID":"`+l.id+`"}`)); !ok || err != nil {
			t.Fatalf("lock %s: %v %v", l.id, ok, err)
		}
	}
	if _, err := s.Pool.Exec(ctx, `UPDATE workspaces SET state_locked_at = now() - interval '1 hour'
		WHERE id IN ($1, $2, $3)`, working.ID, oldRB.ID, cli.ID); err != nil {
		t.Fatal(err)
	}
	released, err := s.ReleaseOrphanedLocks(ctx, 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	for ws, want := range map[uuid.UUID]string{working.ID: "l1", queued.ID: "", oldRB.ID: "", newRB.ID: "rb2", cli.ID: CloudLockPrefix + "x"} {
		if lock, _ := s.StateLockID(ctx, ws); lock != want {
			t.Errorf("workspace %s: lock %q, want %q", ws, lock, want)
		}
		if _, ok := released[ws]; ok != (want == "") {
			t.Errorf("workspace %s: reported released=%v", ws, ok)
		}
	}
}

// A run's state write is recorded only while it applies with a lease: not from a plan, not after a force-cancel.
func TestStateWriteFenced(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	w, run := newWorkspace(t, s, false)
	if _, ok, err := s.ClaimJob(ctx, Runner{Name: "r", Pool: DefaultPool}, time.Minute); err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	v := StateVersion{Serial: 1, Lineage: "lin", BlobKey: "fence/" + w.ID.String(), MD5: "m", Size: 1}
	if err := s.AddStateVersion(ctx, w.ID, run.ID, "", v); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("a planning run must not record state: %v", err)
	}
	if _, err := s.Pool.Exec(ctx, `UPDATE runs SET status = 'applying' WHERE id = $1`, run.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.AddStateVersion(ctx, w.ID, run.ID, "", v); err != nil {
		t.Fatalf("applying run: %v", err)
	}
	s.CancelRun(ctx, run.ID, "a@b.c", false) // asked first: force is for a runner that doesn't stop
	if _, err := s.CancelRun(ctx, run.ID, "a@b.c", true); err != nil {
		t.Fatal(err)
	}
	v.Serial, v.BlobKey = 2, "fence2/"+w.ID.String()
	if err := s.AddStateVersion(ctx, w.ID, run.ID, "", v); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("a force-canceled run must not record state: %v", err)
	}
}

// A cloud API upload is recorded only while its user holds the state lock, checked atomically with
// the insert: a force-unlock that commits while the insert waits for it wins.
func TestCloudStateWriteFenced(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	w, _ := newWorkspace(t, s, false)
	lockID := CloudLockPrefix + "x"
	if _, ok, err := s.LockState(ctx, w.ID, lockID, uuid.Nil, []byte(`{"ID":"`+lockID+`","UserID":"u1"}`)); !ok || err != nil {
		t.Fatalf("lock: %v %v", ok, err)
	}
	v := StateVersion{Serial: 1, Lineage: "lin", BlobKey: "cloud1/" + w.ID.String(), MD5: "m1", Size: 1}
	if err := s.AddStateVersion(ctx, w.ID, uuid.Nil, "u2", v); !errors.Is(err, ErrNotCloudLocked) {
		t.Fatalf("another user's lock: %v", err)
	}
	if err := s.AddStateVersion(ctx, w.ID, uuid.Nil, "u1", v); err != nil {
		t.Fatalf("own lock: %v", err)
	}

	// A force-unlock is under way (uncommitted) when the upload is recorded: the insert waits for it.
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `UPDATE workspaces SET state_lock = NULL, state_locked_at = NULL WHERE id = $1`, w.ID); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- s.AddStateVersion(ctx, w.ID, uuid.Nil, "u1", StateVersion{Serial: 2, Lineage: "lin", BlobKey: "cloud2/" + w.ID.String(), MD5: "m2", Size: 1})
	}()
	select {
	case err := <-done:
		t.Fatalf("the upload didn't wait for the unlock: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, ErrNotCloudLocked) {
		t.Fatalf("an upload after the force-unlock was recorded: %v", err)
	}
	if vs, _ := s.StateVersions(ctx, w.ID, 10); len(vs) != 1 {
		t.Fatalf("%d state versions, want 1", len(vs))
	}
}

func TestExpiredLeaseIsErrored(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	w, run := newWorkspace(t, s, false)
	j, ok, err := s.ClaimJob(ctx, Runner{Name: "r", Pool: DefaultPool}, time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	if err := s.AckJob(ctx, run.ID, j.LeaseID, 10*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.LockState(ctx, w.ID, "l1", run.ID, []byte(`{"ID":"l1"}`)); !ok || err != nil {
		t.Fatalf("lock: %v %v", ok, err)
	}
	time.Sleep(50 * time.Millisecond)
	if u, err := s.UnclaimJobs(ctx); err != nil || len(u) != 0 {
		t.Fatalf("an acknowledged job must never be unclaimed: %v %v", u, err)
	}
	ids, err := s.ReapExpiredLeases(ctx)
	if err != nil || len(ids) != 1 || ids[0] != run.ID {
		t.Fatalf("reap: %v %v", ids, err)
	}
	// The old runner is fenced off once its lease is gone.
	if _, err := s.AppendLog(ctx, run.ID, j.LeaseID, "late", time.Minute, 1<<20); !errors.Is(err, ErrLeaseLost) {
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

// A claim not acknowledged within its lease (the ack window) goes back to the queue, plan and apply
// phase alike, and the late runner is fenced off; an acknowledged one stays.
func TestUnclaim(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	_, run := newWorkspace(t, s, false)
	unclaim := func(want ...uuid.UUID) {
		t.Helper()
		time.Sleep(50 * time.Millisecond)
		u, err := s.UnclaimJobs(ctx)
		if err != nil || len(u) != len(want) {
			t.Fatalf("unclaim: %+v %v, want %v", u, err, want)
		}
		for i := range want {
			if u[i].RunID != want[i] || u[i].Runner != "lost" {
				t.Fatalf("unclaimed %+v, want %v", u[i], want[i])
			}
		}
	}
	listen, err := s.Pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer listen.Release()
	if _, err := listen.Exec(ctx, "LISTEN "+JobsChannel); err != nil {
		t.Fatal(err)
	}

	j, ok, err := s.ClaimJob(ctx, Runner{Name: "lost", Pool: DefaultPool}, 10*time.Millisecond)
	if err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	unclaim(run.ID)
	if st := status(t, s, run.ID); st != PlanQueued {
		t.Fatalf("status %s, want plan_queued", st)
	}
	if r, hist, _ := s.GetRun(ctx, run.ID); r.Runner != nil || !strings.Contains(*hist[len(hist)-1].Message, "not acknowledged by runner lost") {
		t.Fatalf("runner %v, history %+v", r.Runner, hist[len(hist)-1])
	}
	wait, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := listen.Conn().WaitForNotification(wait); err != nil {
		t.Fatalf("unclaim must wake claimers: %v", err)
	}
	if err := s.AckJob(ctx, run.ID, j.LeaseID, time.Minute); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("a late ack must find the lease gone: %v", err)
	}

	// another runner takes it; the plan needs confirmation, then its apply phase is lost the same way
	plan := claim(t, s, "plan")
	if _, err := s.FinishJob(ctx, run.ID, plan.LeaseID, "succeeded", "", nil, &runnerapi.Plan{Add: 1}, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.ConfirmRun(ctx, run.ID, uuid.Nil, "a@b.c"); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.ClaimJob(ctx, Runner{Name: "lost", Pool: DefaultPool}, 10*time.Millisecond); err != nil || !ok {
		t.Fatalf("claim apply: %v %v", ok, err)
	}
	unclaim(run.ID)
	if st := status(t, s, run.ID); st != ApplyQueued {
		t.Fatalf("status %s, want apply_queued", st)
	}
	apply := claim(t, s, "apply")
	if err := s.AckJob(ctx, run.ID, apply.LeaseID, 10*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	unclaim()
	if st := status(t, s, run.ID); st != Applying {
		t.Fatalf("an acknowledged job was unclaimed: %s", st)
	}
}

// TestRunVariables: a workspace's own variables win over attached sets, attached sets over
// global ones, and between sets of one kind the first by name.
func TestRunVariables(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	name := "t-" + strings.ToLower(rand.Text()[:10])
	w, _, err := s.CreateWorkspace(ctx, uuid.Nil, name, "", testTemplate(nil), false, uuid.Nil)
	if err != nil {
		t.Fatal(err)
	}
	other, _, err := s.CreateWorkspace(ctx, uuid.Nil, name+"-other", "", testTemplate(nil), false, uuid.Nil)
	if err != nil {
		t.Fatal(err)
	}
	set := func(t *testing.T, o VarOwner, cat, key, val string) {
		if err := s.SetVariable(ctx, o, Variable{Category: cat, Key: key, Value: []byte(val)}); err != nil {
			t.Fatal(err)
		}
	}
	sets := map[string]uuid.UUID{}
	k := strings.ToUpper(strings.ReplaceAll(name, "-", "_"))
	got := func(t *testing.T, id uuid.UUID) map[string]string {
		vars, err := s.RunVariables(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		m := map[string]string{}
		for _, v := range vars {
			if strings.HasPrefix(v.Key, k) {
				m[v.Category+":"+v.Key] = string(v.Value)
			}
		}
		return m
	}
	var ps VariableSet // the project's set
	step(t, "variable sets attach to workspaces and select exactly their names", func(t *testing.T) {
		for _, n := range []string{"a-global", "b-attached", "c-attached"} {
			if err := s.CreateVariableSet(ctx, name+n, "", n == "a-global"); err != nil {
				t.Fatal(err)
			}
			vs, err := s.GetVariableSet(ctx, name+n)
			if err != nil {
				t.Fatal(err)
			}
			sets[n] = vs.ID
		}
		if err := s.CreateVariableSet(ctx, name+"a-global", "", false); !errors.Is(err, ErrExists) {
			t.Fatalf("duplicate set name: %v", err)
		}
		for _, n := range []string{"b-attached", "c-attached"} {
			if err := s.AttachVariableSet(ctx, sets[n], w.Name, true); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.AttachVariableSet(ctx, sets["b-attached"], "no-such-workspace", true); !errors.Is(err, ErrNotFound) {
			t.Fatalf("attach to an unknown workspace: %v", err)
		}
		// Select makes the selection exactly its names; an unknown name changes nothing, not even the other picks.
		attached := func() []string { vs, _ := s.GetVariableSet(ctx, name+"c-attached"); return vs.Workspaces }
		if err := s.Select(ctx, sets["c-attached"], Pick{SetWorkspaces, nil}, Pick{SetProjects, []string{"no-such-project"}}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("select an unknown project: %v", err)
		}
		if a := attached(); !slices.Equal(a, []string{w.Name}) {
			t.Fatalf("a failed select changed the selection: %v", a)
		}
		if err := s.Select(ctx, sets["c-attached"], Pick{SetWorkspaces, nil}); err != nil || len(attached()) != 0 {
			t.Fatalf("deselect all: %v %v", err, attached())
		}
		if err := s.Select(ctx, sets["c-attached"], Pick{SetWorkspaces, []string{w.Name}}); err != nil || !slices.Equal(attached(), []string{w.Name}) {
			t.Fatalf("select: %v %v", err, attached())
		}
	})
	step(t, "attached sets win over global ones, the workspace's own variables over both", func(t *testing.T) {
		set(t, VarOwner{SetID: sets["a-global"]}, "env", k+"_G", "global")
		set(t, VarOwner{SetID: sets["a-global"]}, "env", k+"_X", "global")
		set(t, VarOwner{SetID: sets["c-attached"]}, "env", k+"_X", "c")
		set(t, VarOwner{SetID: sets["b-attached"]}, "env", k+"_X", "b")
		set(t, VarOwner{SetID: sets["b-attached"]}, "env", k+"_W", "b")
		set(t, VarOwner{WorkspaceID: w.ID}, "env", k+"_W", "old")
		set(t, VarOwner{WorkspaceID: w.ID}, "env", k+"_W", "workspace") // replaces
		set(t, VarOwner{WorkspaceID: w.ID}, "terraform", k+"_W", "tf")  // other category, no clash

		want := map[string]string{"env:" + k + "_G": "global", "env:" + k + "_X": "b", "env:" + k + "_W": "workspace", "terraform:" + k + "_W": "tf"}
		if m := got(t, w.ID); !maps.Equal(m, want) {
			t.Fatalf("got %v, want %v", m, want)
		}
		if m := got(t, other.ID); !maps.Equal(m, map[string]string{"env:" + k + "_G": "global", "env:" + k + "_X": "global"}) {
			t.Fatalf("a workspace without attachments gets only global sets: %v", m)
		}
		if err := s.AttachVariableSet(ctx, sets["b-attached"], w.Name, false); err != nil {
			t.Fatal(err)
		}
		if m := got(t, w.ID); m["env:"+k+"_X"] != "c" {
			t.Fatalf("after detaching b, c must win: %v", m)
		}
	})

	// Project sets rank between the workspace's attached sets and global ones, whatever their names:
	// "a-project" sorts before every set but "a-global".
	step(t, "project sets rank between attached and global sets", func(t *testing.T) {
		p, err := s.CreateProject(ctx, name, "")
		if err != nil {
			t.Fatal(err)
		}
		if err := s.MoveWorkspace(ctx, w.ID, p.ID); err != nil {
			t.Fatal(err)
		}
		if err := s.CreateVariableSet(ctx, name+"a-project", "", false); err != nil {
			t.Fatal(err)
		}
		ps, _ = s.GetVariableSet(ctx, name+"a-project")
		if err := s.AttachVariableSetProject(ctx, ps.ID, p.Name, true); err != nil {
			t.Fatal(err)
		}
		if err := s.AttachVariableSetProject(ctx, ps.ID, "no-such-project", true); !errors.Is(err, ErrNotFound) {
			t.Fatalf("attach to an unknown project: %v", err)
		}
		if ps, _ = s.GetVariableSet(ctx, ps.Name); !slices.Equal(ps.Projects, []string{p.Name}) {
			t.Fatalf("set's projects: %v", ps.Projects)
		}
		for _, key := range []string{"_G", "_X", "_P", "_W"} {
			set(t, VarOwner{SetID: ps.ID}, "env", k+key, "project")
		}
		want := map[string]string{"env:" + k + "_G": "project", "env:" + k + "_X": "c", "env:" + k + "_P": "project",
			"env:" + k + "_W": "workspace", "terraform:" + k + "_W": "tf"}
		if m := got(t, w.ID); !maps.Equal(m, want) {
			t.Fatalf("with a project set: got %v, want %v", m, want)
		}
		if m := got(t, other.ID); m["env:"+k+"_P"] != "" {
			t.Fatalf("a workspace of another project gets the set: %v", m)
		}
	})

	// A VCS-linked workspace gets neither project nor global sets, only those attached to it.
	step(t, "a VCS-linked workspace gets only the sets attached to it", func(t *testing.T) {
		if _, err := s.CreateVCSConnection(ctx, VCSConnection{Name: name, Provider: "github", Token: []byte("x")}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Pool.Exec(context.Background(), `INSERT INTO workspace_vcs (workspace_id, connection_id, repo, branch)
			SELECT $1, id, 'acme/infra', 'main' FROM vcs_connections WHERE name = $2`, w.ID, name); err != nil {
			t.Fatal(err)
		}
		if m := got(t, w.ID); m["env:"+k+"_P"] != "" || m["env:"+k+"_G"] != "" || m["env:"+k+"_X"] != "c" {
			t.Fatalf("VCS-linked: %v", m)
		}
		if shown, _ := s.WorkspaceVariableSets(ctx, w.ID); slices.ContainsFunc(shown, func(vs VariableSet) bool { return vs.ID == ps.ID }) {
			t.Fatal("a VCS-linked workspace lists the project set")
		}
	})
}

func TestAccess(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	creator, _ := s.UpsertUser(ctx, "", "creator@example.com", "user", nil)
	member, _ := s.UpsertUser(ctx, "", "member@example.com", "user", []string{"team-a", "team-b"})
	outsider, _ := s.UpsertUser(ctx, "", "outsider@example.com", "user", []string{"team-c"})
	name := "t-" + strings.ToLower(rand.Text()[:10])
	w, _, err := s.CreateWorkspace(ctx, uuid.Nil, name, "", testTemplate(nil), false, creator)
	if err != nil {
		t.Fatal(err)
	}

	sees := func(user uuid.UUID, admin bool) bool {
		list, err := s.ListWorkspaces(ctx, Viewer{UserID: user, Admin: admin}, "")
		if err != nil {
			t.Fatal(err)
		}
		for _, x := range list {
			if x.ID == w.ID {
				return true
			}
		}
		return false
	}
	level := func(user uuid.UUID) string {
		l, err := s.Access(ctx, w.ID, user)
		if err != nil {
			t.Fatal(err)
		}
		return l
	}
	if !sees(creator, false) || level(creator) != "operate" || sees(member, false) || level(member) != "" || !sees(uuid.Nil, true) {
		t.Fatal("before grants: only the creator and admins")
	}
	s.SetGrant(ctx, w.ID, "team-a", "read")
	if !sees(member, false) || level(member) != "read" || sees(outsider, false) || level(outsider) != "" {
		t.Fatalf("read grant: member %q", level(member))
	}
	s.SetGrant(ctx, w.ID, "team-b", "operate") // the strongest of the member's grants wins
	if level(member) != "operate" {
		t.Fatalf("operate grant: %q", level(member))
	}
	s.SetGrant(ctx, w.ID, "team-b", "")
	s.SetGrant(ctx, w.ID, "team-a", "")
	if sees(member, false) || level(member) != "" {
		t.Fatal("revoked grants must hide the workspace again")
	}
	if g, _ := s.Grants(ctx, w.ID); len(g) != 0 {
		t.Fatalf("grants: %+v", g)
	}

	// a project grant reaches every workspace of the project; the highest level wins
	p, err := s.CreateProject(ctx, "p-"+strings.ToLower(rand.Text()[:10]), "")
	if err != nil {
		t.Fatal(err)
	}
	sibling, _ := newWorkspace(t, s, false)
	for _, id := range []uuid.UUID{w.ID, sibling.ID} {
		if err := s.MoveWorkspace(ctx, id, p.ID); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SetProjectGrant(ctx, p.ID, "team-a", "read"); err != nil {
		t.Fatal(err)
	}
	if l, _ := s.Access(ctx, sibling.ID, member); !sees(member, false) || level(member) != "read" || l != "read" || sees(outsider, false) {
		t.Fatalf("project read grant: %q %q", level(member), l)
	}
	if list, _ := s.Projects(ctx, Viewer{UserID: member}); !slices.ContainsFunc(list, func(x Project) bool { return x.ID == p.ID }) {
		t.Fatal("a project grant shows the project")
	}
	s.SetGrant(ctx, w.ID, "team-b", "operate")
	if level(member) != "operate" {
		t.Fatalf("workspace operate over project read: %q", level(member))
	}
	s.SetGrant(ctx, w.ID, "team-b", "read")
	s.SetProjectGrant(ctx, p.ID, "team-a", "operate")
	if level(member) != "operate" {
		t.Fatalf("project operate over workspace read: %q", level(member))
	}
	if g, _ := s.ProjectGrants(ctx, p.ID); len(g) != 1 || g[0] != (Grant{"team-a", "operate"}) {
		t.Fatalf("project grants: %+v", g)
	}
	s.SetGrant(ctx, w.ID, "team-b", "")
	s.SetProjectGrant(ctx, p.ID, "team-a", "")
	if sees(member, false) || level(member) != "" {
		t.Fatal("revoked project grant must hide the workspace again")
	}

	other, _ := newWorkspace(t, s, false)
	if err := s.SaveGeneral(ctx, w.ID, General{Name: other.Name, AutoApply: true, ExecutionMode: "remote"}); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("rename onto a taken name: %v", err)
	}
	if got, _ := s.GetWorkspaceByID(ctx, w.ID); got.AutoApply || got.ExecutionMode != "remote" {
		t.Fatalf("a failed save changed settings: %+v", got)
	}
	if err := s.SaveGeneral(ctx, w.ID, General{Name: name + "-new", ExecutionMode: "remote"}); err != nil {
		t.Fatal(err)
	}
	if got, err := s.GetWorkspace(ctx, name+"-new"); err != nil || got.ID != w.ID {
		t.Fatalf("renamed: %+v %v", got, err)
	}
	if err := s.SaveGeneral(ctx, uuid.New(), General{Name: name + "-gone", ExecutionMode: "remote"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rename of an unknown workspace: %v", err)
	}
}

func TestCancel(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// Queued: canceled at once, and the workspace is free again.
	w, run := newWorkspace(t, s, false)
	if st, err := s.CancelRun(ctx, run.ID, "a@b.c", false); err != nil || st != Canceled {
		t.Fatalf("cancel queued: %q %v", st, err)
	}
	if _, err := s.CancelRun(ctx, run.ID, "a@b.c", false); !errors.Is(err, ErrNotCancelable) {
		t.Fatalf("cancel twice: %v", err)
	}

	// Running: asked to stop; the runner keeps its lease (Terraform must still save state) and
	// hears about it on its next heartbeat or log post.
	run2, err := s.CreateRun(ctx, w.ID, "apply", uuid.Nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	j := claim(t, s, "plan")
	if cancel, err := s.Heartbeat(ctx, run2.ID, j.LeaseID, time.Minute); err != nil || cancel {
		t.Fatalf("no cancel yet: %v %v", cancel, err)
	}
	if st, err := s.CancelRun(ctx, run2.ID, "a@b.c", false); err != nil || st != Planning {
		t.Fatalf("graceful cancel: %q %v", st, err)
	}
	if cancel, err := s.Heartbeat(ctx, run2.ID, j.LeaseID, time.Minute); err != nil || !cancel {
		t.Fatalf("the runner must hear the cancel and keep its lease: %v %v", cancel, err)
	}
	if cancel, err := s.AppendLog(ctx, run2.ID, j.LeaseID, "interrupted\n", time.Minute, 1<<20); err != nil || !cancel {
		t.Fatalf("log posts carry the cancel too: %v %v", cancel, err)
	}
	if st, _ := s.FinishJob(ctx, run2.ID, j.LeaseID, "canceled", "", nil, nil, nil, nil, nil); st != Canceled {
		t.Fatalf("finish canceled: %q", st)
	}

	// The plan finishes before the cancel reaches Terraform: the run ends, its apply is never queued.
	run4, _ := s.CreateRun(ctx, w.ID, "apply", uuid.Nil, nil)
	j = claim(t, s, "plan")
	if _, err := s.CancelRun(ctx, run4.ID, "a@b.c", false); err != nil {
		t.Fatal(err)
	}
	plan := &runnerapi.Plan{Add: 1}
	if st, err := s.FinishJob(ctx, run4.ID, j.LeaseID, "succeeded", "", nil, plan, nil, nil, nil); err != nil || st != Canceled {
		t.Fatalf("a plan finished after a cancel request: %q %v", st, err)
	}

	// Force: cut off right away; the runner's lease and state lock are gone.
	run3, _ := s.CreateRun(ctx, w.ID, "apply", uuid.Nil, nil)
	j = claim(t, s, "plan")
	s.LockState(ctx, w.ID, "l1", run3.ID, []byte(`{"ID":"l1"}`))
	if _, err := s.CancelRun(ctx, run3.ID, "a@b.c", true); !errors.Is(err, ErrForceFirst) {
		t.Fatalf("force cancel before a cancel was asked: %v", err)
	}
	s.CancelRun(ctx, run3.ID, "a@b.c", false)
	if st, err := s.CancelRun(ctx, run3.ID, "a@b.c", true); err != nil || st != Canceled {
		t.Fatalf("force cancel: %q %v", st, err)
	}
	if _, err := s.Heartbeat(ctx, run3.ID, j.LeaseID, time.Minute); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("a force-canceled runner must be fenced off: %v", err)
	}
	if lock, _ := s.StateLockID(ctx, w.ID); lock != "" {
		t.Fatalf("force cancel must release the state lock: %q", lock)
	}
}

func TestRunMetrics(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	w, run := newWorkspace(t, s, false)
	// planning for 90s, then errored; the trailing errored entry has no end and is ignored
	if _, err := s.Pool.Exec(context.Background(), `INSERT INTO run_status_history (run_id, status, created_at) VALUES
		($1, 'planning', now() + interval '1 hour'), ($1, 'errored', now() + interval '1 hour 90 seconds')`, run.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(context.Background(), `UPDATE runs SET status = 'errored', lease_id = NULL, lease_until = NULL WHERE id = $1`, run.ID); err != nil {
		t.Fatal(err)
	}
	m, err := s.RunMetrics(ctx, w.ID)
	if err != nil || m.Runs != 1 || m.Failed != 1 || m.AvgPlan != 90*time.Second || m.AvgApply != 0 {
		t.Fatalf("metrics: %+v %v", m, err)
	}
}

// TestVCSWorkspaceVariableSets: a workspace linked to a repository gets no global sets, and a
// changed link detaches its sets, since the code they'd reach is chosen by the workspace's users.
func TestVCSWorkspaceVariableSets(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	name := "t-" + strings.ToLower(rand.Text()[:10])
	w, err := s.CreateEmptyWorkspace(ctx, EmptyWorkspace{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	conn, err := s.CreateVCSConnection(ctx, VCSConnection{Name: name, Provider: "github", Token: []byte("x")})
	if err != nil {
		t.Fatal(err)
	}
	k := strings.ToUpper(strings.ReplaceAll(name, "-", "_"))
	for _, n := range []string{"global", "attached"} {
		if err := s.CreateVariableSet(ctx, name+n, "", n == "global"); err != nil {
			t.Fatal(err)
		}
		vs, _ := s.GetVariableSet(ctx, name+n)
		if err := s.SetVariable(ctx, VarOwner{SetID: vs.ID}, Variable{Category: "env", Key: k + "_" + strings.ToUpper(n), Value: []byte(n)}); err != nil {
			t.Fatal(err)
		}
	}
	attach := func() {
		vs, _ := s.GetVariableSet(ctx, name+"attached")
		if err := s.AttachVariableSet(ctx, vs.ID, name, true); err != nil {
			t.Fatal(err)
		}
	}
	check := func(want ...string) {
		t.Helper()
		vars, err := s.RunVariables(ctx, w.ID)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, v := range vars {
			if strings.HasPrefix(v.Key, k) {
				got = append(got, string(v.Value))
			}
		}
		sets, err := s.WorkspaceVariableSets(ctx, w.ID)
		if err != nil {
			t.Fatal(err)
		}
		var shown []string
		for _, vs := range sets {
			shown = append(shown, strings.TrimPrefix(vs.Name, name))
		}
		if !slices.Equal(got, want) || !slices.Equal(shown, want) {
			t.Fatalf("variables from %v, sets shown %v, want %v", got, shown, want)
		}
	}
	attach()
	check("attached", "global")
	if err := s.SetWorkspaceVCS(ctx, w.ID, conn, "org/repo", "main", "", "", true, false); err != nil {
		t.Fatal(err)
	}
	check() // linking detached the set, and global sets don't reach a linked workspace
	attach()
	check("attached")
	if err := s.SetWorkspaceVCS(ctx, w.ID, conn, "org/repo", "main", "", "", true, false); err != nil {
		t.Fatal(err)
	}
	check("attached") // same link: the admin's attachment stands
	if err := s.SetWorkspaceVCS(ctx, w.ID, conn, "org/repo", "main", "", "", false, true); err != nil {
		t.Fatal(err)
	}
	check() // pull request plans run unreviewed code: the admin's attachment is gone
	attach()
	if err := s.SetWorkspaceVCS(ctx, w.ID, conn, "org/repo", "main", "", "", true, true); err != nil {
		t.Fatal(err)
	}
	check("attached") // they were on already
	if err := s.SetWorkspaceVCS(ctx, w.ID, conn, "org/repo", "other", "", "", true, false); err != nil {
		t.Fatal(err)
	}
	check()
}

func TestQueueVCSRun(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	name := "t-" + strings.ToLower(rand.Text()[:10])
	w, err := s.CreateEmptyWorkspace(ctx, EmptyWorkspace{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	conn, err := s.CreateVCSConnection(ctx, VCSConnection{Name: name, Provider: "github", Token: []byte("x")})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetWorkspaceVCS(ctx, w.ID, conn, "org/repo", "main", "", "", true, true); err != nil {
		t.Fatal(err)
	}
	if ws, err := s.VCSTriggered(ctx); err != nil || !slices.ContainsFunc(ws, func(x Workspace) bool { return x.ID == w.ID && x.VCSTrigger && x.VCSPullRequests }) {
		t.Fatalf("triggered workspaces: %v %v", ws, err)
	}
	queue := func(commit string, pr int, want bool) {
		t.Helper()
		if ok, err := s.QueueVCSRun(ctx, w.ID, commit, pr, "test"); err != nil || ok != want {
			t.Fatalf("queue %s #%d: %v %v, want %v", commit, pr, ok, err, want)
		}
	}
	queue("aaa", 0, true)
	queue("bbb", 7, true)  // a pull request's plan runs beside the push run: it reads state, writes nothing
	queue("bbb", 7, false) // one at a time per pull request (and that commit has it)
	for range 2 {
		j := claim(t, s, "plan")
		if j.VCSCommit == "" {
			t.Fatalf("job without pinned commit: %+v", j)
		}
		want := map[string]string{"apply": NeedsConfirmation, "plan": Planned}[j.Action]
		if got, err := s.FinishJob(ctx, j.RunID, j.LeaseID, "succeeded", "", nil, &runnerapi.Plan{Add: 1}, nil, nil, nil); err != nil || got != want {
			t.Fatalf("finish %s: %q %v, want %q", j.Action, got, err, want)
		}
	}
	runs, err := s.Runs(ctx, w.ID, 10)
	i := slices.IndexFunc(runs, func(r Run) bool { return r.Action == "apply" && r.VCSCommit == "aaa" })
	if err != nil || i < 0 {
		t.Fatalf("push run: %+v %v", runs, err)
	}
	if err := s.DiscardRun(ctx, runs[i].ID, "x"); err != nil {
		t.Fatal(err)
	}
	queue("aaa", 0, false) // that commit had its run
	queue("bbb", 7, false) // planned already
	queue("bbb", 0, true)  // merged by fast-forward: the speculative plan doesn't count as its run
}

func TestConcurrentTriggers(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	w, run := newWorkspace(t, s, false)
	if _, err := s.CancelRun(ctx, run.ID, "test", false); err != nil { // free the workspace
		t.Fatal(err)
	}
	// Many users trigger at once: runs_one_active_idx lets exactly one run in, the others wait as pending
	var wg sync.WaitGroup
	var mu sync.Mutex
	won, waiting := 0, 0
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := s.CreateRun(ctx, w.ID, "apply", uuid.Nil, nil)
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			if r.Status == Pending {
				waiting++
			} else {
				won++
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	if won != 1 || waiting != 7 {
		t.Fatalf("%d runs queued and %d pending, want 1 and 7", won, waiting)
	}
}

// A rollback is recorded only while its own lock is still the workspace's: one released under it
// (STALE_LOCK_TIMEOUT, force-unlock) and maybe taken by someone else changes nothing.
func TestRollbackWriteFenced(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	w, _ := newWorkspace(t, s, false)
	v := StateVersion{Serial: 1, Lineage: "lin", BlobKey: "base/" + w.ID.String(), MD5: "m0", Size: 1}
	if err := s.AddStateVersion(ctx, w.ID, uuid.Nil, "", v); err != nil {
		t.Fatal(err)
	}
	vs, _ := s.StateVersions(ctx, w.ID, 1)
	lock := func(id string) {
		t.Helper()
		if _, ok, err := s.LockState(ctx, w.ID, id, uuid.Nil, []byte(`{"ID":"`+id+`"}`)); !ok || err != nil {
			t.Fatalf("lock %s: %v %v", id, ok, err)
		}
	}
	copyOf := func(n int64) StateVersion {
		return StateVersion{Serial: n, Lineage: "lin", BlobKey: fmt.Sprintf("rb%d/%s", n, w.ID), MD5: "m", Size: 1}
	}

	lock("rollback-1")
	// The orphaned-lock release ran meanwhile (timeout 0: every rollback lock is stale).
	if _, err := s.ReleaseOrphanedLocks(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if err := s.AddRollbackVersion(ctx, w.ID, vs[0].ID, "rollback-1", copyOf(2)); !errors.Is(err, ErrLockLost) {
		t.Fatalf("a rollback whose lock was released: %v", err)
	}
	lock("someone-else")
	if err := s.AddRollbackVersion(ctx, w.ID, vs[0].ID, "rollback-1", copyOf(2)); !errors.Is(err, ErrLockLost) {
		t.Fatalf("a rollback whose lock someone else holds now: %v", err)
	}
	if err := s.UnlockState(ctx, w.ID, "someone-else", uuid.Nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.StateVersions(ctx, w.ID, 10); len(got) != 1 {
		t.Fatalf("a fenced-off rollback recorded a version: %d versions", len(got))
	}
	lock("rollback-2")
	if err := s.AddRollbackVersion(ctx, w.ID, vs[0].ID, "rollback-2", copyOf(2)); err != nil {
		t.Fatalf("a rollback holding its lock: %v", err)
	}
}

// TestRunnerPools: a project's jobs go only to runners of its pool; a runner is known by its token, and a
// deleted one is unknown.
func TestRunnerPools(t *testing.T) {
	s, ctx := testStore(t), context.Background()
	prdToken := "prd-runner-token-0123456789abcdef0123"
	if err := s.RegisterRunner(ctx, "prd-1", "prd", prdToken); err != nil {
		t.Fatal(err)
	}
	if err := s.RegisterRunner(ctx, "other", DefaultPool, prdToken); !errors.Is(err, ErrExists) {
		t.Fatalf("another runner with the same token: %v", err)
	}
	devToken, err := s.CreateRunner(ctx, "dev-1", DefaultPool)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateRunner(ctx, "dev-1", DefaultPool); !errors.Is(err, ErrExists) {
		t.Fatalf("the name again: %v", err)
	}
	prd, err := s.RunnerByToken(ctx, prdToken)
	if err != nil || prd.Name != "prd-1" || prd.Pool != "prd" {
		t.Fatalf("by token: %+v %v", prd, err)
	}
	dev, err := s.RunnerByToken(ctx, devToken)
	if err != nil || dev.Name != "dev-1" {
		t.Fatalf("by token: %+v %v", dev, err)
	}

	created, err := s.CreateProject(ctx, "prd-"+strings.ToLower(rand.Text()[:6]), "")
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.Project(ctx, created.Name)
	pid := p.ID
	if err != nil || p.RunnerPool != DefaultPool {
		t.Fatalf("a new project's pool: %+v %v", p, err)
	}
	if err := s.UpdateProject(ctx, pid, p.Name, "", "prd"); err != nil {
		t.Fatal(err)
	}
	w, run := newWorkspace(t, s, false)
	if err := s.MoveWorkspace(ctx, w.ID, pid); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.ClaimJob(ctx, dev, time.Minute); err != nil || ok {
		t.Fatalf("a default-pool runner got a prd job: %v %v", ok, err)
	}
	j, ok, err := s.ClaimJob(ctx, prd, time.Minute)
	if err != nil || !ok || j.RunID != run.ID {
		t.Fatalf("the prd runner: %+v %v %v", j, ok, err)
	}
	if pools, err := s.RunnerPools(ctx); err != nil || !slices.Equal(pools, []string{DefaultPool, "prd"}) {
		t.Fatalf("pools: %v %v", pools, err)
	}

	if _, err := s.DeleteRunner(ctx, prd.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RunnerByToken(ctx, prdToken); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a deleted runner's token: %v", err)
	}
}

// putTemplates makes mods (by name, over the current ones) the workspace's current configuration version without a
// run, as an applied run would.
func putTemplates(t *testing.T, s *Store, workspaceID uuid.UUID, mods ...WorkspaceTemplate) {
	t.Helper()
	err := s.tx(context.Background(), func(q *db.Queries) error {
		cur, have, err := currentConfig(context.Background(), q, workspaceID)
		if err != nil {
			return err
		}
		for _, m := range mods {
			have = withModule(have, m)
		}
		cv, err := newVersion(context.Background(), q, workspaceID, cur, have, SourceForm, uuid.Nil)
		if err != nil {
			return err
		}
		return setCurrent(context.Background(), q, workspaceID, cv)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// A cancel asked while a claim isn't acknowledged yet ends the run when the claim goes back: nothing ran, and
// queuing it again would let the next runner start it.
func TestCancelUnacknowledged(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	_, run := newWorkspace(t, s, false)
	if _, _, err := s.ClaimJob(ctx, Runner{Name: "r", Pool: DefaultPool}, -time.Second); err != nil { // never acknowledged
		t.Fatal(err)
	}
	if _, err := s.CancelRun(ctx, run.ID, "a@b.c", false); err != nil {
		t.Fatal(err)
	}
	if u, err := s.UnclaimJobs(ctx); err != nil || len(u) != 1 || !u[0].Canceled {
		t.Fatalf("unclaim: %+v %v", u, err)
	}
	if st := status(t, s, run.ID); st != Canceled {
		t.Fatalf("status %s, want canceled", st)
	}
}

// A priority set's variable wins over the workspace's own and every other set's, where the set applies.
func TestPriorityVariableSet(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	w, _ := newWorkspace(t, s, false)
	name := "prio-" + strings.ToLower(rand.Text()[:8])
	for _, n := range []string{"a", "b"} {
		if err := s.CreateVariableSet(ctx, name+n, "", true); err != nil {
			t.Fatal(err)
		}
	}
	a, _ := s.GetVariableSet(ctx, name+"a")
	b, _ := s.GetVariableSet(ctx, name+"b")
	put := func(o VarOwner, v string) {
		if err := s.SetVariable(ctx, o, Variable{Category: "terraform", Key: "region", Value: []byte(v)}); err != nil {
			t.Fatal(err)
		}
	}
	put(VarOwner{WorkspaceID: w.ID}, "own")
	put(VarOwner{SetID: a.ID}, "a")
	put(VarOwner{SetID: b.ID}, "b")
	region := func() string {
		vars, err := s.RunVariables(ctx, w.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range vars {
			if v.Key == "region" {
				return string(v.Value)
			}
		}
		return ""
	}
	if got := region(); got != "own" {
		t.Fatalf("without priority: %s, want the workspace's own", got)
	}
	if err := s.UpdateVariableSet(ctx, b.ID, "", true, true); err != nil {
		t.Fatal(err)
	}
	if got := region(); got != "b" {
		t.Fatalf("with priority set b: %s", got)
	}
	if vs, _ := s.GetVariableSet(ctx, name+"b"); !vs.Priority {
		t.Fatal("priority not stored")
	}
}

// A push while a run is active waits as pending; only the newest pending push stays, and it runs when the
// active run ends.
func TestPendingPush(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	w, active := newWorkspace(t, s, false)
	pendingPush := func() []uuid.UUID {
		var ids []uuid.UUID
		rows, _ := s.Pool.Query(ctx, `SELECT id FROM runs WHERE workspace_id = $1 AND status = 'pending' ORDER BY created_at`, w.ID)
		for rows.Next() {
			var id uuid.UUID
			rows.Scan(&id)
			ids = append(ids, id)
		}
		return ids
	}
	for _, c := range []string{"c1", "c1", "c2"} {
		if _, err := s.QueueVCSRun(ctx, w.ID, c, 0, "push "+c); err != nil {
			t.Fatal(err)
		}
	}
	p := pendingPush()
	if len(p) != 1 {
		t.Fatalf("pending pushes: %v, want only the newest", p)
	}
	var commit string
	s.Pool.QueryRow(ctx, `SELECT vcs_commit FROM runs WHERE id = $1`, p[0]).Scan(&commit)
	if commit != "c2" {
		t.Fatalf("the pending push is %s, want c2", commit)
	}
	if _, err := s.CancelRun(ctx, active.ID, "x", false); err != nil {
		t.Fatal(err)
	}
	if st := status(t, s, p[0]); st != PlanQueued {
		t.Fatalf("the push after the active run ended: %s", st)
	}
	if ok, err := s.QueueVCSRun(ctx, w.ID, "c2", 0, "push c2 again"); err != nil || ok {
		t.Fatalf("a commit with a run gets another: %v %v", ok, err)
	}
}
