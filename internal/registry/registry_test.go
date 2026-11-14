package registry

import (
	"fmt"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-config-inspect/tfconfig"

	"github.com/opsolete/opsolete/internal/registry/registrytest"
	"github.com/opsolete/opsolete/internal/templates"
)

var TarGz = registrytest.TarGz

const moduleTF = `
variable "name" {
  type        = string
  description = "Account name"

  validation {
    condition     = length(var.name) < 64
    error_message = "too long"
  }
  validation {
    condition     = can(regex("^[A-Za-z0-9]([A-Za-z0-9-]{0,62}[A-Za-z0-9])?$", var.name))
    error_message = "1-64 letters, digits or -, not starting or ending with -."
  }
}
variable "prefix" {
  type    = string
  default = "ab"
  validation {
    condition     = can(regex("^a|b$", var.prefix)) # a search, not a whole match
    error_message = "starts with a or ends with b"
  }
}
variable "other" {
  type    = string
  default = ""
  validation {
    condition     = can(regex("^${local.re}$", var.other)) # not a literal
    error_message = "x"
  }
}
variable "retention_days" {
  type    = number
  default = 7
}
variable "versioning" {
  type    = bool
  default = false
}
variable "tags" {
  type    = map(string)
  default = {}
}
terraform {
  required_providers {
    azurerm = { source = "hashicorp/azurerm", version = "~> 4.0" }
  }
}
resource "azurerm_storage_account" "this" {}
output "id" {
  description = "Storage account id"
  value       = azurerm_storage_account.this.id
}
`

func TestInspectAndInputs(t *testing.T) {
	ins, err := Inspect(TarGz(t, map[string]string{"./main.tf": moduleTF, "README.md": "hi"}))
	if err != nil {
		t.Fatal(err)
	}
	vars := ins.Variables
	if ins.Readme != "hi" || len(ins.Module.Outputs) != 1 || ins.Module.Outputs["id"].Pos.Filename != "main.tf" ||
		ins.Module.RequiredProviders["azurerm"].Source != "hashicorp/azurerm" || len(ins.Module.ManagedResources) != 1 || ins.Module.Path != "" {
		t.Fatalf("docs data: %+v", ins.Module)
	}
	inputs, skipped := Inputs(vars)
	if len(inputs) != 6 || len(skipped) != 0 || inputs[5].Type != "map" || inputs[5].Default != "{}" {
		t.Fatalf("inputs %+v skipped %v", inputs, skipped)
	}
	if in := inputs[0]; in.Name != "name" || in.Label != "Account name" || !in.Required || in.Type != "string" ||
		in.Pattern != "^[A-Za-z0-9]([A-Za-z0-9-]{0,62}[A-Za-z0-9])?$" {
		t.Fatalf("name input: %+v", in)
	}
	if p := inputs[1].Pattern; p != ".*(?:^a|b$).*" {
		t.Fatalf("prefix pattern: %q", p)
	}
	if p := inputs[2].Pattern; p != "" {
		t.Fatalf("other pattern: %q", p)
	}
	// The form accepts what Terraform's validation accepts.
	tpl := templates.Template{Inputs: inputs[:2]}
	if _, errs := tpl.Validate(map[string]string{"name": "web-01", "prefix": "zzb"}); len(errs) != 0 {
		t.Fatalf("valid values rejected: %v", errs)
	}
	if _, errs := tpl.Validate(map[string]string{"name": "-web", "prefix": "zz"}); len(errs) != 2 {
		t.Fatalf("invalid values accepted: %v", errs)
	}
	if in := inputs[3]; in.Type != "number" || in.Default != "7" || in.Required {
		t.Fatalf("retention input: %+v", in)
	}
	if vars[0].Pos.Filename != "main.tf" {
		t.Fatalf("temp path leaked: %+v", vars[0].Pos)
	}
}

