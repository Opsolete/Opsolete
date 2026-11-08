package app

import (
	"context"
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
