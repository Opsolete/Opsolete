package storage

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/opsolete/opsolete/internal/runnerapi"
)

// A promotion is one run that waits for confirmation even with auto-apply; ended before its apply
// the workspace keeps its templates (removed ones too, and a change made meanwhile); applied, the
// promoted ones are its templates.
func TestPromote(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	w, first := newWorkspace(t, s, true) // auto-apply; template main: job-test git::x v1, n=1
	if _, err := s.CancelRun(ctx, first.ID, "x", false); err != nil {
		t.Fatal(err)
	}
	extra := WorkspaceTemplate{Name: "extra", TemplateID: "other", ModuleSource: "git::y", ModuleRef: "v1", Inputs: map[string]any{"a": "b"}}
	putTemplates(t, s, w.ID, testTemplate(map[string]any{"n": 1.0}), extra) // the first run ended before its apply: none yet
	before, err := s.WorkspaceTemplates(ctx, w.ID)
	if err != nil || len(before) != 2 {
		t.Fatal(before, err)
	}
	promoted := []WorkspaceTemplate{
		{Name: "added", TemplateID: "job-test", ModuleSource: "git::x", ModuleRef: "v1", Inputs: map[string]any{}},
		{Name: "main", TemplateID: "job-test", ModuleSource: "git::x", ModuleRef: "v2", Inputs: map[string]any{"n": 2.0}},
	}
	promote := func(t *testing.T) Run {
		t.Helper()
		r, err := s.Promote(ctx, Promotion{WorkspaceID: w.ID, Read: before, Templates: promoted})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	names := func(t *testing.T) map[string]string { // module -> ref
		t.Helper()
		list, err := s.WorkspaceTemplates(ctx, w.ID)
		if err != nil {
			t.Fatal(err)
		}
		m := map[string]string{}
		for _, x := range list {
			m[x.Name] = x.ModuleRef
		}
		return m
	}
	want := func(t *testing.T, what string, m map[string]string) {
		t.Helper()
		got := names(t)
		if len(got) != len(m) {
			t.Fatalf("%s: %v, want %v", what, got, m)
		}
		for k, v := range m {
			if got[k] != v {
				t.Fatalf("%s: %v, want %v", what, got, m)
			}
		}
	}
	plan := func(t *testing.T, r Run, result string) {
		t.Helper()
		j := claim(t, s, "plan")
		if _, err := s.FinishJob(ctx, r.ID, j.LeaseID, result, "", nil, &runnerapi.Plan{Add: 1, Destroy: 1}, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	previous := map[string]string{"main": "v1", "extra": "v1"}
	after := map[string]string{"main": "v2", "added": "v1"}

	step(t, "discarded: everything back, the removed template too", func(t *testing.T) {
		r := promote(t)
		want(t, "promoted", after)
		plan(t, r, "succeeded")
		if got := status(t, s, r.ID); got != NeedsConfirmation {
			t.Fatalf("auto-apply workspace: %s, want %s", got, NeedsConfirmation)
		}
		if err := s.DiscardRun(ctx, r.ID, "x"); err != nil {
			t.Fatal(err)
		}
		want(t, "discarded", previous)
		got, _ := s.WorkspaceTemplates(ctx, w.ID)
		if !sameTemplates(got, before) {
			t.Fatalf("restored %+v, want %+v", got, before)
		}
	})
	step(t, "failed plan: back", func(t *testing.T) {
		plan(t, promote(t), "failed")
		want(t, "failed plan", previous)
	})
	step(t, "canceled, the removed module's name taken meanwhile: left alone", func(t *testing.T) {
		r := promote(t)
		putTemplates(t, s, w.ID, WorkspaceTemplate{Name: "extra", TemplateID: "other", ModuleSource: "git::y", ModuleRef: "v7", Inputs: map[string]any{}})
		if _, err := s.CancelRun(ctx, r.ID, "x", false); err != nil {
			t.Fatal(err)
		}
		want(t, "canceled", map[string]string{"main": "v1", "extra": "v7"})
		putTemplates(t, s, w.ID, extra)
	})
	step(t, "changed since read, or a run active: refused, nothing changes", func(t *testing.T) {
		stale := append([]WorkspaceTemplate{}, before...)
		stale[0].ModuleRef = "v0"
		if _, err := s.Promote(ctx, Promotion{WorkspaceID: w.ID, Read: stale, Templates: promoted}); !errors.Is(err, ErrChanged) {
			t.Fatalf("stale read: %v", err)
		}
		r, err := s.CreateRun(ctx, w.ID, "apply", uuid.Nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Promote(ctx, Promotion{WorkspaceID: w.ID, Read: before, Templates: promoted}); !errors.Is(err, ErrRunActive) {
			t.Fatalf("active run: %v", err)
		}
		s.CancelRun(ctx, r.ID, "x", false)
		want(t, "refused", previous)
	})
	step(t, "applied: the promoted templates stay", func(t *testing.T) {
		r := promote(t)
		plan(t, r, "succeeded")
		if err := s.ConfirmRun(ctx, r.ID, uuid.Nil, "x"); err != nil {
			t.Fatal(err)
		}
		j := claim(t, s, "apply")
		if _, err := s.FinishJob(ctx, r.ID, j.LeaseID, "succeeded", "", nil, nil, nil, nil); err != nil {
			t.Fatal(err)
		}
		want(t, "applied", after)
	})
	step(t, "a new workspace: created with the templates, removed again on discard", func(t *testing.T) {
		id := uuid.New()
		r, err := s.Promote(ctx, Promotion{WorkspaceID: id, New: &EmptyWorkspace{Name: w.Name + "-new"}, Templates: promoted})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Promote(ctx, Promotion{WorkspaceID: uuid.New(), New: &EmptyWorkspace{Name: w.Name + "-new"}, Templates: promoted}); !errors.Is(err, ErrNameTaken) {
			t.Fatalf("taken name: %v", err)
		}
		if _, err := s.CancelRun(ctx, r.ID, "x", false); err != nil {
			t.Fatal(err)
		}
		if list, _ := s.WorkspaceTemplates(ctx, id); len(list) != 0 {
			t.Fatalf("after cancel: %+v", list)
		}
	})
}

// ModuleIndex counts the current state's resources per top-level module; a state version that
// isn't indexed counts every module as deployed.
func TestModuleIndex(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	w, _ := newWorkspace(t, s, false)
	if m, err := s.ModuleIndex(ctx, w.ID); err != nil || len(m.Resources) != 0 || !m.Indexed || m.Deployed("a") {
		t.Fatalf("no state: %+v %v", m, err)
	}
	addState(t, s, w.ID, 1)
	if m, _ := s.ModuleIndex(ctx, w.ID); m.Indexed || !m.Deployed("a") {
		t.Fatalf("not indexed: %+v", m)
	}
	if err := s.IndexState(ctx, w.ID, []runnerapi.Resource{
		{Address: "module.a.x.y", Module: "module.a"}, {Address: "module.a.module.b.x.y", Module: "module.a.module.b"},
		{Address: "module.c.x.y", Module: "module.c"}, {Address: "x.y"}}); err != nil {
		t.Fatal(err)
	}
	if m, err := s.ModuleIndex(ctx, w.ID); err != nil || len(m.Resources) != 2 || m.Resources["a"] != 2 || m.Resources["c"] != 1 ||
		!m.Deployed("a") || m.Deployed("b") {
		t.Fatalf("counts: %+v %v", m, err)
	}
}

// addState records state version serial of workspace ws, not indexed.
func addState(t *testing.T, s *Store, ws uuid.UUID, serial int) {
	t.Helper()
	if _, err := s.Pool.Exec(context.Background(), `INSERT INTO state_versions (workspace_id, serial, lineage, blob_key, md5, size)
		VALUES ($1, $2, 'l', $3, 'm', 1)`, ws, serial, fmt.Sprint("k", serial)); err != nil {
		t.Fatal(err)
	}
}

// A promotion that removes or replaces a deployed template runs twice: a destroy of those templates
// carrying the rest, then (once applied) the apply run; a destroy that ends otherwise drops it.
func TestPromoteChained(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	w, first := newWorkspace(t, s, true) // main: job-test v1
	if _, err := s.CancelRun(ctx, first.ID, "x", false); err != nil {
		t.Fatal(err)
	}
	putTemplates(t, s, w.ID, testTemplate(map[string]any{"n": 1.0}), // the first run ended before its apply: none yet
		WorkspaceTemplate{Name: "gone", TemplateID: "other", ModuleSource: "git::y", ModuleRef: "v1", Inputs: map[string]any{}},
		WorkspaceTemplate{Name: "swap", TemplateID: "other", ModuleSource: "git::y", ModuleRef: "v1", Inputs: map[string]any{}})
	addState(t, s, w.ID, 1)
	if err := s.IndexState(ctx, w.ID, []runnerapi.Resource{{Address: "module.gone.a.b", Module: "module.gone"}}); err != nil {
		t.Fatal(err)
	}
	read, _ := s.WorkspaceTemplates(ctx, w.ID) // gone, main, swap
	want := []WorkspaceTemplate{
		{Name: "main", TemplateID: "job-test", ModuleSource: "git::x", ModuleRef: "v2", Inputs: map[string]any{"n": 2.0}},
		{Name: "swap", TemplateID: "job-test", ModuleSource: "git::x", ModuleRef: "v1", Inputs: map[string]any{}}, // replaces other
	}
	refs := func(t *testing.T) string { // name:template:ref, by name
		t.Helper()
		list, _ := s.WorkspaceTemplates(ctx, w.ID)
		var out []string
		for _, x := range list {
			out = append(out, x.Name+":"+x.TemplateID+":"+x.ModuleRef)
		}
		return strings.Join(out, " ")
	}
	const before, postA, promoted = "gone:other:v1 main:job-test:v1 swap:other:v1", "main:job-test:v1", "main:job-test:v2 swap:job-test:v1"
	promote := func(t *testing.T) Run {
		t.Helper()
		r, err := s.Promote(ctx, Promotion{WorkspaceID: w.ID, Read: read, Templates: want, Source: "dev", Actor: "a@example.com"})
		if err != nil {
			t.Fatal(err)
		}
		if r.Action != "destroy" || refs(t) != before {
			t.Fatalf("first run %s, templates %s", r.Action, refs(t))
		}
		return r
	}
	finish := func(t *testing.T, r Run, phase, result string, plan *runnerapi.Plan) string {
		t.Helper()
		j := claim(t, s, phase)
		if phase == "plan" && r.Action == "destroy" && strings.Join(j.Target, ",") != "module.gone,module.swap" {
			t.Fatalf("targets: %v", j.Target)
		}
		st, err := s.FinishJob(ctx, r.ID, j.LeaseID, result, "", nil, plan, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
	active := func(t *testing.T) *Run {
		t.Helper()
		var id uuid.UUID
		if err := s.Pool.QueryRow(ctx, `SELECT id FROM runs WHERE workspace_id = $1 AND run_active(status)`, w.ID).Scan(&id); err != nil {
			return nil
		}
		r, _, err := s.GetRun(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return &r
	}
	destroyPlan := &runnerapi.Plan{Destroy: 1}

	for _, end := range []string{Discarded, Errored, Canceled} {
		step(t, "destroy "+end+": no apply run, nothing changes", func(t *testing.T) {
			r := promote(t)
			switch end {
			case Discarded:
				if st := finish(t, r, "plan", "succeeded", destroyPlan); st != NeedsConfirmation {
					t.Fatalf("destroy plan: %s", st)
				}
				if err := s.DiscardRun(ctx, r.ID, "x"); err != nil {
					t.Fatal(err)
				}
			case Errored:
				finish(t, r, "plan", "failed", nil)
			case Canceled:
				if _, err := s.CancelRun(ctx, r.ID, "x", false); err != nil {
					t.Fatal(err)
				}
			}
			if a := active(t); a != nil || refs(t) != before {
				t.Fatalf("after %s: active %v, templates %s", end, a, refs(t))
			}
			var note int
			s.Pool.QueryRow(ctx, `SELECT count(*) FROM run_status_history WHERE run_id = $1 AND message = $2`, r.ID, "promotion not continued: "+end).Scan(&note)
			if note != 1 {
				t.Fatalf("no history note")
			}
		})
	}
	var b *Run
	step(t, "destroy applied: removal rows gone, the apply run queued, waiting even with auto-apply", func(t *testing.T) {
		r := promote(t)
		finish(t, r, "plan", "succeeded", destroyPlan)
		if err := s.ConfirmRun(ctx, r.ID, uuid.Nil, "x"); err != nil {
			t.Fatal(err)
		}
		finish(t, r, "apply", "succeeded", nil)
		if b = active(t); b == nil || b.Action != "apply" || refs(t) != promoted {
			t.Fatalf("after the destroy: run %+v, templates %s", b, refs(t))
		}
		var always bool
		var pending bool
		var audit int
		s.Pool.QueryRow(ctx, `SELECT always_confirm FROM runs WHERE id = $1`, b.ID).Scan(&always)
		s.Pool.QueryRow(ctx, `SELECT promotion IS NOT NULL FROM runs WHERE id = $1`, r.ID).Scan(&pending)
		s.Pool.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE action = 'promote apply queued' AND actor = 'a@example.com' AND detail->>'run' = $1`,
			b.ID.String()).Scan(&audit)
		if !always || pending || audit != 1 {
			t.Fatalf("always_confirm %v, promotion kept %v, audit %d", always, pending, audit)
		}
		if st := finish(t, *b, "plan", "succeeded", &runnerapi.Plan{Add: 1}); st != NeedsConfirmation {
			t.Fatalf("apply run on an auto-apply workspace: %s", st)
		}
	})
	step(t, "the apply run discarded: back to the templates after the destroy", func(t *testing.T) {
		if err := s.DiscardRun(ctx, b.ID, "x"); err != nil {
			t.Fatal(err)
		}
		if refs(t) != postA {
			t.Fatalf("after discard: %s", refs(t))
		}
	})
	step(t, "no changes to destroy: continues as applied", func(t *testing.T) {
		putTemplates(t, s, w.ID,
			WorkspaceTemplate{Name: "gone", TemplateID: "other", ModuleSource: "git::y", ModuleRef: "v1", Inputs: map[string]any{}},
			WorkspaceTemplate{Name: "swap", TemplateID: "other", ModuleSource: "git::y", ModuleRef: "v1", Inputs: map[string]any{}})
		r := promote(t)
		if st := finish(t, r, "plan", "succeeded", nil); st != NoChanges {
			t.Fatalf("destroy plan: %s", st)
		}
		if a := active(t); a == nil || refs(t) != promoted {
			t.Fatalf("after no changes: %v %s", a, refs(t))
		}
	})
}
