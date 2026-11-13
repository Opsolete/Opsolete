package httpserver

import (
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// "Promote to…": the menu entry, the picker, the preview, a refused promotion shown again with its
// field errors, and the promotion opening the target's run.
func TestPromotePages(t *testing.T) {
	store := testStore(t)
	dir := t.TempDir()
	tpl := `{"id": "pw", "name": "Pw", "module": {"source": "git::https://x//m", "ref": "v1"}, "inputs": [
		{"name": "name", "type": "string", "required": true}, {"name": "secret", "type": "string", "required": true, "sensitive": true}]}`
	if err := os.WriteFile(filepath.Join(dir, "pw.json"), []byte(tpl), 0o600); err != nil {
		t.Fatal(err)
	}
	h := newServer(t, dir, false, store)
	sfx := strings.ToLower(newID()[:8])
	src, dst := "prm-"+sfx, "prm-dst-"+sfx
	if rec := post(h, "/templates/pw", url.Values{fieldWorkspace: {src}, "name": {"n1"}, "secret.secret": {"s3cret-dev"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("source: %d %s", rec.Code, rec.Body.String())
	}
	body := get(h, "/workspaces/"+src).Body.String()
	if !strings.Contains(body, `<a href="/workspaces/`+src+`/promote">Promote to…</a>`) {
		t.Fatal("the workspace menu has no Promote entry")
	}
	body = get(h, "/workspaces/"+src+"/promote").Body.String()
	if !strings.Contains(body, "<h1>Promote "+src+"</h1>") || !strings.Contains(body, `name="new"`) || !strings.Contains(body, `<option value="default" selected>`) {
		t.Fatalf("picker:\n%s", body)
	}
	preview := "/workspaces/" + src + "/promote?new=" + dst + "&project=default"
	rec := get(h, preview)
	body = rec.Body.String()
	module := regexp.MustCompile(`<code>module\.([a-z0-9_]+)</code>`).FindStringSubmatch(body)
	digest := regexp.MustCompile(`name="digest" value="([0-9a-f]+)"`).FindStringSubmatch(body)
	if rec.Code != http.StatusOK || module == nil || digest == nil || !strings.Contains(body, "<h1>Promote "+src+" to "+dst+"</h1>") ||
		!strings.Contains(body, `<span class="change pill create">added</span>`) || !strings.Contains(body, "<code>n1</code>") ||
		!strings.Contains(body, `name="secret.`+module[1]+`.secret"`) || strings.Contains(body, "s3cret-dev") {
		t.Fatalf("preview: %d\n%s", rec.Code, body)
	}
	form := url.Values{"new": {dst}, "project": {"default"}, "digest": {digest[1]}}
	rec = post(h, "/workspaces/"+src+"/promote", form)
	field := module[1] + ".secret"
	if body = rec.Body.String(); rec.Code != http.StatusUnprocessableEntity || !strings.Contains(body, "<h1>Promote "+src+" to "+dst+"</h1>") ||
		!strings.Contains(body, `<p class="err" id="`+field+`-error">`) || !strings.Contains(body, `href="#`+field+`-error">`+module[1]+`: secret:`) {
		t.Fatalf("missing secret: %d\n%s", rec.Code, body)
	}
	form.Set("secret."+field, "s3cret-prd")
	rec = post(h, "/workspaces/"+src+"/promote", form)
	if loc := rec.Header().Get("Location"); rec.Code != http.StatusSeeOther || !strings.HasPrefix(loc, "/workspaces/"+dst+"/runs/") {
		t.Fatalf("promote: %d %s %s", rec.Code, loc, rec.Body.String())
	}
	if rec = get(h, "/workspaces/"+src+"/promote?new="+dst+"&project=default"); rec.Code != http.StatusUnprocessableEntity ||
		!strings.Contains(rec.Body.String(), `<p class="err" id="new-error">`) {
		t.Fatalf("the name is taken now: %d", rec.Code)
	}
}
