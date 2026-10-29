// Package config loads application configuration from environment variables.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Addr         string // HTTP listen address
	MetricsAddr  string // Prometheus /metrics listen address; empty turns it off
	DatabaseURL  string // PostgreSQL connection string
	TemplatesDir string // directory with JSON template definitions (a Git checkout)
	// Components; at least one must be on. Off removes its pages and API routes.
	Deployments bool // templates, deployments and the runner API
	Registry    bool // private module registry (pages and Terraform protocol)
	// TemplateEditor enables the web template builder (admins only), which writes into TemplatesDir.
	// Off by default: a template decides which module the runners execute.
	TemplateEditor bool
	// Login: OIDC, or AuthDevUser (everyone is that user, local dev only). Checked by auth.New.
	OIDCIssuer, OIDCClientID, OIDCClientSecret string
	AuthDevUser                                string
	AuthDevGroups                              []string
	AdminEmails                                []string
	// StateStore is where workspace state files are kept: a gocloud.dev/blob URL such as
	// azblob://tfstate, s3://tfstate?region=eu-west-1, gs://tfstate or file:///var/lib/opsolete/state.
	// Required by the server when deployments are on (checked there, so `token` works without it).
	StateStore string
	// PublicURL is how Terraform clients reach this app; its host is the module registry hostname
	// (Terraform requires a dot in it and HTTPS for registry clients).
	PublicURL *url.URL
	// SecretKey encrypts VCS access tokens and variables in Postgres, and state files and saved plans
	// in STATE_STORE, and signs the audit log. Required.
	SecretKey string
	// RunnerToken authenticates runners (their OPS_RUNNER_TOKEN). The operator generates it, so it
	// exists before the first deploy. Required by the server when deployments are on.
	RunnerToken string
	// Organization is the one organization Terraform's cloud block may name.
	Organization string
	// StaleLockTimeout: a rollback's state lock older than this is released as orphaned (its server
	// died mid-rollback). Run locks go with the run's lease; CLI and UI locks never expire.
	StaleLockTimeout time.Duration
}

func Load() (Config, error) {
	c := Config{
		Addr:             getenv("ADDR", ":8080"),
		MetricsAddr:      os.Getenv("METRICS_ADDR"),
		DatabaseURL:      os.Getenv("DATABASE_URL"),
		TemplatesDir:     getenv("TEMPLATES_DIR", "catalog"),
		TemplateEditor:   os.Getenv("TEMPLATE_EDITOR") == "1",
		OIDCIssuer:       os.Getenv("OIDC_ISSUER"),
		OIDCClientID:     os.Getenv("OIDC_CLIENT_ID"),
		OIDCClientSecret: os.Getenv("OIDC_CLIENT_SECRET"),
		AuthDevUser:      os.Getenv("AUTH_DEV_USER"),
		AuthDevGroups:    strings.FieldsFunc(os.Getenv("AUTH_DEV_GROUPS"), func(r rune) bool { return r == ',' }),
		StateStore:       os.Getenv("STATE_STORE"),
		AdminEmails:      strings.Split(os.Getenv("ADMIN_EMAILS"), ","),
		SecretKey:        os.Getenv("SECRET_KEY"),
		RunnerToken:      os.Getenv("RUNNER_TOKEN"),
		Organization:     getenv("ORGANIZATION", "opsolete"),
	}
	if c.DatabaseURL == "" {
		return c, errors.New("DATABASE_URL is required")
	}
	if len(c.SecretKey) < 32 {
		return c, errors.New("SECRET_KEY is required and must be at least 32 characters (e.g. openssl rand -base64 32)")
	}
	var err error
	if c.StaleLockTimeout, err = time.ParseDuration(getenv("STALE_LOCK_TIMEOUT", "15m")); err != nil || c.StaleLockTimeout <= 0 {
		return c, errors.New("STALE_LOCK_TIMEOUT must be a duration like 15m")
	}
	if c.Deployments, err = getbool("DEPLOYMENTS_ENABLED"); err != nil {
		return c, err
	}
	if c.Registry, err = getbool("REGISTRY_ENABLED"); err != nil {
		return c, err
	}
	if !c.Deployments && !c.Registry {
		return c, errors.New("DEPLOYMENTS_ENABLED and REGISTRY_ENABLED are both off: nothing to serve")
	}

	pu, err := url.Parse(getenv("PUBLIC_URL", "http://opsolete.localhost:8080"))
	if err != nil || (pu.Scheme != "http" && pu.Scheme != "https") || !strings.Contains(pu.Hostname(), ".") || pu.Path != "" && pu.Path != "/" ||
		pu.User != nil || pu.RawQuery != "" || pu.ForceQuery || pu.Fragment != "" {
		return c, errors.New("PUBLIC_URL must be like https://opsolete.example.com (a hostname with a dot, no path, credentials, query or fragment)")
	}
	pu.Path = ""
	c.PublicURL = pu
	if c.TemplateEditor { // fail at startup, not at the first save
		f, err := os.CreateTemp(c.TemplatesDir, ".write-check-*")
		if err != nil {
			return c, fmt.Errorf("TEMPLATE_EDITOR=1 saves into TEMPLATES_DIR, which must be a writable directory: %w", err)
		}
		f.Close()
		os.Remove(f.Name())
	}
	return c, nil
}

