package app

import (
	"context"
	"crypto/rand"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/opsolete/opsolete/internal/storage"
	"github.com/opsolete/opsolete/internal/templates"
)

// A rollout upgrades stale remote uses with their inputs plus new defaults, and skips the rest.
func TestRolloutPlan(t *testing.T) {
	a := &App{Secrets: keys("test-secret-key", "")}
	tpl := templates.Template{ID: "sa", Module: templates.Module{Source: "git::x", Ref: "v3"}, Inputs: []templates.Input{
		{Name: "name", Type: "string", Required: true},
		{Name: "tls", Type: "select", Required: true, Options: []templates.Option{{Value: "1.2"}, {Value: "1.3"}}, Default: "1.3"}, // new in v3
		{Name: "key", Type: "string", Sensitive: true, Required: true},
	}}
	sealed := map[string]any{"sealed": "c3RvcmVk"}
	w1, w2 := uuid.New(), uuid.New()
	old := func(in map[string]any) storage.TemplateUse {
		return storage.TemplateUse{WorkspaceID: w1, ExecutionMode: "remote", ModuleSource: "git::x", ModuleRef: "v2", Inputs: in}
	}
	ok := map[string]any{"name": "a", "gone": "dropped in v3", "key": sealed}
	cur, local, busy, missing, ready := old(ok), old(ok), old(ok), old(map[string]any{"key": sealed}), old(ok)
	cur.ModuleRef = "v3"
	local.ExecutionMode = "local"
	busy.RunID = uuid.New()
	ready.WorkspaceID, ready.Module = w2, "sa_c8ad"

	rows, ups, err := a.rolloutPlan(tpl, []storage.TemplateUse{cur, local, busy, missing, ready}, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{RolloutCurrent, RolloutLocal, RolloutBusy, RolloutNeedsInput, RolloutReady}
	for i, r := range rows {
		if r.State != want[i] {
			t.Errorf("row %d: %q, want %q", i, r.State, want[i])
		}
	}
	if m := rows[3].Missing; len(m) != 1 || m[0] != "name" {
		t.Errorf("missing: %q", m)
	}
	if len(ups) != 1 || len(ups[w2]) != 1 {
		t.Fatalf("upgrades: %+v", ups)
	}
	u := ups[w2][0]
	if u.ModuleRef != "v3" || u.Module != "sa_c8ad" || u.From.ModuleRef != "v2" {
		t.Errorf("upgrade: %+v", u)
	}
	if u.Inputs["name"] != "a" || u.Inputs["tls"] != "1.3" || u.Inputs["gone"] != nil || !IsSealed(u.Inputs["key"]) {
		t.Errorf("inputs: %v", u.Inputs)
	}
}

// A rollout with batches upgrades one workspace of its project at a time, retries a busy one,
// pauses on a discarded run, and ends when all are upgraded.
func TestTickRollout(t *testing.T) {
	a, ctx := testApp(t), context.Background()
	store := a.Store
	var err error
	if a.Templates, err = templates.Open("../../catalog"); err != nil {
		t.Fatal(err)
	}
	tpl, _ := a.Templates.Get("greeting")
	sfx := strings.ToLower(rand.Text()[:8])
	p, err := store.CreateProject(ctx, "ro-"+sfx, "")
	if err != nil {
		t.Fatal(err)
	}
	ws := map[string]uuid.UUID{} // name -> id
	for _, n := range []string{"a", "b", "c", "other"} {
		project := p.Name
		if n == "other" {
			project = "" // the default project: outside the rollout
		}
		w, _, err := store.CreateWorkspace(ctx, uuid.Nil, "ro-"+sfx+"-"+n, project, storage.WorkspaceTemplate{Name: "hello", TemplateID: "greeting",
			ModuleSource: tpl.Module.Source, ModuleRef: "1.0.0", Inputs: map[string]any{"greeting": "hi", "name": "x"}}, true, uuid.Nil)
		if err != nil {
			t.Fatal(err)
		}
		ws[n] = w.ID
	}
	end := func(n, status string) { // ends the workspace's active run
		t.Helper()
		if _, err := store.Pool.Exec(context.Background(), `UPDATE runs SET status = $2, lease_id = NULL, lease_until = NULL, updated_at = now() WHERE workspace_id = $1
			AND run_active(status)`, ws[n], status); err != nil {
			t.Fatal(err)
		}
	}
	for _, n := range []string{"a", "c", "other"} { // b stays busy with its first plan
		end(n, storage.Applied)
	}
	ro, err := store.CreateRollout(ctx, storage.Rollout{TemplateID: "greeting", ModuleSource: tpl.Module.Source, ModuleRef: tpl.Module.Ref,
		ProjectID: p.ID, BatchSize: 1})
	if err != nil {
		t.Fatal(err)
	}
	tick := func(wantStatus string, wantRefs ...string) { // refs of a, b, c, other
		t.Helper()
		ro, _ = store.LatestRollout(ctx, "greeting")
		if err := a.tickRollout(ctx, ro); err != nil {
			t.Fatal(err)
		}
		ro, _ = store.LatestRollout(ctx, "greeting")
		if ro.Status != wantStatus {
			t.Fatalf("status %q (%s), want %q", ro.Status, ro.Message, wantStatus)
		}
		for i, n := range []string{"a", "b", "c", "other"} {
			added, err := store.WorkspaceTemplates(ctx, ws[n])
			if err != nil || len(added) != 1 || added[0].ModuleRef != wantRefs[i] {
				t.Fatalf("%s runs %+v, want %q (%v)", n, added, wantRefs[i], err)
			}
		}
	}
	var runID uuid.UUID
	tick("active", "1.1.0", "1.0.0", "1.0.0", "1.0.0") // batch of one: a
	tick("active", "1.1.0", "1.0.0", "1.0.0", "1.0.0") // a's plan is still open
	end("a", storage.Applied)
	tick("active", "1.1.0", "1.0.0", "1.1.0", "1.0.0") // b is busy: c is next
	// discarded through the store: setStatus puts c's configuration back
	if err := store.Pool.QueryRow(context.Background(), `UPDATE runs SET status = 'needs_confirmation' WHERE workspace_id = $1 AND status = 'plan_queued'
		RETURNING id`, ws["c"]).Scan(&runID); err != nil {
		t.Fatal(err)
	}
	if err := store.DiscardRun(ctx, runID, "x"); err != nil {
		t.Fatal(err)
	}
	tick("paused", "1.1.0", "1.0.0", "1.0.0", "1.0.0") // the discard put c back on 1.0.0
	if !strings.Contains(ro.Message, "discarded") {
		t.Fatalf("pause message: %q", ro.Message)
	}
	store.SetRolloutStatus(ctx, ro.ID, "active", "")
	end("b", storage.Errored) // not the rollout's run: doesn't pause it
	tick("active", "1.1.0", "1.1.0", "1.0.0", "1.0.0")
	end("b", storage.Applied)
	tick("active", "1.1.0", "1.1.0", "1.1.0", "1.0.0") // resumed: c is retried
	end("c", storage.Applied)
	tick("done", "1.1.0", "1.1.0", "1.1.0", "1.0.0")

	// a rollout to a version the template no longer pins stops
	ro, err = store.CreateRollout(ctx, storage.Rollout{TemplateID: "greeting", ModuleSource: tpl.Module.Source, ModuleRef: "0.9.0", ProjectID: p.ID})
	if err != nil {
		t.Fatal(err)
	}
	tick("stopped", "1.1.0", "1.1.0", "1.1.0", "1.0.0")
}

func TestRolloutsAdminOnly(t *testing.T) {
	a, ctx := testApp(t), context.Background()
	var err error
	if a.Templates, err = templates.Open("../../catalog"); err != nil {
		t.Fatal(err)
	}
	someone := user(t, a)
	tpl, _ := a.Templates.Get("greeting")
	if _, err := a.Rollouts(ctx, someone, "greeting", ""); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a non-admin reads the rollout page: %v", err)
	}
	if _, err := a.StartRollout(ctx, someone, "greeting", NewRollout{Source: tpl.Module.Source, Ref: tpl.Module.Ref}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a non-admin starts a rollout: %v", err)
	}
	if _, err := a.RolloutAction(ctx, someone, "greeting", RolloutStop); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a non-admin stops a rollout: %v", err)
	}
	admin := Actor{Email: "admin@example.com", Admin: true}
	if _, err := a.StartRollout(ctx, admin, "greeting", NewRollout{Source: tpl.Module.Source, Ref: "0.0.1"}); !IsConflict(err) || !errors.Is(err, ErrChanged) {
		t.Fatalf("a rollout to a version the template no longer pins: %v", err)
	}
	if _, err := a.StartRollout(ctx, admin, "greeting", NewRollout{Source: tpl.Module.Source, Ref: tpl.Module.Ref, BatchSize: -1}); !isValidation(err) {
		t.Fatalf("a negative batch size: %v", err)
	}
}

