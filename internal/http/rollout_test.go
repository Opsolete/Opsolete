package httpserver

import (
	"context"
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/opsolete/opsolete/internal/storage"
	"github.com/opsolete/opsolete/internal/templates"
	"github.com/opsolete/opsolete/internal/vcs"
)

// A rollout upgrades stale remote uses with their inputs plus new defaults, and skips the rest.
func TestRolloutPlan(t *testing.T) {
	s := &Server{SecretKey: vcs.Key("test-secret-key")}
	tpl := templates.Template{ID: "sa", Module: templates.Module{Source: "git::x", Ref: "v3"}, Inputs: []templates.Input{
		{Name: "name", Type: "string", Required: true},
		{Name: "tls", Type: "select", Required: true, Options: []templates.Option{{Value: "1.2"}, {Value: "1.3"}}, Default: "1.3"}, // new in v3
		{Name: "key", Type: "string", Sensitive: true, Required: true},
	}}
	sealed := map[string]any{"sealed": "c3RvcmVk"}
	old := func(in map[string]any) storage.TemplateUse {
		return storage.TemplateUse{WorkspaceID: "w1", ExecutionMode: "remote", ModuleSource: "git::x", ModuleRef: "v2", Inputs: in}
	}
	ok := map[string]any{"name": "a", "gone": "dropped in v3", "key": sealed}
	cur, local, busy, missing, ready := old(ok), old(ok), old(ok), old(map[string]any{"key": sealed}), old(ok)
	cur.ModuleRef = "v3"
	local.ExecutionMode = "local"
	busy.RunID = "r1"
	ready.WorkspaceID, ready.Module = "w2", "sa_c8ad"

	rows, ups, err := s.rolloutPlan(tpl, []storage.TemplateUse{cur, local, busy, missing, ready})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{rolloutCurrent, rolloutLocal, rolloutBusy, rolloutNeedsInput, rolloutReady}
	for i, r := range rows {
		if r.State != want[i] {
			t.Errorf("row %d: %q, want %q", i, r.State, want[i])
		}
	}
	if m := rows[3].Missing; len(m) != 1 || m[0] != "name" {
		t.Errorf("missing: %q", m)
	}
	if len(ups) != 1 || len(ups["w2"]) != 1 {
		t.Fatalf("upgrades: %+v", ups)
	}
	u := ups["w2"][0]
	if u.ModuleRef != "v3" || u.Module != "sa_c8ad" || u.From.ModuleRef != "v2" {
		t.Errorf("upgrade: %+v", u)
	}
	if u.Inputs["name"] != "a" || u.Inputs["tls"] != "1.3" || u.Inputs["gone"] != nil || !isSealed(u.Inputs["key"]) {
		t.Errorf("inputs: %v", u.Inputs)
	}
}

func TestRenderRollout(t *testing.T) {
	s := New(nil, nil, nil, nil, nil, Features{}, nil)
	w := httptest.NewRecorder()
	ro := &storage.Rollout{ModuleRef: "v3", ProjectName: "team-a", BatchSize: 2, Status: "paused", Message: "the run in a: errored"}
	s.render(w, httptest.NewRequest("GET", "/", nil), 200, "rollout.html", rolloutPage{T: templates.Template{ID: "sa", Name: "SA"},
		Pending: 1, Rollout: ro, OpenRuns: 1, Rows: []rolloutRow{
			{TemplateUse: storage.TemplateUse{Workspace: "a", Module: "m", RunID: "r1", RunStatus: "needs_confirmation"}, State: rolloutCurrent},
			{TemplateUse: storage.TemplateUse{Workspace: "b", Module: "n"}, State: rolloutNeedsInput, Missing: []string{"x", "y"}},
		}})
	body := w.Body.String()
	for _, want := range []string{"/workspaces/a/runs/r1", "/workspaces/b/templates/n", "<code>y</code>", "/rollout/resume", "2 workspace(s) at a time",
		"the run in a: errored"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in:\n%s", want, body)
		}
	}
	if strings.Contains(body, `name="batch"`) {
		t.Error("an open rollout must not offer to start another")
	}
}

