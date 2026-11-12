package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/opsolete/opsolete/internal/registry/registrytest"
	"github.com/opsolete/opsolete/internal/storage"
)

func TestLatestVersions(t *testing.T) {
	m := func(name, ver string) storage.Module {
		return storage.Module{Namespace: "p", Name: name, System: "azurerm", Version: ver}
	}
	// Ordered by module, newest upload first: the 1.0.1 backport came after 2.0.0.
	got := latestVersions([]storage.Module{m("a", "1.0.1"), m("a", "2.0.0"), m("a", "1.0.0"), m("b", "0.1.0")})
	if len(got) != 2 || got[0].Version != "2.0.0" || got[0].Older != 2 || got[1].Version != "0.1.0" || got[1].Older != 0 {
		t.Fatalf("%+v", got)
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
	ns := "t" + strings.ToLower(newID()[:8])
	h := newServer(t, t.TempDir(), true, store)

	archive := registrytest.TarGz(t, map[string]string{"README.md": "# Storage <b>module</b>", "main.tf": `
variable "name" {
  type        = string
  description = "Account name: 3-24 lowercase letters"
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
	// add-module wizard: upload is one of the workflows (VCS is off in this server)
	if page := get(h, "/registry/publish").Body.String(); !strings.Contains(page, `href="?type=upload"`) || strings.Contains(page, `href="?type=vcs"`) {
		t.Fatalf("workflow choice: %s", page)
	}
	if page := get(h, "/registry/publish?type=upload").Body.String(); !strings.Contains(page, `action="/registry" enctype="multipart/form-data"`) {
		t.Fatalf("upload step: %s", page)
	}
	if rec := get(h, "/registry/publish?type=vcs"); rec.Code != http.StatusNotFound {
		t.Fatalf("vcs step with VCS off: %d", rec.Code)
	}
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

	// Builder: pick the module, get its variables as fields (a map(string) as key / value rows).
	rec = get(h, "/templates/new?module="+ns+"/storage/azurerm/1.2.0")
	page = rec.Body.String()
	if !strings.Contains(page, `value="ops.example.com/`+ns+`/storage/azurerm"`) || !strings.Contains(page, `value="Account name"`) ||
		!strings.Contains(page, `name="in_help" value="3-24 lowercase letters"`) || // the description's "Title: help", split at import
		!strings.Contains(page, `<option value="map" selected>Key / value</option>`) {
		t.Fatalf("builder prefill: %d %s", rec.Code, page)
	}
	form := url.Values{"name": {"Storage"}, "module_source": {"ops.example.com/" + ns + "/storage/azurerm"}, "module_ref": {"9.9.9"},
		"in_name": {"name"}, "in_type": {"string"}, "in_required": {"yes"}}
	if rec := post(h, "/templates/new", form); rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "has no version") {
		t.Fatalf("unknown registry version accepted: %d", rec.Code)
	}
	form.Set("module_ref", "1.2.0")
	rec = post(h, "/templates/new", form)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("save registry template: %d %s", rec.Code, rec.Body.String())
	}
	tplURL := rec.Header().Get("Location")
	// the list finds it (name, namespace · system); deleting is on its page
	if rec := get(h, "/registry"); !strings.Contains(rec.Body.String(), `href="/registry/`+ns+`/storage/azurerm/`) ||
		!strings.Contains(rec.Body.String(), ns+" · azurerm") || strings.Contains(rec.Body.String(), "/delete") ||
		!strings.Contains(rec.Body.String(), `data-filter-item="storage `+ns+` azurerm uploaded"`) || !strings.Contains(rec.Body.String(), `id="modules-empty" hidden`) {
		t.Fatalf("registry page: %s", rec.Body.String())
	}
	if page := get(h, "/registry/"+ns+"/storage/azurerm/1.2.0").Body.String(); !strings.Contains(page, `action="/registry/`+ns+`/storage/azurerm/delete"`) {
		t.Fatalf("module page: no delete: %s", page)
	}

	// Delete: refused while a template deploys the module, then removes every version.
	del := "/registry/" + ns + "/storage/azurerm/delete"
	if rec := post(h, del, nil); rec.Code != http.StatusConflict {
		t.Fatalf("delete module used by a template: %d", rec.Code)
	}
	if rec := post(h, tplURL+"/delete", nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("delete template: %d", rec.Code)
	}
	if rec := post(h, del, nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("delete module: %d %s", rec.Code, rec.Body.String())
	}
	if rec := authed(base + "versions"); rec.Code != http.StatusNotFound {
		t.Fatalf("deleted module still served: %d", rec.Code)
	}
	if rec := post(h, del, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("delete twice: %d", rec.Code)
	}
}
