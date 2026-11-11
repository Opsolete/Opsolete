package runner

import (
	"encoding/json"
	"reflect"
	"testing"

	tfjson "github.com/hashicorp/terraform-json"

	"github.com/opsolete/opsolete/internal/mask"
	"github.com/opsolete/opsolete/internal/runnerapi"
)

// foundPlan is a plan importing a VM (module.vm, the template), its NIC (module.vm.module.nic) and a data disk
// (module.vm.module.data_disk["0"], for_each over the data_disks rows by lun), shaped like terraform show -json.
const foundPlan = `{
 "format_version": "1.2",
 "configuration": {"root_module": {"module_calls": {"vm": {"module": {
  "variables": {"admin_password": {"sensitive": true}},
  "resources": [{"address": "azurerm_windows_virtual_machine.main", "mode": "managed", "type": "azurerm_windows_virtual_machine", "name": "main",
   "expressions": {
    "name": {"references": ["var.name"]},
    "computer_name": {"references": ["var.name"]},
    "size": {"references": ["var.size"]},
    "admin_password": {"references": ["var.admin_password"]},
    "admin_username": {"references": ["var.admin_username"]},
    "tags": {"references": ["var.owner"]},
    "location": {"references": ["local.region"]},
    "os_disk": [{"storage_account_type": {"references": ["var.os_disk_type"]}}]}}],
  "module_calls": {
   "nic": {"expressions": {"name": {"references": ["var.nic_name", "var.name"]}, "subnet_id": {"references": ["var.subnet_id"]}},
    "module": {"resources": [{"address": "azurerm_network_interface.main", "mode": "managed", "type": "azurerm_network_interface", "name": "main",
     "expressions": {"name": {"references": ["var.name"]},
      "ip_configuration": [{"subnet_id": {"references": ["var.subnet_id"]}}]}}]}},
   "data_disk": {"for_each_expression": {"references": ["local.data_disks"]},
    "expressions": {"size_gb": {"references": ["each.value.size_gb", "each.value"]}, "name": {"references": ["each.value.name", "each.value", "var.name"]}},
    "module": {"resources": [{"address": "azurerm_managed_disk.main", "mode": "managed", "type": "azurerm_managed_disk", "name": "main",
     "expressions": {"disk_size_gb": {"references": ["var.size_gb"]}, "name": {"references": ["var.name"]}}}]}}}}}}}},
 "resource_changes": [
  {"address": "module.vm.azurerm_windows_virtual_machine.main", "module_address": "module.vm", "mode": "managed",
   "type": "azurerm_windows_virtual_machine", "name": "main",
   "change": {"actions": ["update"], "importing": {"id": "/vm"},
    "before": {"name": "test-vm", "computer_name": "TESTVM", "size": "Standard_DS1_v2", "admin_password": "ignored-as-imported",
     "admin_username": "azureuser", "tags": "owner hunter2", "location": "swedencentral", "os_disk": [{"storage_account_type": "Premium_LRS"}]},
    "after": {"name": "test-vm", "computer_name": "test-vm-pc", "size": "Standard_B2s", "admin_username": "azureuser", "location": "westeurope",
     "os_disk": [{"storage_account_type": "Standard_LRS"}]},
    "before_sensitive": {"admin_password": true}}},
  {"address": "module.vm.module.nic.azurerm_network_interface.main", "module_address": "module.vm.module.nic", "mode": "managed",
   "type": "azurerm_network_interface", "name": "main",
   "change": {"actions": ["update"], "importing": {"id": "/nic"},
    "before": {"name": "test-vm235", "ip_configuration": [{"subnet_id": "/subnets/a"}]},
    "after": {"name": "test-vm-nic", "ip_configuration": [{"subnet_id": null}]}}},
  {"address": "module.vm.module.data_disk[\"0\"].azurerm_managed_disk.main", "module_address": "module.vm.module.data_disk[\"0\"]", "mode": "managed",
   "type": "azurerm_managed_disk", "name": "main",
   "change": {"actions": ["update"], "importing": {"id": "/disk"},
    "before": {"disk_size_gb": 32, "name": "test-vm_DataDisk_0"}, "after": {"disk_size_gb": 1, "name": "test-vm_DataDisk_0"}}},
  {"address": "module.other.terraform_data.x", "module_address": "module.other", "mode": "managed", "type": "terraform_data", "name": "x",
   "change": {"actions": ["no-op"], "importing": {"id": "x"}, "before": {"input": "y"}, "after": {"input": "y"}}}
 ]}`

func TestFound(t *testing.T) {
	var p tfjson.Plan
	if err := json.Unmarshal([]byte(foundPlan), &p); err != nil {
		t.Fatal(err)
	}
	mods := []runnerapi.Module{{Name: "vm", Inputs: map[string]any{
		"name": "test-vm", "size": "Standard_B2s", "admin_username": "azureuser", "admin_password": "Ops-x", "owner": "me",
		"data_disks": []any{map[string]any{"lun": 0.0, "size_gb": 1.0, "name": "test-vm_DataDisk_0", "existing": true}},
	}}}
	got := found(&p, mods, mask.New([]string{"hunter2"}))
	want := map[string]map[string]any{"vm": {
		"name":           "test-vm",         // set and passed through: the same
		"size":           "Standard_DS1_v2", // set and passed through: the real one
		"admin_username": "azureuser",
		"os_disk_type":   "Premium_LRS", // unset, in a nested block
		"subnet_id":      "/subnets/a",  // unset, through a module call
		"data_disks":     []any{map[string]any{"lun": 0.0, "size_gb": 32.0, "name": "test-vm_DataDisk_0", "existing": true}},
		// not: computer_name (built from name: the plan sets another value), the NIC's name (two variables),
		// admin_password (sensitive), tags (a secret), location (a local), module.other (not a template)
	}}
	if !reflect.DeepEqual(got, want) {
		gj, _ := json.MarshalIndent(got, "", " ")
		t.Fatalf("found:\n%s", gj)
	}
	if mods[0].Inputs["data_disks"].([]any)[0].(map[string]any)["size_gb"] != 1.0 {
		t.Fatal("the job's inputs changed")
	}
}
