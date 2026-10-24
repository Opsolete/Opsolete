package registry

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/opsolete/opsolete/internal/registry/registrytest"
	"github.com/opsolete/opsolete/internal/templates"
)

var TarGz = registrytest.TarGz

const moduleTF = `
variable "name" {
  type        = string
  description = "Account name"
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
	if len(inputs) != 3 || len(skipped) != 1 || skipped[0] != "tags (map(string))" {
		t.Fatalf("inputs %+v skipped %v", inputs, skipped)
	}
	if in := inputs[0]; in.Name != "name" || in.Label != "Account name" || !in.Required || in.Type != "string" {
		t.Fatalf("name input: %+v", in)
	}
	if in := inputs[1]; in.Type != "number" || in.Default != "7" || in.Required {
		t.Fatalf("retention input: %+v", in)
	}
	if vars[0].Pos.Filename != "main.tf" {
		t.Fatalf("temp path leaked: %+v", vars[0].Pos)
	}
}

func TestInspectRejects(t *testing.T) {
	for name, archive := range map[string][]byte{
		"not gzip":       []byte("plain"),
		"no tf at root":  TarGz(t, map[string]string{"sub/main.tf": moduleTF}),
		"path traversal": TarGz(t, map[string]string{"../evil.tf": moduleTF}),
		"invalid hcl":    TarGz(t, map[string]string{"main.tf": `variable "x" {`}),
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

	got := r.RunnerSource(templates.Module{Source: src, Ref: "1.0.0"}, time.Hour)
	pu, err := url.Parse(got)
	if err != nil || pu.Host != "" || pu.Path != "/v1/archives/acme/storage/azurerm/1.0.0.tar.gz" {
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
	if got := r.RunnerSource(templates.Module{Source: "git::https://x//m", Ref: "v1"}, time.Hour); got != "git::https://x//m?ref=v1" {
		t.Fatalf("git source: %s", got)
	}

	vs := []string{"1.0.0", "1.10.0", "1.2.0"}
	SortVersions(vs)
	if strings.Join(vs, ",") != "1.10.0,1.2.0,1.0.0" {
		t.Fatalf("sort: %v", vs)
	}
}
