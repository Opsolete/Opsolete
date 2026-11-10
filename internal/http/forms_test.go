package httpserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/opsolete/opsolete/internal/app"
	"github.com/opsolete/opsolete/internal/registry/registrytest"
	"github.com/opsolete/opsolete/internal/storage"
	"github.com/opsolete/opsolete/internal/vcs"
)

// noInvalidInput are the UI POST routes without a form a user can fill in wrongly; every other one
// has a row in TestFormsRerender.
var noInvalidInput = map[string]string{
	"POST /oauth/authorization":                                 "consent: its input is the OAuth client's query",
	"POST /account/tokens/{id}/revoke":                          "a button",
	"POST /account/tokens/revoke":                               "a button",
	"POST /account/sessions/revoke":                             "a button",
	"POST /users/{id}/sessions/revoke":                          "a button: the user is in the path",
	"POST /registry/{ns}/{name}/{system}/sync":                  "a button",
	"POST /registry/{ns}/{name}/{system}/unlink":                "a button",
	"POST /registry/{ns}/{name}/{system}/delete":                "a button",
	"POST /vcs/{id}/delete":                                     "a button",
	"POST /catalog/sync":                                        "a button",
	"POST /catalog/delete":                                      "a button",
	"POST /templates/{id}/delete":                               "a button",
	"POST /templates/{id}/rollout/{action}":                     "buttons: the action is in the path",
	"POST /projects/{project}/delete":                           "a button",
	"POST /projects/{project}/environment/clear":                "a button",
	"POST /workspaces/{name}/settings/health":                   "a checkbox",
	"POST /workspaces/{name}/settings/remote-state":             "radios and a picker; unknown ids are ignored",
	"POST /workspaces/{name}/templates/{module}/remove":         "a button",
	"POST /workspaces/{name}/variables/{id}/delete":             "a button",
	"POST /workspaces/{name}/state-versions/{version}/rollback": "a button",
	"POST /workspaces/{name}/runs/{run}/confirm":                "a button",
	"POST /workspaces/{name}/runs/{run}/discard":                "a button",
	"POST /workspaces/{name}/runs/{run}/cancel":                 "buttons (force is hidden)",
	"POST /workspaces/{name}/delete":                            "a button (force is hidden)",
	"POST /variable-sets/{set}":                                 "a description and a checkbox: any value is valid",
	"POST /variable-sets/{set}/delete":                          "a button",
	"POST /variable-sets/{set}/variables/{id}/delete":           "a button",
}

// formRow posts an invalid value to one form.
type formRow struct {
	pattern   string
	path      string // with {placeholders}
	form      url.Values
	page      string // on the re-rendered page: its heading or the form's action
	echo      string // the user's input, as the page shows it; "" for a select, picker or secret
	field     string // the input with the error, shown next to it; "" for a message at the top only
	aria      bool   // field is one input, marked aria-invalid and describedby its error
	multipart bool
}

