package vcs

import (
	"archive/zip"
	"bytes"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/opsolete/opsolete/internal/registry"
	"github.com/opsolete/opsolete/internal/registry/registrytest"
)

const mainTF = `variable "name" { type = string }`

// fakeVCS serves the GitHub and Azure DevOps endpoints this package uses, and checks auth.
func fakeVCS(t *testing.T) *httptest.Server {
	mux := http.NewServeMux()
	gh := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer gh-token" {
				http.Error(w, "bad credentials", http.StatusUnauthorized)
				return
			}
			h(w, r)
		}
	}
	mux.HandleFunc("GET /gh/user/repos", gh(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[{"full_name":"acme/terraform-azurerm-storage"}]`))
	}))
	mux.HandleFunc("GET /gh/repos/acme/terraform-azurerm-storage/tags", gh(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[{"name":"v1.1.0"},{"name":"latest"}]`))
	}))
	mux.HandleFunc("GET /gh/repos/acme/terraform-azurerm-storage/branches/main", gh(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"name":"main","commit":{"sha":"abc123"}}`))
	}))
	mux.HandleFunc("GET /gh/repos/acme/terraform-azurerm-storage/tarball/v1.1.0", gh(func(w http.ResponseWriter, r *http.Request) {
		// Like GitHub: everything under one "owner-repo-sha/" directory.
		w.Write(registrytest.TarGz(t, map[string]string{"acme-storage-abc123/main.tf": mainTF, "acme-storage-abc123/README.md": "hi"}))
	}))
	az := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if _, pw, _ := r.BasicAuth(); pw != "az-pat" {
				w.WriteHeader(http.StatusNonAuthoritativeInfo) // what Azure DevOps really does
				w.Write([]byte("<html>sign in</html>"))
				return
			}
			h(w, r)
		}
	}
	mux.HandleFunc("GET /org/_apis/git/repositories", az(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"value":[{"name":"storage","project":{"name":"Platform Team"}}]}`))
	}))
	mux.HandleFunc("GET /org/Platform Team/_apis/git/repositories/storage/refs", az(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("filter") == "heads/main" { // a prefix match, like the real one
			w.Write([]byte(`{"value":[{"name":"refs/heads/main-old","objectId":"old"},{"name":"refs/heads/main","objectId":"def456"}]}`))
			return
		}
		w.Write([]byte(`{"value":[{"name":"refs/tags/2.0.0"}]}`))
	}))
	mux.HandleFunc("GET /org/Platform Team/_apis/git/repositories/storage/items", az(func(w http.ResponseWriter, r *http.Request) {
		if q := r.URL.Query(); q.Get("versionDescriptor.version") != "2.0.0" || q.Get("$format") != "zip" {
			http.Error(w, "bad query "+r.URL.RawQuery, http.StatusBadRequest)
			return
		}
		var buf bytes.Buffer
		zw := zip.NewWriter(&buf)
		for name, body := range map[string]string{"main.tf": mainTF, "../evil.tf": "x", "sub/x.tf": ""} {
			f, _ := zw.Create(name)
			f.Write([]byte(body))
		}
		zw.Close()
		w.Write(buf.Bytes())
	}))
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	old := HTTPClient
	HTTPClient = srv.Client()
	t.Cleanup(func() { HTTPClient = old })
	return srv
}

