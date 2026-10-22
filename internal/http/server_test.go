package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
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

	"github.com/rudesome/form2stack/internal/auth"
	"github.com/rudesome/form2stack/internal/auth/authtest"
	"github.com/rudesome/form2stack/internal/registry"
	"github.com/rudesome/form2stack/internal/registry/registrytest"
	"github.com/rudesome/form2stack/internal/runnerapi"
	"github.com/rudesome/form2stack/internal/storage"
	"github.com/rudesome/form2stack/internal/templates"
)

var testReg = func() *registry.Registry {
	u, _ := url.Parse("https://f2s.example.com")
	return &registry.Registry{PublicURL: u, Key: []byte("test-key")}
}()

func newTestServer(t *testing.T) http.Handler { return newServer(t, "../../catalog", false, nil) }

// newServer is logged in as an admin dev user.
func newServer(t *testing.T, dir string, editor bool, store *storage.Store) http.Handler {
	return newServerAs(t, dir, editor, store, devAuth(t, store, "admin@example.com", true))
}

func newServerAs(t *testing.T, dir string, editor bool, store *storage.Store, a *auth.Auth) http.Handler {
	tpls, err := templates.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	return New(store, tpls, testReg, a, editor, slog.New(slog.NewTextHandler(io.Discard, nil))).Handler()
}

