package httpserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/opsolete/opsolete/internal/registry/registrytest"
	"github.com/opsolete/opsolete/internal/runnerapi"
)

// TestImportFlow: a workspace's Import scans, with the resource group given, for what the catalog's registry
// templates can take over (read from their modules' code, the templates need nothing for it); the run page lists
// what was found with its template, related resources and fit, and marks what a workspace manages already;
// Import opens the template's form filled in, whose plan imports the resources (IDs from the scan, never the
// browser) with a placeholder for the password Azure never returns; a managed resource can't be imported again.
func TestImportFlow(t *testing.T) {
	store := testStore(t)
	h := newServer(t, t.TempDir(), true, store)
	r := &fakeRunner{h: h}
	ns := "i" + strings.ToLower(newID()[:8])
	files := map[string]string{}
	for _, f := range []string{"main.tf", "variables.tf"} {
		b, err := os.ReadFile("../registry/testdata/vm/" + f)
		if err != nil {
			t.Fatal(err)
		}
		files[f] = string(b)
	}
	if rec := upload(h, map[string]string{"namespace": ns, "name": "vm", "system": "azurerm", "version": "3.1.0"}, registrytest.TarGz(t, files)); rec.Code != http.StatusSeeOther {
		t.Fatalf("upload: %d %s", rec.Code, rec.Body.String())
	}
	tplForm := url.Values{"name": {"VM"}, "module_source": {"ops.example.com/" + ns + "/vm/azurerm"}, "module_ref": {"3.1.0"}, "key": {"name"},
		"in_name": {"name", "os_type", "resource_group_name", "nic_name", "admin_password"}, "in_type": {"string", "select", "string", "string", "string"},
		"in_opt_value_1": {"windows", "linux"}, "in_required": {"0", "2"}, "in_sensitive": {"4"}, "in_show_input": {"", "", "", "", "os_type"},
		"in_show_equals": {"", "", "", "", "windows"}}
	rec := post(h, "/templates/new", tplForm)
	tpl := strings.TrimPrefix(rec.Header().Get("Location"), "/templates/")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("template: %d %s", rec.Code, rec.Body.String())
	}
	if rec := post(h, "/templates/new", url.Values{"name": {"Git one"}, "module_source": {"git::https://example.com/x"}, "module_ref": {"v1"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("git template: %d", rec.Code)
	}
	ws := "im-" + strings.ToLower(newID()[:8])
	if rec := post(h, "/workspaces/new", url.Values{fieldWorkspace: {ws}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("create: %d", rec.Code)
	}
	page := get(h, "/workspaces/"+ws+"/import").Body.String()
	for _, want := range []string{`action="/workspaces/` + ws + `/scans"`, "which of them <strong>VM</strong> can import", "Git one: its module isn", `href="/workspaces/` + ws + `/import" aria-current="page">Import…`} {
		if !strings.Contains(page, want) {
			t.Fatalf("import page lacks %q:\n%s", want, page)
		}
	}

	scan := runID(t, post(h, "/workspaces/"+ws+"/scans", url.Values{"resource_group": {"test1"}}))
	j := r.claim(t, scan, "plan")
	if j.Action != "scan" || j.ModuleSource != "" || j.Scan == nil || j.Scan.ResourceGroup != "test1" || len(j.Scan.Templates) != 1 ||
		len(j.Scan.Templates[0].Entries) != 3 || len(j.Modules) != 1 || j.Modules[0].Name != j.Scan.Templates[0].Module ||
		len(j.Scan.Notes) != 1 || !strings.HasPrefix(j.Scan.Notes[0], "Git one:") {
		t.Fatalf("scan job: %+v %+v", j, j.Scan)
	}
	const sub = "/subscriptions/s/resourceGroups/test1/providers/"
	vmID, nicID := sub+"Microsoft.Compute/virtualMachines/testvm", sub+"Microsoft.Network/networkInterfaces/testvm807"
	imports := []runnerapi.Import{{To: "azurerm_network_interface.self", ID: nicID}, {To: "azurerm_windows_virtual_machine.self[0]", ID: vmID}}
	diskID, vnetID := sub+"Microsoft.Compute/disks/testvm_OsDisk_1", sub+"Microsoft.Network/virtualNetworks/VNET"
	cands := []runnerapi.Candidate{
		{Template: tpl, Name: "testvm", ID: vmID, Fit: "fits", Imports: imports, Placeholder: []string{"admin_password"}, Uses: []string{diskID},
			Inputs: map[string]string{"name": "testvm", "os_type": "windows", "resource_group_name": "test1", "nic_name": "testvm807"}},
		{Template: tpl, Name: "odd", ID: sub + "Microsoft.Compute/virtualMachines/odd", Fit: "replaces",
			Imports: []runnerapi.Import{{To: "azurerm_windows_virtual_machine.self[0]", ID: sub + "Microsoft.Compute/virtualMachines/odd"}},
			Changes: []runnerapi.FitChange{{Address: "azurerm_windows_virtual_machine.self[0]", Action: "replace", Attributes: []string{"zone"}}}},
	}
	inventory := []runnerapi.Found{{ID: vmID, Name: "testvm", Type: "Microsoft.Compute/virtualMachines", ResourceGroup: "test1"},
		{ID: nicID, Name: "testvm807", Type: "Microsoft.Network/networkInterfaces", ResourceGroup: "test1"},
		{ID: diskID, Name: "testvm_OsDisk_1", Type: "Microsoft.Compute/disks", ResourceGroup: "test1"},
		{ID: sub + "Microsoft.Compute/virtualMachines/odd", Name: "odd", Type: "Microsoft.Compute/virtualMachines", ResourceGroup: "test1"},
		{ID: vnetID, Name: "VNET", Type: "Microsoft.Network/virtualNetworks", ResourceGroup: "test1"}}
	body, _ := json.Marshal(runnerapi.FinishRequest{Status: "succeeded", Candidates: cands, Inventory: inventory})
	r.finish(t, string(body))
	if rec := get(h, "/workspaces/"+ws+"/import"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `href="/workspaces/`+ws+`/runs/`+scan+`"`) {
		t.Fatalf("import page with the scan listed: %d %s", rec.Code, rec.Body.String())
	}
	page = get(h, "/workspaces/"+ws+"/runs/"+scan).Body.String()
	link := "/workspaces/" + ws + "/templates/new?template=" + tpl + "&scan=" + scan + "&n=0"
	for _, want := range []string{"Scan for existing resources", "5 resources in resource group <code>test1</code>", "Can be imported <strong>1</strong>",
		`<strong title="` + vmID + `">testvm</strong> <span class="tag" title="template">VM</span>`, "Windows virtual machine · test1 · with network interface",
		`<span title="disks: testvm_OsDisk_1">testvm_OsDisk_1</span> (not imported)`, `<span class="status applied" title="Imports only: nothing changes">fits</span>`,
		`href="` + strings.ReplaceAll(link, "&", "&amp;") + `">Import</a>`, "Template needs work <strong>1</strong>", "Would replace zone",
		"No template <strong>1</strong>", `<strong title="Microsoft.Network/virtualNetworks">Virtual networks</strong>`, "Scan again"} {
		if !strings.Contains(page, want) {
			t.Fatalf("scan page lacks %q:\n%s", want, page)
		}
	}
	if strings.Contains(page, "&amp;n=1\"") {
		t.Fatal("a resource that doesn't fit offers Import")
	}

	// Import: the form filled in, no ID fields, the password left to the placeholder
	form := get(h, link).Body.String()
	for _, want := range []string{"Importing <strong>testvm</strong> (2 resources)", `value="testvm807"`, "a random placeholder is used"} {
		if !strings.Contains(form, want) {
			t.Fatalf("import form lacks %q:\n%s", want, form)
		}
	}
	if strings.Contains(form, nicID) || strings.Contains(form, `autocomplete="new-password" required`) {
		t.Fatal("the form shows IDs or requires the placeholder password")
	}
	values := url.Values{"name": {"testvm"}, "os_type": {"windows"}, "resource_group_name": {"test1"}, "nic_name": {"testvm807"}, "subnet_id": {"/s"}}
	if rec := post(h, "/workspaces/"+ws+"/templates/new?template=x&scan="+scan+"&n=0", values); rec.Code != http.StatusNotFound {
		t.Fatalf("another template than the one found for: %d", rec.Code)
	}
	rec = post(h, link, values)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("import: %d %s", rec.Code, rec.Body.String())
	}
	added := runID(t, rec)
	j = r.claim(t, added, "plan")
	if len(j.Imports) != 2 || !strings.HasPrefix(j.Imports[0].To, "module.testvm_") || j.Imports[1].ID != vmID ||
		!strings.HasPrefix(j.Modules[0].Inputs["admin_password"].(string), "Ops-") {
		t.Fatalf("import job: %+v", j)
	}

	// once applied, the scan shows it managed, and importing it again is refused
	r.finish(t, `{"status":"succeeded","plan":{"add":0,"change":0,"destroy":0,"import":2,"changes":[]}}`)
	if rec := post(h, "/workspaces/"+ws+"/runs/"+added+"/confirm", nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("confirm: %d", rec.Code)
	}
	r.claim(t, added, "apply")
	r.saveState(t, stateV(1))
	r.finish(t, `{"status":"succeeded","resources":[{"address":"module.testvm_x.azurerm_windows_virtual_machine.self[0]","type":"azurerm_windows_virtual_machine",
		"name":"self","provider":"p","module":"module.testvm_x","id":"`+strings.ToUpper(vmID)+`"}]}`)
	if page := get(h, "/workspaces/"+ws+"/runs/"+scan).Body.String(); !strings.Contains(page, "Already managed <strong>1</strong>") ||
		!strings.Contains(page, `<a href="/workspaces/`+ws+`">`+ws+`</a>`) || strings.Contains(page, "&amp;n=0\"") || !strings.Contains(page, "Can be imported <strong>0</strong>") {
		t.Fatalf("an imported resource isn't shown as managed:\n%s", page)
	}
	if rec := post(h, link, values); rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "managed already, in workspace "+ws) {
		t.Fatalf("importing a managed resource again: %d %s", rec.Code, rec.Body.String())
	}
}

