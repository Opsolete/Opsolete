package storage

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/opsolete/opsolete/internal/auth"
)

// Requires a real PostgreSQL: TEST_DATABASE_URL=postgres://... go test ./...
func TestWorkspaceRoundTrip(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.Migrate(ctx); err != nil { // idempotent
		t.Fatal(err)
	}
	w, run := newWorkspace(t, s, false)
	if w.StateKey != "opsolete/job-test/"+w.ID+".tfstate" || w.Inputs["n"] != 1.0 || run.Status != PlanQueued || run.Action != "apply" {
		t.Fatalf("unexpected workspace/run: %+v %+v", w, run)
	}
	if _, _, err := s.CreateWorkspace(ctx, w.Name, "job-test", "git::x", "v1", nil, false, ""); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("duplicate name: %v", err)
	}
	list, err := s.ListWorkspaces(ctx, Viewer{Admin: true})
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
	if err := s.DB.QueryRowContext(ctx, `SELECT email, role, cardinality(groups) FROM users WHERE id = $1`, pre).Scan(&email, &role, &groups); err != nil ||
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
