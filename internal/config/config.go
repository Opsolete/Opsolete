// Package config loads application configuration from environment variables.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"slices"
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
	AdminGroups                                []string
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
	// SecretKeyPrevious is the SECRET_KEY being replaced: the server still opens values sealed with
	// it (`opsolete rotate-key` re-seals them) and verifies the audit entries signed before the
	// rotation with it. Optional.
	SecretKeyPrevious string
	// Organization is the one organization Terraform's cloud block may name.
	Organization string
	// StaleLockTimeout: a rollback's state lock older than this is released as orphaned (its server
	// died mid-rollback). Run locks go with the run's lease; CLI and UI locks never expire.
	StaleLockTimeout time.Duration
	// RunLogRetentionDays: run logs are deleted this many days after the run ended; 0 keeps them forever.
	RunLogRetentionDays int
}

func Load() (Config, error) {
	c := Config{
		Addr:              getenv("ADDR", ":8080"),
		MetricsAddr:       os.Getenv("METRICS_ADDR"),
		DatabaseURL:       os.Getenv("DATABASE_URL"),
		TemplatesDir:      getenv("TEMPLATES_DIR", "catalog"),
		TemplateEditor:    os.Getenv("TEMPLATE_EDITOR") == "1",
		OIDCIssuer:        os.Getenv("OIDC_ISSUER"),
		OIDCClientID:      os.Getenv("OIDC_CLIENT_ID"),
		OIDCClientSecret:  os.Getenv("OIDC_CLIENT_SECRET"),
		AuthDevUser:       os.Getenv("AUTH_DEV_USER"),
		AuthDevGroups:     strings.FieldsFunc(os.Getenv("AUTH_DEV_GROUPS"), func(r rune) bool { return r == ',' }),
		StateStore:        os.Getenv("STATE_STORE"),
		AdminEmails:       strings.Split(os.Getenv("ADMIN_EMAILS"), ","),
		AdminGroups:       strings.FieldsFunc(os.Getenv("ADMIN_GROUPS"), func(r rune) bool { return r == ',' }),
		SecretKey:         os.Getenv("SECRET_KEY"),
		SecretKeyPrevious: os.Getenv("SECRET_KEY_PREVIOUS"),
		Organization:      getenv("ORGANIZATION", "opsolete"),
	}
	for i, g := range c.AuthDevGroups { // "a, b": group names must match the IdP's exactly
		c.AuthDevGroups[i] = strings.TrimSpace(g)
	}
	for i, g := range c.AdminGroups {
		c.AdminGroups[i] = strings.TrimSpace(g)
	}
	if c.DatabaseURL == "" {
		return c, errors.New("DATABASE_URL is required")
	}
	if len(c.SecretKey) < 32 {
		return c, errors.New("SECRET_KEY is required and must be at least 32 characters (e.g. openssl rand -base64 32)")
	}
	if c.SecretKeyPrevious != "" && (len(c.SecretKeyPrevious) < 32 || c.SecretKeyPrevious == c.SecretKey) {
		return c, errors.New("SECRET_KEY_PREVIOUS, when set, must be at least 32 characters and differ from SECRET_KEY")
	}
	var err error
	if c.StaleLockTimeout, err = time.ParseDuration(getenv("STALE_LOCK_TIMEOUT", "15m")); err != nil || c.StaleLockTimeout <= 0 {
		return c, errors.New("STALE_LOCK_TIMEOUT must be a duration like 15m")
	}
	if c.RunLogRetentionDays, err = strconv.Atoi(getenv("RUN_LOG_RETENTION_DAYS", "90")); err != nil || c.RunLogRetentionDays < 0 || c.RunLogRetentionDays > 36500 {
		return c, errors.New("RUN_LOG_RETENTION_DAYS must be a whole number of days up to 36500 (0 keeps run logs forever)")
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
	Token      string        // its own token (opsolete runner-token)
	Name       string        // shown on deployments; defaults to the hostname (pod name)
	Terraform  string        // terraform binary
	JobTimeout time.Duration // limit per run phase; then Terraform is interrupted
	JobGrace   time.Duration // after any interrupt, how long Terraform may take to stop before it's killed
	Sandbox    string        // auto | namespaces | off: each Terraform command in its own Linux namespaces (runner/sandbox_linux.go)
	Once       bool          // run one job (or wait one claim for it) and exit: a pod per phase, started by an autoscaler
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
		return c, errors.New("OPS_RUNNER_TOKEN is required: the runner's token (opsolete runner-token <name>)")
	}
	if c.Terraform == "" {
		if c.Terraform, err = exec.LookPath("terraform"); err != nil {
			return c, errors.New("terraform not found: install it or set OPS_TERRAFORM")
		}
	}
	if c.JobTimeout, err = time.ParseDuration(getenv("OPS_JOB_TIMEOUT", "1h")); err != nil || c.JobTimeout <= 0 {
		return c, errors.New("OPS_JOB_TIMEOUT must be a duration like 45m")
	}
	// 10m: in-flight resource operations (a database being created) can take minutes, and killing
	// Terraform before they finish can leave real resources missing from state.
	if c.JobGrace, err = time.ParseDuration(getenv("OPS_JOB_GRACE", "10m")); err != nil || c.JobGrace <= 0 {
		return c, errors.New("OPS_JOB_GRACE must be a duration like 10m")
	}
	if c.Once, err = strconv.ParseBool(getenv("OPS_RUNNER_ONCE", "false")); err != nil {
		return c, errors.New("OPS_RUNNER_ONCE must be true or false")
	}
	if c.Sandbox = getenv("OPS_SANDBOX", "auto"); !slices.Contains([]string{"auto", "namespaces", "off"}, c.Sandbox) {
		return c, errors.New("OPS_SANDBOX must be auto, namespaces or off")
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
