package registry

import (
	"os"
	"path"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/opsolete/opsolete/internal/registry/registrytest"
	"github.com/opsolete/opsolete/internal/runnerapi"
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

// TestAdoptableTree: the composed VM module (testdata/modules, terraform-modules-examples/virtualmachine 4.x)
// is followed into its registry building blocks: the NIC related to each VM; the data disks (repeated by LUN),
// the shutdown schedule and the NSG are extras of the resource they refer to, one entry per VM they may belong to.
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
		if version != "~> 1.0" {
			t.Errorf("%s: version %q", source, version)
		}
		return dirArchive(t, path.Base(path.Dir(source))), nil
	})
	if err != nil || len(notes) != 0 {
		t.Fatalf("%v %q", err, notes)
	}
	const win, lin, nic = "module.windows[0].azurerm_windows_virtual_machine.this", "module.linux[0].azurerm_linux_virtual_machine.this", "module.nic.azurerm_network_interface.this"
	disk, att := `module.data_disk["{key}"].azurerm_managed_disk.this`, `module.data_disk["{key}"].azurerm_virtual_machine_data_disk_attachment.this`
	sched := "module.shutdown_schedule[0].azurerm_dev_test_global_vm_shutdown_schedule.this"
	var got []string
	for _, e := range entries {
		got = append(got, e.To+" of="+e.Of+" id_from="+e.IDFrom)
	}
	want := []string{nic + " of= id_from=network_interface_ids[0]", win + " of= id_from=", lin + " of= id_from=",
		disk + " of=" + win + " id_from=", disk + " of=" + lin + " id_from=", att + " of=" + win + " id_from=", att + " of=" + lin + " id_from=",
		sched + " of=" + win + " id_from=", sched + " of=" + lin + " id_from=",
		"module.nsg[0].azurerm_network_security_group.this of=" + nic + " id_from=",
		"module.nsg[0].azurerm_network_interface_security_group_association.this of=" + nic + " id_from="}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("entries:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	w := entries[1]
	if w.ShowIf == nil || w.ShowIf.Equals[0] != "windows" || !reflect.DeepEqual(w.Placeholder, []string{"admin_password"}) ||
		w.Inputs["admin_password"] != "" || w.Inputs["image_sku"] != "source_image_reference[0].sku" || w.Inputs["boot_diagnostics_enabled"] != "boot_diagnostics" {
		t.Errorf("windows VM: %+v", w)
	}
	if r := entries[3].Repeat; r == nil || r.Input != "data_disks_json" || r.Key != "lun" ||
		!reflect.DeepEqual(r.Fields, map[string]string{"name": "name", "size_gb": "disk_size_gb", "type": "storage_account_type"}) {
		t.Errorf("data disk repeat: %+v", r)
	}
	if r := entries[5].Repeat; r == nil || !reflect.DeepEqual(r.Fields, map[string]string{"lun": "lun", "caching": "caching"}) {
		t.Errorf("attachment repeat: %+v", r)
	}
	if in := entries[7].Inputs; in["auto_shutdown_time"] != "daily_recurrence_time" || in["auto_shutdown_notification_email"] != "notification_settings[0].email" {
		t.Errorf("schedule: %v", in)
	}
	if l := entries[9].Lists["nsg_rules_json"]; l.Block != "security_rule" || l.Fields["destination_port_range"] != "destination_port_range" || len(l.Fields) != 10 ||
		entries[9].Inputs["nsg_name"] != "name" {
		t.Errorf("nsg: %+v", entries[9])
	}
	if slices.ContainsFunc(entries[3:], runnerapi.ScanEntry.Primary) {
		t.Error("extras are never primaries")
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
