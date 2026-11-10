package runner

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/opsolete/opsolete/internal/runnerapi"
)

// TestFillExtras: a VM's data disks become one JSON list element per LUN (the disk's and its attachment's
// fields together), its NSG's rules a JSON list of blocks; extras fill only inputs the VM didn't, and their
// imports take the LUN in the address.
func TestFillExtras(t *testing.T) {
	const vm, nic = "module.windows[0].azurerm_windows_virtual_machine.this", "module.nic.azurerm_network_interface.this"
	repeat := func(fields map[string]string) *runnerapi.Repeat {
		return &runnerapi.Repeat{Input: "data_disks_json", Key: "lun", Fields: fields}
	}
	entries := []runnerapi.ScanEntry{{To: nic}, {To: vm},
		{To: `module.data_disk["{key}"].azurerm_managed_disk.this`, List: diskList, Of: vm, Inputs: map[string]string{"location": "location"},
			Repeat: repeat(map[string]string{"name": "name", "size_gb": "disk_size_gb", "type": "storage_account_type"})},
		{To: `module.data_disk["{key}"].azurerm_virtual_machine_data_disk_attachment.this`, List: attachmentList, Of: vm,
			Repeat: repeat(map[string]string{"lun": "lun", "caching": "caching"})},
		{To: "module.nsg[0].azurerm_network_security_group.this", List: nsgList, Of: nic, Inputs: map[string]string{"nsg_name": "name"},
			Lists: map[string]runnerapi.BlockList{"nsg_rules_json": {Block: "security_rule", Fields: map[string]string{"name": "name", "priority": "priority"}}}},
	}
	rel := relations{
		"/vm": {
			diskList: {{key: "10", obj: map[string]any{"id": "/d/b", "name": "b", "disk_size_gb": 64.0, "storage_account_type": "Standard_LRS"}},
				{key: "2", obj: map[string]any{"id": "/d/a", "name": "a", "disk_size_gb": 32.0, "storage_account_type": "Premium_LRS", "location": "elsewhere"}},
				{key: `x"]`, obj: map[string]any{"id": "/d/evil"}}},
			attachmentList: {{key: "2", obj: map[string]any{"id": "/VM/dataDisks/a", "lun": 2.0, "caching": "ReadOnly"}},
				{key: "10", obj: map[string]any{"id": "/VM/dataDisks/b", "lun": 10.0, "caching": "None"}}},
		},
		"/nic": {nsgList: {{obj: map[string]any{"id": "/nsg", "name": "vm-nsg", "security_rule": []any{
			map[string]any{"name": "RDP", "priority": 300.0, "protocol": "Tcp"}}}}}},
	}
	raw, used := map[string]string{"location": "westeurope"}, []string{}
	imports := fillExtras(entries, []string{"/NIC", "/VM", "", "", ""}, rel, raw, map[string]string{}, &used)
	want := map[string]string{"location": "westeurope", "nsg_name": "vm-nsg",
		"data_disks_json": `[{"caching":"ReadOnly","lun":2,"name":"a","size_gb":32,"type":"Premium_LRS"},{"caching":"None","lun":10,"name":"b","size_gb":64,"type":"Standard_LRS"}]`,
		"nsg_rules_json":  `[{"name":"RDP","priority":300}]`}
	if !reflect.DeepEqual(raw, want) {
		t.Errorf("form values:\n%v\nwant\n%v", raw, want)
	}
	wantImports := []runnerapi.Import{{To: `module.data_disk["10"].azurerm_managed_disk.this`, ID: "/d/b"}, {To: `module.data_disk["2"].azurerm_managed_disk.this`, ID: "/d/a"},
		{To: `module.data_disk["2"].azurerm_virtual_machine_data_disk_attachment.this`, ID: "/VM/dataDisks/a"},
		{To: `module.data_disk["10"].azurerm_virtual_machine_data_disk_attachment.this`, ID: "/VM/dataDisks/b"},
		{To: "module.nsg[0].azurerm_network_security_group.this", ID: "/nsg"}}
	if !reflect.DeepEqual(imports, wantImports) {
		t.Errorf("imports:\n%v\nwant\n%v", imports, wantImports)
	}
	if got := fillExtras(entries, []string{"/NIC", "", "", "", ""}, rel, map[string]string{}, nil, &used); len(got) != 1 {
		t.Errorf("only the NIC found: only its extras: %v", got)
	}
}

// TestReadGenerated: Terraform's generated config as attributes, nested blocks as lists; expressions left out.
func TestReadGenerated(t *testing.T) {
	f := filepath.Join(t.TempDir(), "gen.tf")
	os.WriteFile(f, []byte(`# __generated__ by Terraform
resource "azurerm_virtual_machine" "v0" {
  name = "vm-test"
  tags = jsonencode({})
  storage_data_disk {
    caching         = "ReadOnly"
    disk_size_gb    = 32
    lun             = 0
    managed_disk_id = "/d/vm-test_DataDisk_0"
    write_accelerator_enabled = null
  }
}
resource "azurerm_dev_test_global_vm_shutdown_schedule" "s0" {
  daily_recurrence_time = "1900"
  notification_settings {
    email = "a@example.com"
  }
}
`), 0o600)
	got, err := readGenerated(f)
	want := map[string]map[string]any{
		"azurerm_virtual_machine.v0": {"name": "vm-test", "storage_data_disk": []any{map[string]any{"caching": "ReadOnly", "disk_size_gb": 32.0, "lun": 0.0,
			"managed_disk_id": "/d/vm-test_DataDisk_0", "write_accelerator_enabled": nil}}},
		"azurerm_dev_test_global_vm_shutdown_schedule.s0": {"daily_recurrence_time": "1900", "notification_settings": []any{map[string]any{"email": "a@example.com"}}},
	}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("%v\n%v\nwant\n%v", err, got, want)
	}
	if scheduleID("/subscriptions/s/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/vm") !=
		"/subscriptions/s/resourceGroups/rg/providers/Microsoft.DevTestLab/schedules/shutdown-computevm-vm" || scheduleID("/x") != "" {
		t.Error("scheduleID")
	}
}
