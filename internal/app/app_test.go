package app

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"
	"gocloud.dev/blob/memblob"

	"github.com/opsolete/opsolete/internal/secrets"
	"github.com/opsolete/opsolete/internal/storage"
	"github.com/opsolete/opsolete/internal/testdb"
	"github.com/opsolete/opsolete/internal/vcs"
)

// testApp returns an App on a migrated database of t's own (testdb), or skips without TEST_DATABASE_URL.
func testApp(t *testing.T) *App {
	s, err := storage.Open(context.Background(), testdb.URL(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	s.AuditKey = []byte("test-audit-key")
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return &App{Store: s, Blobs: memblob.OpenBucket(nil), Secrets: keys("test-secret-key", ""), VCS: vcs.NewClient(nil),
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func keys(current, previous string) *secrets.Keyring {
	k, err := secrets.New(current, previous)
	if err != nil {
		panic(err)
	}
	return k
}

func uniq(prefix string) string { return prefix + strings.ToLower(rand.Text()[:10]) } // several per test

func user(t *testing.T, a *App, groups ...string) Actor {
	email := uniq("u-") + "@example.com"
	id, err := a.Store.UpsertUser(context.Background(), "iss "+email, email, "user", groups)
	if err != nil {
		t.Fatal(err)
	}
	return Actor{UserID: id, Email: email}
}

// emptyWorkspace creates a workspace in the default project that by created (and so operates), also
// where by may not create workspaces: who may is TestNewWorkspaceProject's.
func emptyWorkspace(t *testing.T, a *App, by Actor) storage.Workspace {
	ws, err := a.Store.CreateEmptyWorkspace(context.Background(), storage.EmptyWorkspace{Name: uniq("app-"), CreatedBy: by.UserID})
	if err != nil {
		t.Fatal(err)
	}
	return ws
}

func TestWorkspaceAccess(t *testing.T) {
	a, ctx := testApp(t), context.Background()
	group := uniq("grp-")
	owner, reader, operator, stranger := user(t, a), user(t, a, group), user(t, a, group+"-op"), user(t, a)
	admin := Actor{Email: "admin@example.com", Admin: true}
	ws := emptyWorkspace(t, a, owner)
	if err := a.SetGrant(ctx, admin, ws.Name, group, "read"); err != nil {
		t.Fatal(err)
	}
	if err := a.SetGrant(ctx, admin, ws.Name, group+"-op", "operate"); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		who   string
		actor Actor
		need  Level
		level Level
		err   error
	}{
		{"owner", owner, LevelOperate, LevelOperate, nil},
		{"admin", admin, LevelOperate, LevelOperate, nil},
		{"operate grant", operator, LevelOperate, LevelOperate, nil},
		{"read grant reads", reader, LevelRead, LevelRead, nil},
		{"read grant operates", reader, LevelOperate, LevelRead, ErrForbidden},
		{"no access", stranger, LevelRead, LevelNone, ErrNotFound},
	} {
		_, level, err := a.Workspace(ctx, c.actor, ws.Name, c.need)
		if level != c.level || !errors.Is(err, c.err) || (c.err == nil) != (err == nil) {
			t.Errorf("%s: level %v err %v, want %v %v", c.who, level, err, c.level, c.err)
		}
		if _, _, err2 := a.WorkspaceByID(ctx, c.actor, ws.ID, c.need); (err == nil) != (err2 == nil) {
			t.Errorf("%s: by id %v, by name %v", c.who, err2, err)
		}
	}
	if _, _, err := a.Workspace(ctx, admin, uniq("missing-"), LevelRead); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown workspace: %v", err)
	}
	if err := a.SetGrant(ctx, owner, ws.Name, group, "operate"); !errors.Is(err, ErrForbidden) {
		t.Errorf("a non-admin grants: %v", err)
	}
	if err := a.DeleteWorkspace(ctx, stranger, ws.Name, false); !errors.Is(err, ErrNotFound) {
		t.Errorf("a stranger deletes: %v", err)
	}
	if err := a.DeleteWorkspace(ctx, reader, ws.Name, false); !errors.Is(err, ErrForbidden) {
		t.Errorf("a reader deletes: %v", err)
	}
}

// A workspace's VCS link and execution mode are created with it, or none of them is.
func TestCreateEmptyWorkspaceAtomic(t *testing.T) {
	a, ctx := testApp(t), context.Background()
	admin := Actor{Email: "admin@example.com", Admin: true}
	name := uniq("atomic-")
	_, err := a.Store.CreateEmptyWorkspace(ctx, storage.EmptyWorkspace{Name: name, VCS: &storage.VCSLink{ConnectionID: uuid.New(), Repo: "o/r", Branch: "main"}})
	if err == nil {
		t.Fatal("a link to an unknown connection was created")
	}
	if _, err := a.Store.GetWorkspace(ctx, name); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("failed link left a workspace: %v", err)
	}
	if _, err := a.Store.CreateEmptyWorkspace(ctx, storage.EmptyWorkspace{Name: name, ExecutionMode: "bogus"}); err == nil {
		t.Fatal("a bogus execution mode was saved")
	}
	if _, err := a.Store.GetWorkspace(ctx, name); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("failed execution mode left a workspace: %v", err)
	}
	if _, err := a.CreateEmptyWorkspace(ctx, admin, EmptyWorkspace{Name: name, ExecutionMode: "bogus"}); !isValidation(err) {
		t.Fatalf("bogus mode: %v", err)
	}
	ws, err := a.CreateEmptyWorkspace(ctx, admin, EmptyWorkspace{Name: name, ExecutionMode: "local"})
	if err != nil {
		t.Fatal(err)
	}
	if ws.ExecutionMode != "local" {
		t.Fatalf("mode: %q", ws.ExecutionMode)
	}
	if _, err := a.CreateEmptyWorkspace(ctx, admin, EmptyWorkspace{Name: name}); !isValidation(err) {
		t.Fatalf("taken name: %v", err)
	}
}