// TestFormsRerender posts an invalid value to every UI form: it re-renders (422) with the input
// kept and the error next to its field. A UI POST route needs a row here or in noInvalidInput.
func TestFormsRerender(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	sfx := strings.ToLower(newID()[:8])
	srv := newApp(t, t.TempDir(), store, devAuth(t, store, "forms-admin@example.com", true),
		Features{Deployments: true, Registry: true, Editor: true, VCS: true})
	h := srv.Handler()
	ws, proj, set, ns := "frm-ws-"+sfx, "frm-"+sfx, "frm-set-"+sfx, "frm"+sfx
	must := func(rec *httptest.ResponseRecorder, what string) *httptest.ResponseRecorder {
		t.Helper()
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("fixture %s: %d %s", what, rec.Code, rec.Body.String())
		}
		return rec
	}
	must(post(h, "/projects", url.Values{"name": {proj}}), "project")
	tplForm := url.Values{"name": {"Frm " + sfx}, "module_source": {"git::https://x//m"}, "module_ref": {"v1"}, "in_name": {"size"}, "in_type": {"number"}, "in_per_environment": {"0"}}
	tpl := strings.TrimPrefix(must(post(h, "/templates/new", tplForm), "template").Header().Get("Location"), "/templates/")
	must(post(h, "/templates/"+tpl, url.Values{fieldWorkspace: {ws}, "size": {"1"}}), "workspace")
	tplForm.Set("module_ref", "v2") // the workspace is on v1: a rollout is pending
	must(post(h, "/templates/"+tpl+"/edit", tplForm), "template v2")
	added, err := store.WorkspaceTemplates(ctx, mustWorkspace(t, store, ws).ID)
	if err != nil || len(added) != 1 {
		t.Fatalf("workspace templates: %v %v", added, err)
	}
	must(post(h, "/variable-sets", url.Values{"name": {set}}), "variable set")
	conn := storage.VCSConnection{Name: "frm-" + sfx, Provider: vcs.GitHub, BaseURL: "https://api.github.com"}
	if conn.Token, err = srv.App.Secrets.Seal("tok", app.TokenAD(conn)); err != nil {
		t.Fatal(err)
	}
	connID, err := store.CreateVCSConnection(ctx, conn)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetVCSScope(ctx, connID, "all"); err != nil {
		t.Fatal(err)
	}
	fill := strings.NewReplacer("{ws}", ws, "{proj}", proj, "{set}", set, "{tpl}", tpl, "{conn}", connID.String(), "{module}", added[0].Name).Replace

	long := strings.Repeat("x", 301)
	rows := []formRow{
		{"POST /organization", "/organization", url.Values{"currency": {"GBP"}}, `action="/organization"`, "", "currency", true, false},
		{"POST /registry", "/registry", url.Values{"namespace": {ns}, "name": {"bad name"}, "system": {"aws"}, "version": {"1.0.0"}}, `action="/registry"`, `value="bad name"`, "name", true, true},
		{"POST /registry/publish", "/registry/publish", url.Values{"connection": {"{conn}"}, "repo": {"acme/x"}, "namespace": {ns}, "name": {"net"}, "system": {"aws"}, "dir": {"../x"}},
			`action="/registry/publish"`, `value="../x"`, "dir", true, false},
		{"POST /vcs", "/vcs", url.Values{"provider": {"github"}, "base_url": {"https://echo.example"}, "token": {"t"}}, `action="/vcs"`, `value="https://echo.example"`, "name", true, false},
		{"POST /vcs/{id}/settings", "/vcs/{conn}/settings", url.Values{"wizard": {"1"}, "return": {"workspace"}}, `action="/vcs/{conn}/settings"`, "", "workspace_scope", false, false},
		{"POST /vcs/{id}/token", "/vcs/{conn}/token", url.Values{"token": {" "}}, `action="/vcs/{conn}/token"`, "", "token", true, false},
		{"POST /catalog", "/catalog", url.Values{"connection": {"{conn}"}, "repo": {"acme/x"}, "branch": {"bad..branch"}, "mode": {"pr"}}, `action="/catalog"`, `value="bad..branch"`, "branch", true, false},
		{"POST /templates/{id}", "/templates/{tpl}", url.Values{fieldWorkspace: {"Bad Name"}, "size": {"1"}}, `id="template-form"`, `value="Bad Name"`, fieldWorkspace, true, false},
		{"POST /templates/new", "/templates/new", url.Values{"description": {"echo-desc"}}, `id="builder"`, `value="echo-desc"`, "", false, false},
		{"POST /templates/{id}/edit", "/templates/{tpl}/edit", url.Values{"description": {"echo-desc"}}, `id="builder"`, `value="echo-desc"`, "", false, false},
		{"POST /templates/{id}/rollout", "/templates/{tpl}/rollout", url.Values{"source": {"git::https://x//m"}, "ref": {"v2"}, "batch": {"-1"}}, "<h1>Roll out", `value="-1"`, "batch", true, false},
		{"POST /projects", "/projects", url.Values{"name": {"Bad Name"}}, `id="new-project"`, `value="Bad Name"`, "name", true, false},
		{"POST /projects/{project}/settings", "/projects/{proj}/settings", url.Values{"name": {"Bad Name"}}, `action="/projects/{proj}/settings"`, `value="Bad Name"`, "name", true, false},
		{"POST /projects/{project}/access", "/projects/{proj}/access", url.Values{"group": {"echo-group"}, "level": {"admin"}}, `action="/projects/{proj}/access"`, `value="echo-group"`, "level", true, false},
		{"POST /projects/{project}/environment", "/projects/{proj}/environment", url.Values{"template": {"{tpl}"}, "input": {"size"}, "value": {"abc"}}, `action="/projects/{proj}/environment"`, `value="abc"`, "value", true, false},
		{"POST /workspaces/new", "/workspaces/new", url.Values{"type": {"cli"}, fieldWorkspace: {"Bad Name"}}, "Create a new workspace", `value="Bad Name"`, fieldWorkspace, true, false},
		{"POST /workspaces/{name}/vcs", "/workspaces/{ws}/vcs", url.Values{"type": {"vcs"}, "vcs_connection": {"{conn}"}, "vcs_repo": {"acme/x"}, "vcs_branch": {"bad..branch"}},
			`action="/workspaces/{ws}/vcs"`, `value="bad..branch"`, "vcs_branch", true, false},
		{"POST /workspaces/{name}/templates/new", "/workspaces/{ws}/templates/new?template={tpl}", url.Values{"size": {"abc"}}, `id="template-form"`, `value="abc"`, "size", true, false},
		{"POST /workspaces/{name}/templates/{module}", "/workspaces/{ws}/templates/{module}", url.Values{"size": {"abc"}}, `id="template-form"`, `value="abc"`, "size", true, false},
		{"POST /workspaces/{name}/variables", "/workspaces/{ws}/variables", url.Values{"category": {"env"}, "key": {"PATH"}, "value": {"v"}}, `action="/workspaces/{ws}/variables"`, `value="PATH"`, "key", true, false},
		{"POST /workspaces/{name}/settings", "/workspaces/{ws}/settings", url.Values{"name": {"Not Valid"}, "execution_mode": {"remote"}}, `action="/workspaces/{ws}/settings"`, `value="Not Valid"`, "name", true, false},
		{"POST /workspaces/{name}/access", "/workspaces/{ws}/access", url.Values{"group": {"echo-group"}, "level": {"admin"}}, `action="/workspaces/{ws}/access"`, `value="echo-group"`, "level", true, false},
		{"POST /workspaces/{name}/lock", "/workspaces/{ws}/lock", url.Values{"action": {"lock"}, "reason": {long}}, `action="/workspaces/{ws}/lock"`, ">" + long + "</textarea>", "reason", true, false},
		{"POST /workspaces/{name}/project", "/workspaces/{ws}/project", url.Values{"project": {"nope-" + sfx}}, `action="/workspaces/{ws}/project"`, "", "project", true, false},
		{"POST /workspaces/{name}/promote", "/workspaces/{ws}/promote", url.Values{"new": {"Bad Name"}, "project": {"{proj}"}}, "<h1>Promote {ws}", `value="Bad Name"`, "new", true, false},
		{"POST /workspaces/{name}/runs", "/workspaces/{ws}/runs", url.Values{"action": {"bogus"}}, "<h1>{ws}", "", "", false, false},
		{"POST /workspaces/{name}/scans", "/workspaces/{ws}/scans", url.Values{"template": {"{tpl}"}}, "<h1>{ws}", "", "", false, false}, // not scannable
		{"POST /variable-sets", "/variable-sets", url.Values{"name": {"Bad Name"}, "project": {"{proj}"}}, `action="/variable-sets"`, `value="Bad Name"`, "name", true, false},
		{"POST /variable-sets/{set}/variables", "/variable-sets/{set}/variables", url.Values{"category": {"env"}, "key": {"PATH"}, "value": {"v"}}, `action="/variable-sets/{set}/variables"`, `value="PATH"`, "key", true, false},
		{"POST /variable-sets/{set}/workspaces", "/variable-sets/{set}/workspaces", url.Values{"workspace": {"nope-" + sfx}}, `action="/variable-sets/{set}/workspaces"`, `value="nope-` + sfx + `" checked`, "workspace", false, false},
		{"POST /variable-sets/{set}/projects", "/variable-sets/{set}/projects", url.Values{"project": {"nope-" + sfx}}, `action="/variable-sets/{set}/projects"`, "", "project", false, false},
	}

	// coverage: every UI POST route is a row or has no invalid input
	var formRoutes []string
	for _, r := range rows {
		formRoutes = append(formRoutes, r.pattern)
	}
	for _, p := range srv.patterns {
		method, path, _ := strings.Cut(p, " ")
		if method != http.MethodPost || public(path) {
			continue
		}
		_, none := noInvalidInput[p]
		if in := slices.Contains(formRoutes, p); in == none {
			t.Errorf("%s: needs a row in TestFormsRerender or an entry in noInvalidInput (not both)", p)
		}
	}
	for p := range noInvalidInput {
		if !slices.Contains(srv.patterns, p) {
			t.Errorf("noInvalidInput %q: no such route", p)
		}
	}

	for _, r := range rows {
		form := url.Values{}
		for k, v := range r.form {
			for _, x := range v {
				form.Add(k, fill(x))
			}
		}
		var rec *httptest.ResponseRecorder
		if r.multipart {
			fields := map[string]string{}
			for k := range form {
				fields[k] = form.Get(k)
			}
			rec = upload(h, fields, registrytest.TarGz(t, map[string]string{"main.tf": ""}))
		} else {
			rec = post(h, fill(r.path), form)
		}
		body := rec.Body.String()
		switch {
		case rec.Code != http.StatusUnprocessableEntity:
			t.Errorf("%s: %d, want 422: %.300s", r.pattern, rec.Code, body)
		case !strings.Contains(body, fill(r.page)):
			t.Errorf("%s: not its page (%s)", r.pattern, fill(r.page))
		case !strings.Contains(body, `<div class="alert error" role="alert">`):
			t.Errorf("%s: no error at the top", r.pattern)
		case !strings.Contains(body, r.echo):
			t.Errorf("%s: input %s not kept", r.pattern, r.echo)
		case r.field != "" && !strings.Contains(body, `<p class="err" id="`+r.field+`-error">`):
			t.Errorf("%s: no error next to %s", r.pattern, r.field)
		case r.aria && !strings.Contains(body, r.field+`-error" aria-invalid="true"`):
			t.Errorf("%s: %s not marked invalid", r.pattern, r.field)
		}
	}
}

