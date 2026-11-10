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
		{ID: "ok", Name: "x", Module: tpl.Module, Inputs: []Input{{Name: "b", Type: "bool"}, {Name: "s", Type: "string", ShowIf: &ShowIf{Input: "b", Equals: []string{"yes"}}}}},
		{ID: "ok", Name: "x", Module: tpl.Module, Inputs: []Input{{Name: "r", Type: "select", Options: []Option{{Value: "a"}}}, {Name: "s", Type: "string", ShowIf: &ShowIf{Input: "r", Equals: []string{"b"}}}}},
		{ID: "ok", Name: "x", Module: tpl.Module, Inputs: []Input{{Name: "r", Type: "select", Options: []Option{{Value: "a"}}}, {Name: "s", Type: "string", ShowIf: &ShowIf{Input: "r"}}}},
		{ID: "ok", Name: "x", Module: tpl.Module, Inputs: []Input{{Name: "n", Type: "number", Default: "seven"}}},
		{ID: "ok", Name: "x", Module: tpl.Module, Inputs: []Input{{Name: "n", Type: "number", Default: "NaN"}}},
		{ID: "ok", Name: "x", Module: tpl.Module, Inputs: []Input{{Name: "s", Type: "string", Pattern: "[a-z]+", Default: "ABC"}}},
		{ID: "ok", Name: "x", Module: tpl.Module, Inputs: []Input{{Name: "b", Type: "bool", Default: "yes"}}},
		{ID: "ok", Name: "x", Module: tpl.Module, Inputs: []Input{{Name: "b", Type: "bool", Sensitive: true}}},
		{ID: "ok", Name: "x", Module: tpl.Module, Inputs: []Input{{Name: "p", Type: "string", Sensitive: true, Default: "hunter2"}}},
		{ID: "ok", Name: "x", Module: tpl.Module, Inputs: []Input{{Name: "a", Type: "string", Key: true}, {Name: "b", Type: "string", Key: true}}},
		{ID: "ok", Name: "x", Module: tpl.Module, Inputs: []Input{{Name: "b", Type: "bool", Key: true}}},
		{ID: "ok", Name: "x", Module: tpl.Module, Inputs: []Input{{Name: "p", Type: "string", Sensitive: true, Key: true}}},
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

// A yes / no field can show others; unchecked (nothing posted) is "false".
func TestShowIfBool(t *testing.T) {
	tpl := Template{ID: "b", Name: "b", Module: Module{Source: "s", Ref: "r"}, Inputs: []Input{{Name: "backup", Type: "bool"},
		{Name: "days", Type: "number", Required: true, ShowIf: &ShowIf{Input: "backup", Equals: []string{"true"}}},
		{Name: "why", Type: "string", Required: true, ShowIf: &ShowIf{Input: "backup", Equals: []string{"false"}}}}}
	if err := tpl.check(); err != nil {
		t.Fatal(err)
	}
	for raw, want := range map[string]string{"on": "days", "true": "days", "": "why"} {
		if _, errs := tpl.Validate(map[string]string{"backup": raw}); len(errs) != 1 || errs[want] == "" {
			t.Errorf("backup=%q: %v, want only %s required", raw, errs, want)
		}
	}
}

