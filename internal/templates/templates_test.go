package templates

import (
	"fmt"
	"strings"
	"testing"
)

func TestLoadCatalogAndValidate(t *testing.T) {
	ts, _, err := loadDir("../../catalog")
	if err != nil {
		t.Fatal(err)
	}
	tpl, ok := ts["storage-account"]
	if !ok {
		t.Fatal("storage-account template missing")
	}

	vals, errs := tpl.Validate(map[string]string{"name": "mystore01", "location": "westeurope", "retention_days": "7"})
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if vals["retention_days"] != 7.0 || vals["enable_versioning"] != false {
		t.Fatalf("unexpected values: %v", vals)
	}

	_, errs = tpl.Validate(map[string]string{"name": "BAD NAME", "location": "mars", "retention_days": "x"})
	for _, f := range []string{"name", "location", "retention_days"} {
		if errs[f] == "" {
			t.Errorf("expected error for %s", f)
		}
	}
}

func TestCatalogSave(t *testing.T) {
	dir := t.TempDir()
	c, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	tpl := Template{ID: "vnet", Name: "VNet", Module: Module{Source: "git::x", Ref: "v1"},
		Inputs: []Input{{Name: "cidr", Type: "string", Required: true}}}
	if err := c.Save(tpl, false); err != nil {
		t.Fatal(err)
	}
	if err := c.Save(tpl, false); err == nil {
		t.Fatal("expected error creating an existing id")
	}
	tpl.Name = "Virtual network"
	if err := c.Save(tpl, true); err != nil {
		t.Fatal(err)
	}

	// What was written must load back identically, the same way startup loads it.
	again, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := again.Get("vnet"); !ok || got.Name != "Virtual network" || len(got.Inputs) != 1 {
		t.Fatalf("reloaded: %+v", got)
	}

	bad := []Template{
		{ID: "new", Name: "x", Module: tpl.Module},
		{ID: "ok", Name: "x", Module: tpl.Module, Inputs: []Input{{Name: "a", Type: "string"}, {Name: "a", Type: "string"}}},
		{ID: "ok", Name: "x", Module: tpl.Module, Inputs: []Input{{Name: "r", Type: "select", Options: []Option{{Value: "a"}}, Default: "b"}}},
		{ID: "ok", Name: "x", Module: tpl.Module, Inputs: []Input{{Name: "s", Type: "string", ShowIf: &ShowIf{Input: "later", Equals: []string{"a"}}}, {Name: "later", Type: "select", Options: []Option{{Value: "a"}}}}},
		{ID: "ok", Name: "x", Module: tpl.Module, Inputs: []Input{{Name: "t", Type: "string"}, {Name: "s", Type: "string", ShowIf: &ShowIf{Input: "t", Equals: []string{"x"}}}}},
		{ID: "ok", Name: "x", Module: tpl.Module, Inputs: []Input{{Name: "b", Type: "bool"}, {Name: "s", Type: "string", ShowIf: &ShowIf{Input: "b", Equals: []string{"true"}}}}},
		{ID: "ok", Name: "x", Module: tpl.Module, Inputs: []Input{{Name: "r", Type: "select", Options: []Option{{Value: "a"}}}, {Name: "s", Type: "string", ShowIf: &ShowIf{Input: "r", Equals: []string{"b"}}}}},
		{ID: "ok", Name: "x", Module: tpl.Module, Inputs: []Input{{Name: "r", Type: "select", Options: []Option{{Value: "a"}}}, {Name: "s", Type: "string", ShowIf: &ShowIf{Input: "r"}}}},
		{ID: "ok", Name: "x", Module: tpl.Module, Inputs: []Input{{Name: "n", Type: "number", Default: "seven"}}},
		{ID: "ok", Name: "x", Module: tpl.Module, Inputs: []Input{{Name: "n", Type: "number", Default: "NaN"}}},
		{ID: "ok", Name: "x", Module: tpl.Module, Inputs: []Input{{Name: "s", Type: "string", Pattern: "[a-z]+", Default: "ABC"}}},
		{ID: "ok", Name: "x", Module: tpl.Module, Inputs: []Input{{Name: "b", Type: "bool", Default: "yes"}}},
		{ID: "ok", Name: "x", Module: tpl.Module, Inputs: []Input{{Name: "b", Type: "bool", Sensitive: true}}},
		{ID: "ok", Name: "x", Module: tpl.Module, Inputs: []Input{{Name: "p", Type: "string", Sensitive: true, Default: "hunter2"}}},
	}
	for _, b := range bad {
		if err := c.Save(b, false); err == nil {
			t.Errorf("expected error for %+v", b)
		}
	}
}