// TestRunRegistryToken: a job carries a registry token for its run: modules from the registry (the ones a
// template's module calls) download with it while a runner works on the run, and not after, nor tampered.
func TestRunRegistryToken(t *testing.T) {
	store := testStore(t)
	h := newServer(t, t.TempDir(), true, store)
	r := &fakeRunner{h: h}
	ns := "rt" + strings.ToLower(newID()[:8])
	if rec := upload(h, map[string]string{"namespace": ns, "name": "disk", "system": "azurerm", "version": "1.0.0"},
		registrytest.TarGz(t, map[string]string{"main.tf": `variable "size_gb" {}`})); rec.Code != http.StatusSeeOther {
		t.Fatalf("upload: %d", rec.Code)
	}
	ws := "rt-" + strings.ToLower(newID()[:8])
	if rec := post(h, "/workspaces/new", url.Values{fieldWorkspace: {ws}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("create: %d", rec.Code)
	}
	run := runID(t, post(h, "/workspaces/"+ws+"/scans", url.Values{}))
	j := r.claim(t, run, "plan")
	if j.Registry == nil || j.Registry.Host != "ops.example.com" || !slices.Contains(j.Secrets, j.Registry.Token) {
		t.Fatalf("job's registry access: %+v (masked: %v)", j.Registry, j.Secrets)
	}
	versions := func(token string) int {
		req := httptest.NewRequest("GET", "/v1/modules/"+ns+"/disk/azurerm/versions", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		return do(h, req).Code
	}
	if c := versions(j.Registry.Token); c != http.StatusOK {
		t.Fatalf("while the run works: %d", c)
	}
	if c := versions(j.Registry.Token[:len(j.Registry.Token)-2] + "xx"); c != http.StatusUnauthorized {
		t.Fatalf("tampered: %d", c)
	}
	r.finish(t, `{"status":"succeeded"}`)
	if c := versions(j.Registry.Token); c != http.StatusUnauthorized {
		t.Fatalf("after the run: %d", c)
	}
}
