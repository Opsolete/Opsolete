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

// TestComplexInputs: a string_list, map and object take their stored JSON (defaults, saved values) and their form
// rows alike, check each value like an input, and give Terraform list(string), map(string) and an object; a map
// or object can't pass for a sealed value, and only {"sealed": "..."} does.
func TestComplexInputs(t *testing.T) {
	var tpl Template
	if err := json.Unmarshal([]byte(`{"id": "c", "name": "C", "module": {"source": "x", "ref": "1"}, "inputs": [
  {"name": "dns", "type": "string_list", "pattern": "[0-9.]+", "help": "An IPv4 address", "default": ["10.0.0.4"]},
  {"name": "tags", "type": "map", "default": {"env": "prod"}},
  {"name": "network", "type": "object", "default": {"cidr": "10.0.0.0/16"}, "fields": [
    {"name": "cidr", "label": "CIDR", "type": "string", "required": true}, {"name": "dns", "type": "bool"}, {"name": "size", "type": "number"}]}]}`), &tpl); err != nil {
		t.Fatal(err)
	}
	if err := tpl.check(); err != nil {
		t.Fatal(err)
	}
	if d := tpl.Inputs[1].Default; d != `{"env":"prod"}` {
		t.Fatalf("map default: %s", d)
	}
	ok := func(raw map[string]string, want map[string]any) {
		t.Helper()
		vals, errs := tpl.Validate(raw)
		if len(errs) != 0 || !reflect.DeepEqual(vals, want) {
			t.Errorf("%v: %#v %v", raw, vals, errs)
		}
	}
	// stored (and default) JSON
	ok(map[string]string{"dns": `["10.0.0.4","10.0.0.5"]`, "tags": `{"env":"prod","team":"x"}`, "network": `{"cidr":"10.1.0.0/16","dns":true}`},
		map[string]any{"dns": []any{"10.0.0.4", "10.0.0.5"}, "tags": map[string]any{"env": "prod", "team": "x"}, "network": map[string]any{"cidr": "10.1.0.0/16", "dns": true}})
	// the form's rows; an empty list row is dropped, a key without a value is "" (a map's value may be empty)
	ok(map[string]string{"dns": `[{"value":"10.0.0.4"},{"value":""}]`, "tags": `[{"key":"env","value":"dev"},{"key":"x","value":""}]`, "network": `{"cidr":"10.2.0.0/16","size":"3"}`},
		map[string]any{"dns": []any{"10.0.0.4"}, "tags": map[string]any{"env": "dev", "x": ""}, "network": map[string]any{"cidr": "10.2.0.0/16", "dns": false, "size": 3.0}})
	for raw, want := range map[string]map[string]string{
		`dns`:     {"dns": `[{"value":"abc"}]`, "network": `{"cidr":"x"}`},
		`tags`:    {"tags": `[{"key":"a","value":"1"},{"key":"a","value":"2"}]`, "network": `{"cidr":"x"}`},
		`network`: {"network": `{"size":"x"}`},
		`sealed`:  {"tags": `{"sealed":"x"}`, "network": `{"cidr":"x"}`},
	} {
		_, errs := tpl.Validate(want)
		msg := map[string]string{"dns": "Row 1, Value: An IPv4 address", "tags": "Row 2, Key: a is there already.",
			"network": "CIDR: This field is required.", "sealed": "A map with only the key sealed can't be stored."}[raw]
		field := map[string]string{"dns": "dns", "tags": "tags", "network": "network", "sealed": "tags"}[raw]
		if errs[field] != msg {
			t.Errorf("%s: %q, want %q (%v)", raw, errs[field], msg, errs)
		}
	}
	if !IsSealed(map[string]any{"sealed": "eA=="}) || IsSealed(map[string]any{"sealed": "eA==", "env": "x"}) || IsSealed(map[string]any{"sealed": 1.0}) || IsSealed("x") {
		t.Error("IsSealed: only {\"sealed\": string}")
	}
	got := tpl.Describe(map[string]any{"dns": []any{"10.0.0.4", "10.0.0.5"}, "tags": map[string]any{"team": "x", "env": "prod"},
		"network": map[string]any{"cidr": "10.0.0.0/16"}})
	if got[0].Text != "10.0.0.4\n10.0.0.5" || got[1].Text != "env: prod\nteam: x" || got[2].Text != "CIDR: 10.0.0.0/16" || got[1].Sensitive {
		t.Errorf("described: %+v", got)
	}
	b, _ := json.Marshal(tpl)
	var back Template
	if json.Unmarshal(b, &back) != nil || !reflect.DeepEqual(back.Inputs, tpl.Inputs) {
		t.Errorf("round trip:\n%s", b)
	}
	for _, bad := range []string{`{"name": "o", "type": "object"}`, `{"name": "m", "type": "map", "fields": [{"name": "a", "type": "string"}]}`,
		`{"name": "m", "type": "map", "per_environment": true}`, `{"name": "s", "type": "string_list", "default": [1]}`} {
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
