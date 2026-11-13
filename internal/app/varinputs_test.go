package app

import (
	"context"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/hashicorp/terraform-config-inspect/tfconfig"

	"github.com/opsolete/opsolete/internal/registry"
	"github.com/opsolete/opsolete/internal/storage"
	"github.com/opsolete/opsolete/internal/templates"
)

// TestVarsFillUnlistedInputs: a registry module's variable the template's JSON doesn't list is filled by a Terraform
// variable the workspace gets (here a set of its project): the form doesn't ask it (not required, a posted value
// ignored, nothing stored) and the run gets the variable. Without such a variable it's asked as before, and an input
// the template lists is never filled.
func TestVarsFillUnlistedInputs(t *testing.T) {
	a, ctx := testApp(t), context.Background()
	u, _ := url.Parse("https://ops.example.com")
	a.Registry, a.RegistryEnabled = &registry.Registry{PublicURL: u, Key: []byte("test-key")}, true
	ns := uniq("vi")
	decl := []registry.Variable{{Variable: tfconfig.Variable{Name: "name", Type: "string", Required: true}},
		{Variable: tfconfig.Variable{Name: "vnet", Type: "string", Required: true}},
		{Variable: tfconfig.Variable{Name: "size", Type: "string", Required: true}}}
	if err := a.Store.CreateModule(ctx, storage.Module{Namespace: ns, Name: "m", System: "azurerm", Version: "1.0.0", Variables: decl},
		storage.ModuleDocs{}, []byte("x")); err != nil {
		t.Fatal(err)
	}
	src := u.Host + "/" + ns + "/m/azurerm"
	dir := t.TempDir()
	tpl := `{"id": "sa", "name": "SA", "module": {"source": "` + src + `", "ref": "1.0.0"}, "inputs": [
		{"name": "name", "type": "string", "required": true, "key": true}, {"name": "size", "type": "string", "required": true}]}`
	if err := os.WriteFile(filepath.Join(dir, "sa.json"), []byte(tpl), 0o600); err != nil {
		t.Fatal(err)
	}
	var err error
	if a.Templates, err = templates.Open(dir); err != nil {
		t.Fatal(err)
	}
	sub, set := uniq("sub-"), uniq("net-")
	if _, err := a.CreateProject(ctx, admin, sub, ""); err != nil {
		t.Fatal(err)
	}
	if err := a.CreateVariableSet(ctx, admin, NewVariableSet{Name: set, Project: sub}); err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{"vnet": "VNET", "size": "from-set"} {
		if err := a.SetSetVariable(ctx, admin, set, VariableInput{Category: "terraform", Key: k, Value: v}); err != nil {
			t.Fatal(err)
		}
	}
	tt, err := a.Template(ctx, "sa")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := a.VarInputs(ctx, tt, uuid.Nil, sub); err != nil || !maps.Equal(got, map[string]string{"vnet": "VNET"}) {
		t.Fatalf("what the project's set fills: %v %v", got, err)
	}

	ws, _, err := a.CreateWorkspace(ctx, admin, NewWorkspace{Name: uniq("sa-"), Project: sub, TemplateID: "sa",
		Inputs: map[string]string{"name": "a", "size": "s", "vnet": "posted"}})
	if err != nil {
		t.Fatalf("vnet left to the set: %v", err)
	}
	added, _ := a.Store.WorkspaceTemplates(ctx, ws.ID)
	if _, stored := added[0].Inputs["vnet"]; stored {
		t.Fatalf("a filled input was stored: %v", added[0].Inputs)
	}
	if got, err := a.VarInputs(ctx, tt, ws.ID, ""); err != nil || !maps.Equal(got, map[string]string{"vnet": "VNET"}) {
		t.Fatalf("what the workspace's variables fill: %v %v", got, err)
	}
	_, vars, _, err := a.runVariables(ctx, ws.ID)
	if err != nil {
		t.Fatal(err)
	}
	in, err := a.withWorkspaceVars(ctx, tt, src, "1.0.0", added[0].Inputs, vars)
	if want := map[string]any{"name": "a", "size": "s", "vnet": "VNET"}; err != nil || !maps.Equal(in, want) {
		t.Fatalf("the run's inputs: %v %v", in, err)
	}

	// a project without the set: vnet is asked as before
	other := uniq("other-")
	if _, err := a.CreateProject(ctx, admin, other, ""); err != nil {
		t.Fatal(err)
	}
	_, _, err = a.CreateWorkspace(ctx, admin, NewWorkspace{Name: uniq("sa-"), Project: other, TemplateID: "sa",
		Inputs: map[string]string{"name": "b", "size": "s"}})
	if ve, ok := err.(*ValidationError); !ok || ve.Fields["vnet"] == "" {
		t.Fatalf("vnet without a variable: %v", err)
	}
}
