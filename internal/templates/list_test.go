package templates

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

const diskTemplate = `{"id": "vm", "name": "VM", "module": {"source": "x", "ref": "1"}, "inputs": [
  {"name": "data_disks", "label": "Data disks", "type": "list", "default": [{"lun": 0, "size_gb": 32}], "fields": [
    {"name": "lun", "label": "LUN", "type": "number", "required": true},
    {"name": "size_gb", "type": "number", "required": true},
    {"name": "type", "type": "select", "options": ["Standard_LRS", "Premium_LRS"], "default": "Standard_LRS"},
    {"name": "name", "type": "string", "pattern": "[a-z0-9_-]+"},
    {"name": "existing", "type": "bool"}]}]}`

// TestListInput: a list input's form value is a JSON array of rows; each row is checked field by field, typed,
// optional fields left empty left out; errors name the row and the field. The file format keeps fields and the
// default typed.
func TestListInput(t *testing.T) {
	var tpl Template
	if err := json.Unmarshal([]byte(diskTemplate), &tpl); err != nil {
		t.Fatal(err)
	}
	if err := tpl.check(); err != nil {
		t.Fatal(err)
	}
	if in := tpl.Inputs[0]; in.Default != `[{"lun":0,"size_gb":32}]` || len(in.Fields) != 5 || in.Fields[2].Default != "Standard_LRS" {
		t.Fatalf("decoded: %+v", in)
	}
	vals, errs := tpl.Validate(map[string]string{"data_disks": `[{"lun":"0","size_gb":"32","type":"Premium_LRS","name":"","existing":"on"},{"lun":1,"size_gb":8}]`})
	want := []any{map[string]any{"lun": 0.0, "size_gb": 32.0, "type": "Premium_LRS", "existing": true},
		map[string]any{"lun": 1.0, "size_gb": 8.0, "existing": false}}
	if len(errs) != 0 || !reflect.DeepEqual(vals["data_disks"], want) {
		t.Fatalf("%v %#v", errs, vals["data_disks"])
	}
	for raw, msg := range map[string]string{
		`[{"lun":0,"size_gb":"big"}]`:            "Row 1, size_gb: Enter a number",
		`[{"lun":0,"size_gb":1},{"size_gb":1}]`:  "Row 2, LUN: This field is required.",
		`[{"lun":0,"size_gb":1,"type":"Ultra"}]`: "Row 1, type: Choose one of the options.",
		`[{"lun":0,"size_gb":1,"name":"A B"}]`:   "Row 1, name: Doesn't have the expected format",
		`{"lun":0}`:                              "Enter a list of rows.",
	} {
		if _, errs := tpl.Validate(map[string]string{"data_disks": raw}); !strings.HasPrefix(errs["data_disks"], msg) {
			t.Errorf("%s: %q, want %q", raw, errs["data_disks"], msg)
		}
	}
	if vals, errs := tpl.Validate(map[string]string{"data_disks": ""}); len(errs) != 0 || !reflect.DeepEqual(vals["data_disks"], []any{}) {
		t.Errorf("no rows: an empty list, %v %v", vals, errs)
	}
	b, err := json.Marshal(tpl)
	var back Template
	if err != nil || json.Unmarshal(b, &back) != nil || !reflect.DeepEqual(back.Inputs, tpl.Inputs) {
		t.Fatalf("round trip: %v\n%s", err, b)
	}
	if !strings.Contains(string(b), `"default":[{"lun":0,"size_gb":32}]`) {
		t.Errorf("the default stays a JSON array: %s", b)
	}
	if got := tpl.Describe(map[string]any{"data_disks": want})[0].Text; got != "LUN: 0, size_gb: 32, type: Premium_LRS, existing: true\nLUN: 1, size_gb: 8, existing: false" {
		t.Errorf("described: %q", got)
	}
	for _, bad := range []string{
		`{"name": "l", "type": "list"}`,
		`{"name": "l", "type": "list", "sensitive": true, "fields": [{"name": "a", "type": "string"}]}`,
		`{"name": "l", "type": "list", "fields": [{"name": "a", "type": "list", "fields": [{"name": "b", "type": "string"}]}]}`,
		`{"name": "l", "type": "list", "fields": [{"name": "a", "type": "string"}, {"name": "a", "type": "number"}]}`,
		`{"name": "s", "type": "string", "fields": [{"name": "a", "type": "string"}]}`,
		`{"name": "l", "type": "list", "default": [{"a": "x y"}], "fields": [{"name": "a", "type": "string", "pattern": "[a-z]+"}]}`,
	} {
		var bt Template
		err := json.Unmarshal([]byte(`{"id": "b", "name": "B", "module": {"source": "x", "ref": "1"}, "inputs": [`+bad+`]}`), &bt)
		if err == nil {
			err = bt.check()
		}
		if err == nil {
			t.Errorf("accepted: %s", bad)
		}
	}
}