func TestProviders(t *testing.T) {
	srv := fakeVCS(t)
	ctx := t.Context()
	for _, tc := range []struct {
		conn       Conn
		repo, tag  string
		wantFiles  []string
		wantDenied bool
		head       string
	}{
		{Conn{Provider: GitHub, BaseURL: srv.URL + "/gh", Token: "gh-token"}, "acme/terraform-azurerm-storage", "v1.1.0", []string{"README.md", "main.tf"}, false, "abc123"},
		{Conn{Provider: AzureDevOps, BaseURL: srv.URL + "/org", Token: "az-pat"}, "Platform Team/storage", "2.0.0", []string{"main.tf", "sub/x.tf"}, false, "def456"},
		{Conn{Provider: GitHub, BaseURL: srv.URL + "/gh", Token: "wrong"}, "", "", nil, true, ""},
		{Conn{Provider: AzureDevOps, BaseURL: srv.URL + "/org", Token: "wrong"}, "", "", nil, true, ""},
	} {
		repos, err := tc.conn.Repos(ctx)
		if tc.wantDenied {
			if err == nil {
				t.Errorf("%s: bad token accepted", tc.conn.Provider)
			}
			continue
		}
		if err != nil || !slices.Contains(repos, tc.repo) {
			t.Fatalf("%s repos: %v %v", tc.conn.Provider, repos, err)
		}
		if head, err := tc.conn.Head(ctx, tc.repo, "main"); err != nil || head != tc.head {
			t.Fatalf("%s head of main: %q %v", tc.conn.Provider, head, err)
		}
		tags, err := tc.conn.Tags(ctx, tc.repo)
		if err != nil || !slices.Contains(tags, tc.tag) {
			t.Fatalf("%s tags: %v %v", tc.conn.Provider, tags, err)
		}
		archive, err := tc.conn.Archive(ctx, tc.repo, tc.tag, "tag")
		if err != nil {
			t.Fatalf("%s archive: %v", tc.conn.Provider, err)
		}
		ins, err := registry.Inspect(archive) // module at the archive root, escaping entries dropped
		if err != nil || len(ins.Variables) != 1 {
			t.Fatalf("%s inspect: %v", tc.conn.Provider, err)
		}
		files, _ := readTarGz(archive)
		var names []string
		for _, f := range files {
			names = append(names, f.name)
		}
		slices.Sort(names)
		if !slices.Equal(names, tc.wantFiles) {
			t.Errorf("%s files: %v, want %v", tc.conn.Provider, names, tc.wantFiles)
		}
	}
}

func TestSealAndValidation(t *testing.T) {
	sealed, err := Seal(Key("s1"), "tok")
	if err != nil || bytes.Contains(sealed, []byte("tok")) {
		t.Fatal("token not sealed", err)
	}
	if got, err := Open(Key("s1"), sealed); err != nil || got != "tok" {
		t.Fatalf("open: %q %v", got, err)
	}
	if _, err := Open(Key("s2"), sealed); err == nil {
		t.Fatal("opened with another key")
	}
	if u, err := NormalizeURL(GitHub, ""); err != nil || u != "https://api.github.com" {
		t.Fatalf("github default: %q %v", u, err)
	}
	for _, bad := range []string{"", "http://dev.azure.com/org", "https://dev.azure.com/org?x=1"} {
		if _, err := NormalizeURL(AzureDevOps, bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	for _, bad := range []string{"repo", "a/b/c", "../x", "a/.."} {
		if _, _, err := splitRepo(bad); err == nil || !strings.Contains(err.Error(), "repository") {
			t.Errorf("accepted repo %q", bad)
		}
	}
}

// TestRepoAllowed: workspace repository patterns match case-insensitively; none means none.
func TestRepoAllowed(t *testing.T) {
	for _, c := range []struct {
		patterns, repo string
		want           bool
	}{
		{"", "acme/infra", false},
		{"acme/infra-*", "acme/infra-prod", true},
		{"acme/infra-*", "ACME/Infra-Prod", true},
		{"acme/infra-*", "acme/app", false},
		{"acme/infra-*\nacme/app", "acme/app", true},
		{"Platform Team/*", "platform team/storage", true}, // Azure DevOps projects have spaces
		{"*/*", "anyone/anything", true},
		{"acme/*", "evil/acme", false},
	} {
		if got := RepoAllowed(c.patterns, c.repo); got != c.want {
			t.Errorf("RepoAllowed(%q, %q) = %v", c.patterns, c.repo, got)
		}
	}
	if got, err := CheckRepoPatterns(" acme/a-* \r\n\n Platform Team/b\n"); err != nil || got != "acme/a-*\nPlatform Team/b" {
		t.Errorf("CheckRepoPatterns: %q %v", got, err)
	}
	for _, bad := range []string{"*", "acme", "a/b/c", "acme/[x"} {
		if _, err := CheckRepoPatterns(bad); err == nil {
			t.Errorf("CheckRepoPatterns(%q) must fail", bad)
		}
	}
}
