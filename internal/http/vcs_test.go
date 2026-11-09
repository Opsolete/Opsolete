package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/opsolete/opsolete/internal/registry/registrytest"
	"github.com/opsolete/opsolete/internal/storage"
	"github.com/opsolete/opsolete/internal/templates"
	"github.com/opsolete/opsolete/internal/vcs"
)

// The no-code flow: connect GitHub, publish a repo, provision a workspace from its template,
// push a new tag, and move the workspace to it.
func TestVCSPublishAndUpdate(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	id := strings.ToLower(newID()[:8])
	ns, connName, wsName, tplID := "v"+id, "gh-"+id, "nc-"+id, "nc-"+id

	// Fake GitHub: tags are added during the test.
	var mu sync.Mutex
	tags := map[string]string{"v1.0.0": `variable "db_name" { type = string }`}
	archives := 0 // tarball downloads
	gh := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer secret-pat" {
			http.Error(w, "bad credentials", http.StatusUnauthorized)
			return
		}
		switch p := r.URL.Path; {
		case p == "/user/repos":
			w.Write([]byte(`[{"full_name":"acme/terraform-aws-rds"}]`))
		case p == "/repos/acme/terraform-aws-rds/tags":
			var list []map[string]any
			for tag := range tags {
				list = append(list, map[string]any{"name": tag, "commit": map[string]string{"sha": "c-" + tag}})
			}
			json.NewEncoder(w).Encode(list)
		case strings.HasPrefix(p, "/repos/acme/terraform-aws-rds/tarball/"):
			tag := strings.TrimPrefix(p, "/repos/acme/terraform-aws-rds/tarball/")
			archives++
			files := map[string]string{"acme-rds-1/main.tf": tags[tag]}
			if tag == "v1.1.0" { // a monorepo: the vm folder only exists from this tag on
				files["acme-rds-1/vm/main.tf"] = `variable "size" { type = string }`
			}
			w.Write(registrytest.TarGz(t, files))
		default:
			http.NotFound(w, r)
		}
	}))
	defer gh.Close()

	source := "ops.example.com/" + ns + "/rds/aws"
	dir := t.TempDir()
	tpl := templates.Template{ID: tplID, Name: "RDS", Module: templates.Module{Source: source, Ref: "1.0.0"},
		Inputs: []templates.Input{{Name: "db_name", Label: "DB name", Type: "string", Required: true}}}
	b, _ := json.Marshal(tpl)
	os.WriteFile(filepath.Join(dir, tplID+".json"), b, 0o644)
	app := newApp(t, dir, store, devAuth(t, store, "admin@example.com", true), Features{Deployments: true, Registry: true, VCS: true})
	app.App.VCS = vcs.NewClient(gh.Client())
	h := app.Handler()
	var connID string

	// Connect: a bad token is rejected by the connection check; the good one is stored sealed.
	step(t, "connecting checks the token and stores it sealed", func(t *testing.T) {
		conn := url.Values{"name": {connName}, "provider": {"github"}, "base_url": {gh.URL}, "token": {"wrong"}}
		if rec := post(h, "/vcs", conn); rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "connection check failed") {
			t.Fatalf("bad token: %d", rec.Code)
		}
		conn.Set("token", "secret-pat")
		if rec := post(h, "/vcs", conn); rec.Code != http.StatusSeeOther {
			t.Fatalf("connect: %d %s", rec.Code, rec.Body.String())
		}
		var scope string
		var sealed []byte
		store.Pool.QueryRow(context.Background(), `SELECT id, token, workspace_scope FROM vcs_connections WHERE name = $1`, connName).Scan(&connID, &sealed, &scope)
		if scope != "" {
			t.Fatalf("a new connection must not be open to workspaces: %q", scope)
		}
		// from the publish wizard, advanced settings may stay empty and finishing continues publishing
		wiz := url.Values{"wizard": {"1"}, "return": {"module"}, "workspaces": {"1"}, "workspace_scope": {"selected"}}
		if rec := post(h, "/vcs/"+connID+"/settings", wiz); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/registry/publish?connection="+connID {
			t.Fatalf("finish to the publish wizard: %d %q", rec.Code, rec.Header().Get("Location"))
		}
		if strings.Contains(string(sealed), "secret-pat") || strings.Contains(get(h, "/vcs").Body.String(), "secret-pat") {
			t.Fatal("token stored or shown in plain text")
		}
	})

	// Publish wizard: connection, then repo, then confirm with name and system from the terraform-<system>-<name> repo name.
	step(t, "the publish wizard publishes a repository's tags", func(t *testing.T) {
		if page := get(h, "/registry/publish").Body.String(); !strings.Contains(page, `href="?type=vcs"`) {
			t.Fatalf("workflow choice: %s", page)
		}
		if page := get(h, "/registry/publish?type=vcs").Body.String(); !strings.Contains(page, `href="?connection=`+connID+`"`) {
			t.Fatalf("connection step: %s", page)
		}
		if page := get(h, "/registry/publish?connection="+connID).Body.String(); !strings.Contains(page, `data-filter-item="acme/terraform-aws-rds"`) ||
			!strings.Contains(page, `data-filter="repos"`) {
			t.Fatalf("repo picker: %s", page)
		}
		if page := get(h, "/registry/publish?connection="+connID+"&repo=acme/terraform-aws-rds").Body.String(); !strings.Contains(page, `id="name" name="name" value="rds"`) ||
			!strings.Contains(page, `id="system" name="system" value="aws"`) {
			t.Fatalf("confirm step: %s", page)
		}
		rec := post(h, "/registry/publish", url.Values{"connection": {connID}, "repo": {"acme/terraform-aws-rds"}, "namespace": {ns}})
		if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/registry/"+ns+"/rds/aws/1.0.0" {
			t.Fatalf("publish: %d %q %s", rec.Code, rec.Header().Get("Location"), rec.Body.String())
		}
		if page := get(h, "/registry").Body.String(); !strings.Contains(page, "/registry/"+ns+"/rds/aws/sync") || !strings.Contains(page, connName) {
			t.Fatalf("registry page: %s", page)
		}
	})
	// A module in a folder, with the namespace left to default to the repository name; then unlinked.
	step(t, "a module in a folder is checked on resync without downloading again, then unlinked", func(t *testing.T) {
		folder := url.Values{"connection": {connID}, "repo": {"acme/terraform-aws-rds"}, "name": {"f" + id}, "dir": {"../x"}}
		if rec := post(h, "/registry/publish", folder); rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("a folder outside the repository: %d", rec.Code)
		}
		folder.Set("dir", "/nope/")
		if rec := post(h, "/registry/publish", folder); rec.Code != http.StatusSeeOther {
			t.Fatalf("publish from a folder: %d %s", rec.Code, rec.Body.String())
		}
		if m, err := store.GetVCSModule(ctx, "terraform-aws-rds", "f"+id, "aws"); err != nil || m.Dir != "nope" || !strings.Contains(m.SyncError, `no folder "nope"`) {
			t.Fatalf("linked from a folder: %+v %v", m, err)
		}
		// A tag's commit known to lack the folder isn't downloaded again, and still counts as missing it.
		mu.Lock()
		before := archives
		mu.Unlock()
		post(h, "/registry/terraform-aws-rds/f"+id+"/aws/sync", nil)
		mu.Lock()
		again := archives - before
		mu.Unlock()
		if m, err := store.GetVCSModule(ctx, "terraform-aws-rds", "f"+id, "aws"); again != 0 || err != nil || !strings.Contains(m.SyncError, `no folder "nope"`) {
			t.Fatalf("resync without the folder: %d downloads, %+v %v", again, m, err)
		}
		if rec := post(h, "/registry/terraform-aws-rds/f"+id+"/aws/unlink", nil); rec.Code != http.StatusSeeOther {
			t.Fatalf("unlink: %d", rec.Code)
		}
		if rec := post(h, "/registry/terraform-aws-rds/f"+id+"/aws/unlink", nil); rec.Code != http.StatusNotFound {
			t.Fatalf("unlink twice: %d", rec.Code)
		}
	})
	// Any connection publishes modules; the settings page shows what uses the connection's token.
	t.Run("the connection and module pages show what uses them", func(t *testing.T) {
		if page := get(h, "/vcs").Body.String(); !strings.Contains(page, "1 module") {
			t.Fatalf("used by: %s", page)
		}
		if page := get(h, "/vcs/"+connID+"/settings").Body.String(); !strings.Contains(page, `action="/vcs/`+connID+`/token"`) || !strings.Contains(page, "Update VCS provider") {
			t.Fatalf("settings page: %s", page)
		}
		page := get(h, "/registry/"+ns+"/rds/aws/1.0.0").Body.String()
		if !strings.Contains(page, "No-code ready") || !strings.Contains(page, `href="/templates/`+tplID+`">Provision workspace`) ||
			!strings.Contains(page, "acme/terraform-aws-rds") {
			t.Fatalf("module page: %s", page)
		}
	})

	// Provision a workspace, then free it (no runner in this test).
	step(t, "provision a workspace from the no-code template", func(t *testing.T) {
		rec := post(h, "/templates/"+tplID, url.Values{"ops-workspace": {wsName}, "db_name": {"nocode"}})
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("provision: %d %s", rec.Code, rec.Body.String())
		}
		store.Pool.Exec(context.Background(), `UPDATE runs SET status = 'applied', lease_id = NULL, lease_until = NULL FROM workspaces w WHERE w.id = runs.workspace_id AND w.name = $1`, wsName)
	})

	// New tag with a new required input; sync publishes it, the template moves to it.
	step(t, "sync publishes a new tag and the template moves to it", func(t *testing.T) {
		mu.Lock()
		tags["v1.0.1"] = tags["v1.0.0"] + "\nvariable \"db_encrypted\" { type = bool }"
		mu.Unlock()
		if rec := post(h, "/registry/"+ns+"/rds/aws/sync", nil); rec.Header().Get("Location") != "/registry/"+ns+"/rds/aws/1.0.1" {
			t.Fatalf("sync: %d %q", rec.Code, rec.Header().Get("Location"))
		}
		tpl.Module.Ref = "1.0.1" // db_encrypted isn't added to the JSON: the form takes it from the module's variables
		if err := app.App.Templates.Save(tpl, true); err != nil {
			t.Fatal(err)
		}
	})

	step(t, "the workspace's template updates to the new version, one change at a time", func(t *testing.T) {
		ws, err := store.GetWorkspace(ctx, wsName)
		if err != nil {
			t.Fatal(err)
		}
		added := func() storage.WorkspaceTemplate { // the template, added to the workspace
			t.Helper()
			a, err := store.WorkspaceTemplates(ctx, ws.ID)
			if err != nil || len(a) != 1 {
				t.Fatalf("added templates: %+v %v", a, err)
			}
			return a[0]
		}
		configure := "/workspaces/" + wsName + "/templates/" + added().Name
		if page := get(h, "/workspaces/"+wsName+"?show=templates").Body.String(); !strings.Contains(page, `<span class="tag">update: 1.0.1</span>`) {
			t.Fatalf("no update notice: %s", page)
		}
		if page := get(h, configure).Body.String(); !strings.Contains(page, `value="nocode"`) ||
			!strings.Contains(page, "<code>1.0.0</code> → <code>1.0.1</code>") || !strings.Contains(page, `name="db_encrypted"`) {
			t.Fatalf("configure form: %s", page)
		}
		rec := post(h, configure, url.Values{"db_name": {"nocode"}, "db_encrypted": {"on"}})
		if rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "/workspaces/"+wsName+"/runs/") {
			t.Fatalf("configure: %d %s", rec.Code, rec.Body.String())
		}
		if a := added(); a.ModuleRef != "1.0.1" || a.Inputs["db_encrypted"] != true {
			t.Fatalf("template after configure: %+v", a)
		}
		// The plan is now active, so a second change has to wait.
		if rec := post(h, configure, url.Values{"db_name": {"other"}}); rec.Code != http.StatusConflict {
			t.Fatalf("configure during a run: %d", rec.Code)
		}
		if added().Inputs["db_name"] != "nocode" {
			t.Fatal("rejected configure changed the inputs")
		}
	})

	// A folder added in a later tag: older tags without it are skipped, not a sync error.
	t.Run("a folder added in a later tag skips the older tags", func(t *testing.T) {
		mu.Lock()
		tags["v1.1.0"] = tags["v1.0.1"]
		mu.Unlock()
		vm := url.Values{"connection": {connID}, "repo": {"acme/terraform-aws-rds"}, "name": {"vm" + id}, "dir": {"vm"}}
		if rec := post(h, "/registry/publish", vm); rec.Header().Get("Location") != "/registry/terraform-aws-rds/vm"+id+"/aws/1.1.0" {
			t.Fatalf("publish a folder added later: %d %q", rec.Code, rec.Header().Get("Location"))
		}
		if m, err := store.GetVCSModule(ctx, "terraform-aws-rds", "vm"+id, "aws"); err != nil || m.SyncError != "" {
			t.Fatalf("tags before the folder existed: %+v %v", m, err)
		}
		store.UnlinkVCSModule(ctx, "terraform-aws-rds", "vm"+id, "aws")
	})
}

