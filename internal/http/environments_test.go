package httpserver

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// The Environment values tab, and a deploy or configure form with an input the project sets.
func TestEnvironmentValuesPages(t *testing.T) {
	store := testStore(t)
	dir := t.TempDir()
	h := newServer(t, dir, true, store)
	sfx := strings.ToLower(newID()[:8])
	proj, ws, group := "env-"+sfx, "env-ws-"+sfx, "env-rd-"+sfx
	ok := func(code, want int, what string) {
		t.Helper()
		if code != want {
			t.Fatalf("%s: %d, want %d", what, code, want)
		}
	}
	ok(post(h, "/projects", url.Values{"name": {proj}}).Code, http.StatusSeeOther, "project")
	ok(post(h, "/projects/"+proj+"/access", url.Values{"group": {group}, "level": {"read"}}).Code, http.StatusSeeOther, "grant")
	rec := post(h, "/templates/new", url.Values{"name": {"Env " + sfx}, "module_source": {"git::https://x//m"}, "module_ref": {"v1"},
		"in_name": {"region", "note"}, "in_type": {"string", "string"}, "in_per_environment": {"0"}})
	ok(rec.Code, http.StatusSeeOther, "template")
	tpl := strings.TrimPrefix(rec.Header().Get("Location"), "/templates/")

	body := get(h, "/projects/"+proj+"/environment").Body.String()
	if !strings.Contains(body, "not set: each workspace's own") || !strings.Contains(body, `action="/projects/`+proj+`/environment"`) || strings.Contains(body, ">note<") {
		t.Fatalf("the tab lists the per-environment input, editable for an admin:\n%s", body)
	}
	ok(post(h, "/projects/"+proj+"/environment", url.Values{"template": {tpl}, "input": {"region"}, "value": {"northeurope"}}).Code, http.StatusSeeOther, "set")
	reader := newServerAs(t, dir, false, store, devAuth(t, store, "env-reader@example.com", false, group))
	body = get(reader, "/projects/"+proj+"/environment").Body.String()
	if !strings.Contains(body, "<code>northeurope</code>") || strings.Contains(body, `action="/projects/`+proj+`/environment`) {
		t.Fatalf("a reader sees the value, without a form:\n%s", body)
	}

	// the deploy form for the project shows it read-only; a posted value is ignored
	body = get(h, "/templates/"+tpl+"?project="+proj).Body.String()
	if !strings.Contains(body, `name="region" value="northeurope" disabled data-locked`) || !strings.Contains(body, `Set by project <a href="/projects/`+proj+`/environment">`) {
		t.Fatalf("the deploy form locks region:\n%s", body)
	}
	if body = get(h, "/templates/"+tpl).Body.String(); strings.Contains(body, "data-locked") || !strings.Contains(body, `data-refresh`) {
		t.Fatal("the default project sets nothing; picking a project refreshes the form")
	}
	// picking another project shows the form again for it, as typed, and creates nothing
	rec = post(h, "/templates/"+tpl, url.Values{fieldWorkspace: {ws}, fieldProject: {proj}, "region": {"typed"}, "note": {"typed-note"}, "refresh": {"1"}})
	if body = rec.Body.String(); rec.Code != http.StatusOK || !strings.Contains(body, `value="`+ws+`"`) || !strings.Contains(body, `value="typed-note"`) ||
		!strings.Contains(body, `name="region" value="northeurope" disabled data-locked`) || !strings.Contains(body, `<option value="`+proj+`" selected>`) {
		t.Fatalf("refresh: %d\n%s", rec.Code, body)
	}
	if _, err := store.GetWorkspace(t.Context(), ws); err == nil {
		t.Fatal("refresh created the workspace")
	}
	rec = post(h, "/templates/"+tpl, url.Values{fieldWorkspace: {ws}, fieldProject: {"default"}, "region": {"typed"}, "refresh": {"1"}})
	if body = rec.Body.String(); rec.Code != http.StatusOK || strings.Contains(body, "data-locked") || !strings.Contains(body, `name="region" value="typed"`) {
		t.Fatalf("refresh to a project without values: the field editable, as typed: %d", rec.Code)
	}
	ok(post(h, "/templates/"+tpl, url.Values{fieldWorkspace: {ws}, fieldProject: {proj}, "region": {"posted"}, "note": {"n"}}).Code, http.StatusSeeOther, "deploy")
	added, err := store.WorkspaceTemplates(t.Context(), mustWorkspace(t, store, ws).ID)
	if err != nil || len(added) != 1 || added[0].Inputs["region"] != nil || added[0].Inputs["note"] != "n" {
		t.Fatalf("stored: %v %v", added, err)
	}
	if body = get(h, "/workspaces/"+ws+"/templates/"+added[0].Name).Body.String(); !strings.Contains(body, "data-locked") {
		t.Fatal("the configure form locks region too")
	}
}
