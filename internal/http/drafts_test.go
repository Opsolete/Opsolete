package httpserver

import (
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestDrafts: a refused form keeps a typed secret sealed in a hidden draft, never in clear; posted again with the
// field empty, the draft is the value. A draft moved to another field, or forged, isn't.
func TestDrafts(t *testing.T) {
	store := testStore(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "pw.json"), []byte(`{"id": "pw", "name": "PW", "module": {"source": "git::https://x//m", "ref": "v1"},
		"inputs": [{"name": "name", "type": "string", "required": true, "key": true}, {"name": "pw", "type": "string", "required": true, "sensitive": true},
		{"name": "other", "type": "string", "sensitive": true}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	h := newServer(t, dir, false, store)
	r := &fakeRunner{h: h}
	ws := "dr-" + strings.ToLower(newID()[:8])

	rec := post(h, "/templates/pw", url.Values{fieldWorkspace: {"Bad Name"}, "name": {"n"}, "secret.pw": {"s3cret-typed"}})
	page := rec.Body.String()
	m := regexp.MustCompile(`name="draft\.pw" value="([^"]+)"`).FindStringSubmatch(page)
	if rec.Code != 422 || m == nil || strings.Contains(page, "s3cret-typed") || !strings.Contains(page, "What you typed is kept") {
		t.Fatalf("refused form: %d\n%s", rec.Code, page)
	}
	draft := strings.ReplaceAll(m[1], "&#43;", "+")
	if rec := post(h, "/templates/pw", url.Values{fieldWorkspace: {ws + "-x"}, "name": {"n"}, "draft.other": {draft}}); rec.Code != 422 {
		t.Fatalf("a draft moved to another field: %d", rec.Code)
	}
	if rec := post(h, "/templates/pw", url.Values{fieldWorkspace: {ws + "-y"}, "name": {"n"}, "draft.pw": {"Zm9yZ2Vk"}}); rec.Code != 422 {
		t.Fatalf("a forged draft: %d", rec.Code)
	}
	run := runID(t, post(h, "/templates/pw", url.Values{fieldWorkspace: {ws}, "name": {"n"}, "draft.pw": {draft}}))
	if j := r.claim(t, run, "plan"); j.Modules[0].Inputs["pw"] != "s3cret-typed" {
		t.Fatalf("job inputs: %v", j.Modules[0].Inputs)
	}
}