func devAuth(t *testing.T, store *storage.Store, email string, admin bool) *auth.Auth {
	save := func(context.Context, string, string) (string, error) { return "", nil }
	if store != nil {
		save = store.UpsertUser
	}
	var admins []string
	if admin {
		admins = []string{email}
	}
	a, err := auth.New(context.Background(), auth.Config{DevUser: email, Admins: admins, PublicURL: testReg.PublicURL}, []byte("k"), save)
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
	if rec := get(h, "/deployments?x=1"); rec.Code != http.StatusFound || rec.Header().Get("Location") != "/auth/login?next=%2Fdeployments%3Fx%3D1" {
		t.Fatalf("anonymous page view: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	for _, path := range []string{"/templates/storage-account", "/templates/new", "/registry", "/deployments/x/destroy"} {
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
	if !strings.Contains(page, `value="f2s.example.com/`+ns+`/storage/azurerm"`) || !strings.Contains(page, `value="Account name"`) ||
		!strings.Contains(page, "tags (map(string))") {
		t.Fatalf("builder prefill: %d %s", rec.Code, page)
	}
	form := url.Values{"name": {"Storage"}, "module_source": {"f2s.example.com/" + ns + "/storage/azurerm"}, "module_ref": {"9.9.9"},
		"in_name": {"name"}, "in_type": {"string"}, "in_required": {"yes"}}
	if rec := post(h, "/templates/new", form); rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "has no version") {
		t.Fatalf("unknown registry version accepted: %d", rec.Code)
	}
	form.Set("module_ref", "1.2.0")
	if rec := post(h, "/templates/new", form); rec.Code != http.StatusSeeOther {
		t.Fatalf("save registry template: %d %s", rec.Code, rec.Body.String())
	}
	if rec := get(h, "/registry"); !strings.Contains(rec.Body.String(), "f2s.example.com/"+ns+"/storage/azurerm") {
		t.Fatalf("registry page: %s", rec.Body.String())
	}
}

func TestRunnerAPI(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	claimWait = 100 * time.Millisecond
	// Leftover jobs from earlier runs would be claimed first.
	store.DB.Exec(`UPDATE deployments SET status = 'failed' WHERE status IN ('queued', 'running')`)
	h := newServer(t, "../../catalog", false, store)

	d, err := store.CreateDeployment(ctx, "storage-account", "f2s.example.com/acme/storage/azurerm", "1.0.0", map[string]any{"name": "x"}, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.DB.Exec(`DELETE FROM deployments WHERE id=$1`, d.ID) })

	runnerTok, _ := store.CreateToken(ctx, "r", storage.ScopeRunner)
	registryTok, _ := store.CreateToken(ctx, "reg", storage.ScopeRegistry)
	call := func(token, path, lease, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", runnerapi.Prefix+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set(runnerapi.LeaseHeader, lease)
		return do(h, req)
	}

	if rec := call(registryTok, "/jobs/claim", "", `{"runner":"r1"}`); rec.Code != http.StatusUnauthorized {
		t.Fatalf("a registry token must not claim jobs: %d", rec.Code)
	}
	rec := call(runnerTok, "/jobs/claim", "", `{"runner":"r1"}`)
	var job runnerapi.Job
	if json.Unmarshal(rec.Body.Bytes(), &job); rec.Code != 200 || job.DeploymentID != d.ID || job.Vars["name"] != "x" {
		t.Fatalf("claim: %d %s", rec.Code, rec.Body.String())
	}
	// Registry modules arrive as a fresh, server-relative signed archive path.
	if !strings.HasPrefix(job.ModuleSource, "/v1/archives/acme/storage/azurerm/1.0.0.tar.gz?expires=") {
		t.Fatalf("module source: %s", job.ModuleSource)
	}
	if rec := call(runnerTok, "/jobs/claim", "", `{"runner":"r2"}`); rec.Code != http.StatusNoContent {
		t.Fatalf("second claim must find nothing: %d", rec.Code)
	}

	jobPath := "/jobs/" + d.ID
	if rec := call(runnerTok, jobPath+"/logs", "stolen", "x"); rec.Code != http.StatusConflict {
		t.Fatalf("wrong lease must get 409: %d", rec.Code)
	}
	for _, chunk := range []string{"line one\n", "line two\n\nend\n"} {
		if rec := call(runnerTok, jobPath+"/logs", job.LeaseID, chunk); rec.Code != http.StatusNoContent {
			t.Fatalf("logs: %d %s", rec.Code, rec.Body.String())
		}
	}
	if rec := call(runnerTok, jobPath+"/heartbeat", job.LeaseID, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("heartbeat: %d", rec.Code)
	}
	if rec := call(runnerTok, jobPath+"/finish", job.LeaseID, `{"status":"bogus"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid status accepted: %d", rec.Code)
	}
	if rec := call(runnerTok, jobPath+"/finish", job.LeaseID, `{"status":"succeeded","outputs":{"id":"abc"}}`); rec.Code != http.StatusNoContent {
		t.Fatalf("finish: %d %s", rec.Code, rec.Body.String())
	}

	// Raw log and SSE stream: the stream replays everything, then ends with "done".
	if rec := get(h, "/deployments/"+d.ID+"/logs"); rec.Body.String() != "line one\nline two\n\nend\n" {
		t.Fatalf("raw log: %q", rec.Body.String())
	}
	rec = get(h, "/deployments/"+d.ID+"/logs/stream")
	stream := rec.Body.String()
	if rec.Header().Get("Content-Type") != "text/event-stream" || !strings.Contains(stream, "data: line two\ndata: \ndata: end\n") ||
		!strings.HasSuffix(stream, "event: done\ndata: succeeded\n\n") {
		t.Fatalf("stream: %q", stream)
	}
	// Resume after the first event, as a reconnecting EventSource does.
	first := strings.TrimPrefix(strings.SplitN(stream, "\n", 2)[0], "id: ")
	req := httptest.NewRequest("GET", "/deployments/"+d.ID+"/logs/stream", nil)
	req.Header.Set("Last-Event-ID", first)
	if resumed := do(h, req).Body.String(); strings.Contains(resumed, "line one") || !strings.Contains(resumed, "line two") {
		t.Fatalf("resume: %q", resumed)
	}
	if page := get(h, "/deployments/"+d.ID).Body.String(); !strings.Contains(page, "<code>&#34;abc&#34;</code>") || !strings.Contains(page, "r1") {
		t.Fatalf("detail page lacks outputs/runner: %s", page)
	}

	if job.Action != "apply" {
		t.Fatalf("first run must apply: %q", job.Action)
	}

	// Destroy: not by another plain user, then by an admin; the same row is re-queued.
	other := newServerAs(t, "../../catalog", false, store, devAuth(t, store, "other@example.com", false))
	if page := get(other, "/deployments/"+d.ID).Body.String(); strings.Contains(page, "Destroy resources") {
		t.Fatal("destroy button shown to a user who may not destroy")
	}
	if rec := post(other, "/deployments/"+d.ID+"/destroy", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("destroy by non-owner: %d", rec.Code)
	}
	if !strings.Contains(get(h, "/deployments/"+d.ID).Body.String(), "Destroy resources") {
		t.Fatal("destroy button missing for admin")
	}
	if rec := post(h, "/deployments/"+d.ID+"/destroy", nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("destroy: %d %s", rec.Code, rec.Body.String())
	}
	if rec := post(h, "/deployments/"+d.ID+"/destroy", nil); rec.Code != http.StatusConflict {
		t.Fatalf("destroy while queued: %d", rec.Code)
	}
	rec = call(runnerTok, "/jobs/claim", "", `{"runner":"r1"}`)
	if json.Unmarshal(rec.Body.Bytes(), &job); rec.Code != 200 || job.DeploymentID != d.ID || job.Action != "destroy" {
		t.Fatalf("destroy claim: %d %s", rec.Code, rec.Body.String())
	}
	if rec := call(runnerTok, jobPath+"/finish", job.LeaseID, `{"status":"succeeded"}`); rec.Code != http.StatusNoContent {
		t.Fatalf("finish destroy: %d", rec.Code)
	}
	got, hist, _ := store.GetDeployment(ctx, d.ID)
	if got.Status != "destroyed" || got.Outputs != nil || hist[len(hist)-1].Status != "destroyed" {
		t.Fatalf("after destroy: %+v %+v", got, hist)
	}
	if !strings.HasSuffix(get(h, "/deployments/"+d.ID+"/logs/stream").Body.String(), "event: done\ndata: destroyed\n\n") {
		t.Fatal("log stream must end for destroyed deployments")
	}
	if rec := post(h, "/deployments/"+d.ID+"/destroy", nil); rec.Code != http.StatusConflict {
		t.Fatalf("destroy twice: %d", rec.Code)
	}
}
