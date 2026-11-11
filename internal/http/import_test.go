package httpserver

import (
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TestImportFlow: a template whose module imports what exists (existing) is added with only its key; the plan
// imports and finds; once the plan is discarded, the run page offers its found values, and the form filled in
// with them plans again (docs/resource-import.md).
func TestImportFlow(t *testing.T) {
	store := testStore(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "vm.json"), []byte(`{"id": "vm", "name": "VM", "module": {"source": "git::https://x//m", "ref": "v1"},
		"inputs": [{"name": "name", "type": "string", "required": true, "key": true},
		{"name": "subnet", "type": "string", "required": true},
		{"name": "size", "type": "string"},
		{"name": "nic", "type": "string"},
		{"name": "password", "type": "string", "sensitive": true},
		{"name": "existing", "type": "bool"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	h := newServer(t, dir, false, store)
	r := &fakeRunner{h: h}
	ws := "imp-" + strings.ToLower(newID()[:8])
	if rec := post(h, "/workspaces/new", url.Values{fieldWorkspace: {ws}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("create: %d", rec.Code)
	}
	add := "/workspaces/" + ws + "/templates/new?template=vm"
	page := get(h, add).Body.String()
	if !strings.Contains(page, `>Import existing</a>`) || !regexp.MustCompile(`id="subnet"[^>]*required`).MatchString(page) ||
		strings.Contains(page, `name="existing" checked`) {
		t.Fatalf("add form: no import choice, or importing already:\n%s", page)
	}
	page = get(h, add+"&import=1").Body.String()
	if !strings.Contains(page, `name="existing" checked`) || regexp.MustCompile(`id="subnet"[^>]*required`).MatchString(page) ||
		!regexp.MustCompile(`id="name"[^>]*required`).MatchString(page) || !strings.Contains(page, "Importing: fill in") {
		t.Fatalf("add form importing:\n%s", page)
	}
	if rec := post(h, add, url.Values{"name": {"vm1"}}); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("not importing, subnet empty: %d", rec.Code)
	}
	if rec := post(h, add+"&import=1", url.Values{"name": {"vm1"}}); rec.Code != http.StatusUnprocessableEntity { // unticked: not importing
		t.Fatalf("the import form posted with existing unticked, subnet empty: %d", rec.Code)
	}
	run := runID(t, post(h, add, url.Values{"name": {"vm1"}, "existing": {"on"}, "nic": {"test-vm235"}})) // set by hand: never found

	j := r.claim(t, run, "plan")
	if len(j.Modules) != 1 || j.Modules[0].Inputs["existing"] != true {
		t.Fatalf("job: %+v", j.Modules)
	} else if v, ok := j.Modules[0].Inputs["subnet"]; !ok || v != nil {
		t.Fatalf("subnet in the job: %v (%v), want null", v, ok)
	}
	mod := j.Modules[0].Name
	r.finish(t, `{"status": "succeeded", "resources": [], "plan": {"add": 0, "change": 1, "destroy": 0, "import": 1,
		"imports": [{"to": "module.`+mod+`.x.y", "id": "/vms/vm1"}], "changes": []},
		"found": {"`+mod+`": {"subnet": "/subnets/a", "size": "large", "password": "hunter2"}}}`)
	runURL := "/workspaces/" + ws + "/runs/" + run
	if page := get(h, runURL).Body.String(); !strings.Contains(page, "Found while importing") || !strings.Contains(page, "Discard this plan first") {
		t.Fatalf("run page waiting for confirmation:\n%s", page)
	}
	if rec := post(h, runURL+"/discard", nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("discard: %d", rec.Code)
	}
	again := "/workspaces/" + ws + "/templates/new?template=vm&amp;found=" + run + "&amp;from=" + mod
	if page := get(h, runURL).Body.String(); !strings.Contains(page, again) {
		t.Fatalf("run page after discard: no link %s\n%s", again, page)
	}

	form := strings.ReplaceAll(again, "&amp;", "&")
	page = get(h, form).Body.String()
	for _, want := range []string{`value="/subnets/a"`, `value="large"`, `value="test-vm235"`, `name="existing" checked`, "Filled in with what"} {
		if !strings.Contains(page, want) {
			t.Fatalf("form: no %s\n%s", want, page)
		}
	}
	if strings.Contains(page, "hunter2") {
		t.Fatal("a sensitive found value in the form")
	}
	if rec := get(h, "/workspaces/"+ws+"/templates/new?template=vm&found="+uuid.NewString()+"&from="+mod); rec.Code != http.StatusNotFound {
		t.Fatalf("another run's found values: %d", rec.Code)
	}
	run2 := runID(t, post(h, form, url.Values{"name": {"vm1"}, "existing": {"on"}, "subnet": {"/subnets/a"}, "size": {"large"}}))
	if j := r.claim(t, run2, "plan"); j.Modules[0].Inputs["subnet"] != "/subnets/a" || j.Modules[0].Inputs["size"] != "large" {
		t.Fatalf("job with found values: %+v", j.Modules[0].Inputs)
	}
}
