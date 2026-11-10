package storage

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/opsolete/opsolete/internal/runnerapi"
)

// A run that changes the configuration puts the previous one back if it ends before its apply
// started (discarded, canceled, errored while planning), keeps it once its apply started or when it
// had nothing to change, removes an added template again, and leaves a use changed since alone.
func TestRunConfigRestore(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	w, run := newWorkspace(t, s, false) // template main: job-test git::x v1, n=1
	j := claim(t, s, "plan")
	if _, err := s.FinishJob(ctx, run.ID, j.LeaseID, "succeeded", "", nil, nil, nil); err != nil {
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
		if _, err := s.FinishJob(ctx, r.ID, j.LeaseID, "succeeded", "", nil, p, nil); err != nil {
			t.Fatal(err)
		}
	}
	changes := func(r Run) bool {
		var ok bool
		s.Pool.QueryRow(context.Background(), `SELECT config_changes IS NOT NULL FROM runs WHERE id = $1`, r.ID).Scan(&ok)
		return ok
	}

	// discarded: back to v1, and the run keeps no copy
	r := configure("v2")
	plan(r, true)
	if err := s.DiscardRun(ctx, r.ID, "x"); err != nil {
		t.Fatal(err)
	}
	if got := root(); got != "v1" || changes(r) {
		t.Fatalf("after discard: %s, copy kept %v", got, changes(r))
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
	if _, err := s.FinishJob(ctx, r.ID, j.LeaseID, "failed", "boom", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := root(); got != "v1" {
		t.Fatalf("after a failed plan: %s", got)
	}
	// changed since the run's change: left alone
	r = configure("v2")
	plan(r, true)
	s.Pool.Exec(context.Background(), `UPDATE workspace_templates SET module_ref = 'v9' WHERE workspace_id = $1 AND name = 'main'`, w.ID)
	s.DiscardRun(ctx, r.ID, "x")
	if got := root(); got != "v9" {
		t.Fatalf("a later change was overwritten: %s", got)
	}
	s.Pool.Exec(context.Background(), `UPDATE workspace_templates SET module_ref = 'v1' WHERE workspace_id = $1 AND name = 'main'`, w.ID)
	// no changes to make: the new version stays
	r = configure("v2")
	plan(r, false)
	if got := root(); got != "v2" || changes(r) {
		t.Fatalf("after no changes: %s, copy kept %v", got, changes(r))
	}
	// the apply started, then failed: the new version stays
	r = configure("v3")
	plan(r, true)
	if err := s.ConfirmRun(ctx, r.ID, uuid.Nil, "x"); err != nil {
		t.Fatal(err)
	}
	j = claim(t, s, "apply")
	if _, err := s.FinishJob(ctx, r.ID, j.LeaseID, "failed", "boom", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := root(); got != "v3" {
		t.Fatalf("after a failed apply: %s", got)
	}

	// an added template whose first plan is discarded is removed again; a changed one goes back
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
		t.Fatal("a discarded added template must be removed again")
	}
	s.Pool.Exec(context.Background(), `INSERT INTO workspace_templates (workspace_id, name, template_id, module_source, module_ref, inputs)
		VALUES ($1, 'extra', 'job-test', 'git::x', 'v1', '{}')`, w.ID)
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

// An adoption (a template added with imports) hands its imports to the runner in both phases, always waits for a
// human, also with auto-apply, and ends errored, the template removed again, when its plan replaces or deletes.
func TestAdoption(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	w, run := newWorkspace(t, s, true)
	if j := claim(t, s, "plan"); j.Imports != nil {
		t.Fatalf("a run without imports: %+v", j.Imports)
	} else if _, err := s.FinishJob(ctx, run.ID, j.LeaseID, "succeeded", "", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	imports := []runnerapi.Import{{To: "module.vm.a.b", ID: "/x/1"}, {To: "module.vm.c.d", ID: "/x/2"}}
	adopt := func(plan runnerapi.Plan) (Run, string) {
		t.Helper()
		r, err := s.SetWorkspaceTemplate(ctx, w.ID, WorkspaceTemplate{Name: "vm", TemplateID: "job-test", ModuleSource: "git::x", ModuleRef: "v1",
			Inputs: map[string]any{}, Imports: imports}, true, uuid.Nil)
		if err != nil {
			t.Fatal(err)
		}
		j := claim(t, s, "plan")
		if len(j.Imports) != 2 || j.Imports[1] != imports[1] {
			t.Fatalf("plan phase imports: %+v", j.Imports)
		}
		status, err := s.FinishJob(ctx, r.ID, j.LeaseID, "succeeded", "", nil, &plan, nil)
		if err != nil {
			t.Fatal(err)
		}
		return r, status
	}
	added := func() bool {
		var n int
		s.Pool.QueryRow(ctx, `SELECT count(*) FROM workspace_templates WHERE workspace_id = $1 AND name = 'vm'`, w.ID).Scan(&n)
		return n == 1
	}

	if _, status := adopt(runnerapi.Plan{Import: 2, Add: 1, Destroy: 1}); status != Errored || added() {
		t.Fatalf("a replacing adoption: %s, template kept %v", status, added())
	}
	// an import of a resource the state has already (as its plan phase read it): refused, whatever the plan
	r0, err := s.SetWorkspaceTemplate(ctx, w.ID, WorkspaceTemplate{Name: "vm", TemplateID: "job-test", ModuleSource: "git::x", ModuleRef: "v1",
		Inputs: map[string]any{}, Imports: imports}, true, uuid.Nil)
	if err != nil {
		t.Fatal(err)
	}
	j0 := claim(t, s, "plan")
	have := []runnerapi.Resource{{Address: "module.old.a.b", Type: "a", Name: "b", ID: "/X/1"}} // imports[0] in other case
	if status, err := s.FinishJob(ctx, r0.ID, j0.LeaseID, "succeeded", "", nil, &runnerapi.Plan{Import: 2}, have); err != nil || status != Errored || added() {
		t.Fatalf("adopting a managed resource: %s %v, template kept %v", status, err, added())
	}
	var msg string
	s.Pool.QueryRow(ctx, `SELECT message FROM run_status_history WHERE run_id = $1 ORDER BY created_at DESC LIMIT 1`, r0.ID).Scan(&msg)
	if !strings.Contains(msg, "managed already, by module.old.a.b") {
		t.Fatalf("message: %q", msg)
	}
	r, status := adopt(runnerapi.Plan{Import: 2, Change: 1})
	if status != NeedsConfirmation { // not apply_queued: the workspace auto-applies
		t.Fatalf("an adoption with auto-apply: %s", status)
	}
	if err := s.ConfirmRun(ctx, r.ID, uuid.Nil, "x"); err != nil {
		t.Fatal(err)
	}
	if j := claim(t, s, "apply"); len(j.Imports) != 2 {
		t.Fatalf("apply phase imports: %+v", j.Imports)
	}

	// a workspace created for an adoption
	tpl := testTemplate(nil)
	tpl.Imports = imports[:1]
	if _, _, err := s.CreateWorkspace(ctx, uuid.Nil, w.Name+"-new", "", tpl, false, uuid.Nil); err != nil {
		t.Fatal(err)
	}
	if j := claim(t, s, "plan"); len(j.Imports) != 1 || j.Imports[0] != imports[0] {
		t.Fatalf("new workspace's imports: %+v", j.Imports)
	}
}
