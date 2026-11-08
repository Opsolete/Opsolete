package app

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/opsolete/opsolete/internal/registry"
	"github.com/opsolete/opsolete/internal/storage"
	"github.com/opsolete/opsolete/internal/vcs"
)

// fakeGitHub serves acme/infra with branch main to token "pat" and returns its API root.
func fakeGitHub(t *testing.T) string {
	gh := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Header.Get("Authorization") != "Bearer pat":
			http.Error(w, "bad credentials", http.StatusUnauthorized)
		case r.URL.Path == "/user/repos":
			w.Write([]byte(`[{"full_name":"acme/infra"}]`))
		case r.URL.Path == "/repos/acme/infra/branches/main":
			w.Write([]byte(`{"commit":{"sha":"sha1"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(gh.Close)
	old := vcs.HTTPClient
	vcs.HTTPClient = gh.Client()
	t.Cleanup(func() { vcs.HTTPClient = old })
	return gh.URL
}

func TestVCSConnectionsAdminOnly(t *testing.T) {
	a, ctx := testApp(t), context.Background()
	base := fakeGitHub(t)
	a.VCSEnabled = true
	admin, someone := Actor{Email: "admin@example.com", Admin: true}, user(t, a)
	in := NewVCSConnection{Name: uniq("gh-"), Provider: vcs.GitHub, BaseURL: base, Token: "pat"}
	if _, err := a.CreateVCSConnection(ctx, someone, in); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a non-admin adds a connection: %v", err)
	}
	if _, err := a.CreateVCSConnection(ctx, admin, NewVCSConnection{Name: in.Name, Provider: vcs.GitHub, BaseURL: base, Token: "wrong"}); !isValidation(err) {
		t.Fatalf("a token the provider refuses: %v", err)
	}
	id, err := a.CreateVCSConnection(ctx, admin, in)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.CreateVCSConnection(ctx, admin, in); !isValidation(err) {
		t.Fatalf("a taken name: %v", err)
	}
	if err := a.SetVCSScope(ctx, someone, id, VCSScope{Scope: "all"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a non-admin sets the scope: %v", err)
	}
	if err := a.DeleteVCSConnection(ctx, someone, id); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a non-admin deletes: %v", err)
	}

	// deleting a connection unlinks the workspaces that link it
	if err := a.SetVCSScope(ctx, admin, id, VCSScope{Scope: "all"}); err != nil {
		t.Fatal(err)
	}
	ws := emptyWorkspace(t, a, admin)
	if _, err := a.LinkVCS(ctx, admin, ws.Name, VCSLinkInput{ConnectionID: id, Repo: "acme/infra"}); err != nil {
		t.Fatal(err)
	}
	if err := a.DeleteVCSConnection(ctx, admin, id); err != nil {
		t.Fatal(err)
	}
	if ws, _ := a.Store.GetWorkspaceByID(ctx, ws.ID); ws.VCSConnectionID != uuid.Nil {
		t.Fatalf("still linked to a deleted connection: %+v", ws)
	}
	if err := a.DeleteVCSConnection(ctx, admin, id); err != nil {
		t.Fatalf("deleting a deleted connection: %v", err)
	}
}

func testConnection(t *testing.T, a *App, base, scope string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	c := storage.VCSConnection{Name: uniq("gh-"), Provider: vcs.GitHub, BaseURL: base}
	var err error
	if c.Token, err = a.Secrets.Seal("pat", TokenAD(c)); err != nil {
		t.Fatal(err)
	}
	id, err := a.Store.CreateVCSConnection(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Store.SetVCSScope(ctx, id, scope); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestLinkVCSScope(t *testing.T) {
	a, ctx := testApp(t), context.Background()
	base := fakeGitHub(t)
	a.VCSEnabled = true
	admin := Actor{Email: "admin@example.com", Admin: true}
	closed := testConnection(t, a, base, "") // modules only
	ws := emptyWorkspace(t, a, admin)
	link := VCSLinkInput{ConnectionID: closed, Repo: "acme/infra", Branch: "main"}
	if _, err := a.LinkVCS(ctx, admin, ws.Name, link); !isValidation(err) {
		t.Fatalf("linking a connection closed to workspaces: %v", err)
	}
	name := uniq("app-")
	if _, err := a.CreateEmptyWorkspace(ctx, admin, EmptyWorkspace{Name: name, VCS: link}); !isValidation(err) {
		t.Fatalf("creating a workspace linked through a closed connection: %v", err)
	}
	if _, err := a.Store.GetWorkspace(ctx, name); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a refused link created the workspace: %v", err)
	}
	if conns, _, err := a.LinkableVCS(ctx, admin, ws.Name, "", uuid.Nil); err != nil || slices.ContainsFunc(conns, func(c storage.VCSConnection) bool { return c.ID == closed }) {
		t.Fatalf("a closed connection is offered: %v", err)
	}
	// selecting the workspace allows it, others stay refused
	if err := a.SetVCSScope(ctx, admin, closed, VCSScope{Scope: "selected", Workspaces: []string{ws.Name}}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.LinkVCS(ctx, admin, ws.Name, link); err != nil {
		t.Fatalf("linking a selected workspace: %v", err)
	}
	if _, err := a.LinkVCS(ctx, admin, emptyWorkspace(t, a, admin).Name, link); !isValidation(err) {
		t.Fatalf("linking an unselected workspace: %v", err)
	}
	reader := user(t, a)
	if _, err := a.LinkVCS(ctx, reader, ws.Name, link); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a stranger links: %v", err)
	}
}

func TestLinkVCSDuringRun(t *testing.T) {
	a, ctx := testApp(t), context.Background()
	base := fakeGitHub(t)
	a.VCSEnabled = true
	admin := Actor{Email: "admin@example.com", Admin: true}
	conn := testConnection(t, a, base, "all")
	ws, _, err := a.Store.CreateWorkspace(ctx, uuid.New(), uniq("app-"), "", storage.WorkspaceTemplate{Name: "main", TemplateID: "t",
		ModuleSource: "git::https://x//m", ModuleRef: "v1"}, false, uuid.Nil) // with its first run queued
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.LinkVCS(ctx, admin, ws.Name, VCSLinkInput{ConnectionID: conn, Repo: "acme/infra"}); !IsConflict(err) || !errors.Is(err, storage.ErrRunActive) {
		t.Fatalf("linking during an active run: %v", err)
	}
	if ws, _ := a.Store.GetWorkspaceByID(ctx, ws.ID); ws.VCSConnectionID != uuid.Nil {
		t.Fatal("a refused link was saved")
	}
}

func TestVCSArchiveSignature(t *testing.T) {
	a, ctx := testApp(t), context.Background()
	a.Registry = &registry.Registry{PublicURL: &url.URL{Scheme: "https", Host: "ops.example.com"}, Key: []byte("test-key")}
	run, other := uuid.New(), uuid.New()
	signed, err := url.Parse(a.Registry.SignedArchiveURL(VCSArchivePath(run), time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	q := signed.Query()
	if _, err := a.VCSArchive(ctx, other, q.Get("expires"), q.Get("sig")); !errors.Is(err, ErrForbidden) {
		t.Fatalf("another run's link: %v", err)
	}
	if _, err := a.VCSArchive(ctx, run, q.Get("expires"), q.Get("sig")+"0"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a tampered link: %v", err)
	}
	expired, _ := url.Parse(a.Registry.SignedArchiveURL(VCSArchivePath(run), -time.Minute))
	if _, err := a.VCSArchive(ctx, run, expired.Query().Get("expires"), expired.Query().Get("sig")); !errors.Is(err, ErrForbidden) {
		t.Fatalf("an expired link: %v", err)
	}
	if _, err := a.VCSArchive(ctx, run, strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10), q.Get("sig")); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a longer expiry with the same signature: %v", err)
	}
	if _, err := a.VCSArchive(ctx, run, q.Get("expires"), q.Get("sig")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a valid link to an unknown run: %v", err)
	}
}
