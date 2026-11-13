package httpserver

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/opsolete/opsolete/internal/storage"
)

// TestImportFlow: a template whose module imports what exists (existing) is added with only its key, which plans it
// (a round, never applied); the round's page shows what it found, plans again with it, and applies the last round
// (docs/resource-import.md).
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
		strings.Contains(page, `name="existing"`) {
		t.Fatalf("add form: no import choice, or importing already:\n%s", page)
	}
	page = get(h, add+"&import=1").Body.String()
	if !strings.Contains(page, `<input type="hidden" name="existing" value="on">`) || regexp.MustCompile(`id="subnet"[^>]*required`).MatchString(page) ||
		!regexp.MustCompile(`id="name"[^>]*required`).MatchString(page) || !strings.Contains(page, "Importing: fill in") {
		t.Fatalf("add form importing:\n%s", page)
	}
	if rec := post(h, add, url.Values{"name": {"vm1"}}); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("not importing, subnet empty: %d", rec.Code)
	}

	// round 1: a plan run; the page shows what it found next to what it planned with
	run := runID(t, post(h, add, url.Values{"name": {"vm1"}, "existing": {"on"}, "nic": {"test-vm235"}}))
	j := r.claim(t, run, "plan")
	if j.Action != "plan" || j.Modules[0].Inputs["existing"] != true {
		t.Fatalf("round 1's job: %s %+v", j.Action, j.Modules)
	}
	mod := j.Modules[0].Name
	r.finish(t, `{"status": "succeeded", "resources": [], "plan": {"add": 0, "change": 1, "destroy": 0, "import": 1,
		"imports": [{"to": "module.`+mod+`.x.y", "id": "/vms/vm1"}], "changes": []},
		"found": {"`+mod+`": {"subnet": "/subnets/a", "size": "large", "password": "hunter2"}}}`)
	runURL := "/workspaces/" + ws + "/runs/" + run
	page = get(h, runURL).Body.String()
	for _, want := range []string{`<h2 id="import-head">Import</h2>`, "1 to import · 0 to add · 1 to change · 0 to destroy", `<code>/subnets/a</code>`,
		`action="` + runURL + `/plan-again"`, `action="` + runURL + `/adopt"`, `action="` + runURL + `/change"`} {
		if !strings.Contains(page, want) {
			t.Fatalf("round 1's page lacks %q:\n%s", want, page)
		}
	}
	if strings.Contains(page, "hunter2") || strings.Contains(page, "speculative plan of a pull request") {
		t.Fatal("round 1's page: a sensitive found value, or the pull request text")
	}
	// Change settings: the add form again (the round didn't add the template), with the found values, importing
	rec := post(h, runURL+"/change", nil)
	form := "/workspaces/" + ws + "/templates/new?" + url.Values{"template": {"vm"}, "found": {run}, "from": {mod}}.Encode()
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != form {
		t.Fatalf("change: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	page = get(h, form).Body.String()
	for _, want := range []string{`value="/subnets/a"`, `value="large"`, `value="test-vm235"`, `<input type="hidden" name="existing" value="on">`, "Filled in with what"} {
		if !strings.Contains(page, want) {
			t.Fatalf("form: no %s\n%s", want, page)
		}
	}

	// round 2: planned again with what it found; it only imports, so it's applied as it is: a run that asks
	rec = post(h, runURL+"/plan-again", nil)
	round2 := strings.TrimPrefix(rec.Header().Get("Location"), "/workspaces/"+ws+"/runs/")
	if rec.Code != http.StatusSeeOther || round2 == run {
		t.Fatalf("plan again: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	j = r.claim(t, round2, "plan")
	if j.Modules[0].Inputs["subnet"] != "/subnets/a" || j.Modules[0].Inputs["nic"] != "test-vm235" {
		t.Fatalf("round 2's inputs: %+v", j.Modules[0].Inputs)
	}
	r.finish(t, `{"status": "succeeded", "resources": [], "plan": {"add": 0, "change": 0, "destroy": 0, "import": 1,
		"imports": [{"to": "module.`+mod+`.x.y", "id": "/vms/vm1"}], "changes": []}}`)
	round2URL := "/workspaces/" + ws + "/runs/" + round2
	if page := get(h, round2URL).Body.String(); !strings.Contains(page, "It only imports: apply it.") || strings.Contains(page, "/plan-again") {
		t.Fatalf("round 2's page:\n%s", page)
	}
	if rec := post(h, runURL+"/adopt", nil); rec.Code != http.StatusConflict {
		t.Fatalf("adopting round 1 after round 2: %d", rec.Code)
	}
	apply := runID(t, post(h, round2URL+"/adopt", nil))
	j = r.claim(t, apply, "plan")
	if j.Action != "apply" || j.Modules[0].Inputs["existing"] != true {
		t.Fatalf("the apply's job: %s %+v", j.Action, j.Modules[0].Inputs)
	}
	r.finish(t, `{"status": "succeeded", "resources": [], "plan": {"add": 0, "change": 0, "destroy": 0, "import": 1,
		"imports": [{"to": "module.`+mod+`.x.y", "id": "/vms/vm1"}], "changes": []}}`)
	if page := get(h, "/workspaces/"+ws+"/runs/"+apply).Body.String(); !strings.Contains(page, "a human always confirms that") {
		t.Fatalf("the apply waits, for the import:\n%s", page)
	}
	if st := runStatus(t, store, apply).Status; st != storage.NeedsConfirmation {
		t.Fatalf("the apply: %s", st)
	}
	if rec := post(h, "/workspaces/"+ws+"/runs/"+apply+"/confirm", nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("confirm: %d", rec.Code)
	}
	r.claim(t, apply, "apply")
	r.finish(t, `{"status": "succeeded", "resources": []}`)
	added, err := store.WorkspaceTemplates(context.Background(), mustWorkspace(t, store, ws).ID)
	if err != nil || len(added) != 1 || added[0].Import || added[0].Inputs["subnet"] != "/subnets/a" {
		t.Fatalf("after the import: %+v %v", added, err)
	}
	if rec := get(h, "/workspaces/"+ws+"/templates/new?template=vm&found="+uuid.NewString()+"&from="+mod); rec.Code != http.StatusNotFound {
		t.Fatalf("another run's found values: %d", rec.Code)
	}
	// applied: nothing to go back to, from the apply or from the round it applied, also by an old link (it would turn
	// the import on again)
	for _, id := range []string{apply, round2} {
		u := "/workspaces/" + ws + "/runs/" + id
		if page := get(h, u).Body.String(); strings.Contains(page, `/change"`) || strings.Contains(page, `/adopt"`) {
			t.Fatalf("%s: an applied import still offers to go on:\n%s", id, page)
		}
		if rec := post(h, u+"/change", nil); rec.Code != http.StatusNotFound {
			t.Fatalf("change %s: %d", id, rec.Code)
		}
		if rec := get(h, "/workspaces/"+ws+"/templates/"+mod+"?found="+id); rec.Code != http.StatusNotFound {
			t.Fatalf("found values of %s: %d", id, rec.Code)
		}
	}
}

// TestChangeSettings: a run that ended without its apply goes back to its template's form with what was entered:
// an import round that failed keeps importing, a create that failed keeps creating; with several templates the
// page links each one's form (one button would loop back).
func TestChangeSettings(t *testing.T) {
	store := testStore(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "vm.json"), []byte(`{"id": "vm", "name": "VM", "module": {"source": "git::https://x//m", "ref": "v1"},
		"inputs": [{"name": "name", "type": "string", "required": true, "key": true}, {"name": "subnet", "type": "string", "required": true},
		{"name": "size", "type": "string"}, {"name": "nic", "type": "string"}, {"name": "existing", "type": "bool"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	h := newServer(t, dir, false, store)
	r := &fakeRunner{h: h}
	ws := "cs-" + strings.ToLower(newID()[:8])
	if rec := post(h, "/workspaces/new", url.Values{fieldWorkspace: {ws}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("create: %d", rec.Code)
	}
	// no configuration: no New run (it would fail), but why and where to add one
	if page := get(h, "/workspaces/"+ws).Body.String(); strings.Contains(page, `data-show-dialog="new-run"`) ||
		!strings.Contains(page, `href="/workspaces/`+ws+`/deploy">Deploy a template</a>`) {
		t.Fatalf("a workspace without configuration:\n%s", page)
	}
	add := "/workspaces/" + ws + "/templates/new?template=vm"
	failed := func(form url.Values, found string) (string, string) {
		t.Helper()
		id := runID(t, post(h, add, form))
		r.claim(t, id, "plan")
		r.finish(t, `{"status": "failed", "message": "plan: exit status 1"`+found+`}`)
		u := "/workspaces/" + ws + "/runs/" + id
		return id, u
	}

	// an import round that fails: still Change settings, back to the form importing, with what was entered
	id, u := failed(url.Values{"name": {"vm1"}, "existing": {"on"}, "nic": {"typo"}}, "")
	if page := get(h, u).Body.String(); !strings.Contains(page, `action="`+u+`/change"`) {
		t.Fatalf("a failed round: no Change settings:\n%s", page)
	}
	loc := post(h, u+"/change", nil).Header().Get("Location")
	if !strings.Contains(loc, "found="+id) {
		t.Fatalf("change after a failed round: %q", loc)
	}
	if page := get(h, loc).Body.String(); !strings.Contains(page, `value="typo"`) || !strings.Contains(page, `name="existing" value="on"`) {
		t.Fatalf("the form after a failed round:\n%s", page)
	}
	// a plan that fails creating: back to the form creating, not importing
	_, u = failed(url.Values{"name": {"vm1"}, "subnet": {"/subnets/x"}}, "")
	if page := get(h, u).Body.String(); !strings.Contains(page, "Not applied") {
		t.Fatalf("a failed create: no Not applied box:\n%s", page)
	}
	loc = post(h, u+"/change", nil).Header().Get("Location")
	if page := get(h, loc).Body.String(); !strings.Contains(page, `value="/subnets/x"`) || strings.Contains(page, `name="existing"`) {
		t.Fatalf("the form after a failed create: importing, or not what was entered:\n%s", page)
	}

	// one template applied, a second one's plan fails finding for both: a link per template
	first := runID(t, post(h, add, url.Values{"name": {"vm1"}, "subnet": {"/s"}}))
	r.applied(t, ws, first, 1)
	if page := get(h, "/workspaces/"+ws).Body.String(); !strings.Contains(page, `data-show-dialog="new-run"`) || strings.Contains(page, "Deploy a template</a>") {
		t.Fatalf("a workspace with a template: no New run:\n%s", page)
	}
	mods, _ := store.WorkspaceTemplates(context.Background(), mustWorkspace(t, store, ws).ID)
	if len(mods) != 1 {
		t.Fatalf("templates: %+v", mods)
	}
	second := runID(t, post(h, add, url.Values{"name": {"vm2"}, "subnet": {"/s"}}))
	two := r.claim(t, second, "plan").Modules
	r.finish(t, `{"status": "failed", "message": "x", "found": {"`+two[0].Name+`": {"size": "a"}, "`+two[1].Name+`": {"size": "b"}}}`)
	u = "/workspaces/" + ws + "/runs/" + second
	page := get(h, u).Body.String()
	if strings.Contains(page, `action="`+u+`/change"`) || !strings.Contains(page, "/templates/"+mods[0].Name+"?found="+second) ||
		!strings.Contains(page, "found="+second+"&amp;from=") {
		t.Fatalf("a run with two templates: no link per template:\n%s", page)
	}
}
