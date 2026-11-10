package runner

import (
	"encoding/json"
	"reflect"
	"testing"

	tfjson "github.com/hashicorp/terraform-json"

	"github.com/opsolete/opsolete/internal/runnerapi"
	"github.com/opsolete/opsolete/internal/templates"
)

// TestProbe: a try's arguments come from the candidate (its values, the IDs it imports) and the try (the name, the
// row key, the switch on), else defaults or placeholders that pass the field's pattern; a try that imported fills a
// row of its list (sorted by key, the switch on) or the inputs the candidate lacks, its blocks a list's rows, and
// Found with its addresses in the template's module.
func TestProbe(t *testing.T) {
	var tpl templates.Template
	json.Unmarshal([]byte(`{"id": "vm", "name": "VM", "module": {"source": "x", "ref": "1"}, "inputs": [
  {"name": "location", "type": "string"}, {"name": "time", "type": "string", "pattern": "^([01][0-9]|2[0-3])[0-5][0-9]$"},
  {"name": "nsg_name", "type": "string"}, {"name": "nsg_on", "type": "bool"},
  {"name": "rules", "type": "list", "fields": [{"name": "name", "type": "string", "required": true}, {"name": "priority", "type": "number", "required": true}]},
  {"name": "disks", "type": "list", "fields": [{"name": "lun", "type": "number", "required": true}, {"name": "size_gb", "type": "number", "required": true},
    {"name": "name", "type": "string"}, {"name": "type", "type": "string", "default": "Standard_LRS"}, {"name": "existing", "type": "bool"}]}]}`), &tpl)
	c := runnerapi.Candidate{Name: "vm", Inputs: map[string]string{"location": "swedencentral"},
		Imports: []runnerapi.Import{{To: "module.windows[0].x.main", ID: "/vm"}, {To: "module.nic.y.main", ID: "/nic"}}}
	vals := map[string]any{"location": "swedencentral"}
	disk := runnerapi.Probe{Call: "module.data_disk", List: "disks", Key: "lun", Imports: 2, Args: map[string]runnerapi.ProbeArg{
		"name": {Field: "name", Name: true}, "existing": {Field: "existing", Switch: true}, "lun": {Field: "lun"}, "size_gb": {Field: "size_gb"},
		"type": {Field: "type"}, "location": {Input: "location"}, "time": {Input: "time"},
		"virtual_machine_id": {Refs: []string{"module.windows[0].x.main", "module.linux[0].x.main"}}},
		Fills: []runnerapi.ProbeFill{{To: "azurerm_managed_disk.main", Values: map[string]string{"name": "name", "size_gb": "disk_size_gb", "location": "location"}},
			{To: "azurerm_virtual_machine_data_disk_attachment.main", Values: map[string]string{"lun": "lun"}}}}
	args, ok := probeArgs(tpl, disk, c, vals, "data_0", 3)
	want := map[string]any{"name": "data_0", "existing": true, "lun": 3.0, "size_gb": 1, "type": "Standard_LRS", "location": "swedencentral", "time": "0000",
		"virtual_machine_id": "/vm"}
	if !ok || !reflect.DeepEqual(args, want) {
		t.Fatalf("args %v %#v", ok, args)
	}
	if _, ok := probeArgs(tpl, runnerapi.Probe{Args: map[string]runnerapi.ProbeArg{"vm": {Refs: []string{"module.linux[0].x.main"}}}}, c, vals, "", 0); ok {
		t.Fatal("a ref the candidate doesn't import: no try")
	}
	imp := func(id string, before map[string]any) *tfjson.ResourceChange {
		return &tfjson.ResourceChange{Change: &tfjson.Change{Importing: &tfjson.Importing{ID: id}, Before: before}}
	}
	raw := map[string]string{"location": "swedencentral", "disks": `[{"lun":"2","size_gb":"8"}]`}
	for _, d := range []struct {
		lun  float64
		name string
	}{{5, "b"}, {0, "a"}} {
		got := map[string]*tfjson.ResourceChange{
			"azurerm_managed_disk.main":                         imp("/disks/"+d.name, map[string]any{"name": d.name, "disk_size_gb": 32.0, "location": "elsewhere"}),
			"azurerm_virtual_machine_data_disk_attachment.main": imp("/vm/dataDisks/"+d.name, map[string]any{"lun": d.lun})}
		if !fillTry(tpl, try{probe: disk}, got, raw, &c) {
			t.Fatal("a belonging disk fills a row")
		}
	}
	if !jsonEqual(raw["disks"], `[{"existing":"true","lun":"0","name":"a","size_gb":"32"},{"lun":"2","size_gb":"8"},{"existing":"true","lun":"5","name":"b","size_gb":"32"}]`) ||
		raw["location"] != "swedencentral" {
		t.Fatalf("rows: %s, location %s", raw["disks"], raw["location"])
	}
	nsg := runnerapi.Probe{Call: "module.nsg", Imports: 1, Args: map[string]runnerapi.ProbeArg{"name": {Input: "nsg_name", Name: true}, "on": {Input: "nsg_on", Switch: true}},
		Fills: []runnerapi.ProbeFill{{To: "azurerm_network_security_group.main", Values: map[string]string{"nsg_name": "name", "location": "location"},
			Lists: map[string]runnerapi.BlockList{"rules": {Block: "security_rule", Fields: map[string]string{"name": "name", "priority": "priority"}}}}}}
	got := map[string]*tfjson.ResourceChange{"azurerm_network_security_group.main": imp("/nsg", map[string]any{"name": "vm-nsg", "location": "elsewhere",
		"security_rule": []any{map[string]any{"name": "RDP", "priority": 300.0}}})}
	if !fillTry(tpl, try{probe: nsg}, got, raw, &c) || raw["nsg_name"] != "vm-nsg" || raw["nsg_on"] != "true" || raw["location"] != "swedencentral" ||
		!jsonEqual(raw["rules"], `[{"name":"RDP","priority":"300"}]`) {
		t.Fatalf("single part: %v", raw)
	}
	if fillTry(tpl, try{probe: nsg}, got, raw, &c) {
		t.Fatal("a single part fills once")
	}
	wantFound := []runnerapi.Import{{To: `module.data_disk["5"].azurerm_managed_disk.main`, ID: "/disks/b"},
		{To: `module.data_disk["5"].azurerm_virtual_machine_data_disk_attachment.main`, ID: "/vm/dataDisks/b"},
		{To: `module.data_disk["0"].azurerm_managed_disk.main`, ID: "/disks/a"},
		{To: `module.data_disk["0"].azurerm_virtual_machine_data_disk_attachment.main`, ID: "/vm/dataDisks/a"},
		{To: "module.nsg[0].azurerm_network_security_group.main", ID: "/nsg"}}
	if !reflect.DeepEqual(c.Found, wantFound) {
		t.Fatalf("found: %+v", c.Found)
	}
}

func jsonEqual(a, b string) bool {
	var x, y any
	return json.Unmarshal([]byte(a), &x) == nil && json.Unmarshal([]byte(b), &y) == nil && reflect.DeepEqual(x, y)
}
