package httpserver

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rudesome/form2stack/internal/registry/registrytest"
	"github.com/rudesome/form2stack/internal/runnerapi"
	"github.com/rudesome/form2stack/internal/storage"
	"github.com/rudesome/form2stack/internal/vcs"
)

// Workspaces without a template: blank, with an uploaded state, or planning a VCS branch.
func TestBlankAndVCSWorkspaces(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	claimWait = 100 * time.Millisecond
	store.DB.Exec(`UPDATE runs SET status = 'errored' WHERE status IN ('plan_queued', 'planning', 'needs_confirmation', 'apply_queued', 'applying')`)
	id := strings.ToLower(newID()[:8])
	blank, linked, connName := "bl-"+id, "vc-"+id, "gh-"+id
	t.Cleanup(func() {
		store.DB.Exec(`DELETE FROM workspaces WHERE name IN ($1, $2)`, blank, linked)
		store.DB.Exec(`DELETE FROM vcs_connections WHERE name = $1`, connName)
	})

	var mu sync.Mutex
	head := "sha1"
	gh := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch p := r.URL.Path; {
		case r.Header.Get("Authorization") != "Bearer pat":
			http.Error(w, "bad credentials", http.StatusUnauthorized)
		case p == "/user/repos":
			w.Write([]byte(`[{"full_name":"acme/infra"}]`))
		case p == "/repos/acme/infra/branches/main":
			fmt.Fprintf(w, `{"commit":{"sha":%q}}`, head)
		case strings.HasPrefix(p, "/repos/acme/infra/tarball/"):
			sha := strings.TrimPrefix(p, "/repos/acme/infra/tarball/")
			w.Write(registrytest.TarGz(t, map[string]string{"acme-infra-x/main.tf": "# at " + sha}))
		default:
			http.NotFound(w, r)
		}
	}))
	defer gh.Close()
	old := vcs.HTTPClient
	vcs.HTTPClient = gh.Client()
	t.Cleanup(func() { vcs.HTTPClient = old })

	h := newApp(t, "../../catalog", store, devAuth(t, store, "admin@example.com", true), Features{Deployments: true, Registry: true, VCS: true}).Handler()
	if rec := post(h, "/vcs", url.Values{"name": {connName}, "provider": {"github"}, "base_url": {gh.URL}, "token": {"pat"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("connect: %d %s", rec.Code, rec.Body.String())
	}
	conns, _ := store.VCSConnections(ctx)
	var connID string
	for _, c := range conns {
		if c.Name == connName {
			connID = c.ID
		}
	}

	// Blank: no template, no run, nothing to plan.
	if page := get(h, "/workspaces/new").Body.String(); !strings.Contains(page, `<option value="acme/infra">`) || !strings.Contains(page, connName) {
		t.Fatalf("new workspace page must offer connections and repos: %s", page)
	}
	if rec := post(h, "/workspaces/new", url.Values{"name": {"new"}}); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf(`"new" is reserved: %d`, rec.Code)
	}
	if rec := post(h, "/workspaces/new", url.Values{"name": {blank}}); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/workspaces/"+blank {
		t.Fatalf("create blank: %d %s", rec.Code, rec.Body.String())
	}
	if page := get(h, "/workspaces/"+blank).Body.String(); !strings.Contains(page, "No configuration") || strings.Contains(page, "New run") {
		t.Fatalf("blank workspace page: %s", page)
	}
	if rec := post(h, "/workspaces/"+blank+"/runs", url.Values{"action": {"apply"}}); rec.Code != http.StatusConflict {
		t.Fatalf("a blank workspace has nothing to plan: %d", rec.Code)
	}

	// Upload a state: it must continue the current one.
	upload := func(body string) int {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		f, _ := mw.CreateFormFile("state", "terraform.tfstate")
		f.Write([]byte(body))
		mw.Close()
		req := httptest.NewRequest("POST", "/workspaces/"+blank+"/state", &buf)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		return do(h, req).Code
	}
	for _, c := range []struct {
		body string
		want int
	}{
		{`{"version":4,"serial":3,"lineage":"L1","resources":[]}`, http.StatusSeeOther},
		{`{"version":4,"serial":4,"lineage":"L2","resources":[]}`, http.StatusConflict},
		{`{"version":4,"serial":2,"lineage":"L1","resources":[]}`, http.StatusConflict},
		{`not json`, http.StatusBadRequest},
	} {
		if got := upload(c.body); got != c.want {
			t.Fatalf("upload %s: %d, want %d", c.body, got, c.want)
		}
	}
	ws, _ := store.GetWorkspace(ctx, blank)
	if vs, _ := store.StateVersions(ctx, ws.ID, 5); len(vs) != 1 || vs[0].Serial != 3 {
		t.Fatalf("state versions: %+v", vs)
	}
	if lock, _ := store.StateLockID(ctx, ws.ID); lock != "" {
		t.Fatal("upload must release the state lock")
	}

	// VCS: the branch is checked on link; runs plan its head and apply the same commit.
	form := url.Values{"name": {linked}, "vcs_connection": {connID}, "vcs_repo": {"acme/infra"}, "vcs_branch": {"nope"}}
	if rec := post(h, "/workspaces/new", form); rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "branch nope") {
		t.Fatalf("unknown branch: %d", rec.Code)
	}
	form.Set("vcs_branch", "main")
	if rec := post(h, "/workspaces/new", form); rec.Code != http.StatusSeeOther {
		t.Fatalf("create linked: %d %s", rec.Code, rec.Body.String())
	}
	if rec := post(h, "/workspaces/"+linked+"/runs", url.Values{"action": {"apply"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("start run: %d %s", rec.Code, rec.Body.String())
	}
	runnerTok, _ := store.CreateToken(ctx, "r", storage.ScopeRunner)
	req := httptest.NewRequest("POST", runnerapi.Prefix+"/jobs/claim", strings.NewReader(`{"runner":"r1"}`))
	req.Header.Set("Authorization", "Bearer "+runnerTok)
	rec := do(h, req)
	var job runnerapi.Job
	if json.Unmarshal(rec.Body.Bytes(), &job); rec.Code != 200 || !strings.HasPrefix(job.ModuleSource, "/v1/vcs-archives/"+job.RunID+".tar.gz?") {
		t.Fatalf("claim: %d %s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	head = "sha2" // pushed after the plan started: the run stays on sha1
	mu.Unlock()
	fetch := func(src string) string {
		t.Helper()
		rec := get(h, src)
		zr, err := gzip.NewReader(rec.Body)
		if rec.Code != 200 || err != nil {
			t.Fatalf("archive %s: %d %v", src, rec.Code, err)
		}
		b, _ := io.ReadAll(zr)
		return string(b)
	}
	if tar := fetch(job.ModuleSource); !strings.Contains(tar, "# at sha1") {
		t.Fatalf("archive is not the pinned commit: %q", tar)
	}
	if rec := get(h, strings.Replace(job.ModuleSource, "sig=", "sig=0", 1)); rec.Code != http.StatusForbidden {
		t.Fatalf("tampered archive link: %d", rec.Code)
	}
	if rec := post(h, "/workspaces/"+linked+"/vcs", url.Values{}); rec.Code != http.StatusConflict {
		t.Fatalf("relinking during a run: %d", rec.Code)
	}
	store.DB.Exec(`UPDATE runs SET status = 'errored' WHERE id = $1`, job.RunID)
	if rec := post(h, "/workspaces/"+linked+"/vcs", url.Values{}); rec.Code != http.StatusSeeOther {
		t.Fatalf("unlink: %d %s", rec.Code, rec.Body.String())
	}
	if ws, _ := store.GetWorkspace(ctx, linked); ws.HasConfig() {
		t.Fatal("unlinked workspace still has configuration")
	}
}
