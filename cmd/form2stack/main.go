// Command form2stack runs the server (default), a runner, or creates API tokens:
//
//	form2stack                          serve the web UI, registry and runner API
//	form2stack runner                   execute deployments (no database needed)
//	form2stack token <name> [scope]     create an API token; scope is registry (default) or runner
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/rudesome/form2stack/internal/config"
	httpserver "github.com/rudesome/form2stack/internal/http"
	"github.com/rudesome/form2stack/internal/registry"
	"github.com/rudesome/form2stack/internal/runner"
	"github.com/rudesome/form2stack/internal/storage"
	"github.com/rudesome/form2stack/internal/templates"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	var err error
	switch {
	case len(os.Args) == 1:
		err = serve(log)
	case os.Args[1] == "runner" && len(os.Args) == 2:
		err = runRunner(log)
	case os.Args[1] == "token" && (len(os.Args) == 3 || len(os.Args) == 4):
		err = createToken()
	default:
		err = errors.New("usage: form2stack [runner | token <name> [registry|runner]]")
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
	return store, nil
}

func createToken() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	scope := storage.ScopeRegistry
	if len(os.Args) == 4 {
		scope = os.Args[3]
	}
	if scope != storage.ScopeRegistry && scope != storage.ScopeRunner {
		return errors.New("scope must be registry or runner")
	}
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
	reg := &registry.Registry{PublicURL: cfg.PublicURL, Key: key}

	tpls, err := templates.Open(cfg.TemplatesDir)
	if err != nil {
		return err
	}
	log.Info("templates loaded", "count", len(tpls.List()), "dir", cfg.TemplatesDir, "editor", cfg.TemplateEditor)

	go reapLostRunners(ctx, store, log)

	srv := &http.Server{Addr: cfg.Addr, Handler: httpserver.New(store, tpls, reg, cfg.TemplateEditor, log).Handler(),
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

// reapLostRunners fails running jobs whose runner stopped heartbeating (crash, node loss).
func reapLostRunners(ctx context.Context, store *storage.Store, log *slog.Logger) {
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
			}
		}
	}
}
