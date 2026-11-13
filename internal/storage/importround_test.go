package storage

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/opsolete/opsolete/internal/runnerapi"
)

// An import round is something to go on from only while it's pending: not applied, and nothing else applied
// since. Adopting or planning again from it otherwise would undo that change (a removed template back, another
// template's settings reverted), so storage refuses it inside the transaction too.
func TestImportRoundPending(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	w, first := newWorkspace(t, s, false)
	if _, err := s.FinishJob(ctx, first.ID, claim(t, s, "plan").LeaseID, "succeeded", "", nil, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	// "main" is applied (no changes); a second template "extra" added and applied without resources
	putTemplates(t, s, w.ID, testTemplate(map[string]any{"n": 1.0}), WorkspaceTemplate{Name: "extra", TemplateID: "job-test",
		ModuleSource: "git::x", ModuleRef: "v1", Inputs: map[string]any{}})
	round, err := s.SetWorkspaceTemplateAs(ctx, w.ID, WorkspaceTemplate{Name: "vm", TemplateID: "job-test", ModuleSource: "git::x",
		ModuleRef: "v1", Inputs: map[string]any{}, Import: true}, true, "plan", uuid.Nil)
	if err != nil {
		t.Fatal(err)
	}
	plan := runnerapi.Plan{Import: 1, Imports: []runnerapi.Import{{To: "module.vm.a.b", ID: "/vm/1"}}}
	if st, err := s.FinishJob(ctx, round.ID, claim(t, s, "plan").LeaseID, "succeeded", "", nil, &plan, []runnerapi.Resource{}, nil, nil); err != nil || st != Planned {
		t.Fatalf("round: %s %v", st, err)
	}
	if ok, err := s.RunPending(ctx, round.ID); err != nil || !ok {
		t.Fatalf("a fresh round isn't pending: %v", err)
	}
	s.Pool.Exec(ctx, `UPDATE runs SET updated_at = now() - interval '60 days' WHERE id = $1`, round.ID)
	if n, err := s.PruneSpeculativeRuns(ctx, 30*24*time.Hour); err != nil || n != 0 { // the import session lives on it
		t.Fatalf("retention pruned a pending round: %d %v", n, err)
	}
	// another change applied since: the round would bring "extra" back
	if _, err := s.RemoveWorkspaceTemplate(ctx, w.ID, "extra", uuid.Nil); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.RunPending(ctx, round.ID); ok {
		t.Fatal("a round from before another change is still pending")
	}
	if _, err := s.AdoptRun(ctx, round.ID, uuid.Nil); !errors.Is(err, ErrChanged) {
		t.Fatalf("adopting a stale round: %v", err)
	}
	if _, err := s.QueueConfiguration(ctx, w.ID, round.ID, nil, SourceForm, "plan", uuid.Nil); !errors.Is(err, ErrChanged) {
		t.Fatalf("planning a stale round again: %v", err)
	}
	if n, err := s.PruneSpeculativeRuns(ctx, 30*24*time.Hour); err != nil || n != 1 {
		t.Fatalf("pruned %d stale rounds: %v", n, err)
	}
}

// The import guard: an import Terraform names no id for can't be checked, and the same resource imported twice
// in one plan would be in the state twice.
func TestImportGuardRefusals(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	for _, c := range []struct {
		imports []runnerapi.Import
		want    string
	}{
		{[]runnerapi.Import{{To: "module.main.a.b", ID: ""}}, "no id for module.main.a.b"},
		{[]runnerapi.Import{{To: "module.main.a.b", ID: "/x/1"}, {To: "module.main.a.c", ID: "/X/1"}}, "imported twice"},
	} {
		_, run := newWorkspace(t, s, false)
		plan := runnerapi.Plan{Import: len(c.imports), Imports: c.imports}
		if st, err := s.FinishJob(ctx, run.ID, claim(t, s, "plan").LeaseID, "succeeded", "", nil, &plan, []runnerapi.Resource{}, nil, nil); err != nil || st != Errored {
			t.Fatalf("%v: %s %v", c.imports, st, err)
		}
		var msg string
		s.Pool.QueryRow(ctx, `SELECT message FROM run_status_history WHERE run_id = $1 ORDER BY created_at DESC LIMIT 1`, run.ID).Scan(&msg)
		if !strings.Contains(msg, c.want) {
			t.Errorf("%v: %q, want %q", c.imports, msg, c.want)
		}
	}
}

// A pending run waits for the active one and then runs the workspace's configuration of then: a change applied
// while it waited isn't undone by it.
func TestPendingTakesCurrentConfig(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	w, first := newWorkspace(t, s, false)
	if _, err := s.FinishJob(ctx, first.ID, claim(t, s, "plan").LeaseID, "succeeded", "", nil, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	change, err := s.SetWorkspaceTemplate(ctx, w.ID, WorkspaceTemplate{Name: "extra", TemplateID: "job-test", ModuleSource: "git::x",
		ModuleRef: "v1", Inputs: map[string]any{}}, true, uuid.Nil)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := s.CreateRun(ctx, w.ID, "apply", uuid.Nil, nil)
	if err != nil || pending.Status != Pending {
		t.Fatalf("pending: %+v %v", pending, err)
	}
	plan := &runnerapi.Plan{Add: 1}
	if st, err := s.FinishJob(ctx, change.ID, claim(t, s, "plan").LeaseID, "succeeded", "", nil, plan, nil, nil, nil); err != nil || st != NeedsConfirmation {
		t.Fatalf("change: %s %v", st, err)
	}
	if status(t, s, pending.ID) != Pending {
		t.Fatal("the pending run started while the change waits")
	}
	if err := s.ConfirmRun(ctx, change.ID, uuid.Nil, "a@b.c"); err != nil {
		t.Fatal(err)
	}
	if st, err := s.FinishJob(ctx, change.ID, claim(t, s, "apply").LeaseID, "succeeded", "", nil, nil, []runnerapi.Resource{}, nil, nil); err != nil || st != Applied {
		t.Fatalf("apply: %s %v", st, err)
	}
	if status(t, s, pending.ID) != PlanQueued {
		t.Fatalf("the pending run after the change applied: %s", status(t, s, pending.ID))
	}
	if mods, _ := s.RunTemplates(ctx, pending.ID); len(mods) != 2 {
		t.Fatalf("the pending run's configuration: %+v, want the applied change's", mods)
	}
}
