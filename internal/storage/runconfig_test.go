package storage

import (
	"context"
	"errors"
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
	if _, err := s.FinishJob(ctx, run.ID, j.LeaseID, "succeeded", "", nil, nil, nil, nil); err != nil {
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
		if _, err := s.FinishJob(ctx, r.ID, j.LeaseID, "succeeded", "", nil, p, nil, nil); err != nil {
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
	if _, err := s.FinishJob(ctx, r.ID, j.LeaseID, "failed", "boom", nil, nil, nil, nil); err != nil {
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
	if _, err := s.FinishJob(ctx, r.ID, j.LeaseID, "failed", "boom", nil, nil, nil, nil); err != nil {
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

// A plan that imports (its module's import blocks) always waits for a human, also with auto-apply; it ends
// errored, the template removed again, when it replaces or deletes, or imports a resource a state manages; what
// it found is kept for the form.
func TestImportPlan(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	w, run := newWorkspace(t, s, true)
	if j := claim(t, s, "plan"); true {
		if _, err := s.FinishJob(ctx, run.ID, j.LeaseID, "succeeded", "", nil, nil, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	imports := []runnerapi.Import{{To: "module.vm.a.b", ID: "/x/1"}, {To: "module.vm.c.d", ID: "/x/2"}}
	found := map[string]map[string]any{"vm": {"size": "Standard_DS1_v2"}}
	add := func() Run {
		t.Helper()
		r, err := s.SetWorkspaceTemplate(ctx, w.ID, WorkspaceTemplate{Name: "vm", TemplateID: "job-test", ModuleSource: "git::x", ModuleRef: "v1",
			Inputs: map[string]any{"existing": true}}, true, uuid.Nil)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	finish := func(r Run, plan runnerapi.Plan, have []runnerapi.Resource) string {
		t.Helper()
		status, err := s.FinishJob(ctx, r.ID, claim(t, s, "plan").LeaseID, "succeeded", "", nil, &plan, have, found)
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
	s.Pool.Exec(ctx, `UPDATE runs SET status = 'applied' WHERE id = $1`, r.ID)
	if _, err := s.Pool.Exec(ctx, `DELETE FROM workspace_templates WHERE workspace_id = $1 AND name = 'vm'`, w.ID); err != nil {
		t.Fatal(err)
	}
	r = add()
	if status := finish(r, runnerapi.Plan{Import: 1, Imports: main}, []runnerapi.Resource{{Address: "aws_s3_bucket.old", ID: "/x/3"}}); status != Errored {
		t.Fatalf("a configuration's own import of a managed resource: %s", status)
	}
	r = add()
	if status := finish(r, runnerapi.Plan{Import: 2, Imports: imports, Change: 1}, nil); status != NeedsConfirmation { // the workspace auto-applies
		t.Fatalf("an import with auto-apply: %s", status)
	}
	if got, err := s.RunFound(ctx, r.ID); err != nil || got["vm"].TemplateID != "job-test" || got["vm"].Inputs["size"] != "Standard_DS1_v2" {
		t.Fatalf("found: %v %v", got, err)
	}
	if got, err := s.RunFound(ctx, run.ID); err != nil || got != nil {
		t.Fatalf("found of a plan without imports: %v %v", got, err)
	}
	if _, err := s.RunFound(ctx, uuid.New()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("found of no run: %v", err)
	}
}
