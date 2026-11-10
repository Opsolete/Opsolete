package registry

import (
	"os"
	"reflect"
	"testing"

	"github.com/opsolete/opsolete/internal/registry/registrytest"
	"github.com/opsolete/opsolete/internal/templates"
)

// TestAdoptable: the example VM module (testdata/vm, terraform-modules-examples/virtualmachine) gives the adopt
// entries written by hand for it before: the NIC related through the VMs' network_interface_ids, the VMs shown
// by os_type (now a dropdown), each one's inputs from its attributes, the password a placeholder.
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
	adopt, gotInputs, notes, err := Adoptable(registrytest.TarGz(t, files), inputs)
	if err != nil {
		t.Fatal(err)
	}
	vm := map[string]string{"name": "name", "resource_group_name": "resource_group_name", "location": "location", "size": "size",
		"admin_username": "admin_username", "secure_boot_enabled": "secure_boot_enabled", "vtpm_enabled": "vtpm_enabled",
		"os_disk_type": "os_disk[0].storage_account_type", "image_publisher": "source_image_reference[0].publisher",
		"image_offer": "source_image_reference[0].offer", "image_sku": "source_image_reference[0].sku", "boot_diagnostics_enabled": "boot_diagnostics"}
	linux := map[string]string{"ssh_public_key": "admin_ssh_key[0].public_key"}
	for k, v := range vm {
		if k != "admin_password" {
			linux[k] = v
		}
	}
	help := "Azure portal: the resource → Properties → Resource ID."
	want := []templates.Adopt{
		{To: "azurerm_network_interface.self", Label: "Network interface resource ID", Help: help, List: "azurerm_network_interface",
			IDFrom: "network_interface_ids[0]", Inputs: map[string]string{"nic_name": "name", "resource_group_name": "resource_group_name", "location": "location",
				"ip_configuration_name": "ip_configuration[0].name", "subnet_id": "ip_configuration[0].subnet_id", "public_ip_address_id": "ip_configuration[0].public_ip_address_id"}},
		{To: "azurerm_windows_virtual_machine.self[0]", Label: "Windows virtual machine resource ID", Help: help, List: "azurerm_windows_virtual_machine",
			Require: "provision_vm_agent", ShowIf: &templates.ShowIf{Input: "os_type", Equals: []string{"windows"}}, Inputs: vm, Placeholder: []string{"admin_password"}},
		{To: "azurerm_linux_virtual_machine.self[0]", Label: "Linux virtual machine resource ID", Help: help, List: "azurerm_linux_virtual_machine",
			Require: "disable_password_authentication", ShowIf: &templates.ShowIf{Input: "os_type", Equals: []string{"linux"}}, Inputs: linux},
	}
	if !reflect.DeepEqual(adopt, want) {
		for i := range max(len(adopt), len(want)) {
			if i >= len(adopt) || i >= len(want) || !reflect.DeepEqual(adopt[i], want[i]) {
				t.Errorf("entry %d:\ngot  %+v\nwant %+v", i, at(adopt, i), at(want, i))
			}
		}
	}
	if o := gotInputs[6]; o.Type != "select" || !reflect.DeepEqual(o.Options, []templates.Option{{Value: "windows"}, {Value: "linux"}}) || inputs[6].Type != "string" {
		t.Errorf("os_type: %+v (the caller's inputs must stay unchanged: %+v)", o, inputs[6])
	}
	if !reflect.DeepEqual(notes, []string{"os_type became a dropdown: resources exist depending on its value"}) {
		t.Errorf("notes: %q", notes)
	}
	// the template with them loads: the checks templates run on save
	if _, err := templates.Encode(templates.Template{ID: "vm", Name: "VM", Module: templates.Module{Source: "s", Ref: "r"}, Inputs: gotInputs, Adopt: adopt}); err != nil {
		t.Fatal(err)
	}
}

func at(a []templates.Adopt, i int) any {
	if i < len(a) {
		return a[i]
	}
	return nil
}
