package httpserver

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/opsolete/opsolete/internal/runnerapi"
)

// TestScanFlow: a workspace scans for what a template can adopt; the runner gets the template and its module
// only; the run page lists what it found with its fit, marks what a workspace manages already, and Adopt opens the
// add form filled in, whose save imports the resources with a placeholder for the password Azure never returns.
func TestScanFlow(t *testing.T) {
	store := testStore(t)
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "vm.json"), []byte(`{"id": "vm", "name": "VM", "module": {"source": "git::https://example.com/vm", "ref": "v1"},
		"inputs": [{"name": "name", "type": "string", "required": true, "key": true}, {"name": "os_type", "type": "select", "options": ["windows", "linux"]},
			{"name": "resource_group_name", "type": "string", "required": true}, {"name": "nic_name", "type": "string"},
			{"name": "admin_password", "type": "string", "sensitive": true, "required": true, "show_if": {"input": "os_type", "equals": ["windows"]}}],
		"adopt": [
		{"to": "azurerm_windows_virtual_machine.self[0]", "list": "azurerm_windows_virtual_machine", "require": "provision_vm_agent",
			"show_if": {"input": "os_type", "equals": ["windows"]}, "placeholder": ["admin_password"], "inputs": {"name": "name"}},
		{"to": "azurerm_linux_virtual_machine.self[0]", "list": "azurerm_linux_virtual_machine", "show_if": {"input": "os_type", "equals": ["linux"]}},
		{"to": "azurerm_network_interface.self", "list": "azurerm_network_interface", "id_from": "network_interface_ids[0]", "inputs": {"nic_name": "name"}}]}`), 0o600)
	h := newServer(t, dir, false, store)
	r := &fakeRunner{h: h}
	ws := "sc-" + strings.ToLower(newID()[:8])
	if rec := post(h, "/workspaces/new", url.Values{fieldWorkspace: {ws}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("create: %d", rec.Code)
	}
	if page := get(h, "/workspaces/"+ws+"/deploy").Body.String(); !strings.Contains(page, "Import existing resources") || !strings.Contains(page, `<option value="vm">VM</option>`) {
		t.Fatalf("deploy tab without the scan:\n%s", page)
	}

	scan := runID(t, post(h, "/workspaces/"+ws+"/scans", url.Values{"template": {"vm"}}))
	if j := r.claim(t, scan, "plan"); j.Action != "scan" || j.ModuleSource != "" || len(j.Modules) != 1 || j.Modules[0].Source != "git::https://example.com/vm?ref=v1" ||
		!strings.Contains(string(j.Scan), `"list":"azurerm_windows_virtual_machine"`) {
		t.Fatalf("scan job: %+v %s", j, j.Scan)
	}
	const sub = "/subscriptions/s/resourceGroups/test1/providers/"
	vmID, nicID := sub+"Microsoft.Compute/virtualMachines/testvm", sub+"Microsoft.Network/networkInterfaces/testvm807"
	cands := []runnerapi.Candidate{
		{Name: "testvm", ID: vmID, Fit: "fits", Adopt: []string{vmID, "", nicID},
			Inputs: map[string]string{"name": "testvm", "os_type": "windows", "resource_group_name": "test1", "nic_name": "testvm807"}},
		{Name: "odd", ID: sub + "Microsoft.Compute/virtualMachines/odd", Fit: "replaces", Adopt: []string{sub + "Microsoft.Compute/virtualMachines/odd", "", ""},
			Changes: []runnerapi.FitChange{{Address: "azurerm_windows_virtual_machine.self[0]", Action: "replace", Attributes: []string{"zone"}}}},
	}
	body, _ := json.Marshal(runnerapi.FinishRequest{Status: "succeeded", Candidates: cands})
	r.finish(t, string(body))
	page := get(h, "/workspaces/"+ws+"/runs/"+scan).Body.String()
	adopt := "/workspaces/" + ws + "/templates/new?template=vm&scan=" + scan + "&n=0"
	for _, want := range []string{"Scan for existing resources", "VM: 2 found", `<span class="status applied">fits</span>`, `href="` + strings.ReplaceAll(adopt, "&", "&amp;") + `"`,
		`<span class="status errored">doesn't fit</span>`, `<code>azurerm_windows_virtual_machine.self[0]</code>: zone`, "Scan again"} {
		if !strings.Contains(page, want) {
			t.Fatalf("scan page lacks %q:\n%s", want, page)
		}
	}
	if strings.Contains(page, "n=1") {
		t.Fatal("a resource that doesn't fit offers Adopt")
	}

	// Adopt: the add form, filled in; the password is left to the placeholder
	form := get(h, adopt).Body.String()
	for _, want := range []string{"Adopting <strong>testvm</strong>", `value="testvm807"`, `name="ops-adopt-0" value="` + vmID + `"`, "a random placeholder is used"} {
		if !strings.Contains(form, want) {
			t.Fatalf("adopt form lacks %q:\n%s", want, form)
		}
	}
	if strings.Contains(form, `name="secret.admin_password" type="password" autocomplete="new-password"
       required`) || strings.Contains(form, `autocomplete="new-password" required`) {
		t.Fatal("the placeholder password is required")
	}
	added := runID(t, post(h, adopt, url.Values{"name": {"testvm"}, "os_type": {"windows"}, "resource_group_name": {"test1"}, "nic_name": {"testvm807"},
		"ops-adopt-0": {vmID}, "ops-adopt-2": {nicID}}))
	j := r.claim(t, added, "plan")
	if len(j.Imports) != 2 || !strings.HasPrefix(j.Modules[0].Inputs["admin_password"].(string), "Ops-") {
		t.Fatalf("adoption job: %+v", j)
	}

	// once applied, the scan shows it as managed (IDs compare in any case)
	r.finish(t, `{"status":"succeeded","plan":{"add":0,"change":0,"destroy":0,"import":2,"changes":[]}}`)
	if rec := post(h, "/workspaces/"+ws+"/runs/"+added+"/confirm", nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("confirm: %d", rec.Code)
	}
	r.claim(t, added, "apply")
	r.saveState(t, stateV(1))
	r.finish(t, `{"status":"succeeded","resources":[{"address":"module.testvm_x.azurerm_windows_virtual_machine.self[0]","type":"azurerm_windows_virtual_machine",
		"name":"self","provider":"p","module":"module.testvm_x","id":"`+strings.ToUpper(vmID)+`"}]}`)
	if page := get(h, "/workspaces/"+ws+"/runs/"+scan).Body.String(); !strings.Contains(page, `Already managed in workspace <a href="/workspaces/`+ws+`">`) ||
		strings.Contains(page, "n=0") {
		t.Fatalf("an adopted resource isn't shown as managed:\n%s", page)
	}
	// and adopting it again, by hand, is refused
	again := url.Values{"name": {"testvm2"}, "os_type": {"windows"}, "resource_group_name": {"test1"}, "ops-adopt-0": {strings.ToLower(vmID)}, "ops-adopt-2": {nicID}}
	if rec := post(h, "/workspaces/"+ws+"/templates/new?template=vm", again); rec.Code != http.StatusUnprocessableEntity ||
		!strings.Contains(rec.Body.String(), "Already managed in workspace "+ws) {
		t.Fatalf("adopting a managed resource again: %d %s", rec.Code, rec.Body.String())
	}
}