func isValidation(err error) bool { _, ok := errors.AsType[*ValidationError](err); return ok }

func TestDeleteWorkspaceRemovesBlobs(t *testing.T) {
	a, ctx := testApp(t), context.Background()
	admin := Actor{Email: "admin@example.com", Admin: true}
	ws := emptyWorkspace(t, a, admin)
	key := "test/" + ws.ID.String() + ".tfstate"
	if err := a.Blobs.WriteAll(ctx, key, []byte("{}"), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Store.Pool.Exec(ctx, `INSERT INTO state_versions (workspace_id, serial, lineage, blob_key, md5, size, indexed)
		VALUES ($1, 1, 'l', $2, 'm', 2, true)`, ws.ID, key); err != nil {
		t.Fatal(err)
	}
	if err := a.DeleteWorkspace(ctx, admin, ws.Name, false); err != nil {
		t.Fatal(err)
	}
	if ok, _ := a.Blobs.Exists(ctx, key); ok {
		t.Fatal("state file left behind")
	}
	if _, _, err := a.Workspace(ctx, admin, ws.Name, LevelRead); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted workspace: %v", err)
	}
}

// What would lose state for good or expose it to everyone is admins': deleting a
// workspace that may still manage resources (force), and sharing its state with all workspaces.
func TestAdminOnlyWorkspaceActions(t *testing.T) {
	a, ctx := testApp(t), context.Background()
	owner := user(t, a)
	admin := Actor{Email: "admin@example.com", Admin: true}
	ws := emptyWorkspace(t, a, owner)
	if err := a.ShareState(ctx, owner, ws.Name, true, nil); !errors.Is(err, ErrForbidden) {
		t.Fatalf("an operator shares the state with everyone: %v", err)
	}
	if err := a.ShareState(ctx, admin, ws.Name, true, nil); err != nil {
		t.Fatal(err)
	}
	if err := a.ShareState(ctx, owner, ws.Name, true, nil); err != nil { // already shared: saving the page again
		t.Fatalf("saving a shared workspace's settings: %v", err)
	}
	if err := a.DeleteWorkspace(ctx, owner, ws.Name, true); !errors.Is(err, ErrForbidden) {
		t.Fatalf("an operator force-deletes: %v", err)
	}
	if err := a.DeleteWorkspace(ctx, owner, ws.Name, false); err != nil { // nothing deployed: anyone who operates it
		t.Fatal(err)
	}
}
