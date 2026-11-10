package config

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestLoad(t *testing.T) {
	for _, k := range []string{"ADDR", "PUBLIC_URL", "DEPLOYMENTS_ENABLED", "REGISTRY_ENABLED", "TEMPLATE_EDITOR", "RUN_LOG_RETENTION_DAYS"} {
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
	for _, bad := range []string{"short", "test-secret-key-of-at-least-32-chars"} {
		t.Setenv("SECRET_KEY_PREVIOUS", bad)
		if _, err := Load(); err == nil {
			t.Errorf("SECRET_KEY_PREVIOUS %q should be rejected", bad)
		}
	}
	t.Setenv("SECRET_KEY_PREVIOUS", "")
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

	if c.RunLogRetentionDays != 90 {
		t.Fatalf("RUN_LOG_RETENTION_DAYS default: %d", c.RunLogRetentionDays)
	}
	t.Setenv("RUN_LOG_RETENTION_DAYS", "0")
	if c, err := Load(); err != nil || c.RunLogRetentionDays != 0 {
		t.Fatalf("RUN_LOG_RETENTION_DAYS=0 keeps logs forever: %d %v", c.RunLogRetentionDays, err)
	}
	for _, bad := range []string{"-1", "90d", "1.5", "36501"} {
		t.Setenv("RUN_LOG_RETENTION_DAYS", bad)
		if _, err := Load(); err == nil {
			t.Errorf("RUN_LOG_RETENTION_DAYS %q should be rejected", bad)
		}
	}
	t.Setenv("RUN_LOG_RETENTION_DAYS", "")

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
	t.Setenv("OPS_JOB_GRACE", "")
	if _, err := LoadRunner(); err == nil {
		t.Fatal("expected error without token")
	}
	t.Setenv("OPS_RUNNER_TOKEN", "secret")
	c, err := LoadRunner()
	if err != nil {
		t.Fatal(err)
	}
	if c.Token != "secret" || c.JobTimeout.String() != "1h0m0s" || c.JobGrace.String() != "10m0s" {
		t.Fatalf("runner config: %+v", c)
	}
	if _, set := os.LookupEnv("OPS_RUNNER_TOKEN"); set {
		t.Fatal("token must be removed from the environment")
	}
	for _, v := range []string{"0", "-1m", "10"} {
		for k, val := range map[string]string{"OPS_SERVER_URL": "http://opsolete:8080", "OPS_RUNNER_TOKEN": "secret", "OPS_TERRAFORM": "/bin/terraform"} {
			t.Setenv(k, val) // LoadRunner unsets them
		}
		t.Setenv("OPS_JOB_GRACE", v)
		if _, err := LoadRunner(); err == nil || !strings.Contains(err.Error(), "OPS_JOB_GRACE") {
			t.Fatalf("OPS_JOB_GRACE=%s: err = %v", v, err)
		}
	}
}

// TestDocumented: every environment variable config.go reads is in the configuration page.
func TestDocumented(t *testing.T) {
	src, err := os.ReadFile("config.go")
	if err != nil {
		t.Fatal(err)
	}
	doc, err := os.ReadFile("../../docs/content/docs/config/reference.md")
	if err != nil {
		t.Fatal(err)
	}
	vars := regexp.MustCompile(`(?:Getenv|getenv|getbool)\("([A-Z0-9_]+)"`).FindAllSubmatch(src, -1)
	if len(vars) < 20 {
		t.Fatalf("found only %d env vars in config.go: did the way it reads them change?", len(vars))
	}
	for _, m := range vars {
		if !strings.Contains(string(doc), "`"+string(m[1])+"`") {
			t.Errorf("%s is read by internal/config but not documented in docs/content/docs/config/reference.md", m[1])
		}
	}
}
