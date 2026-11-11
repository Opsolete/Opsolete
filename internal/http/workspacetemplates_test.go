package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/opsolete/opsolete/internal/app"
	"github.com/opsolete/opsolete/internal/runnerapi"
	"github.com/opsolete/opsolete/internal/storage"
)

// TestWorkspaceTemplates: any number of templates can be added to a workspace, each a module the
// runner gets with its inputs; changing or removing one plans the change.
func TestWorkspaceTemplates(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	ws := "wt-" + strings.ToLower(newID()[:8])
	h := newServer(t, "../../catalog", false, store)
	runnerTok := testRunnerToken
	claim := func() runnerapi.Job {
		t.Helper()
		req := httptest.NewRequest("POST", runnerapi.Prefix+"/jobs/claim", strings.NewReader(`{"runner":"r1"}`))
		req.Header.Set("Authorization", "Bearer "+runnerTok)
		rec := do(h, req)
		var j runnerapi.Job
		if json.Unmarshal(rec.Body.Bytes(), &j); rec.Code != 200 {
			t.Fatalf("claim: %d %s", rec.Code, rec.Body.String())
		}
		store.Pool.Exec(context.Background(), `UPDATE runs SET status = 'applied', lease_id = NULL, lease_until = NULL WHERE id = $1`, j.RunID) // free the workspace
		return j
	}
	base := "/workspaces/" + ws + "/templates/"

	if rec := post(h, "/workspaces/new", url.Values{fieldWorkspace: {ws}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	if page := get(h, base+"new?template=greeting").Body.String(); strings.Contains(page, `name="ops-module"`) { // the name comes from the key input only
		t.Fatalf("add form: %s", page)
	}
	if rec := get(h, base+"new?template=nope"); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown template: %d", rec.Code)
	}
	// named returns the name of the added template whose key is key: key_<hex>.
	named := func(key string) string {
		t.Helper()
		w, _ := store.GetWorkspace(ctx, ws)
		added, _ := store.WorkspaceTemplates(ctx, w.ID)
		for _, a := range added {
			if regexp.MustCompile(`^` + key + `_[0-9a-f]{4}$`).MatchString(a.Name) {
				return a.Name
			}
		}
		t.Fatalf("no template named %s_<hex>: %+v", key, added)
		return ""
	}
	add := url.Values{"ops-module": {"ignored"}, "greeting": {"hi"}}
	for _, name := range []string{"one", "two"} {
		add.Set("name", name) // the key input
		if rec := post(h, base+"new?template=greeting", add); rec.Code != http.StatusSeeOther {
			t.Fatalf("add %s: %d %s", name, rec.Code, rec.Body.String())
		}
		claim()
	}
	one, two := named("one"), named("two")
	// a failed add shows the add form again (form.html reads .Module only when configuring)
	if rec := post(h, base+"new?template=greeting", url.Values{"greeting": {"hi"}, "name": {"!!"}}); rec.Code != http.StatusUnprocessableEntity ||
		!strings.Contains(rec.Body.String(), "<h1>Add Greeting (example) to "+ws+"</h1>") || strings.Contains(rec.Body.String(), "<h1>Configure") {
		t.Fatalf("failed add: %d %s", rec.Code, rec.Body.String())
	}

	// Both reach the runner as modules, with their inputs; there is no root module.
	if rec := post(h, "/workspaces/"+ws+"/runs", url.Values{"action": {"apply"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("new run: %d %s", rec.Code, rec.Body.String())
	}
	j := claim()
	if j.ModuleSource != "" || len(j.Modules) != 2 || j.Modules[0].Name != one || j.Modules[1].Inputs["name"] != "two" ||
		!strings.HasSuffix(j.Modules[0].Source, "/examples/greeting/terraform?ref=1.1.0") {
		t.Fatalf("job: %+v", j)
	}

	// Configure one.
	if rec := post(h, base+one, url.Values{"greeting": {"hey"}, "name": {"x"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("configure: %d %s", rec.Code, rec.Body.String())
	}
	if j := claim(); len(j.Modules) != 2 || j.Modules[0].Inputs["greeting"] != "hey" {
		t.Fatalf("configured job: %+v", j)
	}
	// Deployed: a destroy run limited to its module, which keeps it (and its providers) in the
	// configuration until the destroy is applied.
	var vid int64
	if err := store.Pool.QueryRow(context.Background(), `INSERT INTO state_versions (workspace_id, serial, lineage, blob_key, md5, size, indexed)
		SELECT id, 1, 'l', 'wt/' || id || '.tfstate', 'm', 2, true FROM workspaces WHERE name = $1 RETURNING id`, ws).Scan(&vid); err != nil {
		t.Fatal(err)
	}
	store.Pool.Exec(context.Background(), `INSERT INTO state_version_resources (state_version_id, address, type, name, provider, module) VALUES ($1, $2, 'a', 'b', 'p', $3)`, vid, "module."+one+".a.b", "module."+one)
	// each template shows whether it has resources in the state, e.g. after a failed apply
	if page := get(h, "/workspaces/"+ws+"?show=templates").Body.String(); !strings.Contains(page, "<code>"+one+"</code><br><small class=\"muted\" title=\"resources of this template in the current state\">1 resource</small>") ||
		!strings.Contains(page, "<code>"+two+"</code><br><small class=\"muted\" title=\"resources of this template in the current state\">⚠ no resources in state</small>") {
		t.Fatalf("resources per template: %s", page)
	}
	destroy := func(name string, left int) {
		t.Helper()
		if rec := post(h, base+name+"/remove", nil); rec.Code != http.StatusSeeOther || !strings.Contains(rec.Header().Get("Location"), "/runs/") {
			t.Fatalf("remove %s: %d %q", name, rec.Code, rec.Header().Get("Location"))
		}
		dj, ok, err := store.ClaimJob(ctx, "r1", time.Minute)
		if err != nil || !ok || dj.Action != "destroy" || len(dj.Modules) != left+1 || len(dj.Target) != 1 || dj.Target[0] != "module."+name {
			t.Fatalf("destroy job: %+v %v %v", dj, ok, err)
		}
		if err := store.AckJob(ctx, dj.RunID, dj.LeaseID, time.Minute); err != nil {
			t.Fatal(err)
		}
		if _, err := store.FinishJob(ctx, dj.RunID, dj.LeaseID, "succeeded", "", nil, nil, nil, nil); err != nil { // nothing to destroy
			t.Fatal(err)
		}
		if added, _ := store.WorkspaceTemplates(ctx, dj.WorkspaceID); len(added) != left {
			t.Fatalf("templates after removing %s: %+v", name, added)
		}
	}
	destroy(two, 1)
	if rec := post(h, base+two+"/remove", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("removing twice: %d", rec.Code)
	}
	if page := get(h, "/workspaces/"+ws+"?show=templates").Body.String(); !strings.Contains(page, base+one) || strings.Contains(page, base+two) ||
		!strings.Contains(page, ">Greeting (example)</a>") || !strings.Contains(page, "<summary>2 inputs</summary>") {
		t.Fatalf("workspace page: %s", page)
	}
	destroy(one, 0) // the last one too

	// Nothing deployed: it goes right away, without a run.
	store.Pool.Exec(context.Background(), `DELETE FROM state_version_resources WHERE state_version_id = $1`, vid)
	add.Set("name", "three")
	if rec := post(h, base+"new?template=greeting", add); rec.Code != http.StatusSeeOther {
		t.Fatalf("add three: %d %s", rec.Code, rec.Body.String())
	}
	claim()
	if rec := post(h, base+named("three")+"/remove", nil); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/workspaces/"+ws {
		t.Fatalf("removing the last template of an empty workspace: %d %q", rec.Code, rec.Header().Get("Location"))
	}

	// The same key twice: each its own random hex suffix.
	add.Set("name", "Big Room")
	for range 2 {
		if rec := post(h, base+"new?template=greeting", add); rec.Code != http.StatusSeeOther {
			t.Fatalf("add: %d %s", rec.Code, rec.Body.String())
		}
		claim()
	}
	w, _ := store.GetWorkspace(ctx, ws)
	added, _ := store.WorkspaceTemplates(ctx, w.ID)
	names := []string{}
	for _, a := range added {
		names = append(names, a.Name)
	}
	re := regexp.MustCompile(`^big_room_[0-9a-f]{4}$`)
	if len(names) != 2 || names[0] == names[1] || !re.MatchString(names[0]) || !re.MatchString(names[1]) {
		t.Fatalf("added: %v", names)
	}
}

// TestDeleteWorkspace: a workspace managing resources is only deleted, with its state files, when forced.
func TestDeleteWorkspace(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	ws := "del-" + strings.ToLower(newID()[:8])
	app := newApp(t, "../../catalog", store, devAuth(t, store, "admin@example.com", true), Features{Deployments: true})
	h, url0 := app.Handler(), "/workspaces/"+ws
	if rec := post(h, "/workspaces/new", url.Values{fieldWorkspace: {ws}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	w, err := store.GetWorkspace(ctx, ws)
	if err != nil {
		t.Fatal(err)
	}
	key := "test/" + w.ID.String() + ".tfstate"
	if err := app.App.Blobs.WriteAll(ctx, key, []byte("{}"), nil); err != nil {
		t.Fatal(err)
	}
	var vid int64
	if err := store.Pool.QueryRow(context.Background(), `INSERT INTO state_versions (workspace_id, serial, lineage, blob_key, md5, size, indexed)
		VALUES ($1, 1, 'l', $2, 'm', 2, true) RETURNING id`, w.ID, key).Scan(&vid); err != nil {
		t.Fatal(err)
	}
	store.Pool.Exec(context.Background(), `INSERT INTO state_version_resources (state_version_id, address, type, name, provider) VALUES ($1, 'a.b', 'a', 'b', 'p')`, vid)
	if rec := post(h, url0+"/delete", nil); rec.Code != http.StatusConflict {
		t.Fatalf("delete with resources: %d", rec.Code)
	}
	// force deletes it anyway, e.g. when the state files are lost
	if rec := post(h, url0+"/delete", url.Values{"force": {"1"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
	}
	if ok, _ := app.App.Blobs.Exists(ctx, key); ok {
		t.Fatal("state file left behind")
	}
	if rec := get(h, url0); rec.Code != http.StatusNotFound {
		t.Fatalf("deleted workspace: %d", rec.Code)
	}
}

func TestGroupResources(t *testing.T) {
	r := func(addr, mod string) storage.Resource {
		return storage.Resource{Resource: runnerapi.Resource{Address: addr, Module: mod, Provider: "registry.terraform.io/hashicorp/azurerm"}}
	}
	res := []storage.Resource{r("azurerm_resource_group.rg", ""), r("module.vm.azurerm_nic.self", "module.vm"),
		r(`module.app["a"].module.net.azurerm_subnet.s`, `module.app["a"].module.net`), r("module.vm.azurerm_vm.self[0]", "module.vm")}
	added := []storage.WorkspaceTemplate{{Name: "vm", TemplateID: "vm-id", ModuleRef: "3.0.0"}, {Name: "app", TemplateID: "gone", ModuleRef: "2.0.0"}}
	g := groupResources(res, added, map[string]string{"vm-id": "Virtual machine"})
	if len(g) != 3 || g[0].Module != `module.app["a"]` || g[0].Template != "gone" || g[0].Rows[0].Short != "module.net.azurerm_subnet.s" ||
		g[1].Module != "module.vm" || g[1].Template != "Virtual machine" || g[1].Ref != "3.0.0" || len(g[1].Rows) != 2 || g[1].Rows[1].Short != "azurerm_vm.self[0]" ||
		g[2].Module != "" || g[2].Template != "" || g[2].Rows[0].Short != "azurerm_resource_group.rg" || g[2].Rows[0].Provider != "azurerm" {
		t.Fatalf("groups: %+v", g)
	}
}

// TestSensitiveInputs: a sensitive input is sealed in the database, never shown or audit-logged,
// kept when left empty, and reaches the runner in clear.
func TestSensitiveInputs(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	id := strings.ToLower(newID()[:8])
	ws, root := "sens-"+id, "sensroot-"+id
	dir := t.TempDir()
	tpl, _ := os.ReadFile("../../catalog/greeting.json")
	tpl = []byte(strings.Replace(string(tpl), `"required": true, "pattern": "^[A-Za-z0-9 ]{1,40}$", "default": "world", "key": true`, `"required": true, "sensitive": true`, 1))
	os.WriteFile(filepath.Join(dir, "greeting.json"), tpl, 0o644)
	h := newServer(t, dir, false, store)
	claim := func() runnerapi.Job {
		t.Helper()
		req := httptest.NewRequest("POST", runnerapi.Prefix+"/jobs/claim", strings.NewReader(`{"runner":"r1"}`))
		req.Header.Set("Authorization", "Bearer "+testRunnerToken)
		rec := do(h, req)
		var j runnerapi.Job
		if json.Unmarshal(rec.Body.Bytes(), &j); rec.Code != 200 {
			t.Fatalf("claim: %d %s", rec.Code, rec.Body.String())
		}
		store.Pool.Exec(context.Background(), `UPDATE runs SET status = 'applied', lease_id = NULL, lease_until = NULL WHERE id = $1`, j.RunID)
		return j
	}
	leaks := func(what, s string) {
		t.Helper()
		if strings.Contains(s, "hunter2") {
			t.Fatalf("%s shows the sensitive value: %s", what, s)
		}
	}
	base := "/workspaces/" + ws + "/templates/"
	if rec := post(h, "/workspaces/new", url.Values{fieldWorkspace: {ws}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	if page := get(h, base+"new?template=greeting").Body.String(); !strings.Contains(page, `name="secret.name" type="password"`) {
		t.Fatalf("add form: %s", page)
	}
	if rec := post(h, base+"new?template=greeting", url.Values{"greeting": {"hi"}, "secret.name": {"hunter2"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("add: %d %s", rec.Code, rec.Body.String())
	}
	if j := claim(); len(j.Modules) != 1 || j.Modules[0].Inputs["name"] != "hunter2" || !slices.Contains(j.Secrets, "hunter2") {
		t.Fatalf("runner gets the value, and in Secrets to mask it: %+v %v", j.Modules, j.Secrets)
	}
	var stored, audit string
	store.Pool.QueryRow(context.Background(), `SELECT t.inputs::text FROM workspace_templates t JOIN workspaces w ON w.id = t.workspace_id WHERE w.name = $1`, ws).Scan(&stored)
	store.Pool.QueryRow(context.Background(), `SELECT detail::text FROM audit_log WHERE action = $1 ORDER BY id DESC LIMIT 1`, "POST "+base+"new").Scan(&audit)
	if leaks("database", stored); !strings.Contains(stored, `"sealed"`) {
		t.Fatalf("stored: %s", stored)
	}
	if leaks("audit log", audit); !strings.Contains(audit, `"secret.name":"(redacted)"`) {
		t.Fatalf("audit: %s", audit)
	}
	page := get(h, "/workspaces/"+ws+"?show=templates").Body.String()
	if leaks("workspace page", page); !strings.Contains(page, "(sensitive)") {
		t.Fatalf("workspace page: %s", page)
	}
	// Reconfigure: the form is empty, and leaving it empty keeps the value even though it's required.
	var mod string // no key input: the template's name, with a hex suffix
	store.Pool.QueryRow(context.Background(), `SELECT t.name FROM workspace_templates t JOIN workspaces w ON w.id = t.workspace_id WHERE w.name = $1`, ws).Scan(&mod)
	if !regexp.MustCompile(`^greeting_example_[0-9a-f]{4}$`).MatchString(mod) {
		t.Fatalf("module name: %q", mod)
	}
	page = get(h, base+mod).Body.String()
	if leaks("configure form", page); !strings.Contains(page, "A value is set") {
		t.Fatalf("configure form: %s", page)
	}
	if rec := post(h, base+mod, url.Values{"greeting": {"hey"}, "secret.name": {""}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("configure, kept: %d %s", rec.Code, rec.Body.String())
	}
	if j := claim(); j.Modules[0].Inputs["name"] != "hunter2" || j.Modules[0].Inputs["greeting"] != "hey" {
		t.Fatalf("after keeping: %+v", j.Modules)
	}
	// A sealed value moved to another module doesn't open: it's bound to its workspace and module.
	store.Pool.Exec(context.Background(), `UPDATE workspace_templates SET name = 'two' WHERE name = $2 AND workspace_id = (SELECT id FROM workspaces WHERE name = $1)`, ws, mod)
	store.Pool.Exec(context.Background(), `UPDATE runs SET status = 'plan_queued' WHERE id = (SELECT r.id FROM runs r JOIN workspaces w ON w.id = r.workspace_id WHERE w.name = $1 ORDER BY r.created_at DESC LIMIT 1)`, ws)
	req := httptest.NewRequest("POST", runnerapi.Prefix+"/jobs/claim", strings.NewReader(`{"runner":"r1"}`))
	req.Header.Set("Authorization", "Bearer "+testRunnerToken)
	if rec := do(h, req); rec.Code != http.StatusNoContent {
		t.Fatalf("a job with a moved sealed value was handed out: %d %s", rec.Code, rec.Body.String())
	}
	var status string
	store.Pool.QueryRow(context.Background(), `SELECT r.status FROM runs r JOIN workspaces w ON w.id = r.workspace_id WHERE w.name = $1 ORDER BY r.created_at DESC LIMIT 1`, ws).Scan(&status)
	if status != "errored" {
		t.Fatalf("moved sealed value: run %s", status)
	}

	// A workspace created from the template gets it added, its inputs sealed to the new workspace too.
	if rec := post(h, "/templates/greeting", url.Values{"ops-workspace": {root}, "greeting": {"hi"}, "secret.name": {"hunter2"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("create from template: %d %s", rec.Code, rec.Body.String())
	}
	if j := claim(); j.ModuleSource != "" || len(j.Modules) != 1 || j.Modules[0].Inputs["name"] != "hunter2" || j.Vars["name"] != nil {
		t.Fatalf("created workspace's job: %+v", j)
	}
	w, _ := store.GetWorkspace(ctx, root)
	if added, _ := store.WorkspaceTemplates(ctx, w.ID); len(added) != 1 || !app.IsSealed(added[0].Inputs["name"]) {
		t.Fatalf("created workspace's template: %+v", added)
	}
}
