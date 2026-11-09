package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/opsolete/opsolete/internal/registry/registrytest"
	"github.com/opsolete/opsolete/internal/storage"
	"github.com/opsolete/opsolete/internal/vcs"
)

// Tags without the module's folder are downloaded once; the cache keeps only tags still listed and
// modules still linked.
func TestNoFolderCache(t *testing.T) {
	t.Parallel()
	a, ctx := testApp(t), context.Background()
	var mu sync.Mutex
	tags, downloads := `[{"name":"v1.0.0","commit":{"sha":"c1"}},{"name":"v1.1.0","commit":{"sha":"c2"}}]`, 0
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.URL.Path == "/user/repos":
			w.Write([]byte(`[{"full_name":"acme/mono"}]`))
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
	a.VCS = vcs.NewClient(srv.Client())
	admin := Actor{Email: "admin@example.com", Admin: true}
	connID, err := a.CreateVCSConnection(ctx, admin, NewVCSConnection{Name: "gh", Provider: vcs.GitHub, BaseURL: srv.URL, Token: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Store.LinkVCSModule(ctx, storage.VCSModule{Namespace: "acme", Name: "vm", System: "aws", ConnectionID: connID, Repo: "acme/mono", Dir: "vm"}); err != nil {
		t.Fatal(err)
	}
	m, err := a.Store.GetVCSModule(ctx, "acme", "vm", "aws")
	if err != nil {
		t.Fatal(err)
	}
	check := func(wantDownloads int, wantCached ...string) {
		t.Helper()
		if _, err := a.syncTags(ctx, m); err == nil || !strings.Contains(err.Error(), "folder missing") {
			t.Fatalf("sync: %v", err)
		}
		mu.Lock()
		defer mu.Unlock()
		if downloads != wantDownloads || len(a.noFolder.m[noFolderKey(m)]) != len(wantCached) {
			t.Fatalf("%d downloads, cached %v; want %d, %v", downloads, a.noFolder.m, wantDownloads, wantCached)
		}
		for _, c := range wantCached {
			if a.noFolder.m[noFolderKey(m)][c] == nil {
				t.Fatalf("%s not cached: %v", c, a.noFolder.m)
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
	if err := a.Store.UnlinkVCSModule(ctx, "acme", "vm", "aws"); err != nil {
		t.Fatal(err)
	}
	if err := a.PollVCSModules(ctx); err != nil || len(a.noFolder.m) != 0 {
		t.Fatalf("an unlinked module stays cached: %v %v", a.noFolder.m, err)
	}
}