func TestVCSAdminOnly(t *testing.T) {
	store := testStore(t)
	app := newApp(t, t.TempDir(), store, devAuth(t, store, "someone@example.com", false), Features{Registry: true, VCS: true})
	h := app.Handler()
	for _, p := range []string{"/vcs", "/registry/publish"} {
		if rec := get(h, p); rec.Code != http.StatusForbidden {
			t.Errorf("GET %s for non-admin: %d", p, rec.Code)
		}
	}
	for _, p := range []string{"/vcs", "/vcs/x/settings"} {
		if rec := post(h, p, url.Values{"name": {"x"}, "workspace_scope": {"all"}}); rec.Code != http.StatusForbidden {
			t.Errorf("POST %s for non-admin: %d", p, rec.Code)
		}
	}
}

// Without the registry, VCS connections still serve workspaces and the catalog, but publish nothing.
func TestVCSWithoutRegistry(t *testing.T) {
	store := testStore(t)
	app := newApp(t, t.TempDir(), store, devAuth(t, store, "admin@example.com", true), Features{Deployments: true, VCS: true})
	h := app.Handler()
	if rec := post(h, "/registry/publish", url.Values{"connection": {"x"}}); rec.Code != http.StatusNotFound {
		t.Errorf("publish without the registry: %d", rec.Code)
	}
}
