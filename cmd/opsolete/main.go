// Command opsolete runs the server (default), a runner, or creates API tokens:
//
//	opsolete                          serve the web UI, registry and runner API
//	opsolete runner                   execute deployments (no database needed)
//	opsolete token <name>             create a registry API token
package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"gocloud.dev/blob"
	_ "gocloud.dev/blob/azureblob" // azblob:// state stores
	_ "gocloud.dev/blob/fileblob"  // file://
	_ "gocloud.dev/blob/gcsblob"   // gs://
	_ "gocloud.dev/blob/s3blob"    // s3:// (AWS and S3-compatible stores)

	"github.com/opsolete/opsolete/internal/auth"
	"github.com/opsolete/opsolete/internal/config"
	httpserver "github.com/opsolete/opsolete/internal/http"
	"github.com/opsolete/opsolete/internal/registry"
	"github.com/opsolete/opsolete/internal/runner"
	"github.com/opsolete/opsolete/internal/storage"
	"github.com/opsolete/opsolete/internal/templates"
	"github.com/opsolete/opsolete/internal/vcs"
)

func main() {
	out := os.Stdout
	if len(os.Args) > 1 && os.Args[1] == "token" {
		out = os.Stderr // stdout must carry only the token: scripts capture it with $(...)
	}
	log := slog.New(slog.NewJSONHandler(out, nil))
	slog.SetDefault(log) // audit entries are streamed through the default logger
	var err error
	switch {
	case len(os.Args) == 1:
		err = serve(log)
	case os.Args[1] == "runner" && len(os.Args) == 2:
		err = runRunner(log)
	case os.Args[1] == "token" && len(os.Args) == 3:
		err = createToken()
	default:
		err = errors.New("usage: opsolete [runner | token <name>]")
	}
	if err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func openStore(ctx context.Context, cfg config.Config) (*storage.Store, error) {
	store, err := storage.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, err
	}
	if err := store.Migrate(ctx); err != nil {
		store.DB.Close()
		return nil, err
	}
	store.AuditKey = []byte(cfg.SecretKey)
	return store, nil
}

func createToken() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	scope := storage.ScopeRegistry
	ctx := context.Background()
	store, err := openStore(ctx, cfg)
	if err != nil {
		return err
	}
	defer store.DB.Close()
	token, err := store.CreateToken(ctx, os.Args[2], scope)
	if err != nil {
		return err
	}
	if err := store.Audit(ctx, "cli", "create token", map[string]any{"principal": map[string]any{"type": "system"},
		"name": os.Args[2], "scope": scope}); err != nil {
		return fmt.Errorf("token created, but not audited: %w", err)
	}
	fmt.Println(token)
	return nil
}

// runRunner: the first SIGTERM/SIGINT stops claiming and lets the current job finish
// (give pods a terminationGracePeriodSeconds that covers a job); a second one interrupts it.
func runRunner(log *slog.Logger) error {
	cfg, err := config.LoadRunner()
	if err != nil {
		return err
	}
	ctx, drain := context.WithCancel(context.Background())
	hard, interrupt := context.WithCancel(context.Background())
	sig := make(chan os.Signal, 2)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig
		log.Info("shutting down after the current job (signal again to interrupt it)")
		drain()
		<-sig
		log.Warn("interrupting the current job")
		interrupt()
	}()
	return runner.New(cfg, log).Run(ctx, hard)
}

