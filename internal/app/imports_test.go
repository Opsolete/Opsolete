package app

import (
	"context"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/opsolete/opsolete/internal/registry"
	"github.com/opsolete/opsolete/internal/runnerapi"
	"github.com/opsolete/opsolete/internal/storage"
	"github.com/opsolete/opsolete/internal/templates"
)

// importApp is testApp with a catalog of one template "vm" whose module imports what exists (existing): name
// is its key, subnet required and per environment, password sensitive.
func importApp(t *testing.T) *App {
	a := testApp(t)
	dir := t.TempDir()
	tpl := `{"id": "vm", "name": "VM", "module": {"source": "git::https://x//m", "ref": "v1"}, "inputs": [
		{"name": "name", "type": "string", "required": true, "key": true},
		{"name": "subnet", "type": "string", "required": true, "per_environment": true},
		{"name": "size", "type": "string", "default": "small"},
		{"name": "password", "type": "string", "sensitive": true},
		{"name": "existing", "type": "bool"}]}`
	if err := os.WriteFile(filepath.Join(dir, "vm.json"), []byte(tpl), 0o600); err != nil {
		t.Fatal(err)
	}
	var err error
	if a.Templates, err = templates.Open(dir); err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse("https://ops.example.com")
	a.Registry = &registry.Registry{PublicURL: u, Key: []byte("test-key")}
	return a
}

