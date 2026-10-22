package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/rudesome/form2stack/internal/runnerapi"
	"github.com/rudesome/form2stack/internal/storage"
)

// TestWorkspaceTemplates: any number of templates can be added to a workspace, each a module the
// runner gets with its inputs; changing or removing one plans the change.
func TestWorkspaceTemplates(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	claimWait = 100 * time.Millisecond
	store.DB.Exec(`UPDATE runs SET status = 'errored' WHERE status IN ('plan_queued', 'planning', 'needs_confirmation', 'apply_queued', 'applying')`)
	ws := "wt-" + strings.ToLower(newID()[:8])
	t.Cleanup(func() { store.DB.Exec(`DELETE FROM workspaces WHERE name = $1`, ws) })
	h := newServer(t, "../../catalog", false, store)
	runnerTok, _ := store.CreateToken(ctx, "r", storage.ScopeRunner)
	claim := func() runnerapi.Job {
		t.Helper()
		req := httptest.NewRequest("POST", runnerapi.Prefix+"/jobs/claim", strings.NewReader(`{"runner":"r1"}`))
		req.Header.Set("Authorization", "Bearer "+runnerTok)
		rec := do(h, req)
		var j runnerapi.Job
		if json.Unmarshal(rec.Body.Bytes(), &j); rec.Code != 200 {
			t.Fatalf("claim: %d %s", rec.Code, rec.Body.String())
		}
		store.DB.Exec(`UPDATE runs SET status = 'errored' WHERE id = $1`, j.RunID) // free the workspace
		return j
	}
	base := "/workspaces/" + ws + "/templates/"

	if rec := post(h, "/workspaces/new", url.Values{"name": {ws}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	if page := get(h, base+"new?template=greeting").Body.String(); !strings.Contains(page, `name="f2s-module" value="greeting"`) {
		t.Fatalf("add form: %s", page)
	}
	if rec := get(h, base+"new?template=nope"); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown template: %d", rec.Code)
	}
	add := url.Values{"f2s-module": {"new"}, "greeting": {"hi"}, "name": {"a"}}
	if rec := post(h, base+"new?template=greeting", add); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf(`"new" is reserved: %d`, rec.Code)
	}
	for _, name := range []string{"one", "two"} {
		add.Set("f2s-module", name)
		add.Set("name", name)
		if rec := post(h, base+"new?template=greeting", add); rec.Code != http.StatusSeeOther {
			t.Fatalf("add %s: %d %s", name, rec.Code, rec.Body.String())
		}
		claim()
	}
	if rec := post(h, base+"new?template=greeting", add); rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "already has a template named two") {
		t.Fatalf("duplicate name: %d", rec.Code)
	}

	// Both reach the runner as modules, with their inputs; there is no root module.
	if rec := post(h, "/workspaces/"+ws+"/runs", url.Values{"action": {"apply"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("new run: %d %s", rec.Code, rec.Body.String())
	}
	j := claim()
	if j.ModuleSource != "" || len(j.Modules) != 2 || j.Modules[0].Name != "one" || j.Modules[1].Inputs["name"] != "two" ||
		!strings.HasSuffix(j.Modules[0].Source, "/examples/greeting/terraform?ref=1.1.0") {
		t.Fatalf("job: %+v", j)
	}

	// Configure one, remove the other; the last one can't go.
	if rec := post(h, base+"one", url.Values{"greeting": {"hey"}, "name": {"x"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("configure: %d %s", rec.Code, rec.Body.String())
	}
	if j := claim(); len(j.Modules) != 2 || j.Modules[0].Inputs["greeting"] != "hey" {
		t.Fatalf("configured job: %+v", j)
	}
	if rec := post(h, base+"two/remove", nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("remove: %d %s", rec.Code, rec.Body.String())
	}
	if j := claim(); len(j.Modules) != 1 {
		t.Fatalf("job after remove: %+v", j)
	}
	if rec := post(h, base+"one/remove", nil); rec.Code != http.StatusConflict {
		t.Fatalf("removing the only configuration: %d", rec.Code)
	}
	if rec := post(h, base+"two/remove", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("removing twice: %d", rec.Code)
	}
	if page := get(h, "/workspaces/"+ws).Body.String(); !strings.Contains(page, base+"one") || strings.Contains(page, base+"two") {
		t.Fatalf("workspace page: %s", page)
	}
}
