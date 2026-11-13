package templates

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// TestSchema keeps docs/static/schema/template-1.json in agreement with the loader, without a schema validator:
// (a) the field names of each object match the Go structs Template's UnmarshalJSON decodes, (b) the input type enum
// matches InputTypes, (c) a table of templates Parse accepts or rejects, each rejection listing the schema keyword that
// rejects it too. Limit: (c) documents the schema side and doesn't run it; a wrong keyword there passes this test.
func TestSchema(t *testing.T) {
	b, err := os.ReadFile("../../docs/static/schema/template-1.json")
	if err != nil {
		t.Fatal(err)
	}
	type object struct {
		ID         string                     `json:"$id"`
		Properties map[string]json.RawMessage `json:"properties"`
		OneOf      []object                   `json:"oneOf"`
		Enum       []string                   `json:"enum"`
	}
	var schema struct {
		object
		Defs map[string]object `json:"$defs"`
	}
	if err := json.Unmarshal(b, &schema); err != nil {
		t.Fatal(err)
	}
	if schema.ID != SchemaURL {
		t.Errorf("$id %s, want SchemaURL %s", schema.ID, SchemaURL)
	}

	// (a) every JSON field the loader accepts is in the schema, and the other way round
	for name, c := range map[string]struct {
		props map[string]json.RawMessage
		goT   any
	}{
		"template": {schema.Properties, fileTemplate{}},
		"module":   {schema.Defs["module"].Properties, Module{}},
		"input":    {schema.Defs["input"].Properties, fileInput{}},
		"option":   {schema.Defs["option"].OneOf[1].Properties, plainOption{}},
		"show_if":  {schema.Defs["show_if"].Properties, fileShowIf{}},
	} {
		var goFields []string
		rt := reflect.TypeOf(c.goT)
		for i := range rt.NumField() {
			tag, _, _ := strings.Cut(rt.Field(i).Tag.Get("json"), ",")
			goFields = append(goFields, tag)
		}
		slices.Sort(goFields)
		if got := slices.Sorted(func(yield func(string) bool) {
			for k := range c.props {
				if !yield(k) {
					return
				}
			}
		}); !slices.Equal(got, goFields) {
			t.Errorf("%s: schema properties %v, Go fields %v", name, got, goFields)
		}
	}

	// (b) the input types
	var typ object
	if err := json.Unmarshal(schema.Defs["input"].Properties["type"], &typ); err != nil || !slices.Equal(typ.Enum, InputTypes) {
		t.Errorf("type enum %v, InputTypes %v (%v)", typ.Enum, InputTypes, err)
	}

	// (c) one case per rule; schema: the keyword that rejects it in template-1.json ("" for a valid template)
	const head = `{"id": "t", "name": "T", "module": {"source": "s", "ref": "r"}, "inputs": [`
	for _, c := range []struct{ name, inputs, schema, err string }{
		{"valid", `{"name": "s", "label": "S", "help": "h", "type": "string", "required": true, "pattern": "[a-z]+", "default": "abc", "key": true},
			{"name": "n", "type": "number", "default": 0.5, "per_environment": true}, {"name": "b", "type": "bool", "default": false},
			{"name": "o", "type": "select", "options": ["a", {"value": "b", "label": "B"}], "default": "b", "show_if": {"input": "b", "equals": [true]}},
			{"name": "p", "type": "string", "sensitive": true, "show_if": {"input": "o", "equals": ["a", "b"]}}`, "", ""},
		{"number default as a string", `{"name": "n", "type": "number", "default": "7"}`,
			"input allOf[1] then default type number", `template t, input n: default must be a JSON number (7, not "7")`},
		{"bool default as a string", `{"name": "b", "type": "bool", "default": "true"}`,
			"input allOf[2] then default type boolean", `template t, input b: default must be a JSON boolean (true, not "true")`},
		{"string default as a number", `{"name": "s", "type": "string", "default": 7}`,
			"input allOf[0] then default type string", `template t, input s: default must be a JSON string ("7", not 7)`},
		{"select default as a number", `{"name": "o", "type": "select", "options": ["1"], "default": 1}`,
			"input allOf[0] then default type string", `default must be a JSON string ("1", not 1)`},
		{"show_if on a bool as a string", `{"name": "b", "type": "bool"}, {"name": "s", "type": "string", "show_if": {"input": "b", "equals": ["true"]}}`,
			"loader only: equals items are string or boolean", `template t, input s: show_if.equals: each value must be a JSON boolean (true, not "true")`},
		{"show_if on a select as a boolean", `{"name": "o", "type": "select", "options": ["a"]}, {"name": "s", "type": "string", "show_if": {"input": "o", "equals": [true]}}`,
			"loader only: equals items are string or boolean", `show_if.equals: each value must be a JSON string ("true", not true)`},
		{"show_if without values", `{"name": "o", "type": "select", "options": ["a"]}, {"name": "s", "type": "string", "show_if": {"input": "o", "equals": []}}`,
			"show_if equals minItems", "pick at least one value"},
		{"unknown input field", `{"name": "s", "type": "string", "lable": "S"}`, "input additionalProperties", `template t, input s: unknown field "lable"`},
		{"unknown option field", `{"name": "o", "type": "select", "options": [{"value": "a", "text": "A"}]}`,
			"option oneOf[1] additionalProperties", `template t, input o: option: unknown field "text"`},
		{"unknown show_if field", `{"name": "o", "type": "select", "options": ["a"]}, {"name": "s", "type": "string", "show_if": {"input": "o", "equal": ["a"]}}`,
			"show_if additionalProperties", `unknown field "equal"`},
		{"unknown module field", ``, "module additionalProperties", `template t: module: unknown field "version"`},
		{"unknown top-level field", ``, "additionalProperties", `template t: unknown field "tags"`},
		{"missing name", ``, "required name", "name is required"},
		{"input without a type", `{"name": "s"}`, "input required type", `unknown type ""`},
		{"unknown type", `{"name": "s", "type": "text"}`, "input type enum", `unknown type "text"`},
		{"select without options", `{"name": "o", "type": "select"}`, "input allOf[3] then required options", "select needs options"},
		{"option without a value", `{"name": "o", "type": "select", "options": [{"label": "A"}]}`, "option oneOf[1] required value", "an option needs a value"},
		{"bad input name", `{"name": "vm-size", "type": "string"}`, "input name pattern", "invalid input name"},
		{"reserved input name", `{"name": "count", "type": "string"}`, "input name not enum", "invalid input name"},
		{"sensitive bool", `{"name": "b", "type": "bool", "sensitive": true}`, "input allOf[2] then sensitive const false", "only string and number"},
		{"per_environment sensitive", `{"name": "p", "type": "string", "sensitive": true, "per_environment": true}`,
			"input allOf[5] then sensitive const false", `template "t", input "p": a per-environment input can't be sensitive`},
		{"per_environment key", `{"name": "k", "type": "string", "key": true, "per_environment": true}`,
			"input allOf[5] then key const false", `template "t", input "k": a per-environment input can't be the key`},
		{"sensitive with a default", `{"name": "p", "type": "string", "sensitive": true, "default": "x"}`, "input allOf[4] then not required default", "can't have a default"},
	} {
		file := head + c.inputs + `]}`
		switch c.name {
		case "unknown module field":
			file = `{"id": "t", "name": "T", "module": {"source": "s", "ref": "r", "version": "1"}}`
		case "unknown top-level field":
			file = `{"$schema": "x", "id": "t", "name": "T", "module": {"source": "s", "ref": "r"}, "tags": []}`
		case "missing name":
			file = `{"id": "t", "module": {"source": "s", "ref": "r"}}`
		}
		_, _, err := Parse(map[string][]byte{"t.json": []byte(file)})
		switch {
		case c.err == "" && err != nil:
			t.Errorf("%s: %v", c.name, err)
		case c.err != "" && (err == nil || !strings.Contains(err.Error(), c.err)):
			t.Errorf("%s: got %v, want %q (schema: %s)", c.name, err, c.err, c.schema)
		}
	}
}

