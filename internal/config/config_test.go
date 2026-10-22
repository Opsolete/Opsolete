package config

import (
	"os"
	"strings"
	"testing"
)

func TestLoad(t *testing.T) {
	for _, k := range []string{"ADDR", "PUBLIC_URL", "DEPLOYMENTS_ENABLED", "REGISTRY_ENABLED"} {
		t.Setenv(k, "")
	}
	t.Setenv("DATABASE_URL", "")
	if _, err := Load(); err == nil {
		t.Fatal("expected error without DATABASE_URL")
	}

	t.Setenv("DATABASE_URL", "postgres://x")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Addr != ":8080" || c.TemplatesDir != "catalog" || c.PublicURL.Host != "form2stack.localhost:8080" || !c.Deployments || !c.Registry {
		t.Fatalf("unexpected defaults: %+v", c)
	}

	t.Setenv("REGISTRY_ENABLED", "false")
	if c, err := Load(); err != nil || c.Registry || !c.Deployments {
		t.Fatalf("REGISTRY_ENABLED=false: %+v %v", c, err)
	}
	t.Setenv("DEPLOYMENTS_ENABLED", "0")
	if _, err := Load(); err == nil {
		t.Fatal("both components off must be rejected")
	}
	t.Setenv("DEPLOYMENTS_ENABLED", "nope")
	if _, err := Load(); err == nil {
		t.Fatal("invalid switch value must be rejected")
	}
	t.Setenv("DEPLOYMENTS_ENABLED", "")
	t.Setenv("REGISTRY_ENABLED", "")

	for _, bad := range []string{"http://localhost:8080", "ftp://a.b", "https://a.b/sub"} {
		t.Setenv("PUBLIC_URL", bad)
		if _, err := Load(); err == nil {
			t.Errorf("PUBLIC_URL %q should be rejected", bad)
		}
	}
}

func TestLoadRunner(t *testing.T) {
	t.Setenv("F2S_SERVER_URL", "http://form2stack:8080")
	t.Setenv("F2S_RUNNER_TOKEN", "")
	t.Setenv("F2S_TERRAFORM", "/bin/terraform")
	if _, err := LoadRunner(); err == nil {
		t.Fatal("expected error without token")
	}
	t.Setenv("F2S_RUNNER_TOKEN", "secret")
	t.Setenv("F2S_BACKEND", "azurerm")
	t.Setenv("F2S_BACKEND_CONFIG_STORAGE_ACCOUNT_NAME", "st")
	t.Setenv("F2S_BACKEND_CONFIG_CONTAINER_NAME", "tfstate")
	c, err := LoadRunner()
	if err != nil {
		t.Fatal(err)
	}
	if c.Token != "secret" || c.Backend != "azurerm" || c.JobTimeout.String() != "1h0m0s" ||
		strings.Join(c.BackendConfig, ",") != "container_name=tfstate,storage_account_name=st" {
		t.Fatalf("runner config: %+v", c)
	}
	if _, set := os.LookupEnv("F2S_RUNNER_TOKEN"); set {
		t.Fatal("token must be removed from the environment")
	}
}
