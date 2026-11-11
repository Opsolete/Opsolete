package runner

import (
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	tfjson "github.com/hashicorp/terraform-json"

	"github.com/opsolete/opsolete/internal/runnerapi"
)

// TestEscapeValue: nested strings and object keys are escaped too, so list/object inputs can't inject.
func TestEscapeValue(t *testing.T) {
	got := escapeValue(map[string]any{"${k}": []any{"%{x}", 1, map[string]any{"a": "${file(\"/etc/passwd\")}"}}})
	want := map[string]any{"$${k}": []any{"%%{x}", 1, map[string]any{"a": "$${file(\"/etc/passwd\")}"}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v", got)
	}
}

// TestAddedTemplates: templates added to a workspace become module blocks, with or without a root
// module, their outputs root outputs, and form values stay values.
func TestAddedTemplates(t *testing.T) {
	f := &fakeServer{}
	srv := httptest.NewServer(f)
	defer srv.Close()
	r := testRunner(t, srv)
	src := f.module(t, helloModule)
	evil := `${file("/etc/passwd")}`
	mods := []runnerapi.Module{
		{Name: "a", Source: src, Inputs: map[string]any{"greeting": evil}},
		{Name: "b-2", Source: src, Inputs: map[string]any{"greeting": "hi"}},
	}
	j := job("m1", "plan", "apply", "", nil)
	j.Modules = mods
	res, logs := f.do(t, r, j)
	if res.Status != "succeeded" || res.Plan == nil || res.Plan.Add != 2 {
		t.Fatalf("plan: %+v\n%s", res, logs)
	}
	j.Phase = "apply"
	res, logs = f.do(t, r, j)
	if res.Status != "succeeded" || res.Outputs["a__msg"] != evil || res.Outputs["b-2__msg"] != "hi" || res.Outputs["a__secret"] != "(sensitive)" {
		t.Fatalf("apply: %+v\n%s", res, logs)
	}
	if len(res.Resources) != 2 || res.Resources[0].Address != "module.a.terraform_data.this" {
		t.Fatalf("resources: %+v", res.Resources)
	}
	if strings.Contains(logs, "secret-signature") || strings.Contains(logs, "hunter2") {
		t.Fatalf("leaked into the logs:\n%s", logs)
	}

	// Next to a root module, and with one template removed: its resources are destroyed.
	root := job("m2", "plan", "apply", src, map[string]any{"greeting": "root"})
	root.Modules = mods[:1]
	if res, logs = f.do(t, r, root); res.Status != "succeeded" || res.Plan == nil || res.Plan.Add != 1 || res.Plan.Destroy != 1 {
		t.Fatalf("plan with root module: %+v\n%s", res, logs)
	}

	// Removing a template: the destroy limited to it also drops its outputs, which Terraform keeps.
	d := job("m3", "plan", "destroy", "", nil)
	d.Modules, d.Target = mods, []string{"module.a"}
	if res, logs = f.do(t, r, d); res.Status != "succeeded" || res.Plan == nil || res.Plan.Destroy != 1 {
		t.Fatalf("destroy plan: %+v\n%s", res, logs)
	}
	d.Phase = "apply"
	res, logs = f.do(t, r, d)
	var st struct{ Outputs map[string]any }
	json.Unmarshal(f.states["ws1"], &st)
	if _, kept := st.Outputs["a__msg"]; res.Status != "succeeded" || kept || st.Outputs["b-2__msg"] == nil || len(res.Resources) != 1 ||
		!strings.Contains(logs, "terraform apply (outputs only)") || strings.Contains(logs, "hunter2") {
		t.Fatalf("destroy apply: %+v, outputs %v\n%s", res, st.Outputs, logs)
	}
	// the last one: no configuration is left
	d = job("m4", "plan", "destroy", "", nil)
	d.Modules, d.Target = mods[1:], []string{"module.b-2"}
	f.do(t, r, d)
	d.Phase = "apply"
	res, logs = f.do(t, r, d)
	st.Outputs = nil
	json.Unmarshal(f.states["ws1"], &st)
	if res.Status != "succeeded" || len(st.Outputs) != 0 || len(res.Resources) != 0 || strings.Contains(logs, "outputs kept") {
		t.Fatalf("destroy of the last template: %+v, outputs %v\n%s", res, st.Outputs, logs)
	}
}

// TestImports: a template's module imports an existing resource itself (an import block switched by an input,
// Terraform ≥ 1.16); the plan counts and lists the import (an import with changes is also an update), and the
// apply imports it.
func TestImports(t *testing.T) {
	f := &fakeServer{}
	srv := httptest.NewServer(f)
	defer srv.Close()
	r := testRunner(t, srv)
	j := job("i1", "plan", "apply", "", nil)
	j.Modules = []runnerapi.Module{{Name: "a", Source: f.module(t, `
variable "greeting" { type = string }
variable "existing" { default = false }
resource "terraform_data" "this" { input = var.greeting }
import {
  for_each = var.existing ? toset(["this"]) : toset([])
  to       = terraform_data.this
  id       = "existing-${var.greeting}"
}`), Inputs: map[string]any{"greeting": "hi", "existing": true}}}
	res, logs := f.do(t, r, j)
	want := []runnerapi.Change{{Address: "module.a.terraform_data.this", Action: "update"}} // terraform_data imports without its input
	imports := []runnerapi.Import{{To: "module.a.terraform_data.this", ID: "existing-hi"}}
	if res.Status != "succeeded" || res.Plan == nil || res.Plan.Import != 1 || res.Plan.Add != 0 || !reflect.DeepEqual(res.Plan.Changes, want) ||
		!reflect.DeepEqual(res.Plan.Imports, imports) {
		t.Fatalf("plan: %+v\n%s", res.Plan, logs)
	}
	if !strings.Contains(logs, "plan saved: 1 to import, 0 to add, 1 to change, 0 to destroy") {
		t.Fatalf("plan log:\n%s", logs)
	}
	j.Phase = "apply"
	if res, logs = f.do(t, r, j); res.Status != "succeeded" || len(res.Resources) != 1 || res.Resources[0].ID != "existing-hi" {
		t.Fatalf("apply: %+v\n%s", res, logs)
	}
}

// TestSummarizeImport: an import without changes is listed as an import, not dropped like other no-ops.
func TestSummarizeImport(t *testing.T) {
	rc := func(addr string, imported bool, actions ...tfjson.Action) *tfjson.ResourceChange {
		c := &tfjson.Change{Actions: actions}
		if imported {
			c.Importing = &tfjson.Importing{ID: "x"}
		}
		return &tfjson.ResourceChange{Address: addr, Change: c}
	}
	got := summarize([]*tfjson.ResourceChange{rc("a", true, tfjson.ActionNoop), rc("b", false, tfjson.ActionNoop), rc("c", true, tfjson.ActionUpdate)})
	want := runnerapi.Plan{Change: 1, Import: 2, Imports: []runnerapi.Import{{To: "a", ID: "x"}, {To: "c", ID: "x"}},
		Changes: []runnerapi.Change{{Address: "a", Action: "import"}, {Address: "c", Action: "update"}}}
	if !reflect.DeepEqual(got, want) || counts(got) != "2 to import, 0 to add, 1 to change, 0 to destroy" {
		t.Fatalf("got %+v, %q", got, counts(got))
	}
}
