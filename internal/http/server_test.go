package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"gocloud.dev/blob/memblob"

	"github.com/opsolete/opsolete/internal/auth"
	"github.com/opsolete/opsolete/internal/auth/authtest"
	"github.com/opsolete/opsolete/internal/registry"
	"github.com/opsolete/opsolete/internal/registry/registrytest"
	"github.com/opsolete/opsolete/internal/runnerapi"
	"github.com/opsolete/opsolete/internal/storage"
	"github.com/opsolete/opsolete/internal/templates"
	"github.com/opsolete/opsolete/internal/vcs"
)

var testReg = func() *registry.Registry {
	u, _ := url.Parse("https://ops.example.com")
	return &registry.Registry{PublicURL: u, Key: []byte("test-key")}
}()

func newTestServer(t *testing.T) http.Handler { return newServer(t, "../../catalog", false, nil) }

// newServer is logged in as an admin dev user.
func newServer(t *testing.T, dir string, editor bool, store *storage.Store) http.Handler {
	return newServerAs(t, dir, editor, store, devAuth(t, store, "admin@example.com", true))
}

func newServerAs(t *testing.T, dir string, editor bool, store *storage.Store, a *auth.Auth) http.Handler {
	return newApp(t, dir, store, a, Features{Deployments: true, Registry: true, Editor: editor}).Handler()
}

func newApp(t *testing.T, dir string, store *storage.Store, a *auth.Auth, f Features) *Server {
	tpls, err := templates.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	srv := New(store, tpls, testReg, a, memblob.OpenBucket(nil), f, slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv.SecretKey = vcs.Key("test-secret-key")
	return srv
}

func devAuth(t *testing.T, store *storage.Store, email string, admin bool, groups ...string) *auth.Auth {
	save := func(context.Context, string, string, []string) (string, error) { return "", nil }
	if store != nil {
		save = store.UpsertUser
	}
	var admins []string
	if admin {
		admins = []string{email}
	}
	a, err := auth.New(context.Background(), auth.Config{DevUser: email, DevGroups: groups, Admins: admins, PublicURL: testReg.PublicURL}, []byte("k"), save)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// testStore returns a migrated store, or skips without TEST_DATABASE_URL.
func testStore(t *testing.T) *storage.Store {
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	s, err := storage.Open(context.Background(), dbURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.DB.Close() })
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return s
}

func do(h http.Handler, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func post(h http.Handler, path string, form url.Values) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return do(h, req)
}

func get(h http.Handler, path string) *httptest.ResponseRecorder {
	return do(h, httptest.NewRequest("GET", path, nil))
}

func TestPagesWithoutDB(t *testing.T) {
	h := newTestServer(t)
	for path, want := range map[string]string{
		"/healthz":                    "ok",
		"/":                           "Azure Storage Account",
		"/templates/storage-account":  `name="retention_days"`,
		"/.well-known/terraform.json": `"modules.v1":"/v1/modules/"`,
	} {
		rec := get(h, path)
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), want) {
			t.Errorf("GET %s: %d, missing %q", path, rec.Code, want)
		}
	}
}

