package runner

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	tfjson "github.com/hashicorp/terraform-json"

	"github.com/opsolete/opsolete/internal/runnerapi"
	"github.com/opsolete/opsolete/internal/templates"
)

// vmTemplate and vmEntries: the example VM module's template, and what the server reads from its code
// (registry.Adoptable).
const vmTemplate = `{"id": "vm", "name": "VM", "module": {"source": "s", "ref": "r"}, "inputs": [
	{"name": "name", "type": "string", "required": true},
	{"name": "os_type", "type": "select", "options": ["windows", "linux"]},
	{"name": "resource_group_name", "label": "Resource group", "type": "string", "required": true},
	{"name": "subnet_id", "label": "Subnet ID", "type": "string", "required": true},
	{"name": "size", "type": "string"},
	{"name": "secure_boot_enabled", "type": "bool"},
	{"name": "boot_diagnostics_enabled", "type": "bool"},
	{"name": "admin_password", "type": "string", "sensitive": true, "required": true, "show_if": {"input": "os_type", "equals": ["windows"]}},
	{"name": "nic_name", "type": "string"}]}`

var vmEntries = []runnerapi.ScanEntry{
	{To: "azurerm_network_interface.self", List: "azurerm_network_interface", IDFrom: "network_interface_ids[0]",
		Inputs: map[string]string{"nic_name": "name", "subnet_id": "ip_configuration[0].subnet_id", "resource_group_name": "resource_group_name"}},
	{To: "azurerm_windows_virtual_machine.self[0]", List: "azurerm_windows_virtual_machine", Require: "provision_vm_agent",
		ShowIf: &templates.ShowIf{Input: "os_type", Equals: []string{"windows"}}, Placeholder: []string{"admin_password"},
		Inputs: map[string]string{"name": "name", "resource_group_name": "resource_group_name", "size": "size", "secure_boot_enabled": "secure_boot_enabled", "boot_diagnostics_enabled": "boot_diagnostics"}},
	{To: "azurerm_linux_virtual_machine.self[0]", List: "azurerm_linux_virtual_machine", Require: "disable_password_authentication",
		ShowIf: &templates.ShowIf{Input: "os_type", Equals: []string{"linux"}}, Inputs: map[string]string{"name": "name", "resource_group_name": "resource_group_name"}},
}