// What Parse reads, Encode writes back the same: typed values in the file, the same strings in Go.
func TestJSONRoundTrip(t *testing.T) {
	file := []byte(`{"$schema": "` + SchemaURL + `", "id": "t", "name": "T", "description": "", "module": {"source": "s", "ref": "r"}, "inputs": [
		{"name": "n", "label": "N", "help": "h", "type": "number", "required": false, "default": 1e21, "per_environment": true},
		{"name": "f", "label": "", "type": "number", "required": false, "default": 0.5},
		{"name": "b", "label": "", "type": "bool", "required": false, "default": true},
		{"name": "s", "label": "", "type": "string", "required": false, "default": "7", "show_if": {"input": "b", "equals": [false]}}]}`)
	ts, _, err := Parse(map[string][]byte{"t.json": file})
	if err != nil {
		t.Fatal(err)
	}
	got := ts["t"]
	if !got.Inputs[0].PerEnvironment || got.Inputs[1].PerEnvironment {
		t.Errorf("per_environment: %+v", got.Inputs[:2])
	}
	if d := []string{got.Inputs[0].Default, got.Inputs[1].Default, got.Inputs[2].Default, got.Inputs[3].Default, got.Inputs[3].ShowIf.Equals[0]}; !slices.Equal(d, []string{"1000000000000000000000", "0.5", "true", "7", "false"}) {
		t.Errorf("canonical strings: %q", d)
	}
	b, err := Encode(got)
	if err != nil {
		t.Fatal(err)
	}
	var compact bytes.Buffer
	json.Compact(&compact, b)
	for _, want := range []string{`"default":1e+21`, `"default":0.5`, `"default":true`, `"default":"7"`, `"equals":[false]`, `"per_environment":true`, `"$schema":"` + SchemaURL + `"`} {
		if !strings.Contains(compact.String(), want) {
			t.Errorf("encoded lacks %s:\n%s", want, b)
		}
	}
	again, _, err := Parse(map[string][]byte{"t.json": b})
	if err != nil || !reflect.DeepEqual(again["t"], got) {
		t.Errorf("round trip: %v\n%+v\n%+v", err, again["t"], got)
	}
	// the form's show_if data stays strings: the browser compares them with field values
	if j, _ := json.Marshal(got.Inputs[3].ShowIf); string(j) != `{"input":"b","equals":["false"]}` {
		t.Errorf("show_if JSON for the form: %s", j)
	}
	// every catalog file names the schema
	for _, f := range []string{"greeting", "resourcegroup", "storage-account"} {
		b, _ := os.ReadFile("../../catalog/" + f + ".json")
		var s struct {
			Schema string `json:"$schema"`
		}
		if json.Unmarshal(b, &s); s.Schema != SchemaURL {
			t.Errorf("catalog/%s.json: $schema %q", f, s.Schema)
		}
	}
}

// A template that claims import support: its switch is a bool that starts off, the same everywhere, and the
// template has a key.
func TestImportSwitch(t *testing.T) {
	key := `{"name": "name", "type": "string", "key": true}`
	for in, want := range map[string]string{
		`{"name": "existing", "type": "string"}, ` + key:                        "is a bool",
		`{"name": "existing", "type": "bool", "default": true}, ` + key:         "starts off",
		`{"name": "existing", "type": "bool", "per_environment": true}, ` + key: "can't be per environment",
		`{"name": "existing", "type": "bool"}`:                                  "", // no key: the module finds it by other inputs (a VM by name and resource group)
		`{"name": "existing", "type": "bool", "default": false}, ` + key:        "",
	} {
		_, _, err := Parse(map[string][]byte{"t.json": []byte(`{"id": "t", "name": "T", "module": {"source": "s", "ref": "r"}, "inputs": [` + in + `]}`)})
		if want == "" && err != nil || want != "" && (err == nil || !strings.Contains(err.Error(), want)) {
			t.Errorf("%s: %v, want %q", in, err, want)
		}
	}
}
