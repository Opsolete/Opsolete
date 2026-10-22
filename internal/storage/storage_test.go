package storage

import (
	"context"
	"os"
	"testing"
)

// Requires a real PostgreSQL: TEST_DATABASE_URL=postgres://... go test ./...
func TestDeploymentRoundTrip(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	s, err := Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer s.DB.Close()
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(ctx); err != nil { // idempotent
		t.Fatal(err)
	}

	d, err := s.CreateDeployment(ctx, "test-tpl", "git::x", "v1.0.0", map[string]any{"name": "abc"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.DB.Exec(`DELETE FROM deployments WHERE id=$1`, d.ID) })
	if d.Status != "queued" || d.StateKey != "form2stack/test-tpl/"+d.ID+".tfstate" {
		t.Fatalf("unexpected deployment: %+v", d)
	}

	list, err := s.ListDeployments(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range list {
		if x.ID == d.ID && x.Inputs["name"] == "abc" {
			return
		}
	}
	t.Fatal("created deployment not listed")
}