func TestLoginRequired(t *testing.T) {
	is := authtest.New(t)
	a, err := auth.New(context.Background(), auth.Config{Issuer: is.URL, ClientID: authtest.ClientID, ClientSecret: authtest.ClientSecret,
		PublicURL: testReg.PublicURL}, []byte("k"), nil)
	if err != nil {
		t.Fatal(err)
	}
	h := newServerAs(t, "../../catalog", true, nil, a)
	if rec := get(h, "/workspaces?x=1"); rec.Code != http.StatusFound || rec.Header().Get("Location") != "/auth/login?next=%2Fworkspaces%3Fx%3D1" {
		t.Fatalf("anonymous page view: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	for _, path := range []string{"/templates/storage-account", "/templates/new", "/registry", "/workspaces/x/runs"} {
		if rec := post(h, path, url.Values{"name": {"abc"}}); rec.Code != http.StatusUnauthorized {
			t.Errorf("anonymous POST %s: %d", path, rec.Code)
		}
	}
	for _, path := range []string{"/healthz", "/.well-known/terraform.json"} {
		if rec := get(h, path); rec.Code != http.StatusOK {
			t.Errorf("public %s: %d", path, rec.Code)
		}
	}
	if rec := get(h, "/auth/login"); rec.Code != http.StatusFound || !strings.HasPrefix(rec.Header().Get("Location"), is.URL) {
		t.Fatalf("login must redirect to the issuer: %d %q", rec.Code, rec.Header().Get("Location"))
	}
}

func TestCrossSitePostRejected(t *testing.T) {
	req := httptest.NewRequest("POST", "/templates/storage-account", strings.NewReader("name=abc"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	if rec := do(newTestServer(t), req); rec.Code != http.StatusForbidden {
		t.Fatalf("cross-site POST: %d", rec.Code)
	}
}

func TestEditorIsAdminOnly(t *testing.T) {
	h := newServerAs(t, t.TempDir(), true, nil, devAuth(t, nil, "someone@example.com", false))
	if rec := get(h, "/templates/new"); rec.Code != http.StatusForbidden {
		t.Fatalf("builder for non-admin: %d", rec.Code)
	}
	if rec := upload(h, map[string]string{"namespace": "x"}, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("upload for non-admin: %d", rec.Code)
	}
	if page := get(h, "/").Body.String(); strings.Contains(page, "New template") || !strings.Contains(page, "someone@example.com") {
		t.Fatalf("index for non-admin: %s", page)
	}
}

func TestFeatureSwitches(t *testing.T) {
	a := devAuth(t, nil, "admin@example.com", true)
	noRegistry := newApp(t, "../../catalog", nil, a, Features{Deployments: true, Editor: true}).Handler()
	for _, path := range []string{"/registry", "/v1/modules/a/b/c/versions"} {
		if rec := get(noRegistry, path); rec.Code != http.StatusNotFound {
			t.Errorf("registry off, GET %s: %d", path, rec.Code)
		}
	}
	if d := get(noRegistry, "/.well-known/terraform.json").Body.String(); strings.Contains(d, "modules.v1") || !strings.Contains(d, `"tfe.v2":"/api/v2/"`) {
		t.Errorf("registry off, discovery: %s", d)
	}
	if page := get(noRegistry, "/").Body.String(); strings.Contains(page, `href="/registry"`) || !strings.Contains(page, `href="/workspaces"`) {
		t.Fatalf("nav with registry off: %s", page)
	}
	if page := get(noRegistry, "/templates/new").Body.String(); strings.Contains(page, "Start from a registry module") {
		t.Fatal("builder must not offer registry modules when the registry is off")
	}
	// Registry templates are refused while the registry is off.
	form := url.Values{"name": {"x"}, "module_source": {"ops.example.com/acme/storage/azurerm"}, "module_ref": {"1.0.0"}}
	dir := t.TempDir()
	if rec := post(newApp(t, dir, nil, a, Features{Deployments: true, Editor: true}).Handler(), "/templates/new", form); rec.Code != http.StatusUnprocessableEntity ||
		!strings.Contains(rec.Body.String(), "disabled") {
		t.Fatalf("registry template saved with registry off: %d", rec.Code)
	}
	if err := newApp(t, dir, nil, a, Features{Deployments: true}).CheckTemplates(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir+"/reg.json", []byte(`{"id":"reg","name":"r","module":{"source":"ops.example.com/acme/storage/azurerm","ref":"1.0.0"},"inputs":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := newApp(t, dir, nil, a, Features{Deployments: true}).CheckTemplates(); err == nil {
		t.Fatal("startup must fail on a registry template while the registry is off")
	}

	registryOnly := newApp(t, "../../catalog", nil, a, Features{Registry: true, Editor: true}).Handler()
	if rec := get(registryOnly, "/"); rec.Code != http.StatusFound || rec.Header().Get("Location") != "/registry" {
		t.Fatalf("deployments off, GET /: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	for _, path := range []string{"/workspaces", "/templates/storage-account", "/templates/new"} {
		if rec := get(registryOnly, path); rec.Code != http.StatusNotFound {
			t.Errorf("deployments off, GET %s: %d", path, rec.Code)
		}
	}
	if rec := post(registryOnly, runnerapi.Prefix+"/jobs/claim", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("runner API must be off with deployments: %d", rec.Code)
	}
	if rec := get(registryOnly, "/.well-known/terraform.json"); rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "tfe.v2") {
		t.Fatalf("registry protocol must stay on, the cloud API off: %d %s", rec.Code, rec.Body.String())
	}
}

func TestInvalidSubmitRerendersForm(t *testing.T) {
	rec := post(newTestServer(t), "/templates/storage-account", url.Values{"name": {"NO"}, "location": {"westeurope"}})
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "must match") {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestTemplateBuilder(t *testing.T) {
	store := testStore(t)
	dir := t.TempDir()
	if rec := get(newServer(t, dir, false, store), "/templates/new"); rec.Code != http.StatusNotFound {
		t.Fatalf("builder must be off by default, got %d", rec.Code)
	}

	h := newServer(t, dir, true, store)
	form := url.Values{
		"name": {"Virtual network"}, "module_source": {"git::x"}, "module_ref": {"v1"},
		// two filled rows and one blank row, like the page submits
		"in_name":     {"cidr", "region", ""},
		"in_label":    {"Address space", "", ""},
		"in_type":     {"string", "select", "string"},
		"in_required": {"yes", "no", "no"},
		"in_pattern":  {"", "", ""},
		"in_options":  {"", "westeurope, northeurope", ""},
		"in_default":  {"10.0.0.0/16", "westeurope", ""},
	}
	rec := post(h, "/templates/new", form)
	loc := rec.Header().Get("Location")
	id := strings.TrimPrefix(loc, "/templates/")
	if rec.Code != http.StatusSeeOther || len(id) != 36 {
		t.Fatalf("create: %d %q %s", rec.Code, loc, rec.Body.String())
	}
	if rec := get(h, loc); !strings.Contains(rec.Body.String(), `<option selected>westeurope</option>`) ||
		!strings.Contains(rec.Body.String(), `value="10.0.0.0/16"`) {
		t.Fatalf("generated form wrong: %s", rec.Body.String())
	}
	if rec := post(h, "/templates/new", form); rec.Header().Get("Location") == loc {
		t.Fatal("each new template must get its own id")
	}

	form.Set("name", "VNet")
	if rec := post(h, loc+"/edit", form); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != loc {
		t.Fatalf("edit: %d %s", rec.Code, rec.Body.String())
	}
	// A fresh server reads the file back from disk, proving it was persisted.
	if rec := get(newServer(t, dir, false, store), "/"); !strings.Contains(rec.Body.String(), "VNet") {
		t.Fatalf("not persisted: %s", rec.Body.String())
	}
}

func upload(h http.Handler, fields map[string]string, archive []byte) *httptest.ResponseRecorder {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for k, v := range fields {
		mw.WriteField(k, v)
	}
	fw, _ := mw.CreateFormFile("archive", "module.tar.gz")
	fw.Write(archive)
	mw.Close()
	req := httptest.NewRequest("POST", "/registry", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return do(h, req)
}

func TestRegistry(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	ns := "t" + strings.ToLower(newID()[:8]) // unique per run; the test DB is reused
	t.Cleanup(func() { store.DB.Exec(`DELETE FROM modules WHERE namespace=$1`, ns) })
	h := newServer(t, t.TempDir(), true, store)

	archive := registrytest.TarGz(t, map[string]string{"README.md": "# Storage <b>module</b>", "main.tf": `
variable "name" {
  type        = string
  description = "Account name"
}
variable "tags" {
  type = map(string)
}
variable "sku" {
  type    = string
  default = "Standard_LRS"
}
output "id" {
  description = "Storage account id"
  value       = "x"
}
resource "terraform_data" "this" {}
data "terraform_remote_state" "net" {
  backend = "local"
}`})
	fields := map[string]string{"namespace": ns, "name": "storage", "system": "azurerm", "version": "v1.2.0"}
	rec := upload(h, fields, archive)
	docsURL := "/registry/" + ns + "/storage/azurerm/1.2.0"
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != docsURL {
		t.Fatalf("upload: %d %q %s", rec.Code, rec.Header().Get("Location"), rec.Body.String())
	}

	// Generated docs: HTML page and Markdown (tfconfig.RenderMarkdown).
	page := get(h, docsURL).Body.String()
	for _, want := range []string{
		`version = "1.2.0"`, "  name = ", // usage lists required inputs only
		"<code>map(string)</code>", `<code>&#34;Standard_LRS&#34;</code>`, "Storage account id",
		"# Storage &lt;b&gt;module&lt;/b&gt;",                                              // README is escaped, never rendered as HTML
		"<code>terraform_data.this</code>", "<code>data.terraform_remote_state.net</code>", // resources survive the DB round trip
	} {
		if !strings.Contains(page, want) {
			t.Errorf("docs page missing %q", want)
		}
	}
	if strings.Contains(page, "  sku = ") {
		t.Error("optional input must not be in the usage snippet")
	}
	if md := get(h, docsURL+"/docs.md"); md.Code != 200 || !strings.Contains(md.Body.String(), "Storage account id") ||
		!strings.Contains(md.Body.String(), "# Module `"+ns+"/storage/azurerm 1.2.0`") {
		t.Fatalf("docs.md: %d %s", md.Code, md.Body.String())
	}
	if rec := get(h, "/registry/"+ns+"/storage/azurerm/9.9.9"); rec.Code != http.StatusNotFound {
		t.Fatalf("docs for unknown version: %d", rec.Code)
	}
	if rec := upload(h, fields, archive); rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "immutable") {
		t.Fatalf("re-upload must be rejected: %d", rec.Code)
	}
	if rec := upload(h, map[string]string{"namespace": ns, "name": "bad name", "system": "azurerm", "version": "1.0.0"}, archive); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid name accepted: %d", rec.Code)
	}

	// Registry protocol, as Terraform calls it.
	base := "/v1/modules/" + ns + "/storage/azurerm/"
	if rec := get(h, base+"versions"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("versions without token: %d", rec.Code)
	}
	token, err := store.CreateToken(ctx, "test", storage.ScopeRegistry)
	if err != nil {
		t.Fatal(err)
	}
	authed := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		return do(h, req)
	}
	rec = authed(base + "versions")
	var vs struct {
		Modules []struct{ Versions []struct{ Version string } }
	}
	if json.Unmarshal(rec.Body.Bytes(), &vs); rec.Code != 200 || vs.Modules[0].Versions[0].Version != "1.2.0" {
		t.Fatalf("versions: %d %s", rec.Code, rec.Body.String())
	}
	if rec := authed("/v1/modules/" + ns + "/nope/azurerm/versions"); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown module: %d", rec.Code)
	}
	rec = authed(base + "1.2.0/download")
	loc := rec.Header().Get("X-Terraform-Get")
	if rec.Code != http.StatusNoContent || !strings.HasPrefix(loc, "/v1/archives/") {
		t.Fatalf("download: %d %q", rec.Code, loc)
	}
	if rec := get(h, loc); rec.Code != 200 || !bytes.Equal(rec.Body.Bytes(), archive) {
		t.Fatalf("archive fetch: %d", rec.Code)
	}
	if rec := get(h, strings.Replace(loc, "sig=", "sig=0", 1)); rec.Code != http.StatusForbidden {
		t.Fatalf("tampered signature: %d", rec.Code)
	}

	// Builder: pick the module, get its variables as fields; complex types are flagged.
	rec = get(h, "/templates/new?module="+ns+"/storage/azurerm/1.2.0")
	page = rec.Body.String()
	if !strings.Contains(page, `value="ops.example.com/`+ns+`/storage/azurerm"`) || !strings.Contains(page, `value="Account name"`) ||
		!strings.Contains(page, "tags (map(string))") {
		t.Fatalf("builder prefill: %d %s", rec.Code, page)
	}
	form := url.Values{"name": {"Storage"}, "module_source": {"ops.example.com/" + ns + "/storage/azurerm"}, "module_ref": {"9.9.9"},
		"in_name": {"name"}, "in_type": {"string"}, "in_required": {"yes"}}
	if rec := post(h, "/templates/new", form); rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "has no version") {
		t.Fatalf("unknown registry version accepted: %d", rec.Code)
	}
	form.Set("module_ref", "1.2.0")
	if rec := post(h, "/templates/new", form); rec.Code != http.StatusSeeOther {
		t.Fatalf("save registry template: %d %s", rec.Code, rec.Body.String())
	}
	if rec := get(h, "/registry"); !strings.Contains(rec.Body.String(), "ops.example.com/"+ns+"/storage/azurerm") {
		t.Fatalf("registry page: %s", rec.Body.String())
	}
}

