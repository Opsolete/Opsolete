package app

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/opsolete/opsolete/internal/registry"
	"github.com/opsolete/opsolete/internal/runnerapi"
	"github.com/opsolete/opsolete/internal/storage"
	"github.com/opsolete/opsolete/internal/templates"
)

// envApp is testApp with a catalog of one template "env": size (select) and region (pattern) per
// environment, name not; ip per environment, shown only when public.
func envApp(t *testing.T) *App {
	a := testApp(t)
	dir := t.TempDir()
	tpl := `{"id": "env", "name": "Env", "module": {"source": "git::https://x//m", "ref": "v1"}, "inputs": [
		{"name": "size", "type": "select", "options": ["s", "m", "l"], "per_environment": true},
		{"name": "region", "type": "string", "pattern": "[a-z]+", "per_environment": true},
		{"name": "name", "type": "string"},
		{"name": "public", "type": "bool"},
		{"name": "ip", "type": "string", "per_environment": true, "show_if": {"input": "public", "equals": [true]}}]}`
	if err := os.WriteFile(filepath.Join(dir, "env.json"), []byte(tpl), 0o600); err != nil {
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

// Who sets a project's values, and by which rules.
func TestEnvironmentValueAccess(t *testing.T) {
	a, ctx := envApp(t), context.Background()
	ops, rd := uniq("ops-"), uniq("rd-")
	operator, reader, stranger := user(t, a, ops), user(t, a, rd), user(t, a)
	prd := uniq("prd-")
	if _, err := a.CreateProject(ctx, admin, prd, ""); err != nil {
		t.Fatal(err)
	}
	for g, l := range map[string]string{ops: "operate", rd: "read"} {
		if err := a.SetProjectGrant(ctx, admin, prd, g, l); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		name  string
		who   Actor
		input string
		value string
		ok    func(error) bool
	}{
		{"admin", admin, "size", "l", func(err error) bool { return err == nil }},
		{"operator", operator, "region", "west", func(err error) bool { return err == nil }},
		{"reader", reader, "size", "m", func(err error) bool { return errors.Is(err, ErrForbidden) }},
		{"stranger", stranger, "size", "m", func(err error) bool { return errors.Is(err, ErrNotFound) }},
		{"not an option", admin, "size", "xl", isValidation},
		{"pattern", admin, "region", "West 1", isValidation},
		{"empty", admin, "region", " ", isValidation},
		{"not per environment", admin, "name", "x", func(err error) bool { return errors.Is(err, ErrNotFound) }},
	} {
		if err := a.SetEnvironmentValue(ctx, c.who, prd, "env", c.input, c.value, false); !c.ok(err) {
			t.Errorf("%s: %v", c.name, err)
		}
	}
	if err := a.ClearEnvironmentValue(ctx, reader, prd, "env", "size"); !errors.Is(err, ErrForbidden) {
		t.Errorf("a reader clears: %v", err)
	}
	v, err := a.EnvironmentValues(ctx, reader, prd)
	if err != nil || v.CanEdit || len(v.Templates) != 1 || len(v.Templates[0].Inputs) != 3 {
		t.Fatalf("reader's page: %+v %v", v, err)
	}
	if in := v.Templates[0].Inputs; in[0].Value != "l" || in[1].Value != "west" || in[1].UpdatedBy != operator.Email {
		t.Errorf("values: %+v", in)
	}
	if err := a.ClearEnvironmentValue(ctx, operator, prd, "env", "size"); err != nil {
		t.Fatal(err)
	}
	if v, _ := a.EnvironmentValues(ctx, operator, prd); !v.CanEdit || v.Templates[0].Inputs[0].Set {
		t.Errorf("operator's page after clearing: %+v", v)
	}
	if _, err := a.EnvironmentValues(ctx, stranger, prd); !errors.Is(err, ErrNotFound) {
		t.Errorf("a stranger reads: %v", err)
	}
}

// A job gets the workspace's current project's values over its own; the workspace's stored
// configuration never holds them.
func TestEnvironmentValuesInJobs(t *testing.T) {
	a, ctx := envApp(t), context.Background()
	acc, prd := uniq("acc-"), uniq("prd-")
	for _, p := range []string{acc, prd} {
		if _, err := a.CreateProject(ctx, admin, p, ""); err != nil {
			t.Fatal(err)
		}
	}
	set := func(p, input, v string) {
		t.Helper()
		if err := a.SetEnvironmentValue(ctx, admin, p, "env", input, v, false); err != nil {
			t.Fatal(err)
		}
	}
	set(prd, "size", "m")
	set(prd, "region", "prd")
	ws, _, err := a.CreateWorkspace(ctx, admin, NewWorkspace{Name: uniq("env-"), Project: acc, TemplateID: "env",
		Inputs: map[string]string{"size": "s", "region": "own", "name": "n"}})
	if err != nil {
		t.Fatal(err)
	}
	j, ok, err := a.Store.ClaimJob(ctx, storage.Runner{Name: "r", Pool: storage.DefaultPool}, time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	inputs := func(what string, want map[string]any) {
		t.Helper()
		body, err := a.assembleJob(ctx, j)
		var job runnerapi.Job
		if err == nil {
			err = json.Unmarshal(body, &job)
		}
		if err != nil || len(job.Modules) != 1 {
			t.Fatalf("%s: %v", what, err)
		}
		for k, v := range want {
			if job.Modules[0].Inputs[k] != v {
				t.Errorf("%s: %s = %v, want %v", what, k, job.Modules[0].Inputs[k], v)
			}
		}
	}
	inputs("no project value", map[string]any{"size": "s", "region": "own", "name": "n"})
	set(acc, "size", "l")
	inputs("the project's value", map[string]any{"size": "l", "region": "own"})
	if err := a.MoveWorkspace(ctx, admin, ws.Name, prd); err != nil {
		t.Fatal(err)
	}
	inputs("moved: the new project's values", map[string]any{"size": "m", "region": "prd", "name": "n"})
	if err := a.ClearEnvironmentValue(ctx, admin, prd, "env", "size"); err != nil {
		t.Fatal(err)
	}
	inputs("cleared: the workspace's own again", map[string]any{"size": "s", "region": "prd"})
	if err := a.SetEnvironmentValue(ctx, admin, prd, "env", "size", "m", true); err != nil {
		t.Fatal(err)
	}
	inputs("keep own: the workspace's own value", map[string]any{"size": "s", "region": "prd"})
	if err := a.ClearEnvironmentValue(ctx, admin, prd, "env", "size"); err != nil {
		t.Fatal(err)
	}

	// configure: a posted value for an input the project sets is ignored, the workspace's own kept
	if _, err := a.Store.Pool.Exec(ctx, `UPDATE runs SET status = 'applied', lease_id = NULL, lease_until = NULL WHERE workspace_id = $1`, ws.ID); err != nil {
		t.Fatal(err)
	}
	added, err := a.Store.WorkspaceTemplates(ctx, ws.ID)
	if err != nil || len(added) != 1 {
		t.Fatal(added, err)
	}
	_, run, err := a.SaveTemplate(ctx, admin, ws.Name, added[0].Name, "", map[string]string{"size": "l", "region": "Not Valid", "name": "n2"}, nil)
	if err != nil {
		t.Fatalf("save with a locked input: %v", err)
	}
	stored := func(what string, want map[string]any) {
		t.Helper()
		added, _ := a.Store.WorkspaceTemplates(ctx, ws.ID)
		for k, v := range want {
			if added[0].Inputs[k] != v {
				t.Errorf("%s: stored %s = %v, want %v", what, k, added[0].Inputs[k], v)
			}
		}
	}
	stored("configured: region kept, the rest as posted", map[string]any{"size": "l", "region": "own", "name": "n2"})
	// the run's config restore puts back the workspace's own values, untouched by the project's
	if _, err := a.CancelRun(ctx, admin, ws.Name, run.ID, false); err != nil {
		t.Fatal(err)
	}
	stored("restored", map[string]any{"size": "s", "region": "own", "name": "n"})

	// created in a project with values: those inputs get no value of their own
	ws2, _, err := a.CreateWorkspace(ctx, admin, NewWorkspace{Name: uniq("env-"), Project: prd, TemplateID: "env",
		Inputs: map[string]string{"size": "s", "region": "ignored", "name": "n"}})
	if err != nil {
		t.Fatal(err)
	}
	added, _ = a.Store.WorkspaceTemplates(ctx, ws2.ID)
	if in := added[0].Inputs; in["region"] != nil || in["size"] != "s" {
		t.Errorf("stored at creation: %v", in)
	}
}

// Locked: a keep_own value locks an input unless the workspace's own value (stored, or found when importing)
// differs; a plain project value locks it always.
func TestLockedKeepOwn(t *testing.T) {
	tpl := templates.Template{ID: "vm", Inputs: []templates.Input{{Name: "subnet", PerEnvironment: true}, {Name: "size", PerEnvironment: true}, {Name: "name"}}}
	env := EnvironmentMap{"vm": {"subnet": {Value: "prd", KeepOwn: true}, "size": {Value: "m"}, "name": {Value: "x"}}}
	for _, c := range []struct {
		own  map[string]any
		want map[string]string
	}{
		{nil, map[string]string{"subnet": "prd", "size": "m"}},
		{map[string]any{"subnet": "prd", "size": "s"}, map[string]string{"subnet": "prd", "size": "m"}},
		{map[string]any{"subnet": "imported", "size": "s"}, map[string]string{"size": "m"}},
	} {
		if got := env.Locked(tpl, c.own); !maps.Equal(got, c.want) {
			t.Errorf("own %v: %v, want %v", c.own, got, c.want)
		}
	}
}

// A project value isn't applied to an input the form hides (show_if), as the form wouldn't send it.
func TestEnvironmentValueShowIf(t *testing.T) {
	a, ctx := envApp(t), context.Background()
	env := EnvironmentMap{"env": {"ip": {Value: "10.0.0.1"}}}
	for public, want := range map[bool]any{false: nil, true: "10.0.0.1"} {
		got, err := a.withEnvironmentValues(ctx, "env", map[string]any{"public": public}, env)
		if _, set := got["ip"]; err != nil || got["ip"] != want || set != public {
			t.Errorf("public=%v: ip %v, want %v (%v)", public, got["ip"], want, err)
		}
	}
}
