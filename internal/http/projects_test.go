package httpserver

import (
	"context"
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
		store.Pool.Exec(context.Background(), `DELETE FROM workspaces WHERE name = $1`, ws)
		store.Pool.Exec(context.Background(), `DELETE FROM projects WHERE name LIKE $1`, proj+"%")
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
	if body := get(h, "/projects").Body.String(); !strings.Contains(body, `<tr data-name="`+proj+`">`) || !strings.Contains(body, `id="new-project"`) ||
		!strings.Contains(body, `href="/projects/`+proj+`/settings">Edit project`) {
		t.Fatal("an admin's projects list: searchable rows, the New project dialog and Edit project actions")
	}
	if body := get(user, "/projects").Body.String(); strings.Contains(body, `id="new-project"`) || strings.Contains(body, "Edit project") {
		t.Fatal("a user gets neither New project nor Edit project")
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
	// the project's menu: Overview, Workspaces, and Settings with General, Team access and Variable sets
	for _, path := range []string{"", "/workspaces"} {
		if body := get(h, "/projects/"+proj+path).Body.String(); !strings.Contains(body, `href="/workspaces/`+ws+`"`) ||
			!strings.Contains(body, `<nav aria-label="Project">`) {
			t.Fatalf("%s lists the project's workspace in the project's menu", path)
		}
	}
	for _, path := range []string{"/settings", "/settings/access", "/settings/variable-sets"} {
		if body := get(h, "/projects/"+proj+path).Body.String(); !strings.Contains(body, `<nav aria-label="Project settings">`) ||
			!strings.Contains(body, `href="/projects/`+proj+path+`" aria-current="page"`) {
			t.Fatalf("%s shows the settings menu with itself current", path)
		}
	}
	want(get(h, "/projects/"+proj+"/settings/nope"), http.StatusNotFound, "unknown settings page")
	if body := get(h, "/workspaces/"+ws).Body.String(); !strings.Contains(body, `<a href="/projects/`+proj+`">`+proj+`</a> / <a href="/projects/`+proj+`/workspaces">Workspaces</a>`) {
		t.Fatal("a workspace's breadcrumb starts with its project")
	}
	want(get(user, "/projects/"+proj+"/settings"), http.StatusNotFound, "settings of a project the user can't see")
	// a project grant reaches its workspaces; without one they don't exist
	group := "grp-prj-" + sfx
	member := newServerAs(t, "../../catalog", false, store, devAuth(t, store, "proj-member@example.com", false, group))
	want(get(member, "/workspaces/"+ws), http.StatusNotFound, "no access")
	want(post(user, "/projects/"+proj+"/access", url.Values{"group": {group}, "level": {"read"}}), http.StatusForbidden, "a user grants")
	want(post(h, "/projects/"+proj+"/access", url.Values{"group": {group}, "level": {"admin"}}), http.StatusBadRequest, "bad level")
	want(post(h, "/projects/"+proj+"/access", url.Values{"group": {group}, "level": {"read"}}), http.StatusSeeOther, "grant read")
	want(get(member, "/workspaces/"+ws), http.StatusOK, "read through the project")
	want(get(member, "/projects/"+proj), http.StatusOK, "the granted project")
	want(post(member, "/workspaces/"+ws+"/settings", url.Values{"auto_apply": {"on"}}), http.StatusForbidden, "read can't operate")
	// creating a workspace in a project hands it the project's variable sets: read isn't enough
	ws2 := "prj-ws2-" + sfx
	t.Cleanup(func() { store.Pool.Exec(context.Background(), `DELETE FROM workspaces WHERE name = $1`, ws2) })
	want(post(member, "/workspaces/new", url.Values{"name": {ws2}, "ops-project": {proj}}), http.StatusBadRequest, "create with read")
	if strings.Contains(get(member, "/projects/"+proj).Body.String(), "+ New workspace") {
		t.Fatal("read access offers creating a workspace in the project")
	}
	if body := get(h, "/workspaces/"+ws+"/settings/access").Body.String(); !strings.Contains(body, "<code>"+group+"</code> read") {
		t.Fatal("the workspace's Team access page shows the project grant")
	}
	want(post(h, "/projects/"+proj+"/access", url.Values{"group": {group}, "level": {"operate"}}), http.StatusSeeOther, "grant operate")
	want(post(member, "/workspaces/"+ws+"/settings", url.Values{"auto_apply": {"on"}}), http.StatusSeeOther, "operate through the project")
	want(post(member, "/workspaces/new", url.Values{"name": {ws2}, "ops-project": {proj}}), http.StatusSeeOther, "create with operate")
	want(post(h, "/workspaces/"+ws2+"/project", url.Values{"project": {"default"}}), http.StatusSeeOther, "move it out again")
	want(post(h, "/projects/"+proj+"/access", url.Values{"group": {group}}), http.StatusSeeOther, "revoke")
	want(get(member, "/workspaces/"+ws), http.StatusNotFound, "revoked")

	// a variable set attached to the project applies to its workspaces
	set := "prj-set-" + sfx
	t.Cleanup(func() { store.Pool.Exec(context.Background(), `DELETE FROM variable_sets WHERE name = $1`, set) })
	want(post(h, "/variable-sets", url.Values{"name": {set}}), http.StatusSeeOther, "create set")
	want(post(user, "/variable-sets/"+set+"/projects", url.Values{"project": {proj}}), http.StatusForbidden, "a user attaches a set")
	want(post(h, "/variable-sets/"+set+"/projects", url.Values{"project": {proj, "nope-" + sfx}}), http.StatusBadRequest, "unknown project")
	want(post(h, "/variable-sets/"+set+"/projects", url.Values{"project": {proj}}), http.StatusSeeOther, "attach to project")
	if body := get(h, "/variable-sets/"+set).Body.String(); !strings.Contains(body, `value="`+proj+`" checked>`) {
		t.Fatal("the set's page shows the project attached")
	}
	if body := get(h, "/workspaces/"+ws+"/variables").Body.String(); !strings.Contains(body, set) {
		t.Fatal("the project's workspace lists the set")
	}
	if body := get(h, "/projects/"+proj+"/settings/variable-sets").Body.String(); !strings.Contains(body, `href="/variable-sets/`+set+`"`) {
		t.Fatal("the project's Variable sets page lists the set")
	}
	want(post(h, "/variable-sets/"+set+"/projects", nil), http.StatusSeeOther, "detach")

	// created from the project's or workspace's page, a set is attached there
	pset, wset := "prj-new-"+sfx, "ws-new-"+sfx
	t.Cleanup(func() {
		store.Pool.Exec(context.Background(), `DELETE FROM variable_sets WHERE name IN ($1, $2)`, pset, wset)
	})
	want(post(user, "/variable-sets", url.Values{"name": {pset}, "project": {proj}}), http.StatusForbidden, "a user creates a set")
	want(post(h, "/variable-sets", url.Values{"name": {pset}, "project": {"nope-" + sfx}}), http.StatusBadRequest, "unknown project")
	want(post(h, "/variable-sets", url.Values{"name": {pset}, "project": {proj}}), http.StatusSeeOther, "create for project")
	want(post(h, "/variable-sets", url.Values{"name": {wset}, "workspace": {ws}}), http.StatusSeeOther, "create for workspace")
	if body := get(h, "/projects/"+proj+"/settings/variable-sets").Body.String(); !strings.Contains(body, `href="/variable-sets/`+pset+`"`) {
		t.Fatal("a set created for the project is attached to it")
	}
	if body := get(h, "/workspaces/"+ws+"/variables").Body.String(); !strings.Contains(body, pset) || !strings.Contains(body, wset) {
		t.Fatal("the workspace gets the sets created for it and its project")
	}
	if body := get(h, "/workspaces/"+ws+"/variables").Body.String(); strings.Contains(body, set) {
		t.Fatal("a detached set still applies")
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
