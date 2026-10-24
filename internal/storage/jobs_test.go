package storage

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"maps"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opsolete/opsolete/internal/runnerapi"
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
	if st, err := s.FinishJob(ctx, run.ID, plan.LeaseID, "succeeded", "", nil, sum, nil); err != nil || st != NeedsConfirmation {
		t.Fatalf("finish plan: %q %v", st, err)
	}
	if _, ok, _ := s.ClaimJob(ctx, "r", time.Minute); ok {
		t.Fatal("a plan waiting for confirmation must not be claimed")
	}
	if _, err := s.CreateRun(ctx, w.ID, "apply", "", nil); !errors.Is(err, ErrRunActive) {
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
	if _, err := s.Heartbeat(ctx, run.ID, plan.LeaseID, time.Minute); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("the plan phase's lease must be dead: %v", err)
	}
	res := []runnerapi.Resource{
		{Address: "azurerm_storage_account.this", Type: "azurerm_storage_account", Name: "this", Provider: "registry.terraform.io/hashicorp/azurerm"},
		{Address: "module.net.azurerm_subnet.a[0]", Type: "azurerm_subnet", Name: "a", Provider: "registry.terraform.io/hashicorp/azurerm", Module: "module.net"},
	}
	if err := s.AddStateVersion(ctx, w.ID, StateVersion{Serial: 3, Lineage: "lin", BlobKey: "k/" + w.ID, MD5: "m", Size: 1, Outputs: map[string]any{"id": "abc"}}); err != nil {
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
	if got, err := s.Resources(ctx, w.ID); err != nil || len(got) != 2 || got[1].Module != "module.net" || got[0].Workspace != w.Name || got[0].Serial != 3 {
		t.Fatalf("inventory: %+v %v", got, err)
	}

	// A plan without changes ends the run right away.
	again, err := s.CreateRun(ctx, w.ID, "apply", "", nil)
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
	d, err := s.CreateRun(ctx, w.ID, "destroy", "", nil)
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
	if _, err := s.CreateRun(ctx, w.ID, "apply", "", nil); err != nil {
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

// TestRunVariables: a workspace's own variables win over attached sets, attached sets over
// global ones, and between sets of one kind the first by name.
func TestRunVariables(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	name := "t-" + strings.ToLower(rand.Text()[:10])
	w, _, err := s.CreateWorkspace(ctx, name, "job-test", "git::x", "v1", nil, false, "")
	if err != nil {
		t.Fatal(err)
	}
	other, _, err := s.CreateWorkspace(ctx, name+"-other", "job-test", "git::x", "v1", nil, false, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		s.DB.Exec(`DELETE FROM workspaces WHERE id IN ($1, $2)`, w.ID, other.ID)
		s.DB.Exec(`DELETE FROM variable_sets WHERE name LIKE $1`, name+"%")
	})
	set := func(o VarOwner, cat, key, val string) {
		if err := s.SetVariable(ctx, o, Variable{Category: cat, Key: key, Value: []byte(val)}); err != nil {
			t.Fatal(err)
		}
	}
	sets := map[string]string{}
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
	if err := s.AttachVariableSet(ctx, sets["b-attached"], "no-such-workspace", true); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("attach to an unknown workspace: %v", err)
	}
	k := strings.ToUpper(strings.ReplaceAll(name, "-", "_"))
	set(VarOwner{SetID: sets["a-global"]}, "env", k+"_G", "global")
	set(VarOwner{SetID: sets["a-global"]}, "env", k+"_X", "global")
	set(VarOwner{SetID: sets["c-attached"]}, "env", k+"_X", "c")
	set(VarOwner{SetID: sets["b-attached"]}, "env", k+"_X", "b")
	set(VarOwner{SetID: sets["b-attached"]}, "env", k+"_W", "b")
	set(VarOwner{WorkspaceID: w.ID}, "env", k+"_W", "old")
	set(VarOwner{WorkspaceID: w.ID}, "env", k+"_W", "workspace") // replaces
	set(VarOwner{WorkspaceID: w.ID}, "terraform", k+"_W", "tf")  // other category, no clash

	got := func(id string) map[string]string {
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
	want := map[string]string{"env:" + k + "_G": "global", "env:" + k + "_X": "b", "env:" + k + "_W": "workspace", "terraform:" + k + "_W": "tf"}
	if m := got(w.ID); !maps.Equal(m, want) {
		t.Fatalf("got %v, want %v", m, want)
	}
	if m := got(other.ID); !maps.Equal(m, map[string]string{"env:" + k + "_G": "global", "env:" + k + "_X": "global"}) {
		t.Fatalf("a workspace without attachments gets only global sets: %v", m)
	}
	if err := s.AttachVariableSet(ctx, sets["b-attached"], w.Name, false); err != nil {
		t.Fatal(err)
	}
	if m := got(w.ID); m["env:"+k+"_X"] != "c" {
		t.Fatalf("after detaching b, c must win: %v", m)
	}
}

func TestAccess(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	creator, _ := s.UpsertUser(ctx, "creator@example.com", "user", nil)
	member, _ := s.UpsertUser(ctx, "member@example.com", "user", []string{"team-a", "team-b"})
	outsider, _ := s.UpsertUser(ctx, "outsider@example.com", "user", []string{"team-c"})
	name := "t-" + strings.ToLower(rand.Text()[:10])
	w, _, err := s.CreateWorkspace(ctx, name, "job-test", "git::x", "v1", nil, false, creator)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.DB.Exec(`DELETE FROM workspaces WHERE id=$1`, w.ID) })

	sees := func(user string, admin bool) bool {
		list, err := s.ListWorkspaces(ctx, Viewer{UserID: user, Admin: admin})
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
	level := func(user string) string {
		l, err := s.Access(ctx, w.ID, user)
		if err != nil {
			t.Fatal(err)
		}
		return l
	}
	if !sees(creator, false) || level(creator) != "operate" || sees(member, false) || level(member) != "" || !sees("", true) {
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

	other, _ := newWorkspace(t, s, false)
	if err := s.RenameWorkspace(ctx, w.ID, other.Name); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("rename onto a taken name: %v", err)
	}
	if err := s.RenameWorkspace(ctx, w.ID, name+"-new"); err != nil {
		t.Fatal(err)
	}
	if got, err := s.GetWorkspace(ctx, name+"-new"); err != nil || got.ID != w.ID {
		t.Fatalf("renamed: %+v %v", got, err)
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
	run2, err := s.CreateRun(ctx, w.ID, "apply", "", nil)
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
	if st, _ := s.FinishJob(ctx, run2.ID, j.LeaseID, "canceled", "", nil, nil, nil); st != Canceled {
		t.Fatalf("finish canceled: %q", st)
	}

	// Force: cut off right away; the runner's lease and state lock are gone.
	run3, _ := s.CreateRun(ctx, w.ID, "apply", "", nil)
	j = claim(t, s, "plan")
	s.LockState(ctx, w.ID, "l1", []byte(`{"ID":"l1"}`))
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
	if _, err := s.DB.Exec(`INSERT INTO run_status_history (run_id, status, created_at) VALUES
		($1, 'planning', now() + interval '1 hour'), ($1, 'errored', now() + interval '1 hour 90 seconds')`, run.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(`UPDATE runs SET status = 'errored' WHERE id = $1`, run.ID); err != nil {
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
	w, err := s.CreateEmptyWorkspace(ctx, name, false, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateVCSConnection(ctx, VCSConnection{Name: name, Provider: "github", Token: []byte("x")}); err != nil {
		t.Fatal(err)
	}
	var conn string
	s.DB.QueryRow(`SELECT id FROM vcs_connections WHERE name = $1`, name).Scan(&conn)
	t.Cleanup(func() {
		s.DB.Exec(`DELETE FROM workspaces WHERE id = $1`, w.ID)
		s.DB.Exec(`DELETE FROM variable_sets WHERE name LIKE $1`, name+"%")
		s.DB.Exec(`DELETE FROM vcs_connections WHERE id = $1`, conn)
	})
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
	if err := s.SetWorkspaceVCS(ctx, w.ID, conn, "org/repo", "main"); err != nil {
		t.Fatal(err)
	}
	check() // linking detached the set, and global sets don't reach a linked workspace
	attach()
	check("attached")
	if err := s.SetWorkspaceVCS(ctx, w.ID, conn, "org/repo", "main"); err != nil {
		t.Fatal(err)
	}
	check("attached") // same link: the admin's attachment stands
	if err := s.SetWorkspaceVCS(ctx, w.ID, conn, "org/repo", "other"); err != nil {
		t.Fatal(err)
	}
	check()
}
