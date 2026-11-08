package app

import (
	"context"
	"errors"
	"maps"
	"net/url"
	"testing"

	"github.com/hashicorp/terraform-config-inspect/tfconfig"

	"github.com/opsolete/opsolete/internal/registry"
	"github.com/opsolete/opsolete/internal/storage"
	"github.com/opsolete/opsolete/internal/templates"
)

// TestWithWorkspaceVars: an added template gets the workspace's Terraform variables its registry
// module declares, under its own inputs; other sources and undeclared variables are left alone.
func TestWithWorkspaceVars(t *testing.T) {
	a, ctx := testApp(t), context.Background()
	u, _ := url.Parse("https://ops.example.com")
	tpls, err := templates.Open("../../catalog")
	if err != nil {
		t.Fatal(err)
	}
	a.Registry, a.Templates, a.RegistryEnabled = &registry.Registry{PublicURL: u, Key: []byte("test-key")}, tpls, true
	ns := uniq("wv")
	t.Cleanup(func() { a.Store.Pool.Exec(context.Background(), `DELETE FROM modules WHERE namespace = $1`, ns) })
	decl := []registry.Variable{{Variable: tfconfig.Variable{Name: "greeting"}}, {Variable: tfconfig.Variable{Name: "name"}}}
	if err := a.Store.CreateModule(ctx, storage.Module{Namespace: ns, Name: "m", System: "null", Version: "1.0.0", Variables: decl},
		storage.ModuleDocs{}, []byte("x")); err != nil {
		t.Fatal(err)
	}
	src := u.Host + "/" + ns + "/m/null"
	vars := map[string]any{"greeting": "from-set", "name": "from-set", "region": "undeclared"}
	got, err := a.withWorkspaceVars(ctx, "", src, "1.0.0", map[string]any{"name": "own"}, vars)
	if want := map[string]any{"greeting": "from-set", "name": "own"}; err != nil || !maps.Equal(got, want) {
		t.Fatalf("registry module: %v %v", got, err)
	}
	// the greeting template asks both: unset (hidden or left empty), they keep the module's default
	if got, err := a.withWorkspaceVars(ctx, "greeting", src, "1.0.0", map[string]any{}, vars); err != nil || len(got) != 0 {
		t.Fatalf("inputs the template asks: %v %v", got, err)
	}
	if got, err := a.withWorkspaceVars(ctx, "", "git::https://example.com/m.git", "v1", nil, vars); err != nil || got != nil {
		t.Fatalf("external source: %v %v", got, err)
	}
}

// Workspace variables: operate access; variable sets: admins only. Sensitive values are never returned.
func TestVariableAccess(t *testing.T) {
	a, ctx := testApp(t), context.Background()
	group := uniq("grp-")
	owner, reader, stranger := user(t, a), user(t, a, group), user(t, a)
	ws := emptyWorkspace(t, a, owner)
	if err := a.SetGrant(ctx, admin, ws.Name, group, "read"); err != nil {
		t.Fatal(err)
	}
	secret := VariableInput{Category: "env", Key: "ARM_CLIENT_SECRET", Value: "s3cret", Sensitive: true}
	if err := a.SetWorkspaceVariable(ctx, reader, ws.Name, secret); !errors.Is(err, ErrForbidden) {
		t.Errorf("a reader sets: %v", err)
	}
	if err := a.SetWorkspaceVariable(ctx, stranger, ws.Name, secret); !errors.Is(err, ErrNotFound) {
		t.Errorf("a stranger sets: %v", err)
	}
	if err := a.SetWorkspaceVariable(ctx, owner, ws.Name, VariableInput{Category: "env", Key: "PATH"}); !isValidation(err) {
		t.Errorf("a reserved name: %v", err)
	}
	if err := a.SetWorkspaceVariable(ctx, owner, ws.Name, secret); err != nil {
		t.Fatal(err)
	}
	if err := a.SetWorkspaceVariable(ctx, owner, ws.Name, VariableInput{Category: "terraform", Key: "region", Value: "westeurope"}); err != nil {
		t.Fatal(err)
	}

	set := uniq("set-")
	t.Cleanup(func() { a.Store.Pool.Exec(context.Background(), `DELETE FROM variable_sets WHERE name = $1`, set) })
	if err := a.CreateVariableSet(ctx, owner, NewVariableSet{Name: set}); !errors.Is(err, ErrForbidden) {
		t.Errorf("an operator creates a set: %v", err)
	}
	if err := a.CreateVariableSet(ctx, admin, NewVariableSet{Name: set, Workspace: ws.Name}); err != nil {
		t.Fatal(err)
	}
	err := a.CreateVariableSet(ctx, admin, NewVariableSet{Name: set})
	if ve, ok := errors.AsType[*ValidationError](err); !ok || ve.Fields["name"] == "" {
		t.Errorf("a taken set name: %v", err)
	}
	if err := a.SetSetVariable(ctx, owner, set, secret); !errors.Is(err, ErrForbidden) {
		t.Errorf("an operator sets a set's variable: %v", err)
	}
	if err := a.SelectSetWorkspaces(ctx, owner, set, nil); !errors.Is(err, ErrForbidden) {
		t.Errorf("an operator detaches a set: %v", err)
	}
	if err := a.SetSetVariable(ctx, admin, set, VariableInput{Category: "env", Key: "ARM_SET_SECRET", Value: "set-s3cret", Sensitive: true}); err != nil {
		t.Fatal(err)
	}

	wv, err := a.WorkspaceVariables(ctx, reader, ws.Name)
	if err != nil {
		t.Fatal(err)
	}
	if len(wv.Own) != 2 || len(wv.Sets) != 1 || len(wv.Sets[0].Vars) != 1 {
		t.Fatalf("variables: %+v", wv)
	}
	all := append(wv.Own, wv.Sets[0].Vars...)
	if sv, err := a.VariableSet(ctx, admin, set); err != nil || len(sv.Vars) != 1 {
		t.Fatalf("the set: %+v %v", sv, err)
	} else {
		all = append(all, sv.Vars...)
	}
	for _, v := range all {
		if v.Sensitive != (v.Value == "") || v.Key == "region" && v.Value != "westeurope" {
			t.Errorf("%s: value %q (sensitive %v)", v.Key, v.Value, v.Sensitive)
		}
	}
	if _, err := a.WorkspaceVariables(ctx, stranger, ws.Name); !errors.Is(err, ErrNotFound) {
		t.Errorf("a stranger lists: %v", err)
	}
	if err := a.DeleteWorkspaceVariable(ctx, reader, ws.Name, wv.Own[0].ID); !errors.Is(err, ErrForbidden) {
		t.Errorf("a reader deletes: %v", err)
	}
}