// TestCandidates: what terraform query found becomes form values and IDs; the OS comes from the entry that
// found it (Require drops the Linux list's copy of a Windows VM), the NIC from the VM's attribute, IDs take the
// inputs' case, the password a placeholder that is never returned, and missing required values make it invalid.
func TestCandidates(t *testing.T) {
	var tpl templates.Template
	if err := json.Unmarshal([]byte(vmTemplate), &tpl); err != nil {
		t.Fatal(err)
	}
	const sub = "/subscriptions/s/resourceGroups/"
	vm := map[string]any{"id": sub + "TEST1/providers/Microsoft.Compute/virtualMachines/testvm", "name": "testvm", "resource_group_name": "test1",
		"size": "Standard_D2s_v3", "secure_boot_enabled": true, "boot_diagnostics": []any{map[string]any{"storage_account_uri": ""}}, "network_interface_ids": []any{sub + "test1/providers/Microsoft.Network/networkInterfaces/testvm807"}}
	asLinux := map[string]any{"id": vm["id"], "name": "testvm", "disable_password_authentication": nil} // azurerm's Linux list returns it too
	orphan := map[string]any{"id": sub + "rg/providers/Microsoft.Compute/virtualMachines/lonely", "name": "lonely", "resource_group_name": "rg",
		"provision_vm_agent": true, "network_interface_ids": []any{}}
	vm["provision_vm_agent"] = true
	nic := map[string]any{"id": sub + "test1/providers/Microsoft.Network/networkInterfaces/testvm807", "name": "testvm807", "resource_group_name": "test1",
		"ip_configuration": []any{map[string]any{"subnet_id": "/subscriptions/s/resourceGroups/f2s/providers/Microsoft.Network/virtualNetworks/v/subnets/a"}}}
	byType := map[string][]found{
		"azurerm_windows_virtual_machine": {{"testvm", vm}, {"lonely", orphan}},
		"azurerm_linux_virtual_machine":   {{"testvm", asLinux}},
		"azurerm_network_interface":       {{"testvm807", nic}},
	}
	cands, vals := candidates(tpl, vmEntries, byType, func() string { return "Placeholder-1a!" })
	if len(cands) != 2 {
		t.Fatalf("candidates: %+v", cands)
	}
	c := cands[0]
	vmID := sub + "test1/providers/Microsoft.Compute/virtualMachines/testvm"
	wantImports := []runnerapi.Import{{To: "azurerm_network_interface.self", ID: nic["id"].(string)}, {To: "azurerm_windows_virtual_machine.self[0]", ID: vmID}}
	wantIn := map[string]string{"name": "testvm", "os_type": "windows", "resource_group_name": "test1", "size": "Standard_D2s_v3",
		"secure_boot_enabled": "true", "boot_diagnostics_enabled": "true", "nic_name": "testvm807", "subnet_id": "/subscriptions/s/resourceGroups/f2s/providers/Microsoft.Network/virtualNetworks/v/subnets/a"}
	if c.Template != "vm" || c.Fit != "" || c.ID != vmID || !reflect.DeepEqual(c.Imports, wantImports) || !reflect.DeepEqual(c.Inputs, wantIn) ||
		!reflect.DeepEqual(c.Placeholder, []string{"admin_password"}) {
		t.Fatalf("testvm:\n%+v\nwant imports %v\ninputs %v", c, wantImports, wantIn)
	}
	if vals[0]["admin_password"] != "Placeholder-1a!" || vals[0]["secure_boot_enabled"] != true {
		t.Fatalf("values for the fit check: %v", vals[0])
	}
	if l := cands[1]; l.Fit != "invalid" || l.Message != "Subnet ID: This field is required." || len(l.Imports) != 1 {
		t.Fatalf("a VM without NIC: %+v", l)
	}
}

// TestFit: one plan adopts every candidate into a throwaway state; each gets the fit of its own module.
func TestFit(t *testing.T) {
	f := &fakeServer{}
	srv := httptest.NewServer(f)
	defer srv.Close()
	r := testRunner(t, srv)
	src := srv.URL + strings.SplitN(f.module(t, `
variable "greeting" {
  type    = string
  default = null
}
resource "terraform_data" "this" { input = var.greeting }`), "?", 2)[0]
	im := func(id string) []runnerapi.Import { return []runnerapi.Import{{To: "terraform_data.this", ID: id}} }
	cands := []runnerapi.Candidate{{Template: "t", Name: "same", Imports: im("a")}, {Template: "t", Name: "other", Imports: im("b")}, {Template: "t", Name: "bad", Fit: "invalid"}}
	vals := []map[string]any{{}, {"greeting": "hi"}, nil}
	home := t.TempDir()
	tf := &tfProc{bin: r.Terraform, grace: time.Minute, end: time.Now().Add(time.Minute), env: append(os.Environ(), "HOME="+home)}
	var log strings.Builder
	r.fit(context.Background(), tf, t.TempDir(), map[string]string{"t": src}, nil, cands, vals, &log)
	// terraform_data always plans an update after an import; the change is in "other" only
	other := runnerapi.FitChange{Address: "terraform_data.this", Action: "update", Attributes: []string{"input"}}
	if cands[0].Fit == "" || cands[1].Fit != "differs" || !reflect.DeepEqual(cands[1].Changes, []runnerapi.FitChange{other}) || cands[2].Fit != "invalid" {
		t.Fatalf("fit: %+v\n%s", cands, log.String())
	}
}

