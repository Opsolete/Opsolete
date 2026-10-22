package templates

import "testing"

func TestLoadCatalogAndValidate(t *testing.T) {
	ts, err := LoadDir("../../catalog")
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
		{ID: "ok", Name: "x", Module: tpl.Module, Inputs: []Input{{Name: "r", Type: "select", Options: []string{"a"}, Default: "b"}}},
	}
	for _, b := range bad {
		if err := c.Save(b, false); err == nil {
			t.Errorf("expected error for %+v", b)
		}
	}
}