// A rollout with batches upgrades one workspace of its project at a time, retries a busy one,
// pauses on a discarded run, and ends when all are upgraded.
func TestTickRollout(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	s := newApp(t, "../../catalog", store, nil, Features{Deployments: true})
	tpl, _ := s.Templates.Get("greeting")
	store.DB.Exec(`UPDATE template_rollouts SET status = 'stopped' WHERE template_id = 'greeting'`) // from earlier runs
	sfx := strings.ToLower(rand.Text()[:8])
	p, err := store.CreateProject(ctx, "ro-"+sfx, "")
	if err != nil {
		t.Fatal(err)
	}
	ws := map[string]string{} // name -> id
	for _, n := range []string{"a", "b", "c", "other"} {
		project := p.Name
		if n == "other" {
			project = "" // the default project: outside the rollout
		}
		w, _, err := store.CreateWorkspace(ctx, "", "ro-"+sfx+"-"+n, project, storage.WorkspaceTemplate{Name: "hello", TemplateID: "greeting",
			ModuleSource: tpl.Module.Source, ModuleRef: "1.0.0", Inputs: map[string]any{"greeting": "hi", "name": "x"}}, true, "")
		if err != nil {
			t.Fatal(err)
		}
		ws[n] = w.ID
		t.Cleanup(func() { store.DB.Exec(`DELETE FROM workspaces WHERE id = $1`, w.ID) })
	}
	t.Cleanup(func() {
		store.DB.Exec(`DELETE FROM template_rollouts WHERE project_id = $1`, p.ID)
		store.DB.Exec(`DELETE FROM projects WHERE id = $1`, p.ID)
	})
	end := func(n, status string) { // ends the workspace's active run
		t.Helper()
		if _, err := store.DB.Exec(`UPDATE runs SET status = $2, updated_at = now() WHERE workspace_id = $1
			AND status IN ('plan_queued', 'planning', 'needs_confirmation', 'apply_queued', 'applying')`, ws[n], status); err != nil {
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
		if err := s.tickRollout(ctx, ro); err != nil {
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
	tick("active", "1.1.0", "1.0.0", "1.0.0", "1.0.0") // batch of one: a
	tick("active", "1.1.0", "1.0.0", "1.0.0", "1.0.0") // a's plan is still open
	end("a", storage.Applied)
	tick("active", "1.1.0", "1.0.0", "1.1.0", "1.0.0") // b is busy: c is next
	end("c", storage.Discarded)
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

// Starting a rollout needs the version the page showed; one with nothing to do is done at once.
func TestStartRollout(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	h := newServer(t, "../../catalog", false, store)
	store.DB.Exec(`UPDATE template_rollouts SET status = 'stopped' WHERE template_id = 'greeting'`) // from earlier runs
	p, err := store.CreateProject(ctx, "ro-"+strings.ToLower(rand.Text()[:8]), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		store.DB.Exec(`DELETE FROM template_rollouts WHERE project_id = $1`, p.ID)
		store.DB.Exec(`DELETE FROM projects WHERE id = $1`, p.ID)
	})
	form := url.Values{"source": {"opsolete.localhost:8080/examples/greeting/terraform"}, "ref": {"1.0.0"}, "project": {p.Name}, "batch": {"2"}}
	if rec := post(h, "/templates/greeting/rollout", form); rec.Code != http.StatusConflict {
		t.Fatalf("stale version: %d %s", rec.Code, rec.Body)
	}
	form.Set("ref", "1.1.0")
	if rec := post(h, "/templates/greeting/rollout", form); rec.Code != http.StatusSeeOther {
		t.Fatalf("start: %d %s", rec.Code, rec.Body)
	}
	ro, err := store.LatestRollout(ctx, "greeting")
	if err != nil || ro.ProjectID != p.ID || ro.BatchSize != 2 || ro.Status != "done" {
		t.Fatalf("rollout: %+v %v", ro, err)
	}
	if rec := post(h, "/templates/greeting/rollout/pause", nil); rec.Code != http.StatusConflict {
		t.Fatalf("pause a done rollout: %d", rec.Code)
	}
	if rec := do(h, httptest.NewRequest("GET", "/templates/greeting/rollout?project="+p.Name, nil)); rec.Code != http.StatusOK ||
		!strings.Contains(rec.Body.String(), "No workspace in this project") {
		t.Fatalf("page: %d %s", rec.Code, rec.Body)
	}
}
