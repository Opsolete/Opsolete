package httpserver

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestProjectPages(t *testing.T) {
	store := testStore(t)
	h := newServer(t, "../../catalog", false, store)
	user := newServerAs(t, "../../catalog", false, store, devAuth(t, store, "proj-user@example.com", false))
	sfx := strings.ToLower(newID()[:8])
	proj, ws := "prj-"+sfx, "prj-ws-"+sfx
	t.Cleanup(func() {
		store.DB.Exec(`DELETE FROM workspaces WHERE name = $1`, ws)
		store.DB.Exec(`DELETE FROM projects WHERE name LIKE $1`, proj+"%")
	})
	want := func(rec *httptest.ResponseRecorder, code int, what string) {
		t.Helper()
		if rec.Code != code {
			t.Fatalf("%s: %d, want %d: %s", what, rec.Code, code, rec.Body.String())
		}
	}

	want(post(user, "/projects", url.Values{"name": {proj}}), http.StatusForbidden, "a user creates a project")
	want(post(h, "/projects", url.Values{"name": {"new"}}), http.StatusBadRequest, "reserved name")
	want(post(h, "/projects", url.Values{"name": {proj}, "description": {"d"}}), http.StatusSeeOther, "create")
	want(post(h, "/projects", url.Values{"name": {proj}}), http.StatusConflict, "duplicate")
	want(get(user, "/projects/"+proj), http.StatusNotFound, "a user without a workspace in it")
	if body := get(user, "/projects").Body.String(); strings.Contains(body, proj) || !strings.Contains(body, "default") {
		t.Fatal("a user lists only the default project")
	}

	// a user can't put a workspace into a project they can't see; an admin can
	want(post(user, "/workspaces/new", url.Values{"name": {ws}, "ops-project": {proj}}), http.StatusBadRequest, "user picks a hidden project")
	want(post(h, "/workspaces/new", url.Values{"name": {ws}, "ops-project": {proj}}), http.StatusSeeOther, "create in project")
	if body := get(h, "/workspaces?project="+proj).Body.String(); !strings.Contains(body, ws) {
		t.Fatal("the filtered list shows the project's workspace")
	}
	if body := get(h, "/workspaces?project=default").Body.String(); strings.Contains(body, ws) {
		t.Fatal("the default project's list shows another project's workspace")
	}
	if body := get(h, "/projects/"+proj).Body.String(); !strings.Contains(body, ws) {
		t.Fatal("the project page lists its workspace")
	}
	want(post(h, "/projects/"+proj+"/delete", nil), http.StatusConflict, "delete a non-empty project")
	want(post(h, "/projects/default/delete", nil), http.StatusConflict, "delete the default project")

	want(post(h, "/projects/"+proj+"/settings", url.Values{"name": {proj + "-x"}, "description": {"e"}}), http.StatusSeeOther, "rename")
	if body := get(h, "/workspaces/"+ws+"/settings").Body.String(); !strings.Contains(body, `value="`+proj+`-x" selected`) {
		t.Fatal("the workspace's settings show its renamed project")
	}
	want(post(user, "/workspaces/"+ws+"/project", url.Values{"project": {"default"}}), http.StatusForbidden, "a user moves a workspace")
	want(post(h, "/workspaces/"+ws+"/project", url.Values{"project": {"nope-" + sfx}}), http.StatusBadRequest, "move to an unknown project")
	want(post(h, "/workspaces/"+ws+"/project", url.Values{"project": {"default"}}), http.StatusSeeOther, "move")
	want(post(h, "/projects/"+proj+"-x/delete", nil), http.StatusSeeOther, "delete the emptied project")
	want(get(h, "/projects/"+proj+"-x"), http.StatusNotFound, "deleted project")
}
