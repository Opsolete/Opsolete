package httpserver

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/opsolete/opsolete/internal/app"
	"github.com/opsolete/opsolete/internal/auth"
	"github.com/opsolete/opsolete/internal/runnerapi"
	"github.com/opsolete/opsolete/internal/storage"
	"github.com/opsolete/opsolete/internal/templates"
)

// Every page template executes with its model's zero value and with a small filled one, for an
// admin and for a user without a role: a field the template names that the model lacks (or a nil
// one it walks into) fails here, without a database. The DB tests render each page with real data.
func TestPageModels(t *testing.T) {
	s := New(&app.App{Log: testLog(t)}, nil, Features{Deployments: true, Registry: true, VCS: true, Editor: true})
	now := time.Now()
	tpl := templates.Template{ID: "vm", Name: "VM", Module: templates.Module{Source: "s", Ref: "v1"},
		Inputs: []templates.Input{{Name: "size", Type: "select", Options: []templates.Option{{Value: "s"}}}, {Name: "pw", Type: "string", Sensitive: true}}}
	conn := storage.VCSConnection{ID: uuid.New(), Name: "gh", Provider: "github"}
	ws := storage.Workspace{ID: uuid.New(), Name: "ws", ProjectName: "p", VCSConnectionID: conn.ID, VCSRepo: "o/r", VCSBranch: "main", Templates: 1,
		Latest: &storage.Run{Status: storage.Applied}, CreatedAt: now}
	run := storage.Run{ID: uuid.New(), Action: "apply", Status: storage.NeedsConfirmation, Replace: []string{"a.b"}, Plan: &runnerapi.Plan{Add: 1},
		CreatedBy: "a@b", CreatedAt: now, UpdatedAt: now}
	hist := []storage.StatusEntry{{Status: storage.Planning, CreatedAt: now}, {Status: storage.NeedsConfirmation, CreatedAt: now}}
	plan, apply := runPhases(run, hist)
	version := storage.StateVersion{ID: 2, Serial: 2, Indexed: true, CreatedAt: now}
	set := storage.VariableSet{Name: "set", Projects: []string{"p"}, CreatedAt: now}
	vars := varsBlock{Base: "/variable-sets/set", Vars: []app.Variable{{Key: "k"}}, CanEdit: true}
	conns := vcsForm{Conns: []storage.VCSConnection{conn}, Repos: []string{"o/r"}, Form: map[string]string{"type": "vcs", "vcs_connection": conn.ID.String()}}
	picks := workspacePicker("workspace", "Workspaces", []string{"ws"}, []storage.Workspace{ws, {Name: "other"}})
	header := workspacePage{W: ws, Lock: &storage.StateLock{ID: storage.CloudLockPrefix + "x", Who: "a@b", Info: "why"}, Runs: []storage.Run{run}, Active: &run, Current: &runItem{run, true},
		Resources: []storage.Resource{{Resource: runnerapi.Resource{Address: "a.b", Type: "a"}}}, Versions: []storage.StateVersion{version}, Outputs: map[string]any{"o": 1}, Assessment: &run}
	tab := func(name, sub string, set func(*workspacePage)) *workspacePage {
		p := header
		p.Page = workspaceNav("ws", name, sub, true)
		set(&p)
		return &p
	}
	overview := func(show string) *workspacePage {
		return tab("overview", "", func(p *workspacePage) {
			p.overviewTab = &overviewTab{Added: []storage.WorkspaceTemplate{{Name: "vm_1", TemplateID: "vm"}}, Updates: map[string]string{"vm_1": "v2"},
				TemplateNames: map[string]string{"vm": "VM"}, AddedInputs: map[string][]templates.StoredValue{"vm_1": tpl.Describe(nil)},
				ResourceGroups: groupResources(p.Resources, nil, nil), TemplateResources: map[string]int{"vm_1": 1}, LatestPlan: plan, LatestApply: apply, Show: show}
		})
	}
	pages := map[string][]interface{ base() *Page }{
		"index.html":      {&indexPage{}, &indexPage{Templates: []templates.Template{tpl}, Editor: true, CanCreate: true}},
		"workspaces.html": {&workspacesPage{}, &workspacesPage{Workspaces: []storage.Workspace{ws}, Projects: []storage.Project{{Name: "p"}}, Project: "p", CanCreate: true}},
		"workspace.html": {&workspacePage{}, overview(""), overview("outputs"), overview("templates"),
			tab("deploy", "", func(p *workspacePage) { p.deployTab = &deployTab{Catalog: []templates.Template{tpl}} }),
			tab("runs", "", func(p *workspacePage) {
				p.runsTab = &runsTab{Counts: map[string]int{"all": 1}, RunList: []runItem{{run, true}}}
			}),
			tab("states", "", func(p *workspacePage) { p.statesTab = &statesTab{} }),
			tab("states", "", func(p *workspacePage) {
				p.statesTab = &statesTab{State: &version, Prev: &version, Changes: []app.StateChange{{Action: "update", Address: "a.b"}}, RollbackChanges: []app.StateChange{}}
			}),
			tab("variables", "", func(p *workspacePage) {
				p.variablesTab = &variablesTab{Variables: vars, Sets: []setView{{set, vars}}}
			}),
			tab("settings", "", func(p *workspacePage) {
				p.settingsTab = &settingsTab{StateConsumers: picks, Projects: []storage.Project{{Name: "p"}}, StateAddress: "https://x/state"}
			}),
			tab("settings", "health", func(p *workspacePage) { p.settingsTab = &settingsTab{} }),
			tab("settings", "lock", func(p *workspacePage) { p.settingsTab = &settingsTab{} }),
			tab("settings", "lock", func(p *workspacePage) { p.Lock, p.settingsTab = nil, &settingsTab{} }),
			tab("settings", "access", func(p *workspacePage) {
				p.settingsTab = &settingsTab{Grants: []storage.Grant{{Group: "g", Level: "read"}}, ProjectGrants: []storage.Grant{{Group: "h", Level: "operate"}}, Groups: []string{"g"}}
			}),
			tab("settings", "deletion", func(p *workspacePage) { p.settingsTab = &settingsTab{} }),
		},
		"run.html": {&runPage{}, &runPage{Page: workspaceNav("ws", "runs", "", true), W: ws, R: run, History: hist, Plan: plan, Apply: apply}},
		"account.html": {&accountPage{}, &accountPage{Page: Page{Sub: "profile"}, Groups: []string{"g"}}, &accountPage{Page: Page{Sub: "tokens"}, Tokens: []storage.UserToken{{Name: "t"}}},
			&accountPage{Page: Page{Sub: "sessions"}, Sessions: []storage.UserSession{{UserAgent: "ua"}}}},
		"audit.html":     {&auditPage{}, &auditPage{Entries: []auditRow{{Summary: "x", StatusText: "ok"}}, Broken: 3, Access: true}},
		"authorize.html": {&authorizePage{}, &authorizePage{Email: "a@b", Days: 30}},
		"organization.html": {&organizationPage{}, &organizationPage{Name: "org", Currency: "EUR", Uploads: true, DevUploads: true, Saved: true,
			F: &formState{Name: "organization", Fields: map[string]string{"currency": "bad"}}}},
		"projects.html": {&projectsPage{}, &projectsPage{Projects: []storage.Project{{Name: "p", Default: true}}, Org: "org", F: &formState{Name: "project"}}},
		"project.html": {&projectPage{},
			&projectPage{Page: Page{InProject: "p", Tab: "overview"}, P: storage.Project{Name: "p"}, Workspaces: []storage.Workspace{ws}, CanCreate: true},
			&projectPage{Page: Page{InProject: "p", Tab: "workspaces"}, P: storage.Project{Name: "p"}, Workspaces: []storage.Workspace{ws}},
			&projectPage{Page: Page{InProject: "p", Tab: "settings"}, P: storage.Project{Name: "p", Workspaces: 2}},
			&projectPage{Page: Page{InProject: "p", Tab: "settings", Sub: "access"}, P: storage.Project{Name: "p"}, Grants: []storage.Grant{{Group: "g"}}, Groups: []string{"g"}},
			&projectPage{Page: Page{InProject: "p", Tab: "settings", Sub: "variable-sets"}, P: storage.Project{Name: "p"}, Sets: []storage.VariableSet{set}}},
		"variablesets.html": {&variableSetsPage{}, &variableSetsPage{Sets: []storage.VariableSet{set}, F: &formState{Name: "set", Input: map[string][]string{"project": {"p"}}}}},
		"variableset.html":  {&variableSetPage{}, &variableSetPage{Set: set, Variables: vars, Picker: picks, Projects: []pickerItem{{"p", "p", true}}}},
		"newworkspace.html": {&newWorkspacePage{},
			&newWorkspacePage{Templates: []templates.Template{tpl}, VCS: conns, Conn: &conn, Projects: []storage.Project{{Name: "p"}}, Providers: struct{}{}}},
		"workspacevcs.html": {&workspaceVCSPage{}, &workspaceVCSPage{Page: workspaceNav("ws", "vcs", "vcs", true), W: ws, VCS: conns, Conn: &conn, Health: "broken"},
			&workspaceVCSPage{Page: workspaceNav("ws", "vcs", "vcs", true), W: ws, VCS: conns, Edit: true}},
		"module.html": {&modulePage{}, &modulePage{Namespace: "n", Name: "m", System: "s", Version: "1.0.0", Editor: true, CanCreate: true,
			ModuleDocs: app.ModuleDocs{Versions: []string{"1.0.0"}, Templates: []templates.Template{tpl}, Linked: &storage.VCSModule{Repo: "o/r"}}}},
		"registry.html": {&registryPage{}, &registryPage{Modules: []latestModule{{Module: storage.Module{Name: "m"}}}, Templates: true, Upload: true, VCS: true, Admin: true,
			Linked: []storage.VCSModule{{Repo: "o/r"}}}},
		"publish.html": {&publishPage{}, &publishPage{VCS: true, Upload: true}, &publishPage{Type: "upload", Form: map[string]string{}},
			&publishPage{Type: "vcs", Step: 1, Conns: []storage.VCSConnection{conn}}, &publishPage{Type: "vcs", Step: 2, Conn: conn, Repos: []string{"o/r"}, Error: "x"},
			&publishPage{Type: "vcs", Step: 3, Conn: conn, Form: map[string]string{"repo": "o/r"}}},
		"vcs.html":     {&vcsPage{}, &vcsPage{Connections: []storage.VCSConnection{conn}, Error: "x"}},
		"vcsnew.html":  {&newConnection{}, &newConnection{Step: 1}, &newConnection{Step: 2, Form: map[string]string{}}, &newConnection{Step: 3, Conn: conn, Picker: picks, Edit: true}},
		"catalog.html": {&catalogPage{}, &catalogPage{Catalog: &storage.TemplateCatalog{Repo: "o/r"}, Conns: []storage.VCSConnection{conn}, Repos: []string{"o/r"}, Count: 1, Saved: true}},
		"rollout.html": {&rolloutPage{}, &rolloutPage{T: tpl, Rollout: &storage.Rollout{Status: "paused"}, Projects: []storage.Project{{Name: "p"}}}},
		"builder.html": {&builderPage{}, &builderPage{T: tpl, Rows: tpl.Inputs, Editing: true, Warning: "w", Modules: []storage.Module{{Name: "m"}}, Registry: true, PullRequest: "u"}},
		"form.html": {&formPage{}, &formPage{T: tpl, Editor: true, Values: map[string]string{"size": "s"}, Projects: []storage.Project{{Name: "p"}}},
			&formPage{Page: workspaceNav("ws", "templates", "", true), T: tpl, Configure: &ws, Module: "vm_1", Set: map[string]bool{"pw": true}}},
	}
	for _, f := range must(views.ReadDir("views")) {
		if name := f.Name(); name != "layout.html" && name != "variables.html" && pages[name] == nil { // the two hold partials only
			t.Errorf("%s: no page models in TestPageModels", name)
		}
	}
	for name, models := range pages {
		for i, m := range models {
			for _, u := range []auth.User{{Email: "admin@x", Role: "admin"}, {}} {
				w := httptest.NewRecorder()
				r := httptest.NewRequest("GET", "/", nil).WithContext(auth.WithUser(context.Background(), u))
				s.render(w, r, 200, name, m)
				if w.Code != 200 {
					t.Errorf("%s model %d, user %q: status %d", name, i, u.Email, w.Code)
				}
				strictHTML(t, fmt.Sprintf("%s model %d, user %q", name, i, u.Email), w.Body.String())
			}
		}
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

// The deploy form renders show_if fields hidden and disabled while their condition fails.
func TestRenderShowIf(t *testing.T) {
	tpl := templates.Template{ID: "vm", Name: "VM", Inputs: []templates.Input{
		{Name: "os", Type: "select", Options: []templates.Option{{Value: "windows"}, {Value: "linux"}}},
		{Name: "admin_password", Type: "string", Sensitive: true, ShowIf: &templates.ShowIf{Input: "os", Equals: []string{"windows"}}},
	}}
	s := New(&app.App{Log: testLog(t)}, nil, Features{})
	// "": no value yet counts as the first option, as the browser shows it
	for osv, hidden := range map[string]bool{"linux": true, "windows": false, "": false} {
		w := httptest.NewRecorder()
		s.render(w, httptest.NewRequest("GET", "/", nil), 200, "form.html", &formPage{T: tpl, Values: map[string]string{"os": osv}})
		body := w.Body.String()
		if !strings.Contains(body, `name="secret.admin_password"`) {
			t.Fatalf("%s: fields missing:\n%s", osv, body)
		}
		if got := strings.Count(body, " hidden disabled"); got != map[bool]int{true: 1, false: 0}[hidden] {
			t.Errorf("%s: %d hidden, want hidden=%v", osv, got, hidden)
		}
	}
}