func TestWorkspaceFlow(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	claimWait = 100 * time.Millisecond
	// Leftover runs from earlier tests would be claimed first.
	store.DB.Exec(`UPDATE runs SET status = 'errored' WHERE status IN ('plan_queued', 'planning', 'needs_confirmation', 'apply_queued', 'applying')`)
	app := newApp(t, "../../catalog", store, devAuth(t, store, "admin@example.com", true), Features{Deployments: true, Registry: true})
	h := app.Handler()
	other := newServerAs(t, "../../catalog", false, store, devAuth(t, store, "other@example.com", false))

	// Create the workspace from a template.
	name := "wf-" + strings.ToLower(newID()[:8])
	t.Cleanup(func() { store.DB.Exec(`DELETE FROM workspaces WHERE name = $1`, name) })
	form := url.Values{"name": {"acct01"}, "location": {"westeurope"}, "retention_days": {"7"}, "ops-workspace": {"Bad Name"}}
	if rec := post(h, "/templates/storage-account", form); rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "lowercase letters") {
		t.Fatalf("invalid workspace name: %d", rec.Code)
	}
	form.Set("ops-workspace", name)
	rec := post(h, "/templates/storage-account", form)
	runURL := rec.Header().Get("Location")
	if rec.Code != http.StatusSeeOther || !strings.HasPrefix(runURL, "/workspaces/"+name+"/runs/") {
		t.Fatalf("create workspace: %d %q %s", rec.Code, runURL, rec.Body.String())
	}
	runID := strings.TrimPrefix(runURL, "/workspaces/"+name+"/runs/")
	if rec := post(h, "/templates/storage-account", form); rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "already exists") {
		t.Fatalf("duplicate workspace name: %d", rec.Code)
	}
	ws, err := store.GetWorkspace(ctx, name)
	if err != nil {
		t.Fatal(err)
	}

	runnerTok, _ := store.CreateToken(ctx, "r", storage.ScopeRunner)
	registryTok, _ := store.CreateToken(ctx, "reg", storage.ScopeRegistry)
	call := func(method, token, path, lease, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, runnerapi.Prefix+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set(runnerapi.LeaseHeader, lease)
		return do(h, req)
	}
	var job runnerapi.Job
	claimJob := func(phase string) {
		t.Helper()
		rec := call("POST", runnerTok, "/jobs/claim", "", `{"runner":"r1"}`)
		if json.Unmarshal(rec.Body.Bytes(), &job); rec.Code != 200 || job.RunID != runID || job.WorkspaceID != ws.ID || job.Phase != phase {
			t.Fatalf("claim %s: %d %s", phase, rec.Code, rec.Body.String())
		}
	}
	noJob := func(why string) {
		t.Helper()
		if rec := call("POST", runnerTok, "/jobs/claim", "", `{"runner":"r2"}`); rec.Code != http.StatusNoContent {
			t.Fatalf("%s: claim got %d", why, rec.Code)
		}
	}
	stateCall := func(method, query, token, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, runnerapi.StatePrefix+"/"+ws.ID+query, strings.NewReader(body))
		if token != "" {
			req.SetBasicAuth(ws.ID, token)
		}
		return do(h, req)
	}
	state := func(serial int, lineage, resource string) string {
		return fmt.Sprintf(`{"version":4,"serial":%d,"lineage":%q,"resources":[%s]}`, serial, lineage, resource)
	}
	saveState := func(body string) {
		t.Helper()
		for _, c := range [][3]string{{"LOCK", "", `{"ID":"x1"}`}, {"POST", "?ID=x1", body}, {"UNLOCK", "", `{"ID":"x1"}`}} {
			if rec := stateCall(c[0], c[1], job.StateToken, c[2]); rec.Code != http.StatusOK {
				t.Fatalf("save state %s: %d %s", c[0], rec.Code, rec.Body.String())
			}
		}
	}
	finish := func(body string) {
		t.Helper()
		if rec := call("POST", runnerTok, "/jobs/"+runID+"/finish", job.LeaseID, body); rec.Code != http.StatusNoContent {
			t.Fatalf("finish %s: %d %s", body, rec.Code, rec.Body.String())
		}
	}

	// Plan phase.
	if rec := call("POST", registryTok, "/jobs/claim", "", `{"runner":"r1"}`); rec.Code != http.StatusUnauthorized {
		t.Fatalf("a registry token must not claim jobs: %d", rec.Code)
	}
	claimJob("plan")
	if job.Vars["name"] != "acct01" || job.Vars["retention_days"] != 7.0 || job.ModuleSource != "git::https://dev.azure.com/your-org/infra/_git/terraform-modules//storage-account?ref=v1.0.0" {
		t.Fatalf("plan job: %+v", job)
	}
	noJob("one job per run phase")

	// State backend, as Terraform's http backend calls it.
	v1 := state(1, "lin", `{"mode":"managed","type":"azurerm_storage_account","name":"this"}`)
	for _, c := range []struct {
		method, query, token, body string
		want                       int
	}{
		{"GET", "", "", "", http.StatusUnauthorized},
		{"GET", "", "forged", "", http.StatusForbidden},
		{"GET", "", job.StateToken, "", http.StatusNoContent},       // new workspace
		{"POST", "?ID=l1", job.StateToken, v1, http.StatusConflict}, // not locked
		{"LOCK", "", job.StateToken, `{"ID":"l1","Who":"runner"}`, http.StatusOK},
		{"LOCK", "", job.StateToken, `{"ID":"l2"}`, http.StatusLocked},
		{"POST", "?ID=l2", job.StateToken, v1, http.StatusConflict}, // someone else's lock id
		{"POST", "?ID=l1", job.StateToken, `{"not":"state"}`, http.StatusBadRequest},
		{"POST", "?ID=l1", job.StateToken, v1, http.StatusOK},
		{"POST", "?ID=l1", job.StateToken, v1, http.StatusOK},                          // unchanged: no new version
		{"POST", "?ID=l1", job.StateToken, state(0, "lin", ""), http.StatusConflict},   // older serial
		{"POST", "?ID=l1", job.StateToken, state(1, "lin", ""), http.StatusConflict},   // same serial, other content
		{"POST", "?ID=l1", job.StateToken, state(2, "other", ""), http.StatusConflict}, // other lineage
		{"UNLOCK", "", job.StateToken, `{"ID":"l1"}`, http.StatusOK},
	} {
		if rec := stateCall(c.method, c.query, c.token, c.body); rec.Code != c.want {
			t.Fatalf("%s%s %s: %d, want %d: %s", c.method, c.query, c.body, rec.Code, c.want, rec.Body.String())
		}
	}
	if rec := stateCall("GET", "", job.StateToken, ""); rec.Body.String() != v1 {
		t.Fatalf("read back state: %q", rec.Body.String())
	}
	if vs, _ := store.StateVersions(ctx, ws.ID, 10); len(vs) != 1 || vs[0].Serial != 1 || vs[0].Indexed {
		t.Fatalf("state versions: %+v", vs)
	}
	stateCall("LOCK", "", job.StateToken, `{"ID":"l3","Who":"alice"}`)
	if rec := stateCall("LOCK", "", job.StateToken, `{"ID":"l4"}`); rec.Code != http.StatusLocked || !strings.Contains(rec.Body.String(), "alice") {
		t.Fatalf("lock conflict body (shows the holder): %d %s", rec.Code, rec.Body.String())
	}

	// The plan phase saves its plan; only the apply phase may read it back.
	planPath := "/jobs/" + runID + "/artifacts/" + runnerapi.ArtifactPlan
	for _, c := range []struct {
		method, path, lease string
		want                int
	}{
		{"POST", planPath, "stolen", http.StatusConflict},
		{"POST", "/jobs/" + runID + "/artifacts/other", job.LeaseID, http.StatusNotFound},
		{"POST", planPath, job.LeaseID, http.StatusNoContent},
		{"GET", planPath, job.LeaseID, http.StatusConflict}, // not in the apply phase yet
	} {
		if rec := call(c.method, runnerTok, c.path, c.lease, "PLANBYTES"); rec.Code != c.want {
			t.Fatalf("%s %s: %d, want %d", c.method, c.path, rec.Code, c.want)
		}
	}
	lockPath := "/jobs/" + runID + "/artifacts/" + runnerapi.ArtifactLockfile
	if rec := call("POST", runnerTok, lockPath, job.LeaseID, "LOCKBYTES"); rec.Code != http.StatusNoContent {
		t.Fatalf("upload lock file: %d", rec.Code)
	}
	// bucket read access alone must not reveal what the runner uploaded
	assertSealed := func(key, secret string) {
		t.Helper()
		raw, err := app.StateStore.ReadAll(ctx, key)
		if err != nil || !bytes.HasPrefix(raw, []byte(sealedPrefix)) || bytes.Contains(raw, []byte(secret)) {
			t.Fatalf("%s not sealed: %q %v", key, raw, err)
		}
	}
	assertSealed(artifactKey(runID, runnerapi.ArtifactPlan), "PLANBYTES")
	assertSealed(artifactKey(runID, runnerapi.ArtifactLockfile), "LOCKBYTES")
	for _, chunk := range []string{"line one\n", "line two\n\nend\n"} {
		if rec := call("POST", runnerTok, "/jobs/"+runID+"/logs", job.LeaseID, chunk); rec.Code != http.StatusNoContent {
			t.Fatalf("logs: %d %s", rec.Code, rec.Body.String())
		}
	}
	if rec := call("POST", runnerTok, "/jobs/"+runID+"/heartbeat", job.LeaseID, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("heartbeat: %d", rec.Code)
	}
	if rec := call("POST", runnerTok, "/jobs/"+runID+"/finish", job.LeaseID, `{"status":"bogus"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid status accepted: %d", rec.Code)
	}
	finish(`{"status":"succeeded","plan":{"add":1,"change":0,"destroy":0,"changes":[{"address":"azurerm_storage_account.this","action":"create"}]}}`)
	planToken := job.StateToken

	// The plan waits for a human: shown with the changes, and confirmable only by the owner or an admin.
	page := get(h, runURL).Body.String()
	for _, want := range []string{"needs confirmation", "1 to add, 0 to change, 0 to destroy", "<code>azurerm_storage_account.this</code>", "Confirm &amp; apply", "Discard"} {
		if !strings.Contains(page, want) {
			t.Errorf("run page lacks %q", want)
		}
	}
	if page := get(other, runURL).Body.String(); strings.Contains(page, "Confirm &amp;") {
		t.Fatal("confirm button shown to a user who may not confirm")
	}
	if rec := post(other, runURL+"/confirm", nil); rec.Code != http.StatusNotFound { // no access: no workspace
		t.Fatalf("confirm by a user without access: %d", rec.Code)
	}
	if rec := post(h, "/workspaces/"+name+"/runs", url.Values{"action": {"apply"}}); rec.Code != http.StatusConflict {
		t.Fatalf("second run while one waits for confirmation: %d", rec.Code)
	}
	noJob("a plan waiting for confirmation")
	if rec := stateCall("GET", "", planToken, ""); rec.Code != http.StatusForbidden {
		t.Fatalf("no state access while a plan waits for confirmation: %d", rec.Code)
	}
	if lock, _ := store.StateLockID(ctx, ws.ID); lock != "" {
		t.Fatalf("the plan phase's lock survived it: %q", lock)
	}
	if rec := post(h, runURL+"/confirm", nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("confirm: %d %s", rec.Code, rec.Body.String())
	}

	// Apply phase: a new lease, but the run's state password again, because Terraform embeds the
	// backend config (with the password) in the saved plan.
	claimJob("apply")
	if job.StateToken != planToken {
		t.Fatal("the apply phase must get the password the saved plan carries")
	}
	if rec := call("GET", runnerTok, planPath, job.LeaseID, ""); rec.Code != http.StatusOK || rec.Body.String() != "PLANBYTES" {
		t.Fatalf("download plan: %d %q", rec.Code, rec.Body.String())
	}
	if rec := call("GET", runnerTok, lockPath, job.LeaseID, ""); rec.Code != http.StatusOK || rec.Body.String() != "LOCKBYTES" {
		t.Fatalf("download lock file: %d %q", rec.Code, rec.Body.String())
	}
	v2 := strings.Replace(state(2, "lin", `{"mode":"managed","type":"azurerm_storage_account","name":"this"}`), "{",
		`{"outputs":{"id":{"value":"abc","type":"string"},"key":{"value":"s3cret","type":"string","sensitive":true}},`, 1)
	saveState(v2)
	finish(`{"status":"succeeded","outputs":{"id":"abc"},
		"resources":[{"address":"azurerm_storage_account.this","type":"azurerm_storage_account","name":"this","provider":"registry.terraform.io/hashicorp/azurerm"}]}`)
	for _, name := range []string{runnerapi.ArtifactPlan, runnerapi.ArtifactLockfile} {
		if ok, _ := app.StateStore.Exists(ctx, artifactKey(runID, name)); ok {
			t.Fatalf("%s must be deleted once applied", name)
		}
	}
	if rec := stateCall("GET", "", job.StateToken, ""); rec.Code != http.StatusForbidden {
		t.Fatalf("state access after the run: %d", rec.Code)
	}
	vs, _ := store.StateVersions(ctx, ws.ID, 10)
	if vs[0].Serial != 2 || !vs[0].Indexed || vs[0].Resources != 1 {
		t.Fatalf("the apply must index the current state version: %+v", vs)
	}
	assertSealed(vs[0].BlobKey, "s3cret") // indexing read it back, so sealed state still works

	// Raw log and SSE stream: the stream replays everything, then ends with "done".
	if rec := get(h, runURL+"/logs"); rec.Body.String() != "line one\nline two\n\nend\n" {
		t.Fatalf("raw log: %q", rec.Body.String())
	}
	rec = get(h, runURL+"/logs/stream")
	stream := rec.Body.String()
	if rec.Header().Get("Content-Type") != "text/event-stream" || !strings.Contains(stream, "data: line two\ndata: \ndata: end\n") ||
		!strings.HasSuffix(stream, "event: done\ndata: applied\n\n") {
		t.Fatalf("stream: %q", stream)
	}
	first := strings.TrimPrefix(strings.SplitN(stream, "\n", 2)[0], "id: ")
	req := httptest.NewRequest("GET", runURL+"/logs/stream", nil)
	req.Header.Set("Last-Event-ID", first)
	if resumed := do(h, req).Body.String(); strings.Contains(resumed, "line one") || !strings.Contains(resumed, "line two") {
		t.Fatalf("resume: %q", resumed)
	}
	if rec := get(h, "/workspaces/"+name+"/runs/"+newID()+"/logs"); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown run: %d", rec.Code)
	}

	// Workspace page, with its resources (and outputs, in their tab).
	page = get(h, "/workspaces/"+name).Body.String() + get(h, "/workspaces/"+name+"?show=outputs").Body.String()
	for _, want := range []string{"<code>&#34;abc&#34;</code>", "<code>&#34;(sensitive)&#34;</code>", "<code>azurerm_storage_account.this</code>", "v2", "applied", "New run"} {
		if !strings.Contains(page, want) {
			t.Errorf("workspace page lacks %q", want)
		}
	}
	if strings.Contains(page, "s3cret") {
		t.Fatal("sensitive output shown")
	}
	if !strings.Contains(get(h, "/workspaces").Body.String(), `href="/workspaces/`+name+`"`) {
		t.Fatal("workspace not listed")
	}
	if rec := get(h, "/resources"); rec.Code != http.StatusNotFound {
		t.Fatalf("the global resources page is gone: %d", rec.Code)
	}

	// Replace: only addresses of the workspace's state reach the runner.
	if rec := post(h, "/workspaces/"+name+"/runs", url.Values{"action": {"apply"}, "replace": {"-destroy"}}); rec.Code != http.StatusBadRequest {
		t.Fatalf("replace of an unknown address: %d", rec.Code)
	}
	if rec := post(h, "/workspaces/"+name+"/runs", url.Values{"action": {"refresh"}, "replace": {"azurerm_storage_account.this"}}); rec.Code != http.StatusBadRequest {
		t.Fatalf("replace in a refresh-only run: %d", rec.Code)
	}
	rec = post(h, "/workspaces/"+name+"/runs", url.Values{"action": {"apply"}, "replace": {"azurerm_storage_account.this"}})
	runURL = rec.Header().Get("Location")
	runID = strings.TrimPrefix(runURL, "/workspaces/"+name+"/runs/")
	claimJob("plan")
	if len(job.Replace) != 1 || job.Replace[0] != "azurerm_storage_account.this" {
		t.Fatalf("replace job: %+v", job)
	}
	finish(`{"status":"succeeded"}`)

	// Drift check: queued by the schedule once drift detection is on, never applied, kept out of the runs list.
	if rec := post(h, "/workspaces/"+name+"/settings", url.Values{"drift_detection": {"on"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("settings: %d", rec.Code)
	}
	if n, err := store.QueueAssessments(ctx, time.Hour); err != nil || n < 1 {
		t.Fatalf("queue assessments: %d %v", n, err)
	}
	a, err := store.LatestAssessment(ctx, ws.ID)
	if err != nil {
		t.Fatal(err)
	}
	runID = a.ID
	claimJob("plan")
	finish(`{"status":"succeeded","plan":{"add":0,"change":1,"destroy":0,"changes":[{"address":"azurerm_storage_account.this","action":"update"}]}}`)
	if a, _, _ = store.GetRun(ctx, runID); a.Status != storage.Drifted {
		t.Fatalf("assessment with drift: %+v", a)
	}
	if n, _ := store.QueueAssessments(ctx, time.Hour); n != 0 {
		t.Fatalf("assessed again within the interval: %d", n)
	}
	if page := get(h, "/workspaces/"+name).Body.String(); !strings.Contains(page, "Drift detected") || !strings.Contains(get(h, "/workspaces").Body.String(), "drifted") {
		t.Fatalf("drift not shown: %s", page)
	}
	if runs, _ := store.Runs(ctx, ws.ID, 25); runs[0].Action == "assess" {
		t.Fatal("assessments must not be listed as runs")
	}
	post(h, "/workspaces/"+name+"/settings", url.Values{"drift_detection": {}})

	// Destroy: a plan that always asks; discarding it frees the workspace; confirming destroys.
	if strings.Contains(get(h, "/workspaces/"+name).Body.String(), "Destroy…") || !strings.Contains(get(h, "/workspaces/"+name+"/settings/deletion").Body.String(), "Queue destroy plan") {
		t.Fatal("destroy belongs on the Destruction and deletion settings page, not the overview")
	}
	for _, page := range []string{"", "/health", "/access", "/deletion"} { // the settings menu replaces the side nav; "</html>": rendered to the end
		if body := get(h, "/workspaces/"+name+"/settings"+page).Body.String(); !strings.Contains(body, `aria-label="Workspace settings"`) || !strings.Contains(body, "</html>") {
			t.Fatalf("settings%s: no settings menu or render error", page)
		}
	}
	if !strings.Contains(get(h, "/workspaces/"+name+"/settings/health").Body.String(), "Drift detection:") ||
		!strings.Contains(get(h, "/workspaces/"+name+"/settings").Body.String(), "Save settings") {
		t.Fatal("settings General and Health pages")
	}
	if get(h, "/workspaces/"+name+"/settings/nope").Code != http.StatusNotFound {
		t.Fatal("unknown settings page must 404")
	}
	if rec := post(other, "/workspaces/"+name+"/runs", url.Values{"action": {"destroy"}}); rec.Code != http.StatusNotFound {
		t.Fatalf("destroy by a user without access: %d", rec.Code)
	}
	if rec := post(h, "/workspaces/"+name+"/settings", url.Values{"auto_apply": {"on"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("settings: %d", rec.Code)
	}
	destroyRun := func() {
		t.Helper()
		rec := post(h, "/workspaces/"+name+"/runs", url.Values{"action": {"destroy"}})
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("queue destroy: %d %s", rec.Code, rec.Body.String())
		}
		runURL = rec.Header().Get("Location")
		runID = strings.TrimPrefix(runURL, "/workspaces/"+name+"/runs/")
		claimJob("plan")
		if job.Action != "destroy" {
			t.Fatalf("destroy job: %+v", job)
		}
		call("POST", runnerTok, "/jobs/"+runID+"/artifacts/"+runnerapi.ArtifactPlan, job.LeaseID, "DESTROYPLAN")
		finish(`{"status":"succeeded","plan":{"add":0,"change":0,"destroy":1,"changes":[{"address":"azurerm_storage_account.this","action":"delete"}]}}`)
	}
	destroyRun()
	if !strings.Contains(get(h, runURL).Body.String(), "Confirm &amp; destroy") {
		t.Fatal("destroys must ask for confirmation even with auto-apply")
	}
	if rec := post(h, runURL+"/discard", nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("discard: %d", rec.Code)
	}
	if ok, _ := app.StateStore.Exists(ctx, artifactKey(runID, runnerapi.ArtifactPlan)); ok {
		t.Fatal("a discarded plan must be deleted")
	}
	destroyRun()
	post(h, runURL+"/confirm", nil)
	claimJob("apply")
	saveState(state(3, "lin", ""))
	finish(`{"status":"succeeded","resources":[]}`)
	page = get(h, "/workspaces/"+name).Body.String()
	if strings.Contains(page, "azurerm_storage_account.this") || !strings.Contains(page, "destroyed") || !strings.Contains(page, "v3") {
		t.Fatalf("destroyed workspace still lists resources, or misses its state: %s", page)
	}
	if !strings.HasSuffix(get(h, runURL+"/logs/stream").Body.String(), "event: done\ndata: applied\n\n") {
		t.Fatal("log stream must end for finished runs")
	}

	// Rollback: a copy of v2 becomes v4, with v2's outputs and resource index.
	vs, _ = store.StateVersions(ctx, ws.ID, 10)
	rollback := func(id int64) int {
		return post(h, fmt.Sprintf("/workspaces/%s/state-versions/%d/rollback", name, id), nil).Code
	}
	if code := rollback(vs[0].ID); code != http.StatusConflict {
		t.Fatalf("rollback to the current version: %d", code)
	}
	if code := rollback(vs[0].ID + 1000); code != http.StatusNotFound {
		t.Fatalf("rollback to an unknown version: %d", code)
	}
	if code := post(other, fmt.Sprintf("/workspaces/%s/state-versions/%d/rollback", name, vs[1].ID), nil).Code; code != http.StatusNotFound {
		t.Fatalf("rollback by a user without access: %d", code)
	}
	if code := rollback(vs[1].ID); code != http.StatusSeeOther {
		t.Fatalf("rollback: %d", code)
	}
	vs, _ = store.StateVersions(ctx, ws.ID, 10)
	if vs[0].Serial != 4 || !vs[0].Indexed || vs[0].Resources != 1 || vs[0].CopyOf != 2 {
		t.Fatalf("rolled back version: %+v", vs[0])
	}
	if out, _, _ := store.CurrentOutputs(ctx, ws.ID); out["id"] != "abc" {
		t.Fatalf("rolled back outputs: %v", out)
	}
	if page := get(h, "/workspaces/"+name+"/states").Body.String(); !strings.Contains(page, "rollback to v2") {
		t.Fatalf("version list must show the rollback: %s", page)
	}
	if b, _ := app.readBlob(ctx, vs[0].BlobKey); !strings.Contains(string(b), `"serial": 4`) || !strings.Contains(string(b), "azurerm_storage_account") {
		t.Fatalf("rolled back state: %s", b)
	}
	if lock, _ := store.StateLockID(ctx, ws.ID); lock != "" {
		t.Fatal("rollback must release the state lock")
	}
}

// TestTeamsVariablesRenameCancel covers workspace access by IdP group, variables and variable
// sets handed to runners, renaming and canceling, through the HTTP routes.
func TestTeamsVariablesRenameCancel(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	claimWait = 100 * time.Millisecond
	store.DB.Exec(`UPDATE runs SET status = 'errored' WHERE status IN ('plan_queued', 'planning', 'needs_confirmation', 'apply_queued', 'applying')`)
	dir := t.TempDir()
	os.WriteFile(dir+"/vars.json", []byte(`{"id":"vars","name":"Vars","module":{"source":"git::https://x//m","ref":"v1"},
		"inputs":[{"name":"location","type":"string","default":"westeurope"}]}`), 0o600)
	admin := newServerAs(t, dir, false, store, devAuth(t, store, "admin@example.com", true))
	owner := newServerAs(t, dir, false, store, devAuth(t, store, "owner@example.com", false))
	reader := newServerAs(t, dir, false, store, devAuth(t, store, "reader@example.com", false, "grp-readers"))
	operator := newServerAs(t, dir, false, store, devAuth(t, store, "op@example.com", false, "grp-ops"))
	outsider := newServerAs(t, dir, false, store, devAuth(t, store, "out@example.com", false, "grp-other"))

	name := "tp-" + strings.ToLower(newID()[:8])
	t.Cleanup(func() { store.DB.Exec(`DELETE FROM workspaces WHERE name IN ($1, $2)`, name, name+"-renamed") })
	rec := post(owner, "/templates/vars", url.Values{"ops-workspace": {name}, "location": {"westeurope"}})
	runURL := rec.Header().Get("Location")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	wsURL := "/workspaces/" + name

	// Visibility: owner and admin; groups only once granted, and read is read-only.
	for who, h := range map[string]http.Handler{"reader": reader, "operator": operator, "outsider": outsider} {
		if rec := get(h, wsURL); rec.Code != http.StatusNotFound || strings.Contains(get(h, "/workspaces").Body.String(), name) {
			t.Fatalf("%s sees an ungranted workspace: %d", who, rec.Code)
		}
	}
	if rec := post(owner, wsURL+"/access", url.Values{"group": {"grp-readers"}, "level": {"read"}}); rec.Code != http.StatusForbidden {
		t.Fatalf("only admins grant access: %d", rec.Code)
	}
	for group, level := range map[string]string{"grp-readers": "read", "grp-ops": "operate"} {
		if rec := post(admin, wsURL+"/access", url.Values{"group": {group}, "level": {level}}); rec.Code != http.StatusSeeOther {
			t.Fatalf("grant %s: %d", group, rec.Code)
		}
	}
	if !strings.Contains(get(reader, "/workspaces").Body.String(), name) || get(reader, runURL).Code != 200 {
		t.Fatal("a read grant must show the workspace and its runs")
	}
	if !strings.Contains(get(admin, wsURL+"/settings/access").Body.String(), `<option value="grp-ops">`) {
		t.Fatal("grant form must offer the groups seen at login")
	}
	if rec := get(owner, "/workspaces/new?template=vars"); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/templates/vars" ||
		!strings.Contains(get(owner, "/workspaces/new").Body.String(), `<option value="vars">`) {
		t.Fatalf("new workspace can start from a template: %d", rec.Code)
	}
	if rec := post(reader, wsURL+"/runs", url.Values{"action": {"destroy"}}); rec.Code != http.StatusForbidden {
		t.Fatalf("read access must not start runs: %d", rec.Code)
	}
	if page := get(reader, runURL).Body.String(); strings.Contains(page, "Cancel run") {
		t.Fatal("cancel button shown to a reader")
	}
	if get(outsider, wsURL).Code != http.StatusNotFound {
		t.Fatal("other groups still see nothing")
	}

	// Variables: workspace ones by operators, sets by admins; sensitive values never shown again.
	setName := name + "-set"
	t.Cleanup(func() { store.DB.Exec(`DELETE FROM variable_sets WHERE name = $1`, setName) })
	for _, v := range []url.Values{
		{"category": {"env"}, "key": {"ARM_CLIENT_SECRET"}, "value": {"s3cret-ws"}, "sensitive": {"on"}},
		{"category": {"env"}, "key": {"ARM_CLIENT_ID"}, "value": {"client-ws"}},
		{"category": {"terraform"}, "key": {"tags"}, "value": {"from-ws"}},
	} {
		if rec := post(operator, wsURL+"/variables", v); rec.Code != http.StatusSeeOther {
			t.Fatalf("set %v: %d %s", v, rec.Code, rec.Body.String())
		}
	}
	for _, v := range []url.Values{
		{"category": {"env"}, "key": {"PATH"}, "value": {"/tmp"}},
		{"category": {"env"}, "key": {"TF_CLI_ARGS"}, "value": {"-x"}},
		{"category": {"env"}, "key": {"TF_VAR_tags"}, "value": {"x"}},
		{"category": {"env"}, "key": {"GIT_SSH_COMMAND"}, "value": {"sh"}},
		{"category": {"terraform"}, "key": {"location"}, "value": {"x"}}, // a template input
		{"category": {"nope"}, "key": {"A"}},
	} {
		if rec := post(operator, wsURL+"/variables", v); rec.Code != http.StatusBadRequest {
			t.Fatalf("%v must be rejected: %d", v, rec.Code)
		}
	}
	if rec := post(reader, wsURL+"/variables", url.Values{"category": {"env"}, "key": {"A"}}); rec.Code != http.StatusForbidden {
		t.Fatalf("a reader must not set variables: %d", rec.Code)
	}
	if rec := post(operator, "/variable-sets", url.Values{"name": {setName}}); rec.Code != http.StatusForbidden {
		t.Fatalf("only admins manage variable sets: %d", rec.Code)
	}
	for _, step := range []struct {
		path string
		form url.Values
	}{
		{"/variable-sets", url.Values{"name": {setName}}},
		{"/variable-sets/" + setName + "/variables", url.Values{"category": {"env"}, "key": {"ARM_TENANT_ID"}, "value": {"tenant-set"}}},
		{"/variable-sets/" + setName + "/workspaces", url.Values{"workspace": {name}}},
	} {
		if rec := post(admin, step.path, step.form); rec.Code != http.StatusSeeOther {
			t.Fatalf("%s: %d %s", step.path, rec.Code, rec.Body.String())
		}
	}
	// A second set entry with the workspace's key: the workspace's own variable wins.
	post(admin, "/variable-sets/"+setName+"/variables", url.Values{"category": {"env"}, "key": {"ARM_CLIENT_ID"}, "value": {"client-set"}})
	if rec := post(admin, "/variable-sets/"+setName+"/workspaces", url.Values{"workspace": {"no-such-ws"}}); rec.Code != http.StatusBadRequest {
		t.Fatalf("attach to an unknown workspace: %d", rec.Code)
	}
	page := get(reader, wsURL+"/variables").Body.String()
	if strings.Contains(page, "s3cret-ws") || !strings.Contains(page, "client-ws") || !strings.Contains(page, setName) ||
		!strings.Contains(page, "tenant-set") {
		t.Fatalf("workspace page must show non-sensitive values and applied sets, never sensitive ones: %s", page)
	}
	if p := get(admin, "/variable-sets/"+setName).Body.String(); strings.Contains(p, "<option>"+name+"</option>") {
		t.Fatal("an attached workspace must not be offered again")
	}
	if p := get(admin, "/variable-sets/"+setName).Body.String(); strings.Contains(p, "s3cret") || !strings.Contains(p, "tenant-set") {
		t.Fatalf("set page must show non-sensitive values only: %s", p)
	}
	if p := get(admin, "/variable-sets").Body.String(); !strings.Contains(p, setName) || !strings.Contains(p, "</html>") {
		t.Fatalf("variable sets list: %s", p)
	}

	// Any runner gets the run, with the variables.
	runnerTok, _ := store.CreateToken(ctx, "r", storage.ScopeRunner)
	req := httptest.NewRequest("POST", runnerapi.Prefix+"/jobs/claim", strings.NewReader(`{"runner":"r"}`))
	req.Header.Set("Authorization", "Bearer "+runnerTok)
	rec = do(admin, req)
	var job runnerapi.Job
	if json.Unmarshal(rec.Body.Bytes(), &job); rec.Code != 200 || !strings.HasSuffix(runURL, job.RunID) {
		t.Fatalf("claim: %d %s", rec.Code, rec.Body.String())
	}
	if job.Env["ARM_CLIENT_SECRET"] != "s3cret-ws" || job.Env["ARM_CLIENT_ID"] != "client-ws" || job.Env["ARM_TENANT_ID"] != "tenant-set" ||
		job.Vars["tags"] != "from-ws" || job.Vars["location"] != "westeurope" {
		t.Fatalf("job variables: env %v vars %v", job.Env, job.Vars)
	}

	// Cancel: an operator asks; the runner hears it on its next heartbeat and keeps its lease.
	beat := func() int {
		req := httptest.NewRequest("POST", runnerapi.Prefix+"/jobs/"+job.RunID+"/heartbeat", nil)
		req.Header.Set("Authorization", "Bearer "+runnerTok)
		req.Header.Set(runnerapi.LeaseHeader, job.LeaseID)
		return do(admin, req).Code
	}
	if c := beat(); c != http.StatusNoContent {
		t.Fatalf("heartbeat before cancel: %d", c)
	}
	if !strings.Contains(get(operator, runURL).Body.String(), "Cancel run") {
		t.Fatal("cancel button missing for an operator")
	}
	if rec := post(operator, runURL+"/cancel", nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("cancel: %d", rec.Code)
	}
	if c := beat(); c != runnerapi.StatusCancel {
		t.Fatalf("the runner must be asked to cancel: %d", c)
	}
	if !strings.Contains(get(operator, runURL).Body.String(), "Force cancel") {
		t.Fatal("force cancel must be offered once a cancel is pending")
	}
	if rec := post(operator, runURL+"/cancel", url.Values{"force": {"1"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("force cancel: %d", rec.Code)
	}
	if c := beat(); c != http.StatusConflict {
		t.Fatalf("a force-canceled runner must be cut off: %d", c)
	}
	if rec := post(operator, runURL+"/cancel", nil); rec.Code != http.StatusConflict {
		t.Fatalf("canceling a finished run: %d", rec.Code)
	}

	// Rename: same workspace, new URL; the old one is gone; grants and runs stay.
	if rec := post(reader, wsURL+"/settings", url.Values{"name": {name + "-renamed"}}); rec.Code != http.StatusForbidden {
		t.Fatalf("a reader must not rename: %d", rec.Code)
	}
	if rec := post(operator, wsURL+"/settings", url.Values{"name": {"Not Valid"}}); rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid name: %d", rec.Code)
	}
	if rec := post(operator, wsURL+"/settings", url.Values{"name": {name + "-renamed"}}); rec.Code != http.StatusSeeOther ||
		rec.Header().Get("Location") != wsURL+"-renamed/settings" {
		t.Fatalf("rename: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if get(owner, wsURL).Code != http.StatusNotFound {
		t.Fatal("the old name must be free")
	}
	page = get(reader, wsURL+"-renamed/runs").Body.String() + get(reader, wsURL+"-renamed/variables").Body.String()
	if !strings.Contains(page, "canceled") || !strings.Contains(page, "client-ws") || !strings.Contains(page, setName) {
		t.Fatalf("renamed workspace lost its runs or variables: %s", page)
	}
}

// TestBlobEncryption: state store files are sealed with SECRET_KEY; plaintext ones from before still read.
func TestBlobEncryption(t *testing.T) {
	ctx := context.Background()
	s := &Server{StateStore: memblob.OpenBucket(nil), SecretKey: vcs.Key("test-secret-key")}
	if err := s.writeBlob(ctx, "a", []byte(`{"password":"hunter2"}`)); err != nil {
		t.Fatal(err)
	}
	if raw, _ := s.StateStore.ReadAll(ctx, "a"); bytes.Contains(raw, []byte("hunter2")) {
		t.Fatalf("stored in plaintext: %s", raw)
	}
	if b, err := s.readBlob(ctx, "a"); err != nil || string(b) != `{"password":"hunter2"}` {
		t.Fatalf("read back: %s %v", b, err)
	}
	s.StateStore.WriteAll(ctx, "legacy", []byte(`{"serial":1}`), nil)
	if b, err := s.readBlob(ctx, "legacy"); err != nil || string(b) != `{"serial":1}` {
		t.Fatalf("legacy plaintext: %s %v", b, err)
	}
	if _, err := (&Server{StateStore: s.StateStore, SecretKey: vcs.Key("other")}).readBlob(ctx, "a"); err == nil {
		t.Fatal("a wrong key must fail")
	}
}

// Remote state sharing: a run of another workspace may read (never write or lock) a state that is
// shared with it, all workspaces or by name.
func TestRemoteStateSharing(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	app := newApp(t, "../../catalog", store, devAuth(t, store, "admin@example.com", true), Features{Deployments: true})
	h := app.Handler()
	suffix := strings.ToLower(newID()[:8])
	src, err := store.CreateEmptyWorkspace(ctx, "rs-src-"+suffix, false, "")
	if err != nil {
		t.Fatal(err)
	}
	cons, err := store.CreateEmptyWorkspace(ctx, "rs-cons-"+suffix, false, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.DB.Exec(`DELETE FROM workspaces WHERE id::text IN ($1, $2)`, src.ID, cons.ID) })
	run, err := store.CreateRun(ctx, cons.ID, "apply", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	// as if a runner claimed it: the consumer's run is working, so its state credentials are valid
	if _, err := store.DB.Exec(`UPDATE runs SET status = 'planning', lease_id = 'l' WHERE id::text = $1`, run.ID); err != nil {
		t.Fatal(err)
	}
	state := func(method, token string) int {
		req := httptest.NewRequest(method, runnerapi.StatePrefix+"/"+src.ID, strings.NewReader(`{"ID":"x"}`))
		req.SetBasicAuth(cons.ID, token)
		return do(h, req).Code
	}
	share := func(mode string, consumers ...string) {
		t.Helper()
		if rec := post(h, "/workspaces/"+src.Name+"/settings", url.Values{"remote_state": {mode}, "remote_state_consumers": consumers}); rec.Code != http.StatusSeeOther {
			t.Fatalf("share %s: %d %s", mode, rec.Code, rec.Body.String())
		}
	}
	token := app.stateToken(cons.ID, run.ID)
	if c := state("GET", token); c != http.StatusForbidden {
		t.Fatalf("not shared by default: %d", c)
	}
	share("specific", cons.ID, "not-a-uuid", src.ID) // unknown ids and itself are ignored
	if c := state("GET", token); c != http.StatusNoContent {
		t.Fatalf("shared with the consumer: %d", c)
	}
	if c := state("GET", "forged"); c != http.StatusForbidden {
		t.Fatalf("forged consumer credentials: %d", c)
	}
	if c := state("LOCK", token); c != http.StatusForbidden {
		t.Fatalf("a consumer must not lock: %d", c)
	}
	if body := get(h, "/workspaces/"+src.Name+"/settings").Body.String(); !strings.Contains(body, `value="`+cons.ID+`" checked>`) {
		t.Fatal("General settings must show the consumer selected")
	}
	share("all")
	if c := state("GET", token); c != http.StatusNoContent {
		t.Fatalf("shared with all: %d", c)
	}
	share("specific")
	if c := state("GET", token); c != http.StatusForbidden {
		t.Fatalf("sharing turned off: %d", c)
	}
	// the Health page's form leaves sharing alone
	share("all")
	post(h, "/workspaces/"+src.Name+"/settings", url.Values{"page": {"health"}})
	if c := state("GET", token); c != http.StatusNoContent {
		t.Fatalf("Health save changed sharing: %d", c)
	}
}
