package storage

import (
	"context"
	"crypto/rand"
	"strings"
	"testing"
)

// A sync that was fetching when the catalog was reconfigured, or that another sync overtook, must not
// overwrite the row: SyncedTemplateCatalog compares it to the row the sync started from.
func TestStaleCatalogSync(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	name := "cat-stale-" + strings.ToLower(rand.Text()[:8])
	t.Cleanup(func() {
		s.DB.Exec(`DELETE FROM template_catalog`)
		s.DB.Exec(`DELETE FROM vcs_connections WHERE name = $1`, name)
	})
	conn, err := s.CreateVCSConnection(ctx, VCSConnection{Name: name, Provider: "github", Token: []byte("x")})
	if err != nil {
		t.Fatal(err)
	}
	set := func(repo string) TemplateCatalog {
		t.Helper()
		if err := s.SetTemplateCatalog(ctx, TemplateCatalog{ConnectionID: conn, Repo: repo, Branch: "main", Mode: "off"}); err != nil {
			t.Fatal(err)
		}
		tc, err := s.GetTemplateCatalog(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return tc
	}
	synced := func(prev TemplateCatalog, commit, syncErr string) bool {
		t.Helper()
		ok, err := s.SyncedTemplateCatalog(ctx, prev, commit, map[string]string{"t.json": commit}, syncErr)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}

	staleA := set("o/a") // a sync of A starts, and pauses while fetching
	b := set("o/b")      // the admin switches to B
	if synced(staleA, "a1", "") || synced(staleA, "", "a failed") {
		t.Fatal("a sync of the old repository was stored")
	}
	if err := s.CheckedTemplateCatalog(ctx, staleA); err != nil {
		t.Fatal(err)
	}
	if !synced(b, "b1", "") {
		t.Fatal("the current sync was refused")
	}
	// Two syncs both started from b1; the first to finish wins, the other must not replace it.
	tc, _ := s.GetTemplateCatalog(ctx)
	if !synced(tc, "b3", "") || synced(tc, "b2", "") {
		t.Fatal("an overtaken sync was stored")
	}
	if tc, _ = s.GetTemplateCatalog(ctx); tc.Repo != "o/b" || tc.Commit != "b3" || tc.Files["t.json"] != "b3" || tc.SyncError != "" {
		t.Fatalf("catalog = %+v", tc)
	}
}

// synced_at is the last successful sync: a failed one records its error but leaves the time alone.
func TestFailedCatalogSyncKeepsSyncedAt(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	name := "cat-time-" + strings.ToLower(rand.Text()[:8])
	t.Cleanup(func() {
		s.DB.Exec(`DELETE FROM template_catalog`)
		s.DB.Exec(`DELETE FROM vcs_connections WHERE name = $1`, name)
	})
	conn, err := s.CreateVCSConnection(ctx, VCSConnection{Name: name, Provider: "github", Token: []byte("x")})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetTemplateCatalog(ctx, TemplateCatalog{ConnectionID: conn, Repo: "o/a", Branch: "main", Mode: "off"}); err != nil {
		t.Fatal(err)
	}
	tc, _ := s.GetTemplateCatalog(ctx)
	if ok, err := s.SyncedTemplateCatalog(ctx, tc, "c1", map[string]string{}, ""); !ok || err != nil {
		t.Fatal(ok, err)
	}
	s.DB.Exec(`UPDATE template_catalog SET synced_at = now() - interval '1 hour'`)
	tc, _ = s.GetTemplateCatalog(ctx)
	if ok, err := s.SyncedTemplateCatalog(ctx, tc, "", nil, "provider unavailable"); !ok || err != nil {
		t.Fatal(ok, err)
	}
	after, _ := s.GetTemplateCatalog(ctx)
	if after.SyncError != "provider unavailable" || after.SyncedAt == nil || !after.SyncedAt.Equal(*tc.SyncedAt) {
		t.Fatalf("after a failed sync: %+v, synced at %v before", after, tc.SyncedAt)
	}
	if err := s.CheckedTemplateCatalog(ctx, after); err != nil { // the provider is back, branch unchanged
		t.Fatal(err)
	}
	if back, _ := s.GetTemplateCatalog(ctx); back.SyncError != "" || !back.SyncedAt.After(*tc.SyncedAt) {
		t.Fatalf("after a successful check: %+v", back)
	}
}
