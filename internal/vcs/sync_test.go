package vcs

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/opsolete/opsolete/internal/registry/registrytest"
	"github.com/opsolete/opsolete/internal/storage"
	"github.com/opsolete/opsolete/internal/testdb"
)

// Tags without the module's folder are downloaded once; the cache keeps only tags still listed and
// modules still linked.
func TestNoFolderCache(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, err := storage.Open(ctx, testdb.URL(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	tags, downloads := `[{"name":"v1.0.0","commit":{"sha":"c1"}},{"name":"v1.1.0","commit":{"sha":"c2"}}]`, 0
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.URL.Path == "/repos/acme/mono/tags":
			w.Write([]byte(tags))
		case strings.HasPrefix(r.URL.Path, "/repos/acme/mono/tarball/"):
			downloads++
			w.Write(registrytest.TarGz(t, map[string]string{"acme-mono-x/other/main.tf": ""}))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	connID, err := st.CreateVCSConnection(ctx, storage.VCSConnection{Name: "gh", Provider: GitHub, BaseURL: srv.URL, Token: []byte("x")})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.LinkVCSModule(ctx, storage.VCSModule{Namespace: "acme", Name: "vm", System: "aws", ConnectionID: connID, Repo: "acme/mono", Dir: "vm"}); err != nil {
		t.Fatal(err)
	}
	m, err := st.GetVCSModule(ctx, "acme", "vm", "aws")
	if err != nil {
		t.Fatal(err)
	}
	cl := NewClient(srv.Client())
	open := func(storage.VCSConnection) (string, error) { return "t", nil }
	check := func(wantDownloads int, wantCached ...string) {
		t.Helper()
		if _, err := cl.Sync(ctx, st, open, m); err == nil || !strings.Contains(err.Error(), "folder missing") {
			t.Fatalf("sync: %v", err)
		}
		mu.Lock()
		defer mu.Unlock()
		if downloads != wantDownloads || len(cl.noFolder[noFolderKey(m)]) != len(wantCached) {
			t.Fatalf("%d downloads, cached %v; want %d, %v", downloads, cl.noFolder, wantDownloads, wantCached)
		}
		for _, c := range wantCached {
			if cl.noFolder[noFolderKey(m)][c] == nil {
				t.Fatalf("%s not cached: %v", c, cl.noFolder)
			}
		}
		downloads = 0
	}
	check(2, "c1", "c2")
	check(0, "c1", "c2") // known: no download
	mu.Lock()
	tags = `[{"name":"v1.1.0","commit":{"sha":"c2"}}]` // v1.0.0 deleted
	mu.Unlock()
	check(0, "c2")
	if err := st.UnlinkVCSModule(ctx, "acme", "vm", "aws"); err != nil {
		t.Fatal(err)
	}
	if err := cl.Poll(ctx, st, open, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil || len(cl.noFolder) != 0 {
		t.Fatalf("an unlinked module stays cached: %v %v", cl.noFolder, err)
	}
}
