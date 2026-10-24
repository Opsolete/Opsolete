package storage

import (
	"context"
	"errors"
	"testing"
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