// getbool reads an on-by-default switch.
func getbool(key string) (bool, error) {
	b, err := strconv.ParseBool(getenv(key, "true"))
	if err != nil {
		return false, fmt.Errorf("%s must be true or false", key)
	}
	return b, nil
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// Runner configures `opsolete runner`. It needs no database: it talks to the server API only.
type Runner struct {
	ServerURL  *url.URL      // how the runner reaches Opsolete (in k8s: the internal service)
	Token      string        // the server's RUNNER_TOKEN
	Name       string        // shown on deployments; defaults to the hostname (pod name)
	Terraform  string        // terraform binary
	JobTimeout time.Duration // hard limit per deployment
	// Cost estimation with Infracost; off without an API key.
	Infracost       string // infracost binary
	InfracostAPIKey string
}

// LoadRunner reads OPS_* variables and then removes them from the environment, so
// terraform, providers and module code never see the runner token.
func LoadRunner() (Runner, error) {
	host, _ := os.Hostname()
	c := Runner{
		Token:     os.Getenv("OPS_RUNNER_TOKEN"),
		Name:      getenv("OPS_RUNNER_NAME", host),
		Terraform: os.Getenv("OPS_TERRAFORM"),
	}
	u, err := url.Parse(os.Getenv("OPS_SERVER_URL"))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return c, errors.New("OPS_SERVER_URL must be like https://opsolete.example.com")
	}
	c.ServerURL = u
	if c.Token == "" {
		return c, errors.New("OPS_RUNNER_TOKEN is required: the server's RUNNER_TOKEN")
	}
	if c.Terraform == "" {
		if c.Terraform, err = exec.LookPath("terraform"); err != nil {
			return c, errors.New("terraform not found: install it or set OPS_TERRAFORM")
		}
	}
	if c.JobTimeout, err = time.ParseDuration(getenv("OPS_JOB_TIMEOUT", "1h")); err != nil || c.JobTimeout <= 0 {
		return c, errors.New("OPS_JOB_TIMEOUT must be a duration like 45m")
	}
	if c.InfracostAPIKey = os.Getenv("OPS_INFRACOST_API_KEY"); c.InfracostAPIKey != "" {
		if c.Infracost = os.Getenv("OPS_INFRACOST"); c.Infracost == "" {
			if c.Infracost, err = exec.LookPath("infracost"); err != nil {
				return c, errors.New("infracost not found: install it, set OPS_INFRACOST, or unset OPS_INFRACOST_API_KEY")
			}
		}
	}
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); strings.HasPrefix(k, "OPS_") {
			os.Unsetenv(k)
		}
	}
	return c, nil
}