// TestFitPlan: imports only fit, creates and updates differ, replaces and deletes don't fit; each candidate is
// judged by its own module only.
func TestFitPlan(t *testing.T) {
	rc := func(addr string, before, after map[string]any, actions ...tfjson.Action) *tfjson.ResourceChange {
		return &tfjson.ResourceChange{Address: addr, Change: &tfjson.Change{Actions: actions, Before: before, After: after, Importing: &tfjson.Importing{ID: "x"}}}
	}
	rcs := []*tfjson.ResourceChange{
		rc("module.c0.a.vm[0]", nil, nil, tfjson.ActionNoop), rc("module.c0.a.nic", nil, nil, tfjson.ActionNoop),
		rc("module.c1.a.vm[0]", map[string]any{"size": "S", "zone": nil, "tags": map[string]any{}}, map[string]any{"size": "M", "zone": nil, "tags": map[string]any{}}, tfjson.ActionUpdate),
		rc("module.c10.a.nic", nil, nil, tfjson.ActionCreate),
		rc("module.c2.a.vm[0]", map[string]any{"name": "a"}, map[string]any{"name": "b"}, tfjson.ActionDelete, tfjson.ActionCreate),
	}
	cands := make([]runnerapi.Candidate, 11)
	for i := 3; i < 10; i++ {
		cands[i].Fit = "invalid"
	}
	fitPlan(cands, rcs)
	got := []runnerapi.Candidate{cands[0], cands[1], cands[2], cands[10]}
	want := []runnerapi.Candidate{{Fit: "fits"},
		{Fit: "differs", Changes: []runnerapi.FitChange{{Address: "a.vm[0]", Action: "update", Attributes: []string{"size"}}}},
		{Fit: "replaces", Changes: []runnerapi.FitChange{{Address: "a.vm[0]", Action: "replace", Attributes: []string{"name"}}}},
		{Fit: "differs", Changes: []runnerapi.FitChange{{Address: "a.nic", Action: "create"}}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %+v\nwant %+v", got, want)
	}
}

// TestParseQuery: found resources by type; list types the provider lacks are reported, not fatal; other errors are.
func TestParseQuery(t *testing.T) {
	out := `{"type":"version","@message":"Terraform 1.15.3"}
{"type":"diagnostic","@level":"error","@message":"Error: Invalid list resource","diagnostic":{"detail":"The provider hashicorp/azurerm does not support list resource \"azurerm_managed_disk\"."}}
{"type":"list_resource_found","list_resource_found":{"resource_type":"azurerm_network_interface","display_name":"nic1","resource_object":{"id":"/x/nic1"}}}
{"type":"list_complete","@message":"list.azurerm_network_interface.l1: List complete"}`
	var log strings.Builder
	byType, unsupported, err := parseQuery(strings.NewReader(out), &log)
	if err != nil || len(byType["azurerm_network_interface"]) != 1 || byType["azurerm_network_interface"][0].obj["id"] != "/x/nic1" ||
		!reflect.DeepEqual(unsupported, []string{"azurerm_managed_disk"}) || !strings.Contains(log.String(), "List complete") {
		t.Fatalf("%v %v %v\n%s", byType, unsupported, err, log.String())
	}
	if _, _, err := parseQuery(strings.NewReader(`{"type":"diagnostic","@level":"error","@message":"Error: building account"}`), &log); err == nil {
		t.Fatal("an error diagnostic must fail the query")
	}
}

func TestPath(t *testing.T) {
	var v any
	json.Unmarshal([]byte(`{"size": "S", "os_disk": [{"storage_account_type": "Premium_LRS"}], "nics": ["a", "b"], "n": 2, "on": true, "x": null}`), &v)
	for p, want := range map[string]any{"size": "S", "os_disk[0].storage_account_type": "Premium_LRS", "nics[1]": "b", "n": 2.0, "on": true,
		"x": nil, "nope": nil, "nics[2]": nil, "size[0]": nil, "os_disk.storage_account_type": nil} {
		if got := path(v, p); got != want {
			t.Errorf("path(%s) = %v, want %v", p, got, want)
		}
	}
}

// TestQueryFile: a resource group narrows azurerm lists, except the resource group list itself.
func TestQueryFile(t *testing.T) {
	got := queryFile([]string{"azurerm_windows_virtual_machine", "azurerm_resource_group"}, "test1")
	want := `list "azurerm_windows_virtual_machine" "l0" {
  provider         = azurerm
  include_resource = true
  limit            = 1000
  config {
    resource_group_name = "test1"
  }
}
list "azurerm_resource_group" "l1" {
  provider         = azurerm
  include_resource = true
  limit            = 1000
}
`
	if got != want {
		t.Fatalf("got\n%s", got)
	}
	for rg, ok := range map[string]bool{"test1": true, "rg-prod_01(a).x": true, `a"b`: false, "${x}": false, "": false} {
		if rgRe.MatchString(rg) != ok {
			t.Errorf("rgRe(%q) = %v", rg, !ok)
		}
	}
}

// TestUses: a candidate uses the inventory's resources its attributes refer to and it doesn't import, any case.
func TestUses(t *testing.T) {
	var vm any
	json.Unmarshal([]byte(`{"id": "/subscriptions/s/resourceGroups/RG/providers/x/vm", "os_managed_disk_id": "/subscriptions/s/resourceGroups/RG/providers/x/disk",
		"nics": ["/subscriptions/s/resourceGroups/rg/providers/x/nic"], "subnet_id": "/subscriptions/s/resourceGroups/rg/providers/x/vnet/subnets/a", "size": "S"}`), &vm)
	var used []string
	refs(vm, &used)
	c := []runnerapi.Candidate{{Uses: used, Imports: []runnerapi.Import{{ID: "/subscriptions/s/resourceGroups/rg/providers/x/vm"}, {ID: "/subscriptions/s/resourceGroups/rg/providers/x/nic"}}}}
	inv := []runnerapi.Found{{ID: "/subscriptions/s/resourceGroups/rg/providers/x/vm"}, {ID: "/subscriptions/s/resourceGroups/rg/providers/x/disk"},
		{ID: "/subscriptions/s/resourceGroups/rg/providers/x/vnet"}}
	uses(c, inv)
	if want := []string{"/subscriptions/s/resourceGroups/rg/providers/x/disk"}; !reflect.DeepEqual(c[0].Uses, want) {
		t.Fatalf("uses %v, want %v (the subnet isn't the network; the VM and NIC are imported)", c[0].Uses, want)
	}
}

// TestCLIConfig: the registry's credentials block, on top of the runner's own CLI config when it has one.
func TestCLIConfig(t *testing.T) {
	home := t.TempDir()
	reg := runnerapi.RegistryAccess{Host: "ops.example.com:8443", Token: "opsrun.x.y"}
	p, err := writeCLIConfig(home, reg, "http://opsolete:8080/v1/modules/", "")
	b, _ := os.ReadFile(p)
	if err != nil || string(b) != "credentials \"ops.example.com:8443\" {\n  token = \"opsrun.x.y\"\n}\n"+
		"host \"ops.example.com:8443\" {\n  services = {\n    \"modules.v1\" = \"http://opsolete:8080/v1/modules/\"\n  }\n}\n" {
		t.Fatalf("%v %q", err, b)
	}
	own := filepath.Join(t.TempDir(), "runner.tfrc")
	os.WriteFile(own, []byte(`plugin_cache_dir = "/cache"`), 0o600)
	p, err = writeCLIConfig(home, reg, "http://opsolete:8080/v1/modules/", own)
	b, _ = os.ReadFile(p)
	if err != nil || !strings.HasPrefix(string(b), "plugin_cache_dir = \"/cache\"\ncredentials ") {
		t.Fatalf("with the runner's own config: %v %q", err, b)
	}
	if _, err := writeCLIConfig(home, reg, "http://opsolete:8080/v1/modules/", "/nope"); err == nil {
		t.Fatal("a missing runner config must fail the job, not be dropped")
	}
}

// TestSample: a probe's placeholder for a field with a pattern matches it, so the provider's own format checks
// (run before an import) let the try through.
func TestSample(t *testing.T) {
	for pattern, want := range map[string]string{`^([01][0-9]|2[0-3])[0-5][0-9]$`: "0000", `[a-z]{3,24}`: "aaa", `vm-[0-9]+`: "vm-0", `x|y`: "x", ``: "", `(`: "", `a*`: ""} {
		if got := sample(pattern); got != want {
			t.Errorf("%q: %q, want %q", pattern, got, want)
		}
	}
}
