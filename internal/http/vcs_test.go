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

	"github.com/rudesome/form2stack/internal/registry/registrytest"
	"github.com/rudesome/form2stack/internal/templates"
	"github.com/rudesome/form2stack/internal/vcs"
)

// The HCP no-code flow: connect GitHub, publish a repo, provision a workspace from its template,
// push a new tag, and move the workspace to it.
func TestVCSPublishAndUpdate(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	id := strings.ToLower(newID()[:8]) // the test DB is reused
	ns, connName, wsName, tplID := "v"+id, "gh-"+id, "nc-"+id, "nc-"+id
	t.Cleanup(func() {
		store.DB.Exec(`DELETE FROM workspaces WHERE name = $1`, wsName)
		store.DB.Exec(`DELETE FROM vcs_connections WHERE name = $1`, connName)
		store.DB.Exec(`DELETE FROM modules WHERE namespace = $1`, ns)
	})

	// Fake GitHub: tags are added during the test.
	var mu sync.Mutex
	tags := map[string]string{"v1.0.0": `variable "db_name" { type = string }`}
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
			var list []map[string]string
			for tag := range tags {
				list = append(list, map[string]string{"name": tag})
			}
			json.NewEncoder(w).Encode(list)
		case strings.HasPrefix(p, "/repos/acme/terraform-aws-rds/tarball/"):
			w.Write(registrytest.TarGz(t, map[string]string{"acme-rds-1/main.tf": tags[strings.TrimPrefix(p, "/repos/acme/terraform-aws-rds/tarball/")]}))
		default:
			http.NotFound(w, r)
		}
	}))
	defer gh.Close()
	old := vcs.HTTPClient
	vcs.HTTPClient = gh.Client()
	t.Cleanup(func() { vcs.HTTPClient = old })

	source := "f2s.example.com/" + ns + "/rds/aws"
	dir := t.TempDir()
	tpl := templates.Template{ID: tplID, Name: "RDS", Module: templates.Module{Source: source, Ref: "1.0.0"},
		Inputs: []templates.Input{{Name: "db_name", Label: "DB name", Type: "string", Required: true}}}
	b, _ := json.Marshal(tpl)
	os.WriteFile(filepath.Join(dir, tplID+".json"), b, 0o644)
	app := newApp(t, dir, store, devAuth(t, store, "admin@example.com", true), Features{Deployments: true, Registry: true, VCS: true})
	app.SecretKey = vcs.Key("test-secret-key")
	h := app.Handler()

	// Connect: a bad token is rejected by the connection check; the good one is stored sealed.
	conn := url.Values{"name": {connName}, "provider": {"github"}, "base_url": {gh.URL}, "token": {"wrong"}}
	if rec := post(h, "/vcs", conn); rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "connection check failed") {
		t.Fatalf("bad token: %d", rec.Code)
	}
	conn.Set("token", "secret-pat")
	if rec := post(h, "/vcs", conn); rec.Code != http.StatusSeeOther {
		t.Fatalf("connect: %d %s", rec.Code, rec.Body.String())
	}
	var connID string
	var sealed []byte
	store.DB.QueryRow(`SELECT id, token FROM vcs_connections WHERE name = $1`, connName).Scan(&connID, &sealed)
	if strings.Contains(string(sealed), "secret-pat") || strings.Contains(get(h, "/vcs").Body.String(), "secret-pat") {
		t.Fatal("token stored or shown in plain text")
	}

	// Publish: name and system come from the terraform-<system>-<name> repo name.
	if page := get(h, "/registry/publish?connection="+connID).Body.String(); !strings.Contains(page, `<option value="acme/terraform-aws-rds">`) {
		t.Fatalf("repo picker: %s", page)
	}
	rec := post(h, "/registry/publish", url.Values{"connection": {connID}, "repo": {"acme/terraform-aws-rds"}, "namespace": {ns}})
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/registry/"+ns+"/rds/aws/1.0.0" {
		t.Fatalf("publish: %d %q %s", rec.Code, rec.Header().Get("Location"), rec.Body.String())
	}
	if page := get(h, "/registry").Body.String(); !strings.Contains(page, "/registry/"+ns+"/rds/aws/sync") || !strings.Contains(page, connName) {
		t.Fatalf("registry page: %s", page)
	}
	page := get(h, "/registry/"+ns+"/rds/aws/1.0.0").Body.String()
	if !strings.Contains(page, "No-code ready") || !strings.Contains(page, `href="/templates/`+tplID+`">Provision workspace`) ||
		!strings.Contains(page, "acme/terraform-aws-rds") {
		t.Fatalf("module page: %s", page)
	}

	// Provision a workspace, then free it (no runner in this test).
	rec = post(h, "/templates/"+tplID, url.Values{"f2s-workspace": {wsName}, "db_name": {"nocode"}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("provision: %d %s", rec.Code, rec.Body.String())
	}
	store.DB.Exec(`UPDATE runs SET status = 'applied' FROM workspaces w WHERE w.id = runs.workspace_id AND w.name = $1`, wsName)

	// New tag with a new required input; sync publishes it, the template moves to it.
	mu.Lock()
	tags["v1.0.1"] = tags["v1.0.0"] + "\nvariable \"db_encrypted\" { type = bool }"
	mu.Unlock()
	if rec := post(h, "/registry/"+ns+"/rds/aws/sync", nil); rec.Header().Get("Location") != "/registry/"+ns+"/rds/aws/1.0.1" {
		t.Fatalf("sync: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	tpl.Module.Ref = "1.0.1"
	tpl.Inputs = append(tpl.Inputs, templates.Input{Name: "db_encrypted", Label: "Encrypted", Type: "bool"})
	if err := app.Templates.Save(tpl, true); err != nil {
		t.Fatal(err)
	}

	if page := get(h, "/workspaces/"+wsName).Body.String(); !strings.Contains(page, "A new module version is available") {
		t.Fatalf("no update notice: %s", page)
	}
	if page := get(h, "/workspaces/"+wsName+"/configure").Body.String(); !strings.Contains(page, `value="nocode"`) ||
		!strings.Contains(page, "<code>1.0.0</code> → <code>1.0.1</code>") || !strings.Contains(page, `name="db_encrypted"`) {
		t.Fatalf("configure form: %s", page)
	}
	rec = post(h, "/workspaces/"+wsName+"/configure", url.Values{"db_name": {"nocode"}, "db_encrypted": {"on"}})
	if rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "/workspaces/"+wsName+"/runs/") {
		t.Fatalf("configure: %d %s", rec.Code, rec.Body.String())
	}
	ws, err := store.GetWorkspace(ctx, wsName)
	if err != nil || ws.ModuleRef != "1.0.1" || ws.Inputs["db_encrypted"] != true {
		t.Fatalf("workspace after configure: %+v %v", ws, err)
	}
	// The plan is now active, so a second change has to wait.
	if rec := post(h, "/workspaces/"+wsName+"/configure", url.Values{"db_name": {"other"}}); rec.Code != http.StatusConflict {
		t.Fatalf("configure during a run: %d", rec.Code)
	}
	if ws, _ := store.GetWorkspace(ctx, wsName); ws.Inputs["db_name"] != "nocode" {
		t.Fatal("rejected configure changed the inputs")
	}
}

func TestVCSAdminOnly(t *testing.T) {
	app := newApp(t, t.TempDir(), nil, devAuth(t, nil, "someone@example.com", false), Features{Registry: true, VCS: true})
	h := app.Handler()
	for _, p := range []string{"/vcs", "/registry/publish"} {
		if rec := get(h, p); rec.Code != http.StatusForbidden {
			t.Errorf("GET %s for non-admin: %d", p, rec.Code)
		}
	}
	if rec := post(h, "/vcs", url.Values{"name": {"x"}}); rec.Code != http.StatusForbidden {
		t.Errorf("POST /vcs for non-admin: %d", rec.Code)
	}
}