func TestDescribe(t *testing.T) {
	tpl := Template{Inputs: []Input{{Name: "size", Label: "Size", Type: "select", Options: []Option{{Label: "Small", Value: "s"}}},
		{Name: "pw", Sensitive: true}, {Name: "backup", Type: "bool"}, {Name: "disk", Type: "number"}}}
	got := fmt.Sprint(tpl.Describe(map[string]any{"disk": 1e6, "old": "x", "backup": true, "pw": map[string]any{"sealed": "c2"}, "size": "s"}))
	if want := "[{size Size Small false } {pw pw  true } {backup backup Yes false } {disk disk 1000000 false } {old old x false }]"; got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestSensitiveKeepsSpacesAndFormatValue(t *testing.T) {
	tpl := Template{Inputs: []Input{{Name: "pw", Type: "string", Sensitive: true}, {Name: "pin", Type: "number", Sensitive: true}, {Name: "s", Type: "string"}}}
	vals, errs := tpl.Validate(map[string]string{"pw": " hunter2 ", "pin": " 42 ", "s": " x "})
	if len(errs) != 0 || vals["pw"] != " hunter2 " || vals["pin"] != 42.0 || vals["s"] != "x" {
		t.Errorf("got %v %v", vals, errs)
	}
	// only spaces is no new value: sealInputs then keeps the stored one
	if vals, _ := tpl.Validate(map[string]string{"pw": "   "}); vals["pw"] != nil {
		t.Errorf("spaces-only password became a value: %q", vals["pw"])
	}
	for v, want := range map[any]string{1e6: "1000000", 1.5: "1.5", true: "true", "a": "a"} {
		if got := FormatValue(v); got != want {
			t.Errorf("FormatValue(%v) = %q, want %q", v, got, want)
		}
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
	if errs["admin_password"] != "This field is required." || len(errs) != 1 || vals["ssh_key"] != nil || vals["monitoring"] != nil {
		t.Errorf("windows: %v %v", vals, errs)
	}
	// a chain: workspace_id shows only with monitoring on, which shows only on linux
	if _, errs = vm.Validate(map[string]string{"os": "linux", "ssh_key": "k", "monitoring": "on"}); errs["workspace_id"] != "This field is required." {
		t.Errorf("chain: %v", errs)
	}
	// an empty dropdown counts as its first option, as the browser shows it
	if _, errs = vm.Validate(map[string]string{}); errs["admin_password"] != "This field is required." || errs["ssh_key"] != "" {
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

func TestAdoptIDs(t *testing.T) {
	ts, _, err := Parse(map[string][]byte{"vm.json": []byte(`{"id": "vm", "name": "VM", "module": {"source": "s", "ref": "r"}, "inputs": [
		{"name": "os_type", "type": "select", "options": ["windows", "linux"]}],
		"adopt": [
		{"to": "azurerm_windows_virtual_machine.self[0]", "pattern": "(?i)/subscriptions/[^/]+/resourceGroups/[^/]+/providers/Microsoft\\.Compute/virtualMachines/[^/]+",
			"show_if": {"input": "os_type", "equals": ["windows"]}},
		{"to": "azurerm_linux_virtual_machine.self[0]", "show_if": {"input": "os_type", "equals": ["linux"]}},
		{"to": "azurerm_network_interface.self", "help": "A NIC ID", "pattern": "/subscriptions/.+/networkInterfaces/[^/]+"}]}`)})
	if err != nil {
		t.Fatal(err)
	}
	vm := ts["vm"]
	const vmID, nicID = "/subscriptions/s/resourcegroups/rg/providers/Microsoft.Compute/virtualMachines/vm", "/subscriptions/s/resourceGroups/rg/providers/Microsoft.Network/networkInterfaces/nic"
	win := map[string]string{"os_type": "windows"}

	if got, errs := vm.AdoptIDs(win, []string{" ", "", ""}); got != nil || len(errs) != 0 {
		t.Errorf("no IDs: create, got %v %v", got, errs)
	}
	// the linux VM's field is hidden on windows: what it holds is ignored, also for "any given"
	if got, errs := vm.AdoptIDs(win, []string{"", "stale", ""}); got != nil || len(errs) != 0 {
		t.Errorf("only a hidden ID: %v %v", got, errs)
	}
	got, errs := vm.AdoptIDs(win, []string{" " + vmID + " ", "stale", nicID})
	if len(errs) != 0 || len(got) != 2 || got["azurerm_windows_virtual_machine.self[0]"] != vmID || got["azurerm_network_interface.self"] != nicID {
		t.Errorf("windows: %v %v", got, errs)
	}
	if _, errs = vm.AdoptIDs(win, []string{vmID}); errs[AdoptField(2)] == "" || len(errs) != 1 {
		t.Errorf("one given, one missing: %v", errs)
	}
	if _, errs = vm.AdoptIDs(win, []string{nicID, "", vmID}); !strings.Contains(errs[AdoptField(0)], "expected format") || errs[AdoptField(2)] != "A NIC ID" {
		t.Errorf("wrong type pasted: %v", errs)
	}
	if _, errs = vm.AdoptIDs(win, []string{vmID + "\nx", "", nicID}); errs[AdoptField(0)] != "This is not a resource ID." {
		t.Errorf("line break: %v", errs)
	}
	if got, errs = vm.AdoptIDs(map[string]string{"os_type": "linux"}, []string{"", "anything", nicID}); len(errs) != 0 || got["azurerm_linux_virtual_machine.self[0]"] != "anything" {
		t.Errorf("linux, no pattern: %v %v", got, errs)
	}
}
