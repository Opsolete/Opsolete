// Package config loads application configuration from environment variables.
package config

import (
	"errors"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"time"
)

type Config struct {
	Addr         string // HTTP listen address
	DatabaseURL  string // PostgreSQL connection string
	TemplatesDir string // directory with JSON template definitions (a Git checkout)
	// TemplateEditor enables the web template builder (admins only), which writes into TemplatesDir.
	// Off by default: a template decides which module the runners execute.
	TemplateEditor bool
	// Login: OIDC, or AuthDevUser (everyone is that user, local dev only). Checked by auth.New.
	OIDCIssuer, OIDCClientID, OIDCClientSecret string
	AuthDevUser                                string
	AdminEmails                                []string
	// PublicURL is how Terraform clients reach this app; its host is the module registry hostname
	// (Terraform requires a dot in it and HTTPS for registry clients).
	PublicURL *url.URL
}

func Load() (Config, error) {
	c := Config{
		Addr:             getenv("ADDR", ":8080"),
		DatabaseURL:      os.Getenv("DATABASE_URL"),
		TemplatesDir:     getenv("TEMPLATES_DIR", "catalog"),
		TemplateEditor:   os.Getenv("TEMPLATE_EDITOR") == "1",
		OIDCIssuer:       os.Getenv("OIDC_ISSUER"),
		OIDCClientID:     os.Getenv("OIDC_CLIENT_ID"),
		OIDCClientSecret: os.Getenv("OIDC_CLIENT_SECRET"),
		AuthDevUser:      os.Getenv("AUTH_DEV_USER"),
		AdminEmails:      strings.Split(os.Getenv("ADMIN_EMAILS"), ","),
	}
	if c.DatabaseURL == "" {
		return c, errors.New("DATABASE_URL is required")
	}
	pu, err := url.Parse(getenv("PUBLIC_URL", "http://form2stack.localhost:8080"))
	if err != nil || (pu.Scheme != "http" && pu.Scheme != "https") || !strings.Contains(pu.Hostname(), ".") || pu.Path != "" && pu.Path != "/" {
		return c, errors.New("PUBLIC_URL must be like https://form2stack.example.com (a hostname with a dot, no path)")
	}
	pu.Path = ""
	c.PublicURL = pu
	return c, nil
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// Runner configures `form2stack runner`. It needs no database: it talks to the server API only.
type Runner struct {
	ServerURL     *url.URL      // how the runner reaches Form2Stack (in k8s: the internal service)
	Token         string        // runner-scoped API token
	Name          string        // shown on deployments; defaults to the hostname (pod name)
	Terraform     string        // terraform binary
	Backend       string        // terraform backend type, e.g. azurerm, s3, local
	BackendConfig []string      // key=value pairs from F2S_BACKEND_CONFIG_<key>
	StateDir      string        // local backend only: where state files live (a volume)
	JobTimeout    time.Duration // hard limit per deployment
}

var backendRe = regexp.MustCompile(`^[a-z0-9]+$`)

// LoadRunner reads F2S_* variables and then removes them from the environment, so
// terraform, providers and module code never see the runner token or backend secrets.
func LoadRunner() (Runner, error) {
	host, _ := os.Hostname()
	c := Runner{
		Token:     os.Getenv("F2S_RUNNER_TOKEN"),
		Name:      getenv("F2S_RUNNER_NAME", host),
		Terraform: os.Getenv("F2S_TERRAFORM"),
		Backend:   getenv("F2S_BACKEND", "local"),
		StateDir:  getenv("F2S_STATE_DIR", "/state"),
	}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok && strings.HasPrefix(k, "F2S_BACKEND_CONFIG_") {
			c.BackendConfig = append(c.BackendConfig, strings.ToLower(strings.TrimPrefix(k, "F2S_BACKEND_CONFIG_"))+"="+v)
		}
	}
	slices.Sort(c.BackendConfig)
	u, err := url.Parse(os.Getenv("F2S_SERVER_URL"))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return c, errors.New("F2S_SERVER_URL must be like https://form2stack.example.com")
	}
	c.ServerURL = u
	if c.Token == "" {
		return c, errors.New("F2S_RUNNER_TOKEN is required (create one with: form2stack token <name> runner)")
	}
	if !backendRe.MatchString(c.Backend) {
		return c, errors.New("F2S_BACKEND must be a terraform backend type like azurerm, s3 or local")
	}
	if c.Terraform == "" {
		if c.Terraform, err = exec.LookPath("terraform"); err != nil {
			return c, errors.New("terraform not found: install it or set F2S_TERRAFORM")
		}
	}
	if c.JobTimeout, err = time.ParseDuration(getenv("F2S_JOB_TIMEOUT", "1h")); err != nil || c.JobTimeout <= 0 {
		return c, errors.New("F2S_JOB_TIMEOUT must be a duration like 45m")
	}
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); strings.HasPrefix(k, "F2S_") {
			os.Unsetenv(k)
		}
	}
	return c, nil
}
