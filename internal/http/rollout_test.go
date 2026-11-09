package httpserver

import (
	"context"
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/opsolete/opsolete/internal/app"
	"github.com/opsolete/opsolete/internal/storage"
	"github.com/opsolete/opsolete/internal/templates"
)

func TestRenderRollout(t *testing.T) {
	s := New(&app.App{}, nil, Features{})
	w := httptest.NewRecorder()
	r1 := uuid.New()
	ro := &storage.Rollout{ModuleRef: "v3", ProjectName: "team-a", BatchSize: 2, Status: "paused", Message: "the run in a: errored"}
	s.render(w, httptest.NewRequest("GET", "/", nil), 200, "rollout.html", &rolloutPage{T: templates.Template{ID: "sa", Name: "SA"},
		Pending: 1, Rollout: ro, OpenRuns: 1, Rows: []app.RolloutRow{
			{TemplateUse: storage.TemplateUse{Workspace: "a", Module: "m", RunID: r1, RunStatus: "needs_confirmation"}, State: app.RolloutCurrent},
			{TemplateUse: storage.TemplateUse{Workspace: "b", Module: "n"}, State: app.RolloutNeedsInput, Missing: []string{"x", "y"}},
		}})
	body := w.Body.String()
	for _, want := range []string{"/workspaces/a/runs/" + r1.String(), "/workspaces/b/templates/n", "<code>y</code>", "/rollout/resume", "2 workspace(s) at a time",
		"the run in a: errored"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in:\n%s", want, body)
		}
	}
	if strings.Contains(body, `name="batch"`) {
		t.Error("an open rollout must not offer to start another")
	}
}

// Starting a rollout needs the version the page showed; one with nothing to do is done at once.
func TestStartRollout(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	h := newServer(t, "../../catalog", false, store)
	p, err := store.CreateProject(ctx, "ro-"+strings.ToLower(rand.Text()[:8]), "")
	if err != nil {
		t.Fatal(err)
	}
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
