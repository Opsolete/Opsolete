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
	// azblob://tfstate, s3://tfstate?region=eu-west-1, gs://tfstate or file:///var/lib/form2stack/state.
	// Required by the server when deployments are on (checked there, so `token` works without it).
	StateStore string
	// PublicURL is how Terraform clients reach this app; its host is the module registry hostname
	// (Terraform requires a dot in it and HTTPS for registry clients).
	PublicURL *url.URL
	// SecretKey encrypts VCS access tokens in Postgres. Empty: VCS connections are off.
	SecretKey string
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
		AuthDevGroups:    strings.FieldsFunc(os.Getenv("AUTH_DEV_GROUPS"), func(r rune) bool { return r == ',' }),
		StateStore:       os.Getenv("STATE_STORE"),
		AdminEmails:      strings.Split(os.Getenv("ADMIN_EMAILS"), ","),
		SecretKey:        os.Getenv("SECRET_KEY"),
	}
	if c.DatabaseURL == "" {
		return c, errors.New("DATABASE_URL is required")
	}
	if c.SecretKey != "" && len(c.SecretKey) < 32 {
		return c, errors.New("SECRET_KEY must be at least 32 characters (e.g. openssl rand -base64 32)")
	}
	var err error
	if c.Deployments, err = getbool("DEPLOYMENTS_ENABLED"); err != nil {
		return c, err
	}
	if c.Registry, err = getbool("REGISTRY_ENABLED"); err != nil {
		return c, err
	}
	if !c.Deployments && !c.Registry {
		return c, errors.New("DEPLOYMENTS_ENABLED and REGISTRY_ENABLED are both off: nothing to serve")
	}

	pu, err := url.Parse(getenv("PUBLIC_URL", "http://form2stack.localhost:8080"))
	if err != nil || (pu.Scheme != "http" && pu.Scheme != "https") || !strings.Contains(pu.Hostname(), ".") || pu.Path != "" && pu.Path != "/" {
		return c, errors.New("PUBLIC_URL must be like https://form2stack.example.com (a hostname with a dot, no path)")
	}
	pu.Path = ""
	c.PublicURL = pu
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

// Runner configures `form2stack runner`. It needs no database: it talks to the server API only.
type Runner struct {
	ServerURL  *url.URL      // how the runner reaches Form2Stack (in k8s: the internal service)
	Token      string        // runner-scoped API token
	Name       string        // shown on deployments; defaults to the hostname (pod name)
	Terraform  string        // terraform binary
	JobTimeout time.Duration // hard limit per deployment
}

// LoadRunner reads F2S_* variables and then removes them from the environment, so
// terraform, providers and module code never see the runner token.
func LoadRunner() (Runner, error) {
	host, _ := os.Hostname()
	c := Runner{
		Token:     os.Getenv("F2S_RUNNER_TOKEN"),
		Name:      getenv("F2S_RUNNER_NAME", host),
		Terraform: os.Getenv("F2S_TERRAFORM"),
	}
	u, err := url.Parse(os.Getenv("F2S_SERVER_URL"))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return c, errors.New("F2S_SERVER_URL must be like https://form2stack.example.com")
	}
	c.ServerURL = u
	if c.Token == "" {
		return c, errors.New("F2S_RUNNER_TOKEN is required (create one with: form2stack token <name> runner)")
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
