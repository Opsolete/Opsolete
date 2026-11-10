package registry

import (
	"os"
	"path"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/opsolete/opsolete/internal/registry/registrytest"
	"github.com/opsolete/opsolete/internal/templates"
)

// dirArchive packs the .tf files of testdata/modules/<name>.
func dirArchive(t *testing.T, name string) []byte {
	files := map[string]string{}
	ents, err := os.ReadDir("testdata/modules/" + name)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		b, _ := os.ReadFile("testdata/modules/" + name + "/" + e.Name())
		files[e.Name()] = string(b)
	}
	return registrytest.TarGz(t, files)
}

// TestAdoptableTree: the composed VM module (testdata/modules, terraform-modules-examples/virtualmachine 4.x) is
// followed into its registry building blocks: the NIC related to the VMs, each VM for its os_type with its inputs;
// the optional and repeated parts (data disks, schedule, NSG) are left for the form, their modules import
// existing ones (existing = true).
func TestAdoptableTree(t *testing.T) {
	archive := dirArchive(t, "virtualmachine")
	ins, err := Inspect(archive)
	if err != nil {
		t.Fatal(err)
	}
	inputs, _ := Inputs(ins.Variables)
	i := slices.IndexFunc(inputs, func(in templates.Input) bool { return in.Name == "admin_password" })
	inputs[i].Sensitive = true // as the template marks it
	entries, notes, err := Adoptable(archive, inputs, func(source, version string) ([]byte, error) {
		if !strings.HasPrefix(version, "~> 1.") {
			t.Errorf("%s: version %q", source, version)
		}
		return dirArchive(t, path.Base(path.Dir(source))), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.To+" id_from="+e.IDFrom)
	}
	want := []string{"module.nic.azurerm_network_interface.main id_from=network_interface_ids[0]",
		"module.windows[0].azurerm_windows_virtual_machine.main id_from=", "module.linux[0].azurerm_linux_virtual_machine.main id_from="}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("entries:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	wantNotes := []string{"module.data_disk: repeated (for_each), filled in on the form", "module.shutdown_schedule: optional, filled in on the form",
		"module.nsg: optional, filled in on the form"}
	if !reflect.DeepEqual(notes, wantNotes) {
		t.Errorf("notes %q", notes)
	}
	w := entries[1]
	if w.ShowIf == nil || w.ShowIf.Equals[0] != "windows" || !reflect.DeepEqual(w.Placeholder, []string{"admin_password"}) ||
		w.Inputs["admin_password"] != "" || w.Inputs["image_sku"] != "source_image_reference[0].sku" || w.Inputs["boot_diagnostics_enabled"] != "boot_diagnostics" {
		t.Errorf("windows VM: %+v", w)
	}
	if n := entries[0].Inputs; n["nic_name"] != "name" || n["subnet_id"] != "ip_configuration[0].subnet_id" {
		t.Errorf("NIC: %v", n)
	}
}

// TestNewest: the version terraform init would pick for a module call's constraint.
func TestNewest(t *testing.T) {
	vs := []string{"1.0.0", "2.0.0", "1.2.0", "1.3.0-rc1"}
	for c, want := range map[string]string{"~> 1.0": "1.2.0", "": "2.0.0", "1.0.0": "1.0.0", ">= 3": "", "bad": ""} {
		if got, ok := Newest(vs, c); got != want || ok != (want != "") {
			t.Errorf("%q: %q %v", c, got, ok)
		}
	}
}
