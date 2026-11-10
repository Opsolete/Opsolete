package runner

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	tfjson "github.com/hashicorp/terraform-json"

	"github.com/opsolete/opsolete/internal/runnerapi"
	"github.com/opsolete/opsolete/internal/templates"
)

// vmTemplate is the example VM module's template, scan mapping included (docs/resource-import.md).
const vmTemplate = `{"id": "vm", "name": "VM", "module": {"source": "s", "ref": "r"}, "inputs": [
	{"name": "name", "type": "string", "required": true},
	{"name": "os_type", "type": "select", "options": ["windows", "linux"]},
	{"name": "resource_group_name", "type": "string", "required": true},
	{"name": "subnet_id", "type": "string", "required": true},
	{"name": "size", "type": "string"},
	{"name": "secure_boot_enabled", "type": "bool"},
	{"name": "boot_diagnostics_enabled", "type": "bool"},
	{"name": "admin_password", "type": "string", "sensitive": true, "required": true, "show_if": {"input": "os_type", "equals": ["windows"]}},
	{"name": "nic_name", "type": "string"}],
	"adopt": [
	{"to": "azurerm_windows_virtual_machine.self[0]", "list": "azurerm_windows_virtual_machine", "require": "provision_vm_agent",
		"show_if": {"input": "os_type", "equals": ["windows"]}, "placeholder": ["admin_password"],
		"inputs": {"name": "name", "resource_group_name": "resource_group_name", "size": "size", "secure_boot_enabled": "secure_boot_enabled", "boot_diagnostics_enabled": "boot_diagnostics"}},
	{"to": "azurerm_linux_virtual_machine.self[0]", "list": "azurerm_linux_virtual_machine", "require": "disable_password_authentication",
		"show_if": {"input": "os_type", "equals": ["linux"]}, "inputs": {"name": "name", "resource_group_name": "resource_group_name"}},
	{"to": "azurerm_network_interface.self", "list": "azurerm_network_interface", "id_from": "network_interface_ids[0]",
		"inputs": {"nic_name": "name", "subnet_id": "ip_configuration[0].subnet_id", "resource_group_name": "resource_group_name"}}]}`

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
	cands, vals := candidates(tpl, byType, func() string { return "Placeholder-1a!" })
	if len(cands) != 2 {
		t.Fatalf("candidates: %+v", cands)
	}
	c := cands[0]
	wantIDs := []string{sub + "test1/providers/Microsoft.Compute/virtualMachines/testvm", "", nic["id"].(string)}
	wantIn := map[string]string{"name": "testvm", "os_type": "windows", "resource_group_name": "test1", "size": "Standard_D2s_v3",
		"secure_boot_enabled": "true", "boot_diagnostics_enabled": "true", "nic_name": "testvm807", "subnet_id": "/subscriptions/s/resourceGroups/f2s/providers/Microsoft.Network/virtualNetworks/v/subnets/a"}
	if c.Fit != "" || c.ID != wantIDs[0] || !reflect.DeepEqual(c.Adopt, wantIDs) || !reflect.DeepEqual(c.Inputs, wantIn) {
		t.Fatalf("testvm:\n%+v\nwant IDs %v\ninputs %v", c, wantIDs, wantIn)
	}
	if vals[0]["admin_password"] != "Placeholder-1a!" || vals[0]["secure_boot_enabled"] != true {
		t.Fatalf("values for the fit check: %v", vals[0])
	}
	if l := cands[1]; l.Fit != "invalid" || !strings.Contains(l.Message, "subnet_id: This field is required.") || !strings.Contains(l.Message, "ops-adopt-2") {
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
	tpl := templates.Template{Adopt: []templates.Adopt{{To: "terraform_data.this"}}}
	cands := []runnerapi.Candidate{{Name: "same", Adopt: []string{"a"}}, {Name: "other", Adopt: []string{"b"}}, {Name: "bad", Fit: "invalid"}}
	vals := []map[string]any{{}, {"greeting": "hi"}, nil}
	home := t.TempDir()
	tf := &tfProc{bin: r.Terraform, grace: time.Minute, end: time.Now().Add(time.Minute), env: append(os.Environ(), "HOME="+home)}
	var log strings.Builder
	r.fit(context.Background(), tf, t.TempDir(), src, tpl, nil, cands, vals, &log)
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
