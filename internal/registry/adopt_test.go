package registry

import (
	"maps"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/opsolete/opsolete/internal/registry/registrytest"
	"github.com/opsolete/opsolete/internal/runnerapi"
	"github.com/opsolete/opsolete/internal/templates"
)

// TestAdoptable: the example VM module (testdata/vm, terraform-modules-examples/virtualmachine) gives the entries
// written by hand for it before: the NIC related through the VMs' network_interface_ids, each VM for its os_type,
// each one's inputs from its attributes, whatever the variables are called, the ignored password a placeholder.
func TestAdoptable(t *testing.T) {
	files := map[string]string{}
	for _, f := range []string{"main.tf", "variables.tf"} {
		b, err := os.ReadFile("testdata/vm/" + f)
		if err != nil {
			t.Fatal(err)
		}
		files[f] = string(b)
	}
	in := func(name, typ string) templates.Input { return templates.Input{Name: name, Type: typ} }
	pw := in("admin_password", "string")
	pw.Sensitive = true
	osType := in("os_type", "string")
	osType.Default = "windows"
	inputs := []templates.Input{in("name", "string"), in("resource_group_name", "string"), in("location", "string"), in("subnet_id", "string"),
		in("size", "string"), in("admin_username", "string"), osType, pw, in("ssh_public_key", "string"), in("nic_name", "string"),
		in("ip_configuration_name", "string"), in("public_ip_address_id", "string"), in("secure_boot_enabled", "bool"), in("vtpm_enabled", "bool"),
		in("os_disk_type", "string"), in("image_publisher", "string"), in("image_offer", "string"), in("image_sku", "string"), in("boot_diagnostics_enabled", "bool")}
	entries, _, notes, err := Adoptable(registrytest.TarGz(t, files), inputs, nil)
	if err != nil {
		t.Fatal(err)
	}
	vm := map[string]string{"name": "name", "resource_group_name": "resource_group_name", "location": "location", "size": "size",
		"admin_username": "admin_username", "secure_boot_enabled": "secure_boot_enabled", "vtpm_enabled": "vtpm_enabled",
		"os_disk_type": "os_disk[0].storage_account_type", "image_publisher": "source_image_reference[0].publisher",
		"image_offer": "source_image_reference[0].offer", "image_sku": "source_image_reference[0].sku", "boot_diagnostics_enabled": "boot_diagnostics"}
	linux := map[string]string{"ssh_public_key": "admin_ssh_key[0].public_key"}
	maps.Copy(linux, vm)
	want := []runnerapi.ScanEntry{
		{To: "azurerm_network_interface.self", List: "azurerm_network_interface", IDFrom: "network_interface_ids[0]",
			Inputs: map[string]string{"nic_name": "name", "resource_group_name": "resource_group_name", "location": "location",
				"ip_configuration_name": "ip_configuration[0].name", "subnet_id": "ip_configuration[0].subnet_id", "public_ip_address_id": "ip_configuration[0].public_ip_address_id"}},
		{To: "azurerm_windows_virtual_machine.self[0]", List: "azurerm_windows_virtual_machine", Require: "provision_vm_agent",
			ShowIf: &templates.ShowIf{Input: "os_type", Equals: []string{"windows"}}, Inputs: vm, Placeholder: []string{"admin_password"}},
		{To: "azurerm_linux_virtual_machine.self[0]", List: "azurerm_linux_virtual_machine", Require: "disable_password_authentication",
			ShowIf: &templates.ShowIf{Input: "os_type", Equals: []string{"linux"}}, Inputs: linux},
	}
	if !reflect.DeepEqual(entries, want) {
		for i := range max(len(entries), len(want)) {
			if i >= len(entries) || i >= len(want) || !reflect.DeepEqual(entries[i], want[i]) {
				t.Errorf("entry %d:\ngot  %+v\nwant %+v", i, at(entries, i), at(want, i))
			}
		}
	}
	if len(notes) != 0 || inputs[6].Type != "string" {
		t.Errorf("notes %q; the inputs must stay unchanged: %+v", notes, inputs[6])
	}
}

// TestAdoptableSkips: what the reader can't follow is named, not guessed.
func TestAdoptableSkips(t *testing.T) {
	_, _, notes, err := Adoptable(registrytest.TarGz(t, map[string]string{"main.tf": `
variable "n" {}
resource "a_x" "each" {
  for_each = toset(["a"])
}
resource "a_x" "many" {
  count = 2
}
resource "a_x" "ok" { name = var.n }`}), []templates.Input{{Name: "n", Type: "string"}}, nil)
	if err != nil || len(notes) != 2 || !strings.Contains(notes[0], "a_x.each: for_each") || !strings.Contains(notes[1], "a_x.many: its count") {
		t.Fatalf("%q %v", notes, err)
	}
}

func at(a []runnerapi.ScanEntry, i int) any {
	if i < len(a) {
		return a[i]
	}
	return nil
}
