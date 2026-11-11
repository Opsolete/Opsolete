package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/opsolete/opsolete/internal/runnerapi"
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
	added, err := a.Store.WorkspaceTemplates(ctx, ws.ID)
	if err != nil || len(added) != 1 {
		t.Fatal(added, err)
	}
	if in := added[0].Inputs; in["name"] != "vm1" || in["existing"] != true {
		t.Fatalf("stored: %v", in)
	} else if v, ok := in["subnet"]; !ok || v != nil {
		t.Fatalf("subnet stored as %v (%v), want null", v, ok)
	}

	// the plan imports and finds
	j, ok, err := a.Store.ClaimJob(ctx, "r", time.Minute)
	if err != nil || !ok || j.RunID != run.ID {
		t.Fatalf("claim: %v %v", ok, err)
	}
	if err := a.Store.AckJob(ctx, j.RunID, j.LeaseID, time.Minute); err != nil {
		t.Fatal(err)
	}
	mod := added[0].Name
	if _, err := a.FinishJob(ctx, run.ID, j.LeaseID, runnerapi.FinishRequest{Status: "succeeded",
		Plan:  &runnerapi.Plan{Import: 1, Imports: []runnerapi.Import{{To: "module." + mod + ".x.y", ID: "/vm1"}}, Change: 1},
		Found: map[string]map[string]any{mod: {"subnet": "/subnets/a", "size": "large", "password": "x", "other": "y"}}}); err != nil {
		t.Fatal(err)
	}
	tpl, _ := a.Template(ctx, "vm")
	found, err := a.FoundValues(ctx, admin, ws.Name, run.ID, mod, tpl)
	if err != nil || len(found) != 2 || found["subnet"] != "/subnets/a" || found["size"] != "large" {
		t.Fatalf("found: %v %v", found, err)
	}
	if _, err := a.FoundValues(ctx, user(t, a), ws.Name, run.ID, mod, tpl); !errors.Is(err, ErrNotFound) {
		t.Errorf("found, for a stranger: %v", err)
	}
	if _, err := a.FoundValues(ctx, admin, ws.Name, run.ID, "other", tpl); !errors.Is(err, ErrNotFound) {
		t.Errorf("found, for another module: %v", err)
	}

	d, err := a.Run(ctx, admin, ws.Name, run.ID, LevelOperate)
	if err != nil {
		t.Fatal(err)
	}
	if ft, err := a.FoundTemplates(ctx, d); err != nil || len(ft) != 1 || ft[0] != (FoundTemplate{mod, "vm", true}) {
		t.Fatalf("found templates: %+v %v", ft, err)
	}

	// discarded: a workspace keeps the template it was created with; configured with the found values, a
	// project's keep-own subnet doesn't replace the found one (the workspace's own)
	if err := a.DiscardRun(ctx, admin, ws.Name, run.ID); err != nil {
		t.Fatal(err)
	}
	if err := a.SetEnvironmentValue(ctx, admin, prd, "vm", "subnet", "/subnets/project", true); err != nil {
		t.Fatal(err)
	}
	raw := map[string]string{"name": "vm1", "existing": "on", "subnet": "/subnets/a", "size": "large"}
	if _, run, err = a.SaveTemplate(ctx, admin, ws.Name, mod, "", raw, found); err != nil {
		t.Fatal(err)
	}
	added, _ = a.Store.WorkspaceTemplates(ctx, ws.ID)
	if in := added[0].Inputs; in["subnet"] != "/subnets/a" || in["size"] != "large" {
		t.Fatalf("configured with found values: %v", in)
	}

	// a template added by an import plan that is discarded is gone; its found values stay, to add it again with
	if _, err := a.CancelRun(ctx, admin, ws.Name, run.ID, false); err != nil {
		t.Fatal(err)
	}
	two, run, err := a.SaveTemplate(ctx, admin, ws.Name, "", "vm", map[string]string{"name": "vm2", "existing": "on"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	j, _, _ = a.Store.ClaimJob(ctx, "r", time.Minute)
	a.Store.AckJob(ctx, j.RunID, j.LeaseID, time.Minute)
	if _, err := a.FinishJob(ctx, run.ID, j.LeaseID, runnerapi.FinishRequest{Status: "succeeded",
		Plan:  &runnerapi.Plan{Import: 1, Imports: []runnerapi.Import{{To: "module." + two + ".x.y", ID: "/vm2"}}, Change: 1},
		Found: map[string]map[string]any{two: {"subnet": "/subnets/b"}}}); err != nil {
		t.Fatal(err)
	}
	if err := a.DiscardRun(ctx, admin, ws.Name, run.ID); err != nil {
		t.Fatal(err)
	}
	d, _ = a.Run(ctx, admin, ws.Name, run.ID, LevelOperate)
	if ft, err := a.FoundTemplates(ctx, d); err != nil || len(ft) != 1 || ft[0] != (FoundTemplate{two, "vm", false}) {
		t.Fatalf("found templates after discarding an added one: %+v %v", ft, err)
	}
	if found, err := a.FoundValues(ctx, admin, ws.Name, run.ID, two, tpl); err != nil || found["subnet"] != "/subnets/b" {
		t.Fatalf("found of a removed template: %v %v", found, err)
	}
}
