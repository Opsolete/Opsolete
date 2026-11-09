// Command opsolete runs the server (default), a runner, or creates API tokens:
//
//	opsolete                          serve the web UI, registry and runner API
//	opsolete runner                   execute deployments (no database needed)
//	opsolete token <name>             create a registry API token
//	opsolete rotate-key               re-seal everything from SECRET_KEY_PREVIOUS to SECRET_KEY
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

	"github.com/opsolete/opsolete/internal/app"
	"github.com/opsolete/opsolete/internal/auth"
	"github.com/opsolete/opsolete/internal/config"
	httpserver "github.com/opsolete/opsolete/internal/http"
	"github.com/opsolete/opsolete/internal/registry"
	"github.com/opsolete/opsolete/internal/runner"
	"github.com/opsolete/opsolete/internal/secrets"
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
	case os.Args[1] == "rotate-key" && len(os.Args) == 2:
		err = rotateKey(log)
	default:
		err = errors.New("usage: opsolete [runner | token <name> | rotate-key]")
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
		store.Close()
		return nil, err
	}
	store.AuditKey = []byte(cfg.SecretKey)
	if cfg.SecretKeyPrevious != "" {
		store.AuditPreviousKey = []byte(cfg.SecretKeyPrevious)
	}
	return store, nil
}

// rotateKey re-seals everything sealed with SECRET_KEY_PREVIOUS under SECRET_KEY, while the servers
// run with both. State credentials, signed URLs, `terraform login` codes and in-flight logins are
// derived from SECRET_KEY too and end at the restart; browser sessions are random tokens in the
// database and stay.
func rotateKey(log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if cfg.SecretKeyPrevious == "" {
		return errors.New("set SECRET_KEY to the new key and SECRET_KEY_PREVIOUS to the old one")
	}
	keys, err := secrets.New(cfg.SecretKey, cfg.SecretKeyPrevious)
	if err != nil {
		return err
	}
	ctx := context.Background()
	store, err := openStore(ctx, cfg)
	if err != nil {
		return err
	}
	defer store.Close()
	var state *blob.Bucket
	if cfg.StateStore != "" {
		if state, err = blob.OpenBucket(ctx, cfg.StateStore); err != nil {
			return fmt.Errorf("STATE_STORE: %w", err)
		}
		defer state.Close()
	}
	n, err := app.RotateKey(ctx, store, state, keys, log)
	if err != nil {
		return fmt.Errorf("re-sealed %d values, then: %w (fix it and run rotate-key again)", n, err)
	}
	if err := store.Audit(ctx, "cli", "rotate secret key", map[string]any{"principal": map[string]any{"type": "system"}, "resealed": n}); err != nil {
		return fmt.Errorf("re-sealed %d values, but not audited: %w", n, err)
	}
	log.Info("SECRET_KEY rotated: nothing is sealed with SECRET_KEY_PREVIOUS any more; remove it from the servers (or keep it only to verify audit entries from before the rotation)", "resealed", n)
	return nil
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
	defer store.Close()
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
// (give pods a terminationGracePeriodSeconds of at least OPS_JOB_TIMEOUT + OPS_JOB_GRACE + 5m); a
// second one interrupts it (SIGINT, killed after OPS_JOB_GRACE).
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
	defer store.Close()

	key, err := store.SigningKey(ctx)
	if err != nil {
		return err
	}
	// a database dump alone must not forge state credentials, signed URLs or login codes (sessions are hashed random tokens)
	m := hmac.New(sha256.New, []byte(cfg.SecretKey))
	m.Write(key)
	key = m.Sum(nil)
	reg := &registry.Registry{PublicURL: cfg.PublicURL, Key: key}

	// Same key as the registry: the login-state cookie's MAC is domain-separated from signed archive URLs.
	authn, err := auth.New(ctx, auth.Config{Issuer: cfg.OIDCIssuer, ClientID: cfg.OIDCClientID, ClientSecret: cfg.OIDCClientSecret,
		DevUser: cfg.AuthDevUser, DevGroups: cfg.AuthDevGroups, Admins: cfg.AdminEmails, AdminGroups: cfg.AdminGroups, PublicURL: cfg.PublicURL}, key, store)
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

	vcsOn := cfg.Registry || cfg.Deployments // modules for the registry, workspaces and the catalog for deployments
	keys, err := secrets.New(cfg.SecretKey, cfg.SecretKeyPrevious)
	if err != nil {
		return err
	}
	ops := &app.App{Store: store, Blobs: state, Secrets: keys, Templates: tpls, Registry: reg, Log: log,
		VCS:             vcs.NewClient(nil),
		RunLogRetention: time.Duration(cfg.RunLogRetentionDays) * 24 * time.Hour,
		Notify:          &app.Notifier{DB: store.Pool.Config().ConnConfig, Log: log}}
	server := httpserver.New(ops, authn, httpserver.Features{
		Deployments: cfg.Deployments, Registry: cfg.Registry, Editor: cfg.TemplateEditor, VCS: vcsOn})
	server.Organization = cfg.Organization
	server.RunnerToken = cfg.RunnerToken
	// A catalog in Git (Settings → Templates) replaces TEMPLATES_DIR: its last loaded commit now, then what the leader syncs.
	if cfg.Deployments {
		if err := ops.LoadCatalog(ctx); err != nil {
			return err
		}
		if mode, commit := tpls.Source(); mode != "" {
			log.Info("templates loaded from git", "count", len(tpls.List()), "commit", commit, "editor", mode)
		}
	}
	if err := server.App.CheckTemplates(); err != nil {
		return err
	}
	log.Info("components", "deployments", cfg.Deployments, "registry", cfg.Registry, "vcs", vcsOn)
	sched := &app.Scheduler{Jobs: jobs(cfg, ops), DB: store.Pool.Config().ConnConfig, Retry: 5 * time.Second, Log: log,
		OnLead: func(on bool) {
			v := 0.0
			if on {
				v = 1
			}
			httpserver.SchedulerLeader.Set(v)
		}}
	defer sched.Start(ctx)()      // runs before store.Close: jobs stop first
	defer ops.Notify.Start(ctx)() // every replica: its runner claims and log streams wait on it

	if cfg.MetricsAddr != "" { // its own listener, so /metrics isn't reachable through the public ingress
		ln, err := net.Listen("tcp", cfg.MetricsAddr)
		if err != nil {
			return fmt.Errorf("METRICS_ADDR: %w", err)
		}
		msrv := &http.Server{Handler: server.MetricsHandler(), ReadHeaderTimeout: 10 * time.Second}
		go msrv.Serve(ln)
		defer msrv.Close()
		log.Info("metrics listening", "addr", cfg.MetricsAddr)
	}
	srv := &http.Server{Addr: cfg.Addr, Handler: server.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// Shutdown doesn't cancel requests; this ends log streams and runner long-polls on SIGTERM.
		BaseContext: func(net.Listener) context.Context { return ctx }}
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()
	log.Info("listening", "addr", cfg.Addr, "public_url", cfg.PublicURL.String())
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	<-drained // ListenAndServe returns as soon as Shutdown starts; keep the store open until requests finish
	return nil
}