func TestInspectRejects(t *testing.T) {
	many := map[string]string{"main.tf": moduleTF}
	for i := range maxEntries {
		many[fmt.Sprintf("e/%d", i)] = ""
	}
	for name, archive := range map[string][]byte{
		"too many entries": TarGz(t, many),
		"not gzip":         []byte("plain"),
		"no tf at root":    TarGz(t, map[string]string{"sub/main.tf": moduleTF}),
		"path traversal":   TarGz(t, map[string]string{"../evil.tf": moduleTF}),
		"invalid hcl":      TarGz(t, map[string]string{"main.tf": `variable "x" {`}),
	} {
		if _, err := Inspect(archive); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestAddressAndSigning(t *testing.T) {
	u, _ := url.Parse("https://ops.example.com")
	r := &Registry{PublicURL: u, Key: []byte("k")}

	ns, name, sys, src, err := r.Address("Acme", "storage", "azurerm")
	if err != nil || ns != "acme" || src != "ops.example.com/acme/storage/azurerm" {
		t.Fatalf("address: %s %s %s %s %v", ns, name, sys, src, err)
	}
	if _, _, _, _, err := r.Address("acme", "bad name", "azurerm"); err == nil {
		t.Fatal("expected invalid name")
	}
	if _, _, _, ok := r.ParseSource("other.example.com/acme/storage/azurerm"); ok {
		t.Fatal("foreign registry must not parse as ours")
	}

	got, ver := r.RunnerSource(templates.Module{Source: src, Ref: "1.0.0"}, time.Hour)
	pu, err := url.Parse(got)
	if err != nil || ver != "" || pu.Host != "" || pu.Path != "/v1/archives/acme/storage/azurerm/1.0.0.tar.gz" {
		t.Fatalf("runner source: %s", got)
	}
	q := pu.Query()
	if !r.VerifyArchiveURL(pu.Path, q.Get("expires"), q.Get("sig")) {
		t.Fatal("signature should verify")
	}
	if r.VerifyArchiveURL("/v1/archives/acme/other/azurerm/1.0.0.tar.gz", q.Get("expires"), q.Get("sig")) {
		t.Fatal("signature must be bound to the path")
	}
	if r.VerifyArchiveURL(pu.Path, "1", r.sig(pu.Path, 1)) {
		t.Fatal("expired signature must fail")
	}
	if got, ver := r.RunnerSource(templates.Module{Source: "git::https://x//m", Ref: "v1"}, time.Hour); got != "git::https://x//m?ref=v1" || ver != "" {
		t.Fatalf("git source: %s %s", got, ver)
	}
	if got, ver := r.RunnerSource(templates.Module{Source: "git::https://x//m?depth=1", Ref: "v1"}, time.Hour); got != "git::https://x//m?depth=1&ref=v1" || ver != "" {
		t.Fatalf("git source with a query: %s %s", got, ver)
	}
	// another registry's module (the public one, another Opsolete): a version argument, no ?ref= (Terraform refuses it)
	for _, other := range []string{"other.example.com/acme/storage/azurerm", "Azure/avm-res-resources-resourcegroup/azurerm"} {
		if got, ver := r.RunnerSource(templates.Module{Source: other, Ref: "1.1.0"}, time.Hour); got != other || ver != "1.1.0" {
			t.Fatalf("%s: %s %s", other, got, ver)
		}
	}

	vs := []string{"1.0.0", "1.10.0", "1.2.0"}
	SortVersions(vs)
	if strings.Join(vs, ",") != "1.10.0,1.2.0,1.0.0" {
		t.Fatalf("sort: %v", vs)
	}
}

func TestResolve(t *testing.T) {
	vars := []Variable{
		{Variable: tfconfig.Variable{Name: "name", Type: "string", Required: true}, Pattern: "^[a-z0-9]+$"},
		{Variable: tfconfig.Variable{Name: "location", Type: "string", Default: "westeurope"}},
		{Variable: tfconfig.Variable{Name: "count_x", Type: "number", Default: 1}},
		{Variable: tfconfig.Variable{Name: "secret", Type: "string", Sensitive: true}},
		{Variable: tfconfig.Variable{Name: "zone", Type: "string", Default: "a"}, Pattern: "^[a-c]$"},
		{Variable: tfconfig.Variable{Name: "vm", Type: "string", Description: "VM name: 1-64 letters: or digits."}},
		{Variable: tfconfig.Variable{Name: "size", Type: "string", Description: "Size: small or large."}},
	}
	overrides := []templates.Input{
		{Name: "location", Label: "Region", Type: "select", Options: []templates.Option{{Value: "westeurope"}, {Value: "northeurope"}}},
		{Name: "gone", Type: "string", Required: true},      // no longer in the module
		{Name: "name", Type: "number", Pattern: "^[a-z]+$"}, // wrong type in the JSON
		{Name: "size", Label: "VM size", Help: "Pick one."},
	}
	got := Resolve(overrides, vars)
	want := []templates.Input{
		{Name: "location", Label: "Region", Type: "select", Options: []templates.Option{{Value: "westeurope"}, {Value: "northeurope"}}, Default: "westeurope"},
		{Name: "name", Label: "name", Type: "string", Required: true, Pattern: "^[a-z]+$"},
		{Name: "size", Label: "VM size", Help: "Pick one.", Type: "string"},
		{Name: "count_x", Label: "count_x", Type: "number", Default: "1"},
		{Name: "zone", Label: "zone", Type: "string", Default: "a", Pattern: "^[a-c]$"},  // the module's pattern
		{Name: "vm", Label: "VM name", Help: "1-64 letters: or digits.", Type: "string"}, // the description split once
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %+v\nwant %+v", got, want)
	}
}

// A module variable declared sensitive is a sensitive input once the template lists it, without a default.
func TestResolveSensitive(t *testing.T) {
	vars := []Variable{{Variable: tfconfig.Variable{Name: "secret", Type: "string", Sensitive: true, Default: "x"}}, {Variable: tfconfig.Variable{Name: "pw", Type: "string"}}}
	got := Resolve([]templates.Input{{Name: "secret", Type: "string"}, {Name: "pw", Type: "string", Sensitive: true}}, vars)
	want := []templates.Input{{Name: "secret", Label: "secret", Type: "string", Sensitive: true}, {Name: "pw", Label: "pw", Type: "string", Sensitive: true}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %+v\nwant %+v", got, want)
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
variable "dns" { type = list(string) }
variable "zones" { type = set(string) }
variable "network" {
  type = object({
    cidr = string
    dns  = optional(bool, true)
  })
}
variable "counts" { type = map(number) }
`})
	ins, err := Inspect(archive)
	if err != nil {
		t.Fatal(err)
	}
	inputs, skipped := Inputs(ins.Variables)
	slices.Sort(skipped)
	if len(inputs) != 5 || !reflect.DeepEqual(skipped, []string{"counts (map(number))", "nested (list(object({ ports = list(number) })))"}) {
		t.Fatalf("%+v %q", inputs, skipped)
	}
	byName := map[string]templates.Input{}
	for _, in := range inputs {
		byName[in.Name] = in
	}
	if byName["tags"].Type != "map" || byName["dns"].Type != "string_list" || byName["zones"].Type != "string_list" ||
		!reflect.DeepEqual(byName["network"].Fields, []templates.Input{{Name: "cidr", Label: "cidr", Type: "string", Required: true},
			{Name: "dns", Label: "dns", Type: "bool", Default: "true"}}) || byName["network"].Type != "object" {
		t.Fatalf("other types: %+v", byName)
	}
	in := byName["data_disks"]
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

// A module's sensitive variables: strings and numbers are sensitive inputs without their default; complex ones
// (stored and shown in plain text otherwise) can't be form fields. An untyped variable with a map default isn't a
// string field; Terraform's mixed-case names are kept.
func TestInputsSensitiveAndUntyped(t *testing.T) {
	v := func(name, typ string, def any, sensitive bool) Variable {
		var x Variable
		x.Name, x.Type, x.Default, x.Sensitive = name, typ, def, sensitive
		return x
	}
	in, skipped := Inputs([]Variable{v("adminPassword", "string", "changeme", true), v("settings", "map(string)", nil, true),
		v("tags", "", map[string]any{}, false), v("size", "", "small", false)})
	if len(in) != 2 || in[0].Name != "adminPassword" || !in[0].Sensitive || in[0].Default != "" || in[1].Name != "size" || in[1].Type != "string" {
		t.Fatalf("inputs %+v", in)
	}
	if len(skipped) != 2 || !strings.Contains(skipped[0], "settings") || !strings.Contains(skipped[1], "tags") {
		t.Fatalf("skipped %v", skipped)
	}
}