func mustWorkspace(t *testing.T, store *storage.Store, name string) storage.Workspace {
	t.Helper()
	ws, err := store.GetWorkspace(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	return ws
}

// Two operators on different settings pages: each saves only its own fields, so neither undoes the
// other's change, whatever the other page showed when it was loaded.
func TestSettingsPagesSaveOwnFields(t *testing.T) {
	store := testStore(t)
	h := newServer(t, "../../catalog", false, store)
	name := "set-own-" + strings.ToLower(newID()[:8])
	if rec := post(h, "/workspaces/new", url.Values{fieldWorkspace: {name}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	general, health := get(h, "/workspaces/"+name+"/settings").Body.String(), get(h, "/workspaces/"+name+"/settings/health").Body.String()
	if strings.Contains(general, `name="drift_detection"`) || strings.Contains(health, `name="auto_apply"`) || strings.Contains(health, `name="page"`) {
		t.Fatal("a settings page posts another page's fields")
	}
	// all three loaded with everything off, saved in any order (map order)
	for path, form := range map[string]url.Values{
		"/settings":              {"name": {name}, "execution_mode": {"remote"}, "auto_apply": {"on"}},
		"/settings/health":       {"drift_detection": {"on"}},
		"/settings/remote-state": {"remote_state": {"all"}},
	} {
		if rec := post(h, "/workspaces/"+name+path, form); rec.Code != http.StatusSeeOther {
			t.Fatalf("%s: %d %s", path, rec.Code, rec.Body.String())
		}
	}
	if ws := mustWorkspace(t, store, name); !ws.AutoApply || !ws.DriftDetection || !ws.GlobalRemoteState {
		t.Fatalf("a save undid another page's: %+v", ws)
	}
}