// jobs is the server's background work. It runs on one replica at a time (app.Scheduler), except Local jobs.
func jobs(cfg config.Config, a *app.App) []app.Job {
	var js []app.Job
	if cfg.Registry {
		js = append(js, app.Job{Name: "vcs modules", Every: 5 * time.Minute,
			Run: a.PollVCSModules})
	}
	if !cfg.Deployments {
		return js
	}
	logged := func(f func(context.Context)) func(context.Context) error { // these log their own errors
		return func(ctx context.Context) error { f(ctx); return nil }
	}
	return append(js,
		// runs whose runner stopped heartbeating (crash, node loss) fail
		app.Job{Name: "reap leases", Every: 15 * time.Second, Run: logged(a.ReapLeases)},
		app.Job{Name: "orphaned locks", Every: 15 * time.Second,
			Run: logged(func(ctx context.Context) { a.ReleaseOrphanedLocks(ctx, cfg.StaleLockTimeout) })},
		app.Job{Name: "rollouts", Every: 15 * time.Second, Run: logged(a.TickRollouts)},
		app.Job{Name: "drift checks", Every: 15 * time.Second, Run: logged(a.QueueDriftChecks)},
		app.Job{Name: "retention", Every: time.Hour,
			Run: func(ctx context.Context) error { _, err := a.Retention(ctx); return err }},
		app.Job{Name: "vcs runs", Every: time.Minute,
			Run: a.PollVCSRuns},
		app.Job{Name: "catalog sync", Every: time.Minute,
			Run: a.PullCatalog},
		// every replica serves templates from memory: reload them when the leader (or an edit) changed the snapshot
		app.Job{Name: "catalog reload", Every: 10 * time.Second, Local: true,
			Run: a.ReloadCatalog},
	)
}