// Importing (existing on): only the key input is required, a required one left empty is null; the plan's found
// values are offered to operators, sensitive ones never, and count as the workspace's own against a project's
// keep-own value.
func TestImport(t *testing.T) {
	a, ctx := importApp(t), context.Background()
	prd := uniq("prd-")
	if _, err := a.CreateProject(ctx, admin, prd, ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.CreateWorkspace(ctx, admin, NewWorkspace{Name: uniq("vm-"), Project: prd, TemplateID: "vm",
		Inputs: map[string]string{"name": "vm1"}}); !isValidation(err) {
		t.Fatalf("not importing, subnet empty: %v", err)
	}
	if _, _, err := a.CreateWorkspace(ctx, admin, NewWorkspace{Name: uniq("vm-"), Project: prd, TemplateID: "vm",
		Inputs: map[string]string{"existing": "on"}}); !isValidation(err) {
		t.Fatalf("importing, the key empty: %v", err)
	}
	ws, run, err := a.CreateWorkspace(ctx, admin, NewWorkspace{Name: uniq("vm-"), Project: prd, TemplateID: "vm",
		Inputs: map[string]string{"name": "vm1", "existing": "on"}})
	if err != nil {
		t.Fatalf("importing with the key only: %v", err)
	}
	if run.Action != "plan" {
		t.Fatalf("an import's first round: %s, want a plan", run.Action)
	}
	added, err := a.Store.WorkspaceTemplates(ctx, ws.ID)
	if err != nil || len(added) != 1 {
		t.Fatal(added, err)
	}
	if in := added[0].Inputs; in["name"] != "vm1" || !added[0].Import {
		t.Fatalf("stored: %v, import %v", in, added[0].Import)
	} else if _, stored := in["existing"]; stored {
		t.Fatalf("the import switch stored as an input: %v", in)
	} else if v, ok := in["subnet"]; !ok || v != nil {
		t.Fatalf("subnet stored as %v (%v), want null", v, ok)
	}
	mod := added[0].Name
	claim := func(id uuid.UUID) storage.Job {
		t.Helper()
		j, ok, err := a.Store.ClaimJob(ctx, storage.Runner{Name: "r", Pool: storage.DefaultPool}, time.Minute)
		if err != nil || !ok || j.RunID != id {
			t.Fatalf("claim %s: %+v %v %v", id, j, ok, err)
		}
		if err := a.Store.AckJob(ctx, j.RunID, j.LeaseID, time.Minute); err != nil {
			t.Fatal(err)
		}
		return j
	}
	finish := func(id uuid.UUID, j storage.Job, res runnerapi.FinishRequest) string {
		t.Helper()
		st, err := a.FinishJob(ctx, id, j.LeaseID, res)
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
	imports := []runnerapi.Import{{To: "module." + mod + ".x.y", ID: "/vm1"}}

	// round 1: the runner gets the switch on; the plan imports and finds (a map behind the name input doesn't fit it)
	j := claim(run.ID)
	body, err := a.assembleJob(ctx, j)
	if err != nil || !strings.Contains(string(body), `"existing":true`) {
		t.Fatalf("job: %s %v", body, err)
	}
	if st := finish(run.ID, j, runnerapi.FinishRequest{Status: "succeeded", Plan: &runnerapi.Plan{Import: 1, Imports: imports, Change: 1},
		Found: map[string]map[string]any{mod: {"subnet": "/subnets/a", "size": "large", "password": "x", "name": map[string]any{"a": 1.0}}}}); st != storage.Planned {
		t.Fatalf("round 1: %s", st)
	}
	tpl, _ := a.Template(ctx, "vm")
	found, err := a.FoundValues(ctx, admin, ws.Name, run.ID, mod, tpl)
	if err != nil || found["subnet"] != "/subnets/a" || found["size"] != "large" || found["name"] != "vm1" || found["existing"] != true ||
		found["password"] != nil {
		t.Fatalf("found (over the inputs the plan ran with, sensitive never, a value that doesn't fit its input never): %v %v", found, err)
	}
	if _, err := a.FoundValues(ctx, user(t, a), ws.Name, run.ID, mod, tpl); !errors.Is(err, ErrNotFound) {
		t.Errorf("found, for a stranger: %v", err)
	}
	s, err := a.ImportRound(ctx, admin, ws.Name, run.ID)
	if err != nil || !s.CanReplan || !s.CanAdopt || len(s.Modules) != 1 {
		t.Fatalf("round 1: %+v %v", s, err)
	}

	// round 2: planned again with the found values; a project's keep-own subnet doesn't replace the found one
	if err := a.SetEnvironmentValue(ctx, admin, prd, "vm", "subnet", "/subnets/project", true); err != nil {
		t.Fatal(err)
	}
	round2, err := a.PlanImportAgain(ctx, admin, ws.Name, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	mods, _ := a.Store.RunTemplates(ctx, round2.ID)
	if len(mods) != 1 || mods[0].Inputs["subnet"] != "/subnets/a" || mods[0].Inputs["size"] != "large" || !mods[0].Import {
		t.Fatalf("round 2's version: %+v", mods)
	}
	if _, err := a.AdoptImport(ctx, admin, ws.Name, run.ID); !IsConflict(err) {
		t.Fatalf("adopting an older round: %v", err)
	}
	j = claim(round2.ID)
	finish(round2.ID, j, runnerapi.FinishRequest{Status: "succeeded", Plan: &runnerapi.Plan{Import: 1, Imports: imports},
		Found: map[string]map[string]any{mod: {"subnet": "/subnets/a"}}})
	if s, err := a.ImportRound(ctx, admin, ws.Name, round2.ID); err != nil || s.CanReplan || !s.CanAdopt {
		t.Fatalf("round 2: %+v %v", s, err)
	}

	// adopted: an apply run of exactly round 2's version, which asks; applied, the module stops importing
	apply, err := a.AdoptImport(ctx, admin, ws.Name, round2.ID)
	if err != nil || apply.Action != "apply" {
		t.Fatalf("adopt: %+v %v", apply, err)
	}
	if m, _ := a.Store.RunTemplates(ctx, apply.ID); len(m) != 1 || m[0].Inputs["subnet"] != "/subnets/a" || !m[0].Import {
		t.Fatalf("the apply's version: %+v", m)
	}
	j = claim(apply.ID)
	if st := finish(apply.ID, j, runnerapi.FinishRequest{Status: "succeeded", Plan: &runnerapi.Plan{Import: 1, Imports: imports}}); st != storage.NeedsConfirmation {
		t.Fatalf("the apply's plan, auto-apply or not: %s", st)
	}
	if r, _, _ := a.Store.GetRun(ctx, apply.ID); r.ConfirmReason != storage.ReasonImport {
		t.Fatalf("reason: %q", r.ConfirmReason)
	}
	if err := a.ConfirmRun(ctx, admin, ws.Name, apply.ID); err != nil {
		t.Fatal(err)
	}
	j = claim(apply.ID)
	if st := finish(apply.ID, j, runnerapi.FinishRequest{Status: "succeeded", Resources: []runnerapi.Resource{}}); st != storage.Applied {
		t.Fatalf("apply: %s", st)
	}
	if added, _ = a.Store.WorkspaceTemplates(ctx, ws.ID); len(added) != 1 || added[0].Import || added[0].Inputs["subnet"] != "/subnets/a" {
		t.Fatalf("after the import: %+v", added)
	}

	// a round whose plan would replace something can't be adopted; adding one more template plans it, never applies it
	two, run3, err := a.SaveTemplate(ctx, admin, ws.Name, "", "vm", map[string]string{"name": "vm2", "existing": "on"}, nil)
	if err != nil || run3.Action != "plan" {
		t.Fatalf("add importing: %v %v", run3, err)
	}
	j = claim(run3.ID)
	finish(run3.ID, j, runnerapi.FinishRequest{Status: "succeeded", Plan: &runnerapi.Plan{Import: 1, Imports: []runnerapi.Import{{To: "module." + two + ".x.y", ID: "/vm2"}}, Add: 1, Destroy: 1}})
	if s, err := a.ImportRound(ctx, admin, ws.Name, run3.ID); err != nil || s.CanAdopt || !strings.Contains(s.Why, "replace or delete") {
		t.Fatalf("a replacing round: %+v %v", s, err)
	}
	if added, _ = a.Store.WorkspaceTemplates(ctx, ws.ID); len(added) != 1 {
		t.Fatalf("a planned round added the template: %+v", added)
	}
}

// An input the project sets (per environment, not keep-own) can't take what an import plan found: it shows why,
// and it never asks for another round (planning again would set it back to the project's value: a loop).
func TestImportProjectValue(t *testing.T) {
	a, ctx := importApp(t), context.Background()
	prd := uniq("prd-")
	if _, err := a.CreateProject(ctx, admin, prd, ""); err != nil {
		t.Fatal(err)
	}
	if err := a.SetEnvironmentValue(ctx, admin, prd, "vm", "subnet", "/subnets/project", false); err != nil {
		t.Fatal(err)
	}
	ws, run, err := a.CreateWorkspace(ctx, admin, NewWorkspace{Name: uniq("vm-"), Project: prd, TemplateID: "vm",
		Inputs: map[string]string{"name": "vm1", "existing": "on"}})
	if err != nil {
		t.Fatal(err)
	}
	added, _ := a.Store.WorkspaceTemplates(ctx, ws.ID)
	mod := added[0].Name
	j, ok, err := a.Store.ClaimJob(ctx, storage.Runner{Name: "r", Pool: storage.DefaultPool}, time.Minute)
	if err != nil || !ok {
		t.Fatal(err)
	}
	a.Store.AckJob(ctx, j.RunID, j.LeaseID, time.Minute)
	if _, err := a.FinishJob(ctx, run.ID, j.LeaseID, runnerapi.FinishRequest{Status: "succeeded",
		Plan:  &runnerapi.Plan{Import: 1, Imports: []runnerapi.Import{{To: "module." + mod + ".x.y", ID: "/vm1"}}, Change: 1},
		Found: map[string]map[string]any{mod: {"subnet": "/subnets/a"}}}); err != nil {
		t.Fatal(err)
	}
	s, err := a.ImportRound(ctx, admin, ws.Name, run.ID)
	if err != nil || s.CanReplan || len(s.Modules) != 1 || len(s.Modules[0].Values) != 1 || s.Modules[0].Values[0].Fixed != "set by the project" {
		t.Fatalf("round: %+v %v", s, err)
	}
	if _, err := a.PlanImportAgain(ctx, admin, ws.Name, run.ID); !IsConflict(err) {
		t.Fatalf("planning again with nothing it can take: %v", err)
	}
}
