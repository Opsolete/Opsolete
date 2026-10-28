package httpserver

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/opsolete/opsolete/internal/registry/registrytest"
	"github.com/opsolete/opsolete/internal/vcs"
)

// fakeGitRepo is one GitHub repository: branches point at commits, a commit is a set of files.
type fakeGitRepo struct {
	mu       sync.Mutex
	branches map[string]string
	commits  map[string]map[string]string
	n        int
	pulls    []string // head branches
}

func blobSHA(content string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(content))) }

// commit puts files on top of branch's head and moves the branch.
func (g *fakeGitRepo) commit(branch string, files map[string]string) {
	next := map[string]string{}
	for k, v := range g.commits[g.branches[branch]] {
		next[k] = v
	}
	for k, v := range files {
		next[k] = v
	}
	g.n++
	sha := fmt.Sprintf("c%039d", g.n)
	g.commits[sha], g.branches[branch] = next, sha
}

func (g *fakeGitRepo) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer pat" {
			http.Error(w, "bad credentials", http.StatusUnauthorized)
			return
		}
		p := strings.TrimPrefix(r.URL.Path, "/repos/acme/catalog")
		switch {
		case r.URL.Path == "/user/repos":
			w.Write([]byte(`[{"full_name":"acme/catalog"}]`))
		case r.Method == "GET" && strings.HasPrefix(p, "/branches/"):
			sha, ok := g.branches[strings.TrimPrefix(p, "/branches/")]
			if !ok {
				http.NotFound(w, r)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"commit": map[string]string{"sha": sha}})
		case r.Method == "GET" && strings.HasPrefix(p, "/tarball/"):
			files := map[string]string{}
			for k, v := range g.commits[strings.TrimPrefix(p, "/tarball/")] {
				files["acme-catalog-x/"+k] = v
			}
			w.Write(registrytest.TarGz(t, files))
		case r.Method == "GET" && strings.HasPrefix(p, "/contents/"):
			c, ok := g.commits[r.URL.Query().Get("ref")][strings.TrimPrefix(p, "/contents/")]
			if !ok {
				http.NotFound(w, r)
				return
			}
			json.NewEncoder(w).Encode(map[string]string{"sha": blobSHA(c)})
		case r.Method == "PUT" && strings.HasPrefix(p, "/contents/"):
			var req struct{ Message, Content, Branch, SHA string }
			json.NewDecoder(r.Body).Decode(&req)
			file := strings.TrimPrefix(p, "/contents/")
			cur, exists := g.commits[g.branches[req.Branch]][file]
			switch {
			case exists && req.SHA == "":
				http.Error(w, `{"message":"sha wasn't supplied"}`, http.StatusUnprocessableEntity)
			case exists && req.SHA != blobSHA(cur):
				http.Error(w, `{"message":"does not match"}`, http.StatusConflict)
			default:
				b, _ := base64.StdEncoding.DecodeString(req.Content)
				g.commit(req.Branch, map[string]string{file: string(b)})
				w.WriteHeader(http.StatusCreated)
				w.Write([]byte(`{}`))
			}
		case r.Method == "POST" && p == "/git/refs":
			var req struct{ Ref, SHA string }
			json.NewDecoder(r.Body).Decode(&req)
			g.branches[strings.TrimPrefix(req.Ref, "refs/heads/")] = req.SHA
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{}`))
		case r.Method == "POST" && p == "/pulls":
			var req struct{ Head, Base string }
			json.NewDecoder(r.Body).Decode(&req)
			g.pulls = append(g.pulls, req.Head)
			fmt.Fprintf(w, `{"html_url":"https://github.example/acme/catalog/pull/%d"}`, len(g.pulls))
		default:
			io.Copy(io.Discard, r.Body)
			http.NotFound(w, r)
		}
	})
}

func tplJSON(id, name string) string {
	return fmt.Sprintf(`{"id":%q,"name":%q,"module":{"source":"git::https://example.com/m.git","ref":"v1"},"inputs":[{"name":"n","type":"string"}]}`, id, name)
}

func TestTemplateCatalogInGit(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	id := strings.ToLower(newID()[:8])
	connName := "cat-" + id
	t.Cleanup(func() {
		store.DB.Exec(`DELETE FROM template_catalog`)
		store.DB.Exec(`DELETE FROM vcs_connections WHERE name = $1`, connName)
	})
	g := &fakeGitRepo{branches: map[string]string{}, commits: map[string]map[string]string{}}
	g.commit("main", map[string]string{"catalog/web.json": tplJSON("web-"+id, "Web"), "README.md": "not a template"})
	gh := httptest.NewTLSServer(g.handler(t))
	defer gh.Close()
	old := vcs.HTTPClient
	vcs.HTTPClient = gh.Client()
	t.Cleanup(func() { vcs.HTTPClient = old })

	app := newApp(t, t.TempDir(), store, devAuth(t, store, "admin@example.com", true), Features{Deployments: true, Registry: true, VCS: true})
	h := app.Handler()
	if rec := post(h, "/vcs", url.Values{"name": {connName}, "provider": {"github"}, "base_url": {gh.URL}, "token": {"pat"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("connect: %d %s", rec.Code, rec.Body.String())
	}
	var connID string
	store.DB.QueryRow(`SELECT id FROM vcs_connections WHERE name = $1`, connName).Scan(&connID)
	if rec := get(h, "/templates/new"); rec.Code != http.StatusNotFound {
		t.Fatalf("no editor before a catalog is set up: %d", rec.Code)
	}
	if page := get(h, "/catalog").Body.String(); !strings.Contains(page, `<select id="repo" name="repo" disabled>`) {
		t.Fatalf("repo dropdown before choosing a connection: %s", page)
	}
	if page := get(h, "/catalog?connection="+connID).Body.String(); !strings.Contains(page, `<option value="acme/catalog">acme/catalog</option>`) {
		t.Fatalf("repo dropdown: %s", page)
	}

	// Commit mode: the templates load from the folder, the builder commits to the branch.
	set := url.Values{"connection": {connID}, "repo": {"acme/catalog"}, "branch": {"main"}, "dir": {"/catalog/"}, "mode": {"commit"}}
	if rec := post(h, "/catalog", set); rec.Code != http.StatusSeeOther {
		t.Fatalf("set up catalog: %d %s", rec.Code, rec.Body.String())
	}
	if _, ok := app.Templates.Get("web-" + id); !ok || len(app.Templates.List()) != 1 {
		t.Fatalf("templates from git: %v", app.Templates.List())
	}
	if page := get(h, "/catalog").Body.String(); !strings.Contains(page, `<option value="acme/catalog" selected>`) {
		t.Fatalf("the configured repo is selected: %s", page)
	}
	edit := url.Values{"name": {"Web site"}, "module_source": {"git::https://example.com/m.git"}, "module_ref": {"v2"}, "in_name": {"n"}, "in_type": {"string"}}
	if rec := post(h, "/templates/web-"+id+"/edit", edit); rec.Code != http.StatusSeeOther {
		t.Fatalf("save to git: %d %s", rec.Code, rec.Body.String())
	}
	g.mu.Lock()
	saved := g.commits[g.branches["main"]]["catalog/web.json"]
	g.mu.Unlock()
	if !strings.Contains(saved, `"ref": "v2"`) {
		t.Fatalf("committed file: %s", saved)
	}
	if tpl, _ := app.Templates.Get("web-" + id); tpl.Module.Ref != "v2" {
		t.Fatalf("catalog must reload after the commit: %+v", tpl)
	}
	// Another instance: starts from the snapshot in Postgres, without the provider.
	other := newApp(t, t.TempDir(), store, devAuth(t, store, "admin@example.com", true), Features{Deployments: true, Registry: true, VCS: true})
	if err := vcs.LoadCatalog(ctx, store, other.Templates); err != nil {
		t.Fatal(err)
	}
	if tpl, _ := other.Templates.Get("web-" + id); tpl.Module.Ref != "v2" {
		t.Fatalf("snapshot: %+v", tpl)
	}

	// Someone changes the file in Git: a save based on the old version is refused, not overwritten.
	g.mu.Lock()
	g.commit("main", map[string]string{"catalog/web.json": tplJSON("web-"+id, "Changed in git")})
	g.mu.Unlock()
	edit.Set("module_ref", "v3")
	if rec := post(h, "/templates/web-"+id+"/edit", edit); rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "review and save again") {
		t.Fatalf("stale save: %d %s", rec.Code, rec.Body.String())
	}
	if tpl, _ := app.Templates.Get("web-" + id); tpl.Name != "Changed in git" {
		t.Fatalf("a refused save reloads the catalog: %+v", tpl)
	}
	// A commit with an invalid template isn't loaded: the last good one stays, the error is shown.
	g.mu.Lock()
	g.commit("main", map[string]string{"catalog/bad.json": `{"id":"bad"}`})
	g.mu.Unlock()
	if rec := post(h, "/catalog/sync", nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("sync: %d", rec.Code)
	}
	if page := get(h, "/catalog").Body.String(); !strings.Contains(page, "Last sync failed") || !strings.Contains(page, "bad.json") {
		t.Fatalf("sync error: %s", page)
	}
	if _, ok := app.Templates.Get("web-" + id); !ok {
		t.Fatal("the last good catalog must stay")
	}
	g.mu.Lock()
	g.commit("main", map[string]string{"catalog/bad.json": tplJSON("fixed-"+id, "Fixed")})
	g.mu.Unlock()

	// Pull request mode: a new template goes to a new branch and a pull request; the catalog waits for the merge.
	set.Set("mode", "pr")
	if rec := post(h, "/catalog", set); rec.Code != http.StatusSeeOther {
		t.Fatalf("pr mode: %d %s", rec.Code, rec.Body.String())
	}
	n := len(app.Templates.List())
	rec := post(h, "/templates/new", url.Values{"name": {"Queue"}, "module_source": {"git::https://example.com/q.git"}, "module_ref": {"v1"}})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "https://github.example/acme/catalog/pull/1") {
		t.Fatalf("pull request: %d %s", rec.Code, rec.Body.String())
	}
	g.mu.Lock()
	head := g.pulls[0]
	var added string
	for f, c := range g.commits[g.branches[head]] {
		if strings.Contains(c, `"Queue"`) {
			added = f
		}
	}
	mainHasIt := strings.Contains(fmt.Sprint(g.commits[g.branches["main"]]), `"Queue"`)
	g.mu.Unlock()
	if !strings.HasPrefix(head, "opsolete/template-") || !strings.HasPrefix(added, "catalog/") || mainHasIt || len(app.Templates.List()) != n {
		t.Fatalf("pr branch %q file %q, on main %v, catalog %d -> %d", head, added, mainHasIt, n, len(app.Templates.List()))
	}

	// The connection can't be deleted from under the catalog; disconnecting goes back to TEMPLATES_DIR.
	if rec := post(h, "/vcs/"+connID+"/delete", nil); rec.Code != http.StatusConflict {
		t.Fatalf("delete a connection in use: %d", rec.Code)
	}
	if rec := post(h, "/catalog/delete", nil); rec.Code != http.StatusSeeOther || len(app.Templates.List()) != 0 {
		t.Fatalf("disconnect: %d, %d templates", rec.Code, len(app.Templates.List()))
	}
	if mode, _ := app.Templates.Source(); mode != "" {
		t.Fatalf("mode after disconnect: %q", mode)
	}
}

// Module uploads are an organization setting (formerly TEMPLATE_EDITOR).
func TestModuleUploadsSetting(t *testing.T) {
	store := testStore(t)
	t.Cleanup(func() { store.SetModuleUploads(context.Background(), false) })
	h := newServer(t, t.TempDir(), false, store)
	if rec := upload(h, map[string]string{"namespace": "x"}, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("upload while off: %d", rec.Code)
	}
	if rec := post(h, "/organization", url.Values{"currency": {"USD"}, "module_uploads": {"1"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("turn on: %d", rec.Code)
	}
	if page := get(h, "/registry/publish").Body.String(); !strings.Contains(page, "?type=upload") {
		t.Fatalf("publish page with uploads on: %s", page)
	}
}
