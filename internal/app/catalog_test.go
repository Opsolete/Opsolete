package app

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/opsolete/opsolete/internal/registry"
	"github.com/opsolete/opsolete/internal/storage"
	"github.com/opsolete/opsolete/internal/templates"
	"github.com/opsolete/opsolete/internal/vcs"
)

var testRegistry = &registry.Registry{PublicURL: &url.URL{Scheme: "https", Host: "ops.example.com"}, Key: []byte("test-key")}

func TestCatalogTemplatesAdminOnly(t *testing.T) {
	a, ctx := testApp(t), context.Background()
	var err error
	if a.Templates, err = templates.Open(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	a.Registry, a.RegistryEnabled, a.TemplateEditor = testRegistry, true, true
	admin, someone := Actor{Email: "admin@example.com", Admin: true}, user(t, a)
	tpl := templates.Template{Name: "Bucket", Module: templates.Module{Source: "git::https://example.com/m.git", Ref: "v1"}}

	if _, _, err := a.SaveCatalogTemplate(ctx, someone, tpl, false); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a non-admin saves a template: %v", err)
	}
	if err := a.SetTemplateCatalog(ctx, someone, CatalogSettings{Mode: "pr"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a non-admin sets the catalog: %v", err)
	}
	if err := a.DisconnectCatalog(ctx, someone); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a non-admin disconnects the catalog: %v", err)
	}
	if _, err := a.TemplateCatalog(ctx, someone); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a non-admin reads the catalog settings: %v", err)
	}
	if err := a.SetTemplateCatalog(ctx, admin, CatalogSettings{Mode: "pr"}); !isValidation(err) {
		t.Fatalf("a catalog without a connection: %v", err)
	}
	for name, bad := range map[string]templates.Template{
		"no name":                  {Module: tpl.Module},
		"unknown registry version": {Name: "x", Module: templates.Module{Source: "ops.example.com/acme/none/aws", Ref: "9.9.9"}},
	} {
		if _, _, err := a.SaveCatalogTemplate(ctx, admin, bad, false); !isValidation(err) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	saved, pr, err := a.SaveCatalogTemplate(ctx, admin, tpl, false)
	if err != nil || pr != "" || !uuidRe.MatchString(saved.ID) {
		t.Fatalf("save: %+v %q %v", saved, pr, err)
	}
	if _, ok := a.Templates.Get(saved.ID); !ok {
		t.Fatal("the saved template isn't in the catalog")
	}
	tpl.ID = "not-there"
	if _, _, err := a.SaveCatalogTemplate(ctx, admin, tpl, true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("editing an unknown template: %v", err)
	}
	a.TemplateEditor = false
	if _, _, err := a.DeleteCatalogTemplate(ctx, admin, saved.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("deleting with the builder off: %v", err)
	}
	a.TemplateEditor = true
	if _, _, err := a.DeleteCatalogTemplate(ctx, admin, saved.ID); err != nil {
		t.Fatal(err)
	}
}

func TestUploadModuleRefused(t *testing.T) {
	a, ctx := testApp(t), context.Background()
	var err error
	if a.Templates, err = templates.Open(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	a.Registry, a.RegistryEnabled = testRegistry, true
	admin, someone := Actor{Email: "admin@example.com", Admin: true}, user(t, a)
	was, err := a.Store.ModuleUploads(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Store.SetModuleUploads(context.Background(), was) })
	in := ModuleUpload{Namespace: "acme", Name: "bucket", System: "aws", Version: "1.0.0"}
	if err := a.SetModuleUploads(ctx, someone, true); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a non-admin turns uploads on: %v", err)
	}
	if err := a.SetModuleUploads(ctx, admin, false); err != nil {
		t.Fatal(err)
	}
	if _, err := a.UploadModule(ctx, admin, in); !errors.Is(err, ErrForbidden) {
		t.Fatalf("an upload while uploads are off: %v", err)
	}
	if err := a.SetModuleUploads(ctx, admin, true); err != nil {
		t.Fatal(err)
	}
	if _, err := a.UploadModule(ctx, someone, in); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a non-admin uploads: %v", err)
	}
	if _, err := a.UploadModule(ctx, admin, in); !isValidation(err) {
		t.Fatalf("an upload without archive: %v", err)
	}
	if err := a.DeleteModule(ctx, someone, "acme", "bucket", "aws"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a non-admin deletes a module: %v", err)
	}
}

func TestRevokeOwnTokensOnly(t *testing.T) {
	a, ctx := testApp(t), context.Background()
	me, them := user(t, a), user(t, a)
	theirs, err := a.Store.CreateUserToken(ctx, them.UserID, "t", UserTokenTTL)
	if err != nil {
		t.Fatal(err)
	}
	list, err := a.UserTokens(ctx, them)
	if err != nil || len(list) != 1 {
		t.Fatalf("their tokens: %v %v", list, err)
	}
	if err := a.RevokeUserToken(ctx, me, list[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := a.RevokeUserTokens(ctx, me); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, err := a.TokenUser(ctx, theirs); err != nil {
		t.Fatalf("another user revoked their token: %v", err)
	}
	if err := a.RevokeUserToken(ctx, them, list[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, err := a.TokenUser(ctx, theirs); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a revoked token still works: %v", err)
	}
}

func TestDeleteVCSConnectionDuringRun(t *testing.T) {
	a, ctx := testApp(t), context.Background()
	base := fakeGitHub(t)
	a.VCSEnabled = true
	admin := Actor{Email: "admin@example.com", Admin: true}
	conn := testConnection(t, a, base, "all")
	ws := emptyWorkspace(t, a, admin)
	if _, err := a.LinkVCS(ctx, admin, ws.Name, VCSLinkInput{ConnectionID: conn, Repo: "acme/infra"}); err != nil {
		t.Fatal(err)
	}
	if ok, err := a.Store.QueueVCSRun(ctx, ws.ID, "sha1", 0, "push"); err != nil || !ok {
		t.Fatalf("queue a run: %v %v", ok, err)
	}
	if err := a.DeleteVCSConnection(ctx, admin, conn); !IsConflict(err) || !errors.Is(err, storage.ErrRunActive) {
		t.Fatalf("deleting a connection a running workspace links: %v", err)
	}
	if ws, _ := a.Store.GetWorkspaceByID(ctx, ws.ID); ws.VCSConnectionID != conn {
		t.Fatal("a refused delete unlinked the workspace")
	}
}

// A token replaced by someone else during the connection check is a race: a conflict.
func TestReplaceVCSTokenRace(t *testing.T) {
	a, ctx := testApp(t), context.Background()
	admin := Actor{Email: "admin@example.com", Admin: true}
	var conn storage.VCSConnection
	gh := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// another admin replaces the token meanwhile
		if _, err := a.Store.Pool.Exec(context.Background(), `UPDATE vcs_connections SET token = token || '\x00'::bytea WHERE id = $1`, conn.ID); err != nil {
			t.Error(err)
		}
		w.Write([]byte(`[]`))
	}))
	t.Cleanup(gh.Close)
	old := vcs.HTTPClient
	vcs.HTTPClient = gh.Client()
	t.Cleanup(func() { vcs.HTTPClient = old })
	conn.ID = testConnection(t, a, gh.URL, "")
	if _, err := a.ReplaceVCSToken(ctx, admin, conn.ID, "new"); !IsConflict(err) || !errors.Is(err, ErrChanged) {
		t.Fatalf("a token replaced meanwhile: %v", err)
	}
}
