package httpserver

import (
	"context"
	"encoding/json"
	"maps"
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

	"github.com/hashicorp/terraform-config-inspect/tfconfig"

	"github.com/opsolete/opsolete/internal/registry"
	"github.com/opsolete/opsolete/internal/runnerapi"
	"github.com/opsolete/opsolete/internal/storage"
	"github.com/opsolete/opsolete/internal/templates"
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
		store.DB.Exec(`UPDATE runs SET status = 'applied' WHERE id = $1`, j.RunID) // free the workspace
		return j
	}
	base := "/workspaces/" + ws + "/templates/"

	if rec := post(h, "/workspaces/new", url.Values{"name": {ws}}); rec.Code != http.StatusSeeOther {
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
	if err := store.DB.QueryRow(`INSERT INTO state_versions (workspace_id, serial, lineage, blob_key, md5, size, indexed)
		SELECT id, 1, 'l', 'wt/' || id || '.tfstate', 'm', 2, true FROM workspaces WHERE name = $1 RETURNING id`, ws).Scan(&vid); err != nil {
		t.Fatal(err)
	}
	store.DB.Exec(`INSERT INTO state_version_resources (state_version_id, address, type, name, provider, module) VALUES ($1, $2, 'a', 'b', 'p', $3)`, vid, "module."+one+".a.b", "module."+one)
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
		if _, err := store.FinishJob(ctx, dj.RunID, dj.LeaseID, "succeeded", "", nil, nil, nil); err != nil { // nothing to destroy
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
	store.DB.Exec(`DELETE FROM state_version_resources WHERE state_version_id = $1`, vid)
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
	t.Cleanup(func() { store.DB.Exec(`DELETE FROM workspaces WHERE name = $1`, ws) })
	app := newApp(t, "../../catalog", store, devAuth(t, store, "admin@example.com", true), Features{Deployments: true})
	h, url0 := app.Handler(), "/workspaces/"+ws
	if rec := post(h, "/workspaces/new", url.Values{"name": {ws}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	w, err := store.GetWorkspace(ctx, ws)
	if err != nil {
		t.Fatal(err)
	}
	key := "test/" + w.ID + ".tfstate"
	if err := app.StateStore.WriteAll(ctx, key, []byte("{}"), nil); err != nil {
		t.Fatal(err)
	}
	var vid int64
	if err := store.DB.QueryRow(`INSERT INTO state_versions (workspace_id, serial, lineage, blob_key, md5, size, indexed)
		VALUES ($1, 1, 'l', $2, 'm', 2, true) RETURNING id`, w.ID, key).Scan(&vid); err != nil {
		t.Fatal(err)
	}
	store.DB.Exec(`INSERT INTO state_version_resources (state_version_id, address, type, name, provider) VALUES ($1, 'a.b', 'a', 'b', 'p')`, vid)
	if rec := post(h, url0+"/delete", nil); rec.Code != http.StatusConflict {
		t.Fatalf("delete with resources: %d", rec.Code)
	}
	// force deletes it anyway, e.g. when the state files are lost
	if rec := post(h, url0+"/delete", url.Values{"force": {"1"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
	}
	if ok, _ := app.StateStore.Exists(ctx, key); ok {
		t.Fatal("state file left behind")
	}
	if rec := get(h, url0); rec.Code != http.StatusNotFound {
		t.Fatalf("deleted workspace: %d", rec.Code)
	}
}

// moduleNameRe is a Terraform module call name; "new" is the add route.
var moduleNameRe = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,62}$`)

func TestModuleName(t *testing.T) {
	for name, want := range map[string]string{"Greeting (example)": "greeting_example", "Virtual machine – Windows": "virtual_machine_windows",
		"2 VMs": "t_2_vms", "": "t", "New": "template", strings.Repeat("a ", 40): strings.Repeat("a_", 31) + "a"} {
		if got := moduleName(name); got != want || !moduleNameRe.MatchString(got) {
			t.Errorf("moduleName(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestDefaultModule(t *testing.T) {
	keyed := templates.Template{Name: "Virtual machine", Inputs: []templates.Input{{Name: "vm", Type: "string", Key: true}}}
	long := strings.Repeat("a", 63)
	taken := []storage.WorkspaceTemplate{{Name: "web_prod_01"}, {Name: "virtual_machine"}, {Name: long}}
	for _, c := range []struct {
		t    templates.Template
		raw  map[string]string
		want string // a regexp
	}{
		{keyed, map[string]string{"vm": "Web-Prod 01"}, `web_prod_01_[0-9a-f]{4}`},
		{keyed, map[string]string{"vm": "db"}, `db_[0-9a-f]{4}`},             // always a suffix
		{keyed, map[string]string{"vm": " "}, `virtual_machine_[0-9a-f]{4}`}, // empty key: the template's name
		{templates.Template{Name: "Virtual machine"}, nil, `virtual_machine_[0-9a-f]{4}`},
		{keyed, map[string]string{"vm": long}, `a{58}_[0-9a-f]{4}`},
	} {
		if got := defaultModule(c.t, c.raw, taken); !regexp.MustCompile("^"+c.want+"$").MatchString(got) || !moduleNameRe.MatchString(got) {
			t.Errorf("defaultModule(%v) = %q, want %s", c.raw, got, c.want)
		}
	}
}

func TestGroupResources(t *testing.T) {
	r := func(addr, mod string) storage.Resource {
		return storage.Resource{Resource: runnerapi.Resource{Address: addr, Module: mod, Provider: "registry.terraform.io/hashicorp/azurerm"}}
	}
	res := []storage.Resource{r("azurerm_resource_group.rg", ""), r("module.vm.azurerm_nic.self", "module.vm"),
		r(`module.app["a"].module.net.azurerm_subnet.s`, `module.app["a"].module.net`), r("module.vm.azurerm_vm.self[0]", "module.vm")}
	ws := storage.Workspace{TemplateID: "root-id", ModuleRef: "1.0.0"}
	added := []storage.WorkspaceTemplate{{Name: "vm", TemplateID: "vm-id", ModuleRef: "3.0.0"}, {Name: "app", TemplateID: "gone", ModuleRef: "2.0.0"}}
	g := groupResources(res, ws, added, map[string]string{"root-id": "Base", "vm-id": "Virtual machine"})
	if len(g) != 3 || g[0].Module != `module.app["a"]` || g[0].Template != "gone" || g[0].Rows[0].Short != "module.net.azurerm_subnet.s" ||
		g[1].Module != "module.vm" || g[1].Template != "Virtual machine" || g[1].Ref != "3.0.0" || len(g[1].Rows) != 2 || g[1].Rows[1].Short != "azurerm_vm.self[0]" ||
		g[2].Module != "" || g[2].Template != "Base" || g[2].Rows[0].Short != "azurerm_resource_group.rg" || g[2].Rows[0].Provider != "azurerm" {
		t.Fatalf("groups: %+v", g)
	}
}

// TestSensitiveInputs: a sensitive input is sealed in the database, never shown or audit-logged,
// kept when left empty, and reaches the runner in clear.
func TestSensitiveInputs(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	claimWait = 100 * time.Millisecond
	store.DB.Exec(`UPDATE runs SET status = 'errored' WHERE status IN ('plan_queued', 'planning', 'needs_confirmation', 'apply_queued', 'applying')`)
	id := strings.ToLower(newID()[:8])
	ws, root := "sens-"+id, "sensroot-"+id
	t.Cleanup(func() { store.DB.Exec(`DELETE FROM workspaces WHERE name IN ($1, $2)`, ws, root) })
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
		store.DB.Exec(`UPDATE runs SET status = 'applied' WHERE id = $1`, j.RunID)
		return j
	}
	leaks := func(what, s string) {
		t.Helper()
		if strings.Contains(s, "hunter2") {
			t.Fatalf("%s shows the sensitive value: %s", what, s)
		}
	}
	base := "/workspaces/" + ws + "/templates/"
	if rec := post(h, "/workspaces/new", url.Values{"name": {ws}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	if page := get(h, base+"new?template=greeting").Body.String(); !strings.Contains(page, `name="secret.name" type="password"`) {
		t.Fatalf("add form: %s", page)
	}
	if rec := post(h, base+"new?template=greeting", url.Values{"greeting": {"hi"}, "secret.name": {"hunter2"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("add: %d %s", rec.Code, rec.Body.String())
	}
	if j := claim(); len(j.Modules) != 1 || j.Modules[0].Inputs["name"] != "hunter2" {
		t.Fatalf("runner gets the value: %+v", j.Modules)
	}
	var stored, audit string
	store.DB.QueryRow(`SELECT t.inputs::text FROM workspace_templates t JOIN workspaces w ON w.id = t.workspace_id WHERE w.name = $1`, ws).Scan(&stored)
	store.DB.QueryRow(`SELECT detail::text FROM audit_log WHERE action = $1 ORDER BY id DESC LIMIT 1`, "POST "+base+"new").Scan(&audit)
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
	store.DB.QueryRow(`SELECT t.name FROM workspace_templates t JOIN workspaces w ON w.id = t.workspace_id WHERE w.name = $1`, ws).Scan(&mod)
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
	store.DB.Exec(`UPDATE workspace_templates SET name = 'two' WHERE name = $2 AND workspace_id = (SELECT id FROM workspaces WHERE name = $1)`, ws, mod)
	store.DB.Exec(`UPDATE runs SET status = 'plan_queued' WHERE id = (SELECT r.id FROM runs r JOIN workspaces w ON w.id = r.workspace_id WHERE w.name = $1 ORDER BY r.created_at DESC LIMIT 1)`, ws)
	req := httptest.NewRequest("POST", runnerapi.Prefix+"/jobs/claim", strings.NewReader(`{"runner":"r1"}`))
	req.Header.Set("Authorization", "Bearer "+testRunnerToken)
	if rec := do(h, req); rec.Code != http.StatusNoContent {
		t.Fatalf("a job with a moved sealed value was handed out: %d %s", rec.Code, rec.Body.String())
	}
	var status string
	store.DB.QueryRow(`SELECT r.status FROM runs r JOIN workspaces w ON w.id = r.workspace_id WHERE w.name = $1 ORDER BY r.created_at DESC LIMIT 1`, ws).Scan(&status)
	if status != "errored" {
		t.Fatalf("moved sealed value: run %s", status)
	}

	// The root inputs of a workspace created from the template: sealed to the new workspace too.
	if rec := post(h, "/templates/greeting", url.Values{"ops-workspace": {root}, "greeting": {"hi"}, "secret.name": {"hunter2"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("create from template: %d %s", rec.Code, rec.Body.String())
	}
	if j := claim(); j.Vars["name"] != "hunter2" {
		t.Fatalf("root vars: %+v", j.Vars)
	}
	if w, _ := store.GetWorkspace(ctx, root); !isSealed(w.Inputs["name"]) {
		t.Fatalf("root inputs: %+v", w.Inputs)
	}
}

// TestRemoveRootTemplate: the template a workspace was created from can be removed too. Next to
// added templates, the destroy targets only the root's resources; alone, it destroys everything.
func TestRemoveRootTemplate(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	claimWait = 100 * time.Millisecond
	store.DB.Exec(`UPDATE runs SET status = 'errored' WHERE status IN ('plan_queued', 'planning', 'needs_confirmation', 'apply_queued', 'applying')`)
	id := strings.ToLower(newID()[:8])
	ws, solo, empty := "root-"+id, "rootsolo-"+id, "rootnone-"+id
	t.Cleanup(func() { store.DB.Exec(`DELETE FROM workspaces WHERE name IN ($1, $2, $3)`, ws, solo, empty) })
	h := newServer(t, "../../catalog", false, store)
	free := func() {
		store.DB.Exec(`UPDATE runs SET status = 'applied' WHERE status IN ('plan_queued', 'needs_confirmation')`)
	}
	create := func(name string) storage.Workspace {
		t.Helper()
		if rec := post(h, "/templates/greeting", url.Values{"ops-workspace": {name}, "greeting": {"hi"}, "name": {"root"}}); rec.Code != http.StatusSeeOther {
			t.Fatalf("create %s: %d %s", name, rec.Code, rec.Body.String())
		}
		free()
		w, err := store.GetWorkspace(ctx, name)
		if err != nil || w.TemplateID != "greeting" {
			t.Fatalf("created %s: %+v %v", name, w, err)
		}
		return w
	}
	state := func(w storage.Workspace, indexed bool, addrs ...string) {
		t.Helper()
		var vid int64
		if err := store.DB.QueryRow(`INSERT INTO state_versions (workspace_id, serial, lineage, blob_key, md5, size, indexed)
			VALUES ($1, 1, 'l', $3, 'm', 2, $2) RETURNING id`, w.ID, indexed, "rt/"+w.ID+newID()[:4]+".tfstate").Scan(&vid); err != nil {
			t.Fatal(err)
		}
		for _, a := range addrs {
			mod := ""
			if strings.HasPrefix(a, "module.") {
				mod = strings.Join(strings.SplitN(a, ".", 3)[:2], ".")
			}
			store.DB.Exec(`INSERT INTO state_version_resources (state_version_id, address, type, name, provider, module) VALUES ($1, $2, 'a', 'b', 'p', $3)`, vid, a, mod)
		}
	}
	remove := func(name string) *httptest.ResponseRecorder { return post(h, "/workspaces/"+name+"/root/remove", nil) }
	finish := func(want []string) storage.Job {
		t.Helper()
		j, ok, err := store.ClaimJob(ctx, "r1", time.Minute)
		if err != nil || !ok || j.Action != "destroy" || !slices.Equal(j.Target, want) || j.ModuleSource == "" { // the root stays until it's destroyed
			t.Fatalf("destroy job: %+v %v %v, want target %q", j, ok, err, want)
		}
		if _, err := store.FinishJob(ctx, j.RunID, j.LeaseID, "succeeded", "", nil, nil, nil); err != nil {
			t.Fatal(err)
		}
		return j
	}

	// Next to an added template: the destroy targets the root's resources only, and the added one stays.
	w := create(ws)
	if rec := post(h, "/workspaces/"+ws+"/templates/new?template=greeting", url.Values{"greeting": {"hi"}, "name": {"one"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("add: %d %s", rec.Code, rec.Body.String())
	}
	free()
	added, _ := store.WorkspaceTemplates(ctx, w.ID)
	if page := get(h, "/workspaces/"+ws+"?show=templates").Body.String(); !strings.Contains(page, `action="/workspaces/`+ws+`/root/remove"`) {
		t.Fatalf("no Remove on the root row: %s", page)
	}
	state(w, false, "terraform_data.greeting", "module."+added[0].Name+".terraform_data.greeting")
	if rec := remove(ws); rec.Code != http.StatusConflict { // unindexed: the root's resources aren't known
		t.Fatalf("remove with an unindexed state: %d", rec.Code)
	}
	state(w, true, "terraform_data.greeting", "module."+added[0].Name+".terraform_data.greeting")
	if rec := remove(ws); rec.Code != http.StatusSeeOther || !strings.Contains(rec.Header().Get("Location"), "/runs/") {
		t.Fatalf("remove: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if j := finish([]string{"terraform_data.greeting"}); len(j.Modules) != 1 {
		t.Fatalf("the added template is missing from the destroy: %+v", j.Modules)
	}
	if w, _ := store.GetWorkspace(ctx, ws); w.TemplateID != "" || w.ModuleSource != "" || len(w.Inputs) != 0 || w.Templates != 1 {
		t.Fatalf("after removing the root: %+v", w)
	}
	if rec := remove(ws); rec.Code != http.StatusNotFound {
		t.Fatalf("removing twice: %d", rec.Code)
	}

	// Alone: a destroy of everything.
	state(create(solo), true, "terraform_data.greeting")
	if rec := remove(solo); rec.Code != http.StatusSeeOther {
		t.Fatalf("remove solo: %d", rec.Code)
	}
	finish(nil)
	if w, _ := store.GetWorkspace(ctx, solo); w.TemplateID != "" {
		t.Fatalf("solo after the destroy: %+v", w)
	}

	// Nothing deployed: removed right away, without a run.
	create(empty)
	if rec := remove(empty); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/workspaces/"+empty {
		t.Fatalf("remove undeployed: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if w, _ := store.GetWorkspace(ctx, empty); w.TemplateID != "" {
		t.Fatalf("undeployed after removing: %+v", w)
	}
}

// TestWithWorkspaceVars: an added template gets the workspace's Terraform variables its registry
// module declares, under its own inputs; other sources and undeclared variables are left alone.
func TestWithWorkspaceVars(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	s := newApp(t, "../../catalog", store, nil, Features{Registry: true})
	ns := "wv" + strings.ToLower(newID()[:8])
	t.Cleanup(func() { store.DB.Exec(`DELETE FROM modules WHERE namespace = $1`, ns) })
	decl := []registry.Variable{{Variable: tfconfig.Variable{Name: "greeting"}}, {Variable: tfconfig.Variable{Name: "name"}}}
	if err := store.CreateModule(ctx, storage.Module{Namespace: ns, Name: "m", System: "null", Version: "1.0.0", Variables: decl},
		storage.ModuleDocs{}, []byte("x")); err != nil {
		t.Fatal(err)
	}
	src := testReg.PublicURL.Host + "/" + ns + "/m/null"
	vars := map[string]any{"greeting": "from-set", "name": "from-set", "region": "undeclared"}
	got, err := s.withWorkspaceVars(ctx, "", src, "1.0.0", map[string]any{"name": "own"}, vars)
	if want := map[string]any{"greeting": "from-set", "name": "own"}; err != nil || !maps.Equal(got, want) {
		t.Fatalf("registry module: %v %v", got, err)
	}
	// the greeting template asks both: unset (hidden or left empty), they keep the module's default
	if got, err := s.withWorkspaceVars(ctx, "greeting", src, "1.0.0", map[string]any{}, vars); err != nil || len(got) != 0 {
		t.Fatalf("inputs the template asks: %v %v", got, err)
	}
	if got, err := s.withWorkspaceVars(ctx, "", "git::https://example.com/m.git", "v1", nil, vars); err != nil || got != nil {
		t.Fatalf("external source: %v %v", got, err)
	}
}
