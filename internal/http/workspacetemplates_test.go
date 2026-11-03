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

	"github.com/opsolete/opsolete/internal/runnerapi"
	"github.com/opsolete/opsolete/internal/storage"
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
		store.DB.Exec(`UPDATE runs SET status = 'errored' WHERE id = $1`, j.RunID) // free the workspace
		return j
	}
	base := "/workspaces/" + ws + "/templates/"

	if rec := post(h, "/workspaces/new", url.Values{"name": {ws}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	if page := get(h, base+"new?template=greeting").Body.String(); !strings.Contains(page, `name="ops-module" value="greeting_example"`) { // from the name "Greeting (example)"
		t.Fatalf("add form: %s", page)
	}
	if rec := get(h, base+"new?template=nope"); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown template: %d", rec.Code)
	}
	add := url.Values{"ops-module": {"new"}, "greeting": {"hi"}, "name": {"a"}}
	if rec := post(h, base+"new?template=greeting", add); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf(`"new" is reserved: %d`, rec.Code)
	}
	for _, name := range []string{"one", "two"} {
		add.Set("ops-module", name)
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

	// Configure one.
	if rec := post(h, base+"one", url.Values{"greeting": {"hey"}, "name": {"x"}}); rec.Code != http.StatusSeeOther {
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
	store.DB.Exec(`INSERT INTO state_version_resources (state_version_id, address, type, name, provider) VALUES ($1, 'module.one.a.b', 'a', 'b', 'p')`, vid)
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
	destroy("two", 1)
	if rec := post(h, base+"two/remove", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("removing twice: %d", rec.Code)
	}
	if page := get(h, "/workspaces/"+ws+"?show=templates").Body.String(); !strings.Contains(page, base+"one") || strings.Contains(page, base+"two") ||
		!strings.Contains(page, ">Greeting (example)</a>") || !strings.Contains(page, "<summary>2 inputs</summary>") {
		t.Fatalf("workspace page: %s", page)
	}
	destroy("one", 0) // the last one too

	// Nothing deployed: it goes right away, without a run.
	store.DB.Exec(`DELETE FROM state_version_resources WHERE state_version_id = $1`, vid)
	add.Set("ops-module", "three")
	if rec := post(h, base+"new?template=greeting", add); rec.Code != http.StatusSeeOther {
		t.Fatalf("add three: %d %s", rec.Code, rec.Body.String())
	}
	claim()
	if rec := post(h, base+"three/remove", nil); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/workspaces/"+ws {
		t.Fatalf("removing the last template of an empty workspace: %d %q", rec.Code, rec.Header().Get("Location"))
	}
}

// TestDeleteWorkspace: a workspace can be deleted, with its state files, once it manages nothing.
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
	store.DB.Exec(`DELETE FROM state_version_resources WHERE state_version_id = $1`, vid)
	if rec := post(h, url0+"/delete", nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
	}
	if ok, _ := app.StateStore.Exists(ctx, key); ok {
		t.Fatal("state file left behind")
	}
	if rec := get(h, url0); rec.Code != http.StatusNotFound {
		t.Fatalf("deleted workspace: %d", rec.Code)
	}
}

func TestModuleName(t *testing.T) {
	for name, want := range map[string]string{"Greeting (example)": "greeting_example", "Virtual machine – Windows": "virtual_machine_windows",
		"2 VMs": "t_2_vms", "": "t", "New": "template", strings.Repeat("a ", 40): strings.Repeat("a_", 31) + "a"} {
		if got := moduleName(name); got != want || !moduleNameRe.MatchString(got) && got != "template" {
			t.Errorf("moduleName(%q) = %q, want %q", name, got, want)
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
