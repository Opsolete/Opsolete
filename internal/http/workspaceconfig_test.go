package httpserver

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opsolete/opsolete/internal/registry/registrytest"
	"github.com/opsolete/opsolete/internal/runnerapi"
	"github.com/opsolete/opsolete/internal/storage"
	"github.com/opsolete/opsolete/internal/vcs"
)

// Workspaces without a template: blank, or planning a VCS branch.
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
		case p == "/repos/acme/infra/pulls":
			w.Write([]byte(`[{"number":9,"head":{"sha":"pr9","repo":{"full_name":"acme/infra"}}}]`))
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

	app := newApp(t, "../../catalog", store, devAuth(t, store, "admin@example.com", true), Features{Deployments: true, Registry: true, VCS: true})
	h := app.Handler()
	// Add-VCS-provider wizard started from the new-workspace wizard: it ends back there with the new connection.
	if page := get(h, "/vcs/new?provider=github&return=workspace").Body.String(); !strings.Contains(page, `name="return" value="workspace"`) {
		t.Fatalf("set up provider step: %s", page)
	}
	rec := post(h, "/vcs", url.Values{"name": {connName}, "provider": {"github"}, "base_url": {gh.URL}, "token": {"pat"}, "return": {"workspace"}})
	loc := rec.Header().Get("Location")
	if rec.Code != http.StatusSeeOther || !strings.HasPrefix(loc, "/vcs/new?connection=") || !strings.HasSuffix(loc, "&return=workspace") {
		t.Fatalf("connect: %d %q %s", rec.Code, loc, rec.Body.String())
	}
	connID := strings.TrimSuffix(strings.TrimPrefix(loc, "/vcs/new?connection="), "&return=workspace")
	if page := get(h, loc).Body.String(); !strings.Contains(page, `action="/vcs/`+connID+`/settings"`) || strings.Contains(page, "Skip and") {
		t.Fatalf("advanced settings step: %s", page)
	}

	// A new connection only publishes modules until an admin allows workspaces.
	if page := get(h, "/workspaces/new?type=vcs").Body.String(); strings.Contains(page, connName) {
		t.Fatalf("a modules-only connection must not be offered: %s", page)
	}
	refused := url.Values{"name": {linked}, "vcs_connection": {connID}, "vcs_repo": {"acme/infra"}, "vcs_branch": {"main"}}
	if rec := post(h, "/workspaces/new", refused); rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "may not link") {
		t.Fatalf("linking through a connection closed to workspaces: %d", rec.Code)
	}
	wizard := url.Values{"wizard": {"1"}, "return": {"workspace"}, "workspace_scope": {"selected"}, "modules": {"1"}}
	if rec := post(h, "/vcs/"+connID+"/settings", wizard); rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "enable workspaces") {
		t.Fatalf("the workspace flow needs workspaces enabled: %d", rec.Code)
	}
	wizard.Set("workspaces", "1")
	if rec := post(h, "/vcs/"+connID+"/settings", wizard); rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "allow all workspaces") {
		t.Fatalf("a new workspace can't be selected yet: %d", rec.Code)
	}
	wizard.Set("workspace_scope", "all")
	if rec := post(h, "/vcs/"+connID+"/settings", wizard); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/workspaces/new?type=vcs&conn="+connID {
		t.Fatalf("allow workspaces and continue: %d %q %s", rec.Code, rec.Header().Get("Location"), rec.Body.String())
	}

	// Blank: no template, no run, nothing to plan.
	if page := get(h, "/workspaces/new?type=vcs").Body.String(); !strings.Contains(page, `href="?type=vcs&conn=`+connID+`"`) || !strings.Contains(page, connName) {
		t.Fatalf("new workspace wizard must offer the connection: %s", page)
	}
	if page := get(h, "/workspaces/new?type=vcs&other=1").Body.String(); !strings.Contains(page, `href="/vcs/new?provider=github&return=workspace"`) {
		t.Fatalf("connect to a different VCS must offer the providers: %s", page)
	}
	if page := get(h, "/vcs/new?provider=azure-devops").Body.String(); !strings.Contains(page, `name="provider" value="azure-devops"`) || !strings.Contains(page, "Organization URL") {
		t.Fatalf("the add-VCS wizard must set up the chosen provider: %s", page)
	}
	if page := get(h, "/workspaces/new?type=vcs&conn="+connID).Body.String(); !strings.Contains(page, `<option value="acme/infra">`) {
		t.Fatalf("new workspace wizard must offer the connection's repos: %s", page)
	}
	if page := get(h, "/workspaces/new?type=vcs&conn="+connID+"&repo=acme/infra").Body.String(); !strings.Contains(page, `value="infra"`) ||
		!strings.Contains(page, `name="vcs_repo" value="acme/infra"`) {
		t.Fatalf("new workspace wizard settings step: %s", page)
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
	rec = do(h, req)
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

	// VCS events: a new commit queues an apply run of it, an open pull request a speculative plan.
	if rec := post(h, "/workspaces/"+linked+"/vcs", url.Values{"vcs_connection": {connID}, "vcs_repo": {"acme/infra"}, "vcs_branch": {"main"},
		"vcs_trigger": {"1"}, "vcs_pull_requests": {"1"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("turn on vcs triggers: %d %s", rec.Code, rec.Body.String())
	}
	lw, _ := store.GetWorkspace(ctx, linked)
	claimVCS := func(wantAction, wantTar string) {
		t.Helper()
		if err := vcs.TriggerRuns(ctx, store, app.SecretKey, []storage.Workspace{lw}); err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		head = "sha3" // pushed after the run was queued: it stays on its commit
		mu.Unlock()
		req := httptest.NewRequest("POST", runnerapi.Prefix+"/jobs/claim", strings.NewReader(`{"runner":"r1"}`))
		req.Header.Set("Authorization", "Bearer "+runnerTok)
		var job runnerapi.Job
		if rec := do(h, req); json.Unmarshal(rec.Body.Bytes(), &job) != nil || job.Action != wantAction {
			t.Fatalf("claim %s: %d %s", wantAction, rec.Code, rec.Body.String())
		}
		if tar := fetch(job.ModuleSource); !strings.Contains(tar, wantTar) {
			t.Fatalf("%s run archive: %q, want %s", wantAction, tar, wantTar)
		}
		store.DB.Exec(`UPDATE runs SET status = 'errored' WHERE id = $1`, job.RunID)
	}
	claimVCS("apply", "# at sha2")
	claimVCS("apply", "# at sha3")
	claimVCS("plan", "# at pr9") // the branch's commits all had their run

	// The admin narrows the connection by attaching a workspace, which scopes it to selected workspaces:
	// attached ones keep linking, the detached workspace's next run fails instead of planning.
	if page := get(h, "/vcs/"+connID+"/settings").Body.String(); !strings.Contains(page, "<option>"+linked+"</option>") {
		t.Fatalf("every existing workspace must be attachable: %s", page)
	}
	if rec := post(h, "/vcs/"+connID+"/workspaces", url.Values{"workspace": {"no-such-ws"}}); rec.Code != http.StatusBadRequest {
		t.Fatalf("attach an unknown workspace: %d", rec.Code)
	}
	if rec := post(h, "/vcs/"+connID+"/workspaces", url.Values{"workspace": {linked}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("attach: %d", rec.Code)
	}
	if page := get(h, "/vcs/"+connID+"/settings").Body.String(); !strings.Contains(page, `href="/workspaces/`+linked+`/vcs"`) || strings.Contains(page, "<option>"+linked+"</option>") {
		t.Fatalf("settings page must list the attached workspace: %s", page)
	}
	// Workspace level, as in HCP Terraform: a linked workspace shows its source; Change source is a wizard.
	if page := get(h, "/workspaces/"+linked+"/vcs").Body.String(); !strings.Contains(page, "Connected to VCS") ||
		!strings.Contains(page, "acme/infra") || !strings.Contains(page, `href="/workspaces/`+linked+`/vcs/edit"`) || strings.Contains(page, "no longer allows") {
		t.Fatalf("linked workspace vcs page: %s", page)
	}
	if page := get(h, "/workspaces/"+linked+"/vcs/edit").Body.String(); !strings.Contains(page, "Choose your workflow") {
		t.Fatalf("change source: %s", page)
	}
	if page := get(h, "/workspaces/"+linked+"/vcs/edit?type=vcs").Body.String(); !strings.Contains(page, "conn="+connID) {
		t.Fatalf("an attached workspace must be offered the connection: %s", page)
	}
	if page := get(h, "/workspaces/"+linked+"/vcs/edit?type=vcs&conn="+connID).Body.String(); !strings.Contains(page, `<option value="acme/infra">`) {
		t.Fatalf("change source must offer the connection's repos: %s", page)
	}
	if page := get(h, "/workspaces/"+linked+"/vcs/edit?type=vcs&conn="+connID+"&repo=acme/infra").Body.String(); !strings.Contains(page, `name="vcs_repo" value="acme/infra"`) {
		t.Fatalf("change source settings step: %s", page)
	}
	if page := get(h, "/workspaces/"+blank+"/vcs?type=vcs").Body.String(); strings.Contains(page, "conn="+connID) {
		t.Fatalf("an unselected workspace must not be offered the connection: %s", page)
	}
	if rec := post(h, "/vcs/"+connID+"/workspaces", url.Values{"detach": {linked}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("detach: %d", rec.Code)
	}
	if rec := post(h, "/workspaces/"+linked+"/runs", url.Values{"action": {"apply"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("start run: %d %s", rec.Code, rec.Body.String())
	}
	req = httptest.NewRequest("POST", runnerapi.Prefix+"/jobs/claim", strings.NewReader(`{"runner":"r1"}`))
	req.Header.Set("Authorization", "Bearer "+runnerTok)
	do(h, req)
	if runs, _ := store.Runs(ctx, lw.ID, 1); len(runs) == 0 || runs[0].Status != storage.Errored {
		t.Fatalf("a run of a detached workspace must fail: %+v", runs)
	}
	if page := get(h, "/workspaces/"+linked+"/vcs").Body.String(); !strings.Contains(page, "no longer allows") {
		t.Fatalf("a detached workspace must be warned: %s", page)
	}
	if rec := post(h, "/workspaces/"+linked+"/vcs", url.Values{"type": {"cli"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("unlink: %d %s", rec.Code, rec.Body.String())
	}
	if ws, _ := store.GetWorkspace(ctx, linked); ws.HasConfig() {
		t.Fatal("unlinked workspace still has configuration")
	}
}
