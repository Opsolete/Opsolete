package config

import (
	"os"
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
	t.Setenv("SECRET_KEY", "")
	if _, err := Load(); err == nil {
		t.Fatal("expected error without SECRET_KEY")
	}
	t.Setenv("SECRET_KEY", "test-secret-key-of-at-least-32-chars")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Addr != ":8080" || c.TemplatesDir != "catalog" || c.PublicURL.Host != "opsolete.localhost:8080" || !c.Deployments || !c.Registry {
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

	for _, bad := range []string{"http://localhost:8080", "ftp://a.b", "https://a.b/sub", "https://u:p@a.b", "https://a.b?x=1", "https://a.b?", "https://a.b#f"} {
		t.Setenv("PUBLIC_URL", bad)
		if _, err := Load(); err == nil {
			t.Errorf("PUBLIC_URL %q should be rejected", bad)
		}
	}
	t.Setenv("PUBLIC_URL", "")

	t.Setenv("TEMPLATE_EDITOR", "1")
	t.Setenv("TEMPLATES_DIR", t.TempDir())
	if _, err := Load(); err != nil {
		t.Fatalf("editor with a writable catalog: %v", err)
	}
	t.Setenv("TEMPLATES_DIR", "/nonexistent/catalog")
	if _, err := Load(); err == nil {
		t.Fatal("editor with an unwritable catalog must fail at startup")
	}
}

func TestLoadRunner(t *testing.T) {
	t.Setenv("OPS_SERVER_URL", "http://opsolete:8080")
	t.Setenv("OPS_RUNNER_TOKEN", "")
	t.Setenv("OPS_TERRAFORM", "/bin/terraform")
	if _, err := LoadRunner(); err == nil {
		t.Fatal("expected error without token")
	}
	t.Setenv("OPS_RUNNER_TOKEN", "secret")
	c, err := LoadRunner()
	if err != nil {
		t.Fatal(err)
	}
	if c.Token != "secret" || c.JobTimeout.String() != "1h0m0s" {
		t.Fatalf("runner config: %+v", c)
	}
	if _, set := os.LookupEnv("OPS_RUNNER_TOKEN"); set {
		t.Fatal("token must be removed from the environment")
	}
}
