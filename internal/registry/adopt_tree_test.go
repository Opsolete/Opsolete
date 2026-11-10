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
	// its data disks and rules are lists of objects: rows in the form
	for name, fields := range map[string]string{"data_disks": "lun size_gb name type caching existing",
		"nsg_rules": "name priority direction access protocol source_port_range destination_port_range source_address_prefix destination_address_prefix description"} {
		i := slices.IndexFunc(inputs, func(in templates.Input) bool { return in.Name == name })
		var got []string
		for _, f := range inputs[i].Fields {
			got = append(got, f.Name)
		}
		if inputs[i].Type != "list" || strings.Join(got, " ") != fields || inputs[i].Default != "[]" {
			t.Errorf("%s: %+v", name, inputs[i])
		}
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

// TestListInputs: a list(object({...})) variable of strings, numbers and bools is a list input, its fields in the
// order written, optional ones not required with optional()'s default; other complex types stay skipped. A
// template's JSON adds a label or options to a field like to an input.
func TestListInputs(t *testing.T) {
	archive := registrytest.TarGz(t, map[string]string{"main.tf": `
variable "data_disks" {
  type = list(object({
    lun      = number
    size_gb  = number
    type     = optional(string, "Standard_LRS")
    name     = optional(string)
    existing = optional(bool, false)
  }))
  default = [{ lun = 0, size_gb = 32 }]
}
variable "tags" { type = map(string) }
variable "nested" { type = list(object({ ports = list(number) })) }
`})
	ins, err := Inspect(archive)
	if err != nil {
		t.Fatal(err)
	}
	inputs, skipped := Inputs(ins.Variables)
	slices.Sort(skipped)
	if len(inputs) != 1 || !reflect.DeepEqual(skipped, []string{"nested (list(object({ ports = list(number) })))", "tags (map(string))"}) {
		t.Fatalf("%+v %q", inputs, skipped)
	}
	in := inputs[0]
	want := []templates.Input{{Name: "lun", Label: "lun", Type: "number", Required: true}, {Name: "size_gb", Label: "size_gb", Type: "number", Required: true},
		{Name: "type", Label: "type", Type: "string", Default: "Standard_LRS"}, {Name: "name", Label: "name", Type: "string"},
		{Name: "existing", Label: "existing", Type: "bool", Default: "false"}}
	if in.Type != "list" || in.Default != `[{"lun":0,"size_gb":32}]` || !reflect.DeepEqual(in.Fields, want) {
		t.Fatalf("%+v", in)
	}
	got := Resolve([]templates.Input{{Name: "data_disks", Label: "Data disks", Fields: []templates.Input{
		{Name: "type", Type: "select", Label: "Disk type", Options: []templates.Option{{Value: "Standard_LRS"}, {Value: "Premium_LRS"}}}}}}, ins.Variables)
	if f := got[0].Fields[2]; got[0].Label != "Data disks" || f.Label != "Disk type" || f.Type != "select" || len(f.Options) != 2 || f.Default != "Standard_LRS" {
		t.Fatalf("resolved: %+v", got[0])
	}
}