func serve(log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	store, err := openStore(ctx, cfg)
	if err != nil {
		return err
	}
	defer store.DB.Close()

	key, err := store.SigningKey(ctx)
	if err != nil {
		return err
	}
	// a database dump alone must not forge sessions, state credentials or signed URLs
	m := hmac.New(sha256.New, []byte(cfg.SecretKey))
	m.Write(key)
	key = m.Sum(nil)
	reg := &registry.Registry{PublicURL: cfg.PublicURL, Key: key}

	// Same key as the registry: cookie MACs are domain-separated from signed archive URLs.
	authn, err := auth.New(ctx, auth.Config{Issuer: cfg.OIDCIssuer, ClientID: cfg.OIDCClientID, ClientSecret: cfg.OIDCClientSecret,
		DevUser: cfg.AuthDevUser, DevGroups: cfg.AuthDevGroups, Admins: cfg.AdminEmails, PublicURL: cfg.PublicURL}, key, store.UpsertUser)
	if err != nil {
		return err
	}
	if authn.Dev() {
		log.Warn("AUTH_DEV_USER is set: every visitor is logged in as that user; never use this in a shared environment", "user", cfg.AuthDevUser)
	}

	tpls, err := templates.Open(cfg.TemplatesDir)
	if err != nil {
		return err
	}
	log.Info("templates loaded", "count", len(tpls.List()), "dir", cfg.TemplatesDir, "editor", cfg.TemplateEditor)

	var state *blob.Bucket
	if cfg.Deployments {
		if len(cfg.RunnerToken) < 32 {
			return errors.New("RUNNER_TOKEN is required for deployments and must be at least 32 characters (e.g. openssl rand -hex 32); runners get the same value as OPS_RUNNER_TOKEN")
		}
		if cfg.StateStore == "" {
			return errors.New("STATE_STORE is required for deployments, e.g. azblob://tfstate, s3://tfstate?region=…, gs://tfstate or file:///path")
		}
		if state, err = blob.OpenBucket(ctx, cfg.StateStore); err != nil {
			return fmt.Errorf("STATE_STORE: %w", err)
		}
		defer state.Close()
		// Fail at startup, not at the first run, if the store is unreachable or the credentials are wrong.
		if _, err := state.Exists(ctx, "opsolete-startup-check"); err != nil {
			return fmt.Errorf("STATE_STORE not usable: %w", err)
		}
	}

	vcsOn := cfg.Registry
	app := httpserver.New(store, tpls, reg, authn, state, httpserver.Features{
		Deployments: cfg.Deployments, Registry: cfg.Registry, Editor: cfg.TemplateEditor, VCS: vcsOn}, log)
	app.Organization = cfg.Organization
	app.RunnerToken = cfg.RunnerToken
	app.SecretKey = vcs.Key(cfg.SecretKey)
	// values sealed before they were bound to their row only open after this
	if n, err := vcs.Reseal(ctx, store, app.SecretKey); err != nil {
		return fmt.Errorf("re-seal variables and VCS tokens: %w", err)
	} else if n > 0 {
		log.Info("re-sealed variables and VCS tokens", "count", n)
	}
	if vcsOn {
		go vcs.Poll(ctx, store, app.SecretKey, 5*time.Minute, log)
		if cfg.Deployments {
			go vcs.PollRuns(ctx, store, app.SecretKey, time.Minute, log)
			// A catalog in Git (Settings → Templates) replaces TEMPLATES_DIR: its last loaded commit now, then the branch head.
			if err := vcs.LoadCatalog(ctx, store, tpls); err != nil {
				return err
			}
			if mode, commit := tpls.Source(); mode != "" {
				log.Info("templates loaded from git", "count", len(tpls.List()), "commit", commit, "editor", mode)
			}
			go vcs.PollCatalog(ctx, store, app.SecretKey, tpls, time.Minute, log)
		}
	}
	if err := app.CheckTemplates(); err != nil {
		return err
	}
	log.Info("components", "deployments", cfg.Deployments, "registry", cfg.Registry, "vcs", vcsOn)
	if cfg.Deployments {
		go reapLostRunners(ctx, store, app, cfg.StaleLockTimeout, log)
	}

	if cfg.MetricsAddr != "" { // its own listener, so /metrics isn't reachable through the public ingress
		ln, err := net.Listen("tcp", cfg.MetricsAddr)
		if err != nil {
			return fmt.Errorf("METRICS_ADDR: %w", err)
		}
		msrv := &http.Server{Handler: app.MetricsHandler(), ReadHeaderTimeout: 10 * time.Second}
		go msrv.Serve(ln)
		defer msrv.Close()
		log.Info("metrics listening", "addr", cfg.MetricsAddr)
	}
	srv := &http.Server{Addr: cfg.Addr, Handler: app.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// Shutdown doesn't cancel requests; this ends log streams and runner long-polls on SIGTERM.
		BaseContext: func(net.Listener) context.Context { return ctx }}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()
	log.Info("listening", "addr", cfg.Addr, "public_url", cfg.PublicURL.String())
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// driftInterval is how often workspaces with drift detection are assessed.
const driftInterval = 24 * time.Hour

// reapLostRunners fails running jobs whose runner stopped heartbeating (crash, node loss), and
// releases orphaned state locks, and queues the drift checks that are due.
func reapLostRunners(ctx context.Context, store *storage.Store, app *httpserver.Server, staleLock time.Duration, log *slog.Logger) {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			ids, err := store.ReapExpiredLeases(ctx)
			if err != nil {
				log.Error("reap expired leases", "err", err)
			}
			for _, id := range ids {
				log.Warn("runner lost, deployment failed", "id", id)
				app.DeleteArtifacts(ctx, id) // its saved plan will never be applied
			}
			released, err := store.ReleaseOrphanedLocks(ctx, staleLock)
			if err != nil {
				log.Error("release orphaned state locks", "err", err)
			}
			for ws, info := range released {
				log.Warn("orphaned state lock released", "workspace", ws, "lock", info)
			}
			if n, err := store.QueueAssessments(ctx, driftInterval); err != nil {
				log.Error("queue drift checks", "err", err)
			} else if n > 0 {
				log.Info("drift checks queued", "count", n)
			}
		}
	}
}