func TestValidateEdgeCases(t *testing.T) {
	tpl := Template{Inputs: []Input{{Name: "n", Type: "number"}, {Name: "s", Type: "string", Pattern: "[a-z]{3}"}}}
	for _, raw := range []map[string]string{{"n": "NaN"}, {"n": "Inf"}, {"s": "ABCabc!!"}} {
		if _, errs := tpl.Validate(raw); len(errs) == 0 {
			t.Errorf("%v must be rejected", raw)
		}
	}
	if _, errs := tpl.Validate(map[string]string{"n": "1.5", "s": "abc"}); len(errs) != 0 {
		t.Errorf("valid values rejected: %v", errs)
	}
	bad := Template{ID: "x", Name: "x", Module: Module{Source: "s", Ref: "r"}, Inputs: []Input{{Name: "source", Type: "string"}}}
	if bad.check() == nil {
		t.Error("an input named source must be rejected")
	}
}

func TestShowIf(t *testing.T) {
	files := map[string][]byte{"vm.json": []byte(`{"id": "vm", "name": "VM", "module": {"source": "s", "ref": "r"}, "inputs": [
		{"name": "os", "type": "select", "options": [{"label": "Windows", "value": "windows"}, "linux"], "default": "linux"},
		{"name": "admin_password", "type": "string", "required": true, "sensitive": true, "show_if": {"input": "os", "equals": ["windows"]}},
		{"name": "ssh_key", "type": "string", "required": true, "show_if": {"input": "os", "equals": ["linux"]}},
		{"name": "monitoring", "type": "select", "options": ["off", "on"], "show_if": {"input": "os", "equals": ["linux"]}},
		{"name": "workspace_id", "type": "string", "required": true, "show_if": {"input": "monitoring", "equals": ["on"]}}]}`)}
	ts, _, err := Parse(files)
	if err != nil {
		t.Fatal(err)
	}
	vm := ts["vm"]

	vals, errs := vm.Validate(map[string]string{"os": "linux", "ssh_key": "k", "admin_password": "ignored", "monitoring": "off"})
	if len(errs) != 0 || fmt.Sprint(vals) != "map[monitoring:off os:linux ssh_key:k]" {
		t.Errorf("linux: %v %v", vals, errs)
	}
	// windows: the password; ssh_key and monitoring (and so workspace_id) are hidden
	vals, errs = vm.Validate(map[string]string{"os": "windows", "ssh_key": "k", "monitoring": "on"})
	if errs["admin_password"] != "required" || len(errs) != 1 || vals["ssh_key"] != nil || vals["monitoring"] != nil {
		t.Errorf("windows: %v %v", vals, errs)
	}
	// a chain: workspace_id shows only with monitoring on, which shows only on linux
	if _, errs = vm.Validate(map[string]string{"os": "linux", "ssh_key": "k", "monitoring": "on"}); errs["workspace_id"] != "required" {
		t.Errorf("chain: %v", errs)
	}
	// an empty dropdown counts as its first option, as the browser shows it
	if _, errs = vm.Validate(map[string]string{}); errs["admin_password"] != "required" || errs["ssh_key"] != "" {
		t.Errorf("empty dropdown: %v", errs)
	}

	// Plain options stay plain strings when written back.
	b, err := Encode(vm)
	if err != nil || !strings.Contains(string(b), `"linux"`) || !strings.Contains(string(b), `"label": "Windows"`) {
		t.Errorf("encode: %v\n%s", err, b)
	}
}

// A catalog over the limits fails before any file is parsed.
func TestParseLimits(t *testing.T) {
	big := map[string][]byte{"a.json": make([]byte, MaxFileSize+1)}
	many := map[string][]byte{}
	for i := range MaxCount + 1 {
		many[fmt.Sprintf("%d.json", i)] = []byte("{}")
	}
	total := map[string][]byte{}
	for i := range MaxTotalSize/MaxFileSize + 1 {
		total[fmt.Sprintf("%d.json", i)] = make([]byte, MaxFileSize)
	}
	for name, files := range map[string]map[string][]byte{"file": big, "count": many, "total": total} {
		if _, _, err := Parse(files); err == nil || !strings.Contains(err.Error(), "at most") && !strings.Contains(err.Error(), "total over") {
			t.Errorf("%s limit: %v", name, err)
		}
	}
}
