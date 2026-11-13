package e2e

import (
	"context"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"testing"

	"gocloud.dev/blob/memblob"

	"github.com/opsolete/opsolete/internal/app"
	"github.com/opsolete/opsolete/internal/registry"
	"github.com/opsolete/opsolete/internal/secrets"
	"github.com/opsolete/opsolete/internal/storage"
	"github.com/opsolete/opsolete/internal/templates"
	"github.com/opsolete/opsolete/internal/testdb"
	"github.com/opsolete/opsolete/internal/vcs"
)

// TestRealVCS reads real repositories (only with OPS_VCS_E2E=1 and the tokens below; read-only: no push, no
// pull request): a GitHub connection publishes a module's tags (OPS_E2E_GITHUB_REPO's resourcegroup folder,
// tags resourcegroup-v*) to the registry, an Azure DevOps connection loads the template catalog from a
// repository's JSON files (OPS_E2E_ADO_ORG, OPS_E2E_ADO_REPO as project/repo).
func TestRealVCS(t *testing.T) {
	if os.Getenv("OPS_VCS_E2E") != "1" {
		t.Skip("OPS_VCS_E2E=1 with OPS_E2E_GITHUB_TOKEN, OPS_E2E_GITHUB_REPO, OPS_E2E_ADO_TOKEN, OPS_E2E_ADO_ORG, OPS_E2E_ADO_REPO runs it")
	}
	env := func(k string) string {
		v := os.Getenv(k)
		if v == "" {
			t.Fatalf("%s is not set", k)
		}
		return v
	}
	ctx := context.Background()
	store, err := storage.Open(ctx, testdb.URL(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	store.AuditKey = []byte("e2e-audit-key")
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	keys, _ := secrets.New("e2e-secret-key", "")
	tpls, err := templates.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	public, _ := url.Parse("https://opsolete.test")
	a := &app.App{Store: store, Blobs: memblob.OpenBucket(nil), Secrets: keys, Templates: tpls, Log: slog.New(slog.DiscardHandler),
		Registry: &registry.Registry{PublicURL: public, Key: []byte("e2e-registry-key")}, RegistryEnabled: true, VCSEnabled: true,
		VCS: vcs.NewClient(nil)}
	admin := app.Actor{Email: "vcs-admin@example.com", Admin: true}

	t.Run("GitHub: a module's tags publish to the registry", func(t *testing.T) {
		conn, err := a.CreateVCSConnection(ctx, admin, app.NewVCSConnection{Name: "github", Provider: "github", Token: env("OPS_E2E_GITHUB_TOKEN")})
		if err != nil {
			t.Fatalf("connect (the token lists repositories): %v", err)
		}
		_, published, err := a.PublishVCSModule(ctx, admin, app.NewVCSModule{ConnectionID: conn, Repo: env("OPS_E2E_GITHUB_REPO"),
			Dir: "resourcegroup", TagPrefix: "resourcegroup-", Namespace: "e2e", Name: "resourcegroup", System: "azurerm"})
		if err != nil {
			t.Fatalf("publish: %v", err)
		}
		if len(published) == 0 {
			t.Fatal("no version published from the tags")
		}
		m, err := store.GetModule(ctx, "e2e", "resourcegroup", "azurerm", strings.TrimPrefix(published[0], "v"))
		if err != nil || len(m.Variables) == 0 {
			t.Fatalf("module %s: %+v %v", published[0], m.Variables, err)
		}
	})

	t.Run("Azure DevOps: the template catalog loads from a repository", func(t *testing.T) {
		conn, err := a.CreateVCSConnection(ctx, admin, app.NewVCSConnection{Name: "ado", Provider: "azure-devops",
			BaseURL: env("OPS_E2E_ADO_ORG"), Token: env("OPS_E2E_ADO_TOKEN")})
		if err != nil {
			t.Fatalf("connect (the token lists repositories): %v", err)
		}
		if err := a.SetTemplateCatalog(ctx, admin, app.CatalogSettings{ConnectionID: conn, Repo: env("OPS_E2E_ADO_REPO"), Mode: "off"}); err != nil {
			t.Fatalf("catalog: %v", err)
		}
		if mode, commit := a.Templates.Source(); mode == "" || commit == "" || len(a.Templates.List()) == 0 {
			t.Fatalf("catalog loaded: mode %q commit %q, %d templates", mode, commit, len(a.Templates.List()))
		}
	})
}