// A required per-environment input the workspace's project sets isn't missing; the upgrade doesn't store the project's value.
func TestRolloutPlanEnvironment(t *testing.T) {
	a := &App{Secrets: keys("test-secret-key", "")}
	tpl := templates.Template{ID: "sa", Module: templates.Module{Source: "git::x", Ref: "v3"}, Inputs: []templates.Input{
		{Name: "name", Type: "string", Required: true},
		{Name: "region", Type: "string", Required: true, PerEnvironment: true}, // new in v3
	}}
	w := uuid.New()
	use := storage.TemplateUse{WorkspaceID: w, ExecutionMode: "remote", ModuleSource: "git::x", ModuleRef: "v2", Inputs: map[string]any{"name": "a"}}
	rows, _, err := a.rolloutPlan(tpl, []storage.TemplateUse{use}, nil)
	if err != nil || rows[0].State != RolloutNeedsInput || len(rows[0].Missing) != 1 || rows[0].Missing[0] != "region" {
		t.Fatalf("no project value: %+v %v", rows, err)
	}
	rows, ups, err := a.rolloutPlan(tpl, []storage.TemplateUse{use}, map[uuid.UUID]EnvironmentMap{w: {"sa": {"region": {Value: "west"}}}})
	if err != nil || rows[0].State != RolloutReady || len(ups[w]) != 1 {
		t.Fatalf("project value: %+v %v", rows, err)
	}
	if in := ups[w][0].Inputs; in["name"] != "a" || in["region"] != nil {
		t.Errorf("upgrade inputs: %v", in)
	}
}
