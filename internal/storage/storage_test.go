package storage

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opsolete/opsolete/internal/auth"
)

// A database with a migration this version doesn't know is refused, and the message tells a newer
// Opsolete's database (never to be wiped) from a 0.x one.
func TestMigrateRefusesUnknownMigrations(t *testing.T) {
	s, ctx := testStore(t), context.Background()
	if _, err := s.Pool.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ('migrations/999_future.sql')`); err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(ctx); err == nil || !strings.Contains(err.Error(), "newer Opsolete") || strings.Contains(err.Error(), "empty database") {
		t.Fatalf("a newer version's database: %v", err)
	}
	if _, err := s.Pool.Exec(ctx, `DELETE FROM schema_migrations; INSERT INTO schema_migrations (version) VALUES ('migrations/001_init.sql')`); err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(ctx); err == nil || !strings.Contains(err.Error(), "0.5 or earlier") {
		t.Fatalf("a 0.x database: %v", err)
	}
}

// Requires a real PostgreSQL: TEST_DATABASE_URL=postgres://... go test ./...
func TestWorkspaceRoundTrip(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.Migrate(ctx); err != nil { // idempotent
		t.Fatal(err)
	}
	w, run := newWorkspace(t, s, false)
	added, _ := s.WorkspaceTemplates(ctx, w.ID)
	if w.Templates != 1 || len(added) != 1 || added[0].Inputs["n"] != 1.0 ||
		run.Status != PlanQueued || run.Action != "apply" {
		t.Fatalf("unexpected workspace/run: %+v %+v", w, run)
	}
	if _, _, err := s.CreateWorkspace(ctx, uuid.Nil, w.Name, "", testTemplate(nil), false, uuid.Nil); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("duplicate name: %v", err)
	}
	list, err := s.ListWorkspaces(ctx, Viewer{Admin: true}, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range list {
		if x.Name == w.Name && x.Latest != nil && x.Latest.Status == PlanQueued {
			return
		}
	}
	t.Fatal("created workspace not listed with its latest run")
}

// Users are keyed by OIDC subject: an email can move with its subject, but not to another subject.
func TestUpsertUserSubject(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	sfx := fmt.Sprint(time.Now().UnixNano())
	a, b := "iss sub-a-"+sfx, "iss sub-b-"+sfx
	old, nu := "old-"+sfx+"@example.com", "new-"+sfx+"@example.com"

	pre, err := s.UpsertUser(ctx, "", old, "user", nil) // a user from before subjects: bound at first login
	if err != nil {
		t.Fatal(err)
	}
	if id, err := s.UpsertUser(ctx, a, old, "admin", []string{"g1"}); err != nil || id != pre {
		t.Fatalf("first login binds the existing user: %q %v (want %q)", id, err, pre)
	}
	if _, err := s.UpsertUser(ctx, b, old, "admin", nil); !errors.Is(err, auth.ErrEmailTaken) {
		t.Fatalf("another subject with a reused email: %v", err)
	}
	if id, err := s.UpsertUser(ctx, a, nu, "user", nil); err != nil || id != pre {
		t.Fatalf("email change keeps the user: %q %v", id, err)
	}
	var email, role string
	var groups int
	if err := s.Pool.QueryRow(ctx, `SELECT email, role, cardinality(groups) FROM users WHERE id = $1`, pre).Scan(&email, &role, &groups); err != nil ||
		email != nu || role != "user" || groups != 0 {
		t.Fatalf("email, role and groups follow the last login: %q %q %v %v", email, role, groups, err)
	}
	other, err := s.UpsertUser(ctx, b, old, "admin", nil) // the freed address: a new user, not the old account
	if err != nil || other == pre {
		t.Fatalf("freed email for another subject: %q %v", other, err)
	}
	if _, err := s.UpsertUser(ctx, a, old, "user", nil); !errors.Is(err, auth.ErrEmailTaken) {
		t.Fatalf("subject moving to an email another subject holds: %v", err)
	}
}
