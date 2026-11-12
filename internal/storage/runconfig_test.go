package storage

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/opsolete/opsolete/internal/runnerapi"
)

// A run that changes the configuration plans a new version: ended before its apply started (discarded, canceled,
// errored while planning) the workspace keeps its current one, a template it added isn't there and a change made
// meanwhile stays; once its apply started, or with nothing to change, the workspace takes the run's version.
func TestRunConfigVersions(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	w, run := newWorkspace(t, s, false) // template main: job-test git::x v1, n=1
	j := claim(t, s, "plan")
	if _, err := s.FinishJob(ctx, run.ID, j.LeaseID, "succeeded", "", nil, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	root := func() string {
		t.Helper()
		var ref string
		if err := s.Pool.QueryRow(context.Background(), `SELECT module_ref FROM workspace_templates WHERE workspace_id = $1 AND name = 'main'`, w.ID).Scan(&ref); err != nil {
			t.Fatal(err)
		}
		return ref
	}
	configure := func(ref string) Run {
		t.Helper()
		r, err := s.SetWorkspaceTemplate(ctx, w.ID, WorkspaceTemplate{Name: "main", TemplateID: "job-test", ModuleSource: "git::x", ModuleRef: ref,
			Inputs: map[string]any{"n": 2.0}}, false, uuid.Nil)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	plan := func(r Run, change bool) {
		t.Helper()
		var p *runnerapi.Plan
		if change {
			p = &runnerapi.Plan{Change: 1}
		}
		j := claim(t, s, "plan")
		if _, err := s.FinishJob(ctx, r.ID, j.LeaseID, "succeeded", "", nil, p, nil, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	// while it runs, the workspace shows the version it plans
	r := configure("v2")
	if got := root(); got != "v2" {
		t.Fatalf("while planning: %s", got)
	}
	// discarded: v1
	plan(r, true)
	if err := s.DiscardRun(ctx, r.ID, "x"); err != nil {
		t.Fatal(err)
	}
	if got := root(); got != "v1" {
		t.Fatalf("after discard: %s", got)
	}
	// canceled while queued
	r = configure("v2")
	if _, err := s.CancelRun(ctx, r.ID, "x", false); err != nil {
		t.Fatal(err)
	}
	if got := root(); got != "v1" {
		t.Fatalf("after cancel: %s", got)
	}
	// errored while planning
	r = configure("v2")
	j = claim(t, s, "plan")
	if _, err := s.FinishJob(ctx, r.ID, j.LeaseID, "failed", "boom", nil, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := root(); got != "v1" {
		t.Fatalf("after a failed plan: %s", got)
	}
	// changed since the run's change: left alone
	r = configure("v2")
	plan(r, true)
	putTemplates(t, s, w.ID, WorkspaceTemplate{Name: "main", TemplateID: "job-test", ModuleSource: "git::x", ModuleRef: "v9", Inputs: map[string]any{"n": 1.0}})
	s.DiscardRun(ctx, r.ID, "x")
	if got := root(); got != "v9" {
		t.Fatalf("a later change was overwritten: %s", got)
	}
	putTemplates(t, s, w.ID, WorkspaceTemplate{Name: "main", TemplateID: "job-test", ModuleSource: "git::x", ModuleRef: "v1", Inputs: map[string]any{"n": 1.0}})
	// no changes to make: the workspace takes the new version
	r = configure("v2")
	plan(r, false)
	if got := root(); got != "v2" {
		t.Fatalf("after no changes: %s", got)
	}
	// the apply started, then failed: the new version stays
	r = configure("v3")
	plan(r, true)
	if err := s.ConfirmRun(ctx, r.ID, uuid.Nil, "x"); err != nil {
		t.Fatal(err)
	}
	j = claim(t, s, "apply")
	if _, err := s.FinishJob(ctx, r.ID, j.LeaseID, "failed", "boom", nil, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := root(); got != "v3" {
		t.Fatalf("after a failed apply: %s", got)
	}

	// an added template whose first plan is discarded isn't there; a changed one is as it was
	add := WorkspaceTemplate{Name: "extra", TemplateID: "job-test", ModuleSource: "git::x", ModuleRef: "v1", Inputs: map[string]any{}}
	r, err := s.SetWorkspaceTemplate(ctx, w.ID, add, true, uuid.Nil)
	if err != nil {
		t.Fatal(err)
	}
	plan(r, true)
	s.DiscardRun(ctx, r.ID, "x")
	var n int
	s.Pool.QueryRow(context.Background(), `SELECT count(*) FROM workspace_templates WHERE workspace_id = $1 AND name = 'extra'`, w.ID).Scan(&n)
	if n != 0 {
		t.Fatal("a discarded added template is there")
	}
	putTemplates(t, s, w.ID, add)
	add.ModuleRef = "v2"
	if r, err = s.SetWorkspaceTemplate(ctx, w.ID, add, false, uuid.Nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CancelRun(ctx, r.ID, "x", false); err != nil {
		t.Fatal(err)
	}
	var ref string
	s.Pool.QueryRow(context.Background(), `SELECT module_ref FROM workspace_templates WHERE workspace_id = $1 AND name = 'extra'`, w.ID).Scan(&ref)
	if ref != "v1" {
		t.Fatalf("a changed added template after cancel: %s", ref)
	}
}

// A plan that imports (its module's import blocks) always waits for a human, also with auto-apply; it ends
// errored, the template removed again, when it replaces or deletes, or imports a resource a state manages; what
// it found is kept for the form.
func TestImportPlan(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	w, run := newWorkspace(t, s, true)
	if j := claim(t, s, "plan"); true {
		if _, err := s.FinishJob(ctx, run.ID, j.LeaseID, "succeeded", "", nil, nil, nil, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	imports := []runnerapi.Import{{To: "module.vm.a.b", ID: "/x/1"}, {To: "module.vm.c.d", ID: "/x/2"}}
	found := map[string]map[string]any{"vm": {"size": "Standard_DS1_v2"}}
	add := func() Run {
		t.Helper()
		r, err := s.SetWorkspaceTemplate(ctx, w.ID, WorkspaceTemplate{Name: "vm", TemplateID: "job-test", ModuleSource: "git::x", ModuleRef: "v1",
			Inputs: map[string]any{}, Import: true}, true, uuid.Nil)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	finish := func(r Run, plan runnerapi.Plan, have []runnerapi.Resource) string {
		t.Helper()
		status, err := s.FinishJob(ctx, r.ID, claim(t, s, "plan").LeaseID, "succeeded", "", nil, &plan, have, found, nil)
		if err != nil {
			t.Fatal(err)
		}
		return status
	}
	added := func() bool {
		var n int
		s.Pool.QueryRow(ctx, `SELECT count(*) FROM workspace_templates WHERE workspace_id = $1 AND name = 'vm'`, w.ID).Scan(&n)
		return n == 1
	}
	message := func(r Run) string {
		var msg string
		s.Pool.QueryRow(ctx, `SELECT message FROM run_status_history WHERE run_id = $1 ORDER BY created_at DESC LIMIT 1`, r.ID).Scan(&msg)
		return msg
	}

	r := add()
	if status := finish(r, runnerapi.Plan{Import: 2, Imports: imports, Add: 1, Destroy: 1}, nil); status != Errored || added() {
		t.Fatalf("a replacing import: %s, template kept %v", status, added())
	}
	// a resource the state has already (as its plan read it), in other case: refused, whatever the plan
	r = add()
	have := []runnerapi.Resource{{Address: "module.old.a.b", Type: "a", Name: "b", ID: "/X/1"}}
	if status := finish(r, runnerapi.Plan{Import: 2, Imports: imports}, have); status != Errored || added() ||
		!strings.Contains(message(r), "managed already, by module.old.a.b") {
		t.Fatalf("importing a managed resource: %s, template kept %v, %q", status, added(), message(r))
	}
	// imports outside the templates (a VCS configuration's own): no template rules, the one-state rule still
	main := []runnerapi.Import{{To: "aws_s3_bucket.b", ID: "/x/3"}}
	r = add()
	if status := finish(r, runnerapi.Plan{Import: 1, Imports: main, Add: 1, Destroy: 1}, nil); status != ApplyQueued {
		t.Fatalf("a configuration's own import with auto-apply: %s", status)
	}
	s.Pool.Exec(ctx, `WITH done AS (UPDATE runs SET status = 'applied' WHERE id = $1 RETURNING runs.workspace_id, runs.cv_id) UPDATE workspaces cur SET current_cv_id = COALESCE(done.cv_id, cur.current_cv_id) FROM done WHERE cur.id = done.workspace_id`, r.ID)
	if _, err := s.RemoveWorkspaceTemplate(ctx, w.ID, "vm", uuid.Nil); err != nil { // nothing deployed: at once
		t.Fatal(err)
	}
	r = add()
	if status := finish(r, runnerapi.Plan{Import: 1, Imports: main}, []runnerapi.Resource{{Address: "aws_s3_bucket.old", ID: "/x/3"}}); status != Errored {
		t.Fatalf("a configuration's own import of a managed resource: %s", status)
	}
	// another template has the switch on too, but imports nothing
	putTemplates(t, s, w.ID, WorkspaceTemplate{Name: "main", TemplateID: "job-test", ModuleSource: "git::x", ModuleRef: "v1", Inputs: map[string]any{}, Import: true})
	r = add()
	if status := finish(r, runnerapi.Plan{Import: 2, Imports: imports, Change: 1}, nil); status != NeedsConfirmation { // the workspace auto-applies
		t.Fatalf("an import with auto-apply: %s", status)
	}
	if got, err := s.RunFound(ctx, r.ID); err != nil || got["vm"].TemplateID != "job-test" || got["vm"].Inputs["size"] != "Standard_DS1_v2" {
		t.Fatalf("found: %v %v", got, err)
	}
	// applied: the template stops importing; the other one keeps its switch
	if err := s.ConfirmRun(ctx, r.ID, uuid.Nil, "x"); err != nil {
		t.Fatal(err)
	}
	if status, err := s.FinishJob(ctx, r.ID, claim(t, s, "apply").LeaseID, "succeeded", "", nil, nil, []runnerapi.Resource{}, nil, nil); err != nil || status != Applied {
		t.Fatalf("apply: %s %v", status, err)
	}
	existing := func(name string) any {
		var imp bool
		s.Pool.QueryRow(ctx, `SELECT importing FROM workspace_templates WHERE workspace_id = $1 AND name = $2`, w.ID, name).Scan(&imp)
		return imp
	}
	if existing("vm") != false || existing("main") != true {
		t.Fatalf("existing after the apply: vm %v, main %v", existing("vm"), existing("main"))
	}
	if got, err := s.RunFound(ctx, run.ID); err != nil || got != nil {
		t.Fatalf("found of a plan without imports: %v %v", got, err)
	}
	if _, err := s.RunFound(ctx, uuid.New()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("found of no run: %v", err)
	}
}

// A run that ends without its apply offers what it set its templates to (its version, sealed inputs left out),
// beside what its plan found; the workspace doesn't have a template it added.
func TestRunFoundEntered(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	w, run := newWorkspace(t, s, false)
	if _, err := s.FinishJob(ctx, run.ID, claim(t, s, "plan").LeaseID, "succeeded", "", nil, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	r, err := s.SetWorkspaceTemplate(ctx, w.ID, WorkspaceTemplate{Name: "vm", TemplateID: "job-test", ModuleSource: "git::x", ModuleRef: "v1",
		Inputs: map[string]any{"name": "vm1", "pw": map[string]any{"sealed": "opsolete:1:k:x"}}}, true, uuid.Nil)
	if err != nil {
		t.Fatal(err)
	}
	if status, err := s.FinishJob(ctx, r.ID, claim(t, s, "plan").LeaseID, "failed", "plan: exit status 1", nil, nil, nil, nil, nil); err != nil || status != Errored {
		t.Fatalf("failed plan: %s %v", status, err)
	}
	got, err := s.RunFound(ctx, r.ID)
	if want := (Found{TemplateID: "job-test", Saved: map[string]any{"name": "vm1"}}); err != nil || len(got) != 1 || !reflect.DeepEqual(got["vm"], want) {
		t.Fatalf("kept: %+v %v", got, err)
	}
	var n int
	s.Pool.QueryRow(ctx, `SELECT count(*) FROM workspace_templates WHERE workspace_id = $1 AND name = 'vm'`, w.ID).Scan(&n)
	if n != 0 {
		t.Fatal("the added template stayed")
	}
}

// A speculative plan (an import's round) of a new version that has nothing to change leaves the workspace's version
// alone: it's never applied.
func TestPlanRoundKeepsVersion(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	w, run := newWorkspace(t, s, false)
	if _, err := s.FinishJob(ctx, run.ID, claim(t, s, "plan").LeaseID, "succeeded", "", nil, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	r, err := s.SetWorkspaceTemplateAs(ctx, w.ID, WorkspaceTemplate{Name: "vm", TemplateID: "job-test", ModuleSource: "git::x", ModuleRef: "v1",
		Inputs: map[string]any{}, Import: true}, true, "plan", uuid.Nil)
	if err != nil {
		t.Fatal(err)
	}
	if st, err := s.FinishJob(ctx, r.ID, claim(t, s, "plan").LeaseID, "succeeded", "", nil, nil, nil, nil, nil); err != nil || st != NoChanges {
		t.Fatalf("round: %s %v", st, err)
	}
	if mods, _ := s.WorkspaceTemplates(ctx, w.ID); len(mods) != 1 || mods[0].Name != "main" {
		t.Fatalf("the workspace took a plan's version: %+v", mods)
	}
}

// ApplyImportConflict: a resource another workspace started managing after a plan that imports it ran is caught
// again when the apply is claimed.
func TestApplyImportConflict(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	_, run := newWorkspace(t, s, false)
	plan := runnerapi.Plan{Import: 1, Imports: []runnerapi.Import{{To: "module.main.a.b", ID: "/x/9"}}}
	if _, err := s.FinishJob(ctx, run.ID, claim(t, s, "plan").LeaseID, "succeeded", "", nil, &plan, []runnerapi.Resource{}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if msg, err := s.ApplyImportConflict(ctx, run.ID); err != nil || msg != "" {
		t.Fatalf("before: %q %v", msg, err)
	}
	other, first := newWorkspace(t, s, false)
	if _, err := s.CancelRun(ctx, first.ID, "x", false); err != nil {
		t.Fatal(err)
	}
	addState(t, s, other.ID, 1)
	if err := s.IndexState(ctx, other.ID, []runnerapi.Resource{{Address: "a.b", ID: "/X/9"}}); err != nil {
		t.Fatal(err)
	}
	if msg, err := s.ApplyImportConflict(ctx, run.ID); err != nil || !strings.Contains(msg, "managed already, in workspace "+other.Name) {
		t.Fatalf("after: %q %v", msg, err)
	}
}
