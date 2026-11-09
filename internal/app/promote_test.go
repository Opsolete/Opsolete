package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/opsolete/opsolete/internal/runnerapi"
	"github.com/opsolete/opsolete/internal/storage"
	"github.com/opsolete/opsolete/internal/templates"
)

// promoteApp is testApp with templates web (size per environment, password sensitive), db and cache,
// all pinned to v2 in the catalog.
func promoteApp(t *testing.T) *App {
	a := testApp(t)
	dir := t.TempDir()
	for id, inputs := range map[string]string{
		"web": `{"name": "name", "type": "string", "required": true}, {"name": "opt", "type": "string"},
			{"name": "size", "type": "select", "options": ["s", "m", "l"], "required": true, "per_environment": true},
			{"name": "password", "type": "string", "required": true, "sensitive": true}`,
		"db":    `{"name": "tier", "type": "string"}`,
		"cache": `{"name": "x", "type": "string"}`,
	} {
		body := `{"id": "` + id + `", "name": "T ` + id + `", "module": {"source": "git::https://x//` + id + `", "ref": "v2"}, "inputs": [` + inputs + `]}`
		if err := os.WriteFile(filepath.Join(dir, id+".json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var err error
	if a.Templates, err = templates.Open(dir); err != nil {
		t.Fatal(err)
	}
	return a
}

// promoteFixture: source in project dev with web (v2) and db; target in project prd (auto-apply)
// with web at v1 (own size and password), "db" holding template cache, and cache: what step 2's
// decisions call update, replace and remove.
type promoteFixture struct {
	dev, prd     string // projects
	source, tgt  storage.Workspace
	tgtTemplates []storage.WorkspaceTemplate
}

func newPromoteFixture(t *testing.T, a *App) promoteFixture {
	ctx := context.Background()
	f := promoteFixture{dev: uniq("dev-"), prd: uniq("prd-")}
	for _, p := range []string{f.dev, f.prd} {
		if _, err := a.CreateProject(ctx, admin, p, ""); err != nil {
			t.Fatal(err)
		}
	}
	var err error
	if f.source, err = a.Store.CreateEmptyWorkspace(ctx, storage.EmptyWorkspace{Name: uniq("src-"), Project: f.dev}); err != nil {
		t.Fatal(err)
	}
	if f.tgt, err = a.Store.CreateEmptyWorkspace(ctx, storage.EmptyWorkspace{Name: uniq("tgt-"), Project: f.prd, AutoApply: true}); err != nil {
		t.Fatal(err)
	}
	insert := func(ws uuid.UUID, module, tpl, ref string, inputs map[string]any) {
		t.Helper()
		raw, _ := json.Marshal(inputs)
		if _, err := a.Store.Pool.Exec(ctx, `INSERT INTO workspace_templates (workspace_id, name, template_id, module_source, module_ref, inputs)
			VALUES ($1, $2, $3, 'git::https://x//'||$3, $4, $5)`, ws, module, tpl, ref, raw); err != nil {
			t.Fatal(err)
		}
	}
	insert(f.source.ID, "web", "web", "v2", map[string]any{"name": "app", "opt": "x", "size": "s", "password": seal(t, a, f.source.ID, "web", "password", "devpw")})
	insert(f.source.ID, "db", "db", "v2", map[string]any{"tier": "gold"})
	insert(f.tgt.ID, "web", "web", "v1", map[string]any{"name": "old", "size": "l", "password": seal(t, a, f.tgt.ID, "web", "password", "prdpw")})
	insert(f.tgt.ID, "db", "cache", "v1", map[string]any{"x": "1"})
	insert(f.tgt.ID, "cache", "cache", "v1", map[string]any{"x": "2"})
	if f.tgtTemplates, err = a.Store.WorkspaceTemplates(ctx, f.tgt.ID); err != nil {
		t.Fatal(err)
	}
	return f
}

func seal(t *testing.T, a *App, ws uuid.UUID, module, input, v string) map[string]any {
	plain, _ := json.Marshal(v)
	b, err := a.Secrets.Seal(string(plain), InputAD(ws, module, input))
	if err != nil {
		t.Fatal(err)
	}
	return map[string]any{"sealed": base64.StdEncoding.EncodeToString(b)}
}

// opened is the workspace's module's inputs, sealed ones opened.
func opened(t *testing.T, a *App, ws uuid.UUID, module string) map[string]any {
	t.Helper()
	list, err := a.Store.WorkspaceTemplates(context.Background(), ws)
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range list {
		if x.Name == module {
			in, _, err := a.OpenInputs(x.Inputs, ws, module)
			if err != nil {
				t.Fatal(err)
			}
			return in
		}
	}
	t.Fatalf("no module %s in %+v", module, list)
	return nil
}

func TestPromote(t *testing.T) {
	a, ctx := promoteApp(t), context.Background()
	f := newPromoteFixture(t, a)
	to := PromoteTarget{Name: f.tgt.Name}
	var p Promotion
	var run storage.Run

	step(t, "preview: update, replace, remove; per-environment and sensitive values kept", func(t *testing.T) {
		var err error
		if p, err = a.PreviewPromotion(ctx, admin, f.source.Name, to, nil); err != nil {
			t.Fatal(err)
		}
		if len(p.Modules) != 3 || p.Changes != 3 || p.Asks() {
			t.Fatalf("preview: %+v", p)
		}
		cache, db, web := p.Modules[0], p.Modules[1], p.Modules[2]
		if cache.Module != "cache" || cache.Change != PromoteRemove || db.Change != PromoteReplace || db.Template != "T db" ||
			web.Change != PromoteUpdate || web.FromRef != "v1" || web.ToRef != "v2" {
			t.Fatalf("modules: %+v", p.Modules)
		}
		in := map[string]PromoteInput{}
		for _, x := range web.Inputs {
			in[x.Name] = x
		}
		if len(in) != 4 || in["name"].From != "old" || in["name"].To != "app" || in["opt"].From != "" || in["opt"].To != "x" ||
			in["size"].Env != EnvOwn || in["size"].To != "l" || !in["password"].Kept {
			t.Fatalf("web inputs: %+v", web.Inputs)
		}
	})
	step(t, "a stale preview is refused", func(t *testing.T) {
		if _, _, err := a.Promote(ctx, admin, f.source.Name, to, nil, "stale"); !errors.Is(err, ErrChanged) {
			t.Fatalf("stale digest: %v", err)
		}
	})
	step(t, "promoted: an exact copy, own size and password kept, one run that waits", func(t *testing.T) {
		var err error
		if _, run, err = a.Promote(ctx, admin, f.source.Name, to, nil, p.Digest); err != nil {
			t.Fatal(err)
		}
		list, _ := a.Store.WorkspaceTemplates(ctx, f.tgt.ID)
		if len(list) != 2 || list[0].Name != "db" || list[0].TemplateID != "db" || list[0].Inputs["tier"] != "gold" ||
			list[1].ModuleRef != "v2" || list[1].ModuleSource != "git::https://x//web" {
			t.Fatalf("target: %+v", list)
		}
		web := opened(t, a, f.tgt.ID, "web")
		if web["name"] != "app" || web["opt"] != "x" || web["size"] != "l" || web["password"] != "prdpw" {
			t.Fatalf("target web: %v", web)
		}
		var always bool
		var entries int
		a.Store.Pool.QueryRow(ctx, `SELECT always_confirm FROM runs WHERE id = $1`, run.ID).Scan(&always)
		a.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE action = 'promote' AND detail->>'run' = $1 AND detail->>'source' = $2`,
			run.ID.String(), f.source.Name).Scan(&entries)
		if !always || entries != 1 {
			t.Fatalf("always_confirm %v, audit entries %d", always, entries)
		}
	})
	step(t, "promoted again: nothing to change", func(t *testing.T) {
		p, err := a.PreviewPromotion(ctx, admin, f.source.Name, to, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := a.Promote(ctx, admin, f.source.Name, to, nil, p.Digest); !errors.Is(err, ErrNotPromotable) {
			t.Fatalf("nothing to change: %v", err)
		}
	})
	step(t, "planned on an auto-apply target: waits; discarded: the previous templates are back", func(t *testing.T) {
		j, ok, err := a.Store.ClaimJob(ctx, "r", time.Minute)
		if err != nil || !ok || j.RunID != run.ID {
			t.Fatalf("claim: %+v %v %v", j, ok, err)
		}
		if err := a.Store.AckJob(ctx, j.RunID, j.LeaseID, time.Minute); err != nil {
			t.Fatal(err)
		}
		if _, err := a.Store.FinishJob(ctx, run.ID, j.LeaseID, "succeeded", "", nil, &runnerapi.Plan{Add: 1, Destroy: 2}, nil); err != nil {
			t.Fatal(err)
		}
		if r, _, _ := a.Store.GetRun(ctx, run.ID); r.Status != storage.NeedsConfirmation {
			t.Fatalf("status %s", r.Status)
		}
		if err := a.DiscardRun(ctx, admin, f.tgt.Name, run.ID); err != nil {
			t.Fatal(err)
		}
		list, _ := a.Store.WorkspaceTemplates(ctx, f.tgt.ID)
		got, _ := json.Marshal(list)
		want, _ := json.Marshal(f.tgtTemplates)
		if string(got) != string(want) {
			t.Fatalf("restored %s, want %s", got, want)
		}
	})
	step(t, "the project's value applies: not the target's own, not copied", func(t *testing.T) {
		if err := a.SetEnvironmentValue(ctx, admin, f.prd, "web", "size", "m"); err != nil {
			t.Fatal(err)
		}
		p, err := a.PreviewPromotion(ctx, admin, f.source.Name, to, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, x := range p.Modules[2].Inputs {
			if x.Name == "size" && (x.Env != EnvProject || x.To != "m") {
				t.Fatalf("size: %+v", x)
			}
		}
		if _, _, err := a.Promote(ctx, admin, f.source.Name, to, nil, p.Digest); err != nil {
			t.Fatal(err)
		}
		if web := opened(t, a, f.tgt.ID, "web"); web["size"] != "l" { // the own value stays stored, the project's applies at run time
			t.Fatalf("stored size: %v", web["size"])
		}
		if _, err := a.Store.Pool.Exec(ctx, `UPDATE runs SET status = 'applied' WHERE workspace_id = $1 AND run_active(status)`, f.tgt.ID); err != nil {
			t.Fatal(err)
		}
		if err := a.ClearEnvironmentValue(ctx, admin, f.prd, "web", "size"); err != nil {
			t.Fatal(err)
		}
	})
	step(t, "a new target: per-environment and sensitive values asked for", func(t *testing.T) {
		nt := PromoteTarget{Name: uniq("new-"), New: true, Project: f.prd}
		p, err := a.PreviewPromotion(ctx, admin, f.source.Name, nt, nil)
		if err != nil {
			t.Fatal(err)
		}
		if !p.Asks() || p.Modules[0].Change != PromoteAdd || p.Modules[1].Change != PromoteAdd {
			t.Fatalf("new target preview: %+v", p)
		}
		_, _, err = a.Promote(ctx, admin, f.source.Name, nt, nil, p.Digest)
		if ve, ok := errors.AsType[*ValidationError](err); !ok || len(ve.Fields) != 2 || ve.Fields["web.size"] == "" || ve.Fields["web.password"] == "" {
			t.Fatalf("missing values: %v", err)
		}
		_, run, err := a.Promote(ctx, admin, f.source.Name, nt, map[string]string{"web.size": "m", "web.password": "newpw"}, p.Digest)
		if err != nil {
			t.Fatal(err)
		}
		ws, err := a.Store.GetWorkspace(ctx, nt.Name)
		if err != nil || ws.ProjectName != f.prd || ws.AutoApply || run.WorkspaceID != ws.ID {
			t.Fatalf("new workspace: %+v %v", ws, err)
		}
		if web := opened(t, a, ws.ID, "web"); web["size"] != "m" || web["password"] != "newpw" || web["name"] != "app" {
			t.Fatalf("new web: %v", web)
		}
		if _, err := a.PreviewPromotion(ctx, admin, f.source.Name, nt, nil); !isValidation(err) {
			t.Fatalf("a taken name: %v", err)
		}
	})
}

// Removing or replacing a deployed template: the preview says it runs twice, Promote queues the destroy.
func TestPromoteChained(t *testing.T) {
	a, ctx := promoteApp(t), context.Background()
	f := newPromoteFixture(t, a)
	if _, err := a.Store.Pool.Exec(ctx, `INSERT INTO state_versions (workspace_id, serial, lineage, blob_key, md5, size) VALUES ($1, 1, 'l', 'k', 'm', 1)`, f.tgt.ID); err != nil {
		t.Fatal(err)
	}
	if err := a.Store.IndexState(ctx, f.tgt.ID, []runnerapi.Resource{{Address: "module.cache.a.b", Module: "module.cache"},
		{Address: "module.cache.a.c", Module: "module.cache"}}); err != nil {
		t.Fatal(err)
	}
	to := PromoteTarget{Name: f.tgt.Name}
	p, err := a.PreviewPromotion(ctx, admin, f.source.Name, to, nil)
	if err != nil || len(p.Destroy) != 2 || p.Destroy[0] != "cache" || p.Destroy[1] != "db" || p.DestroyResources != 2 {
		t.Fatalf("preview: %v %v %d", err, p.Destroy, p.DestroyResources)
	}
	_, run, err := a.Promote(ctx, admin, f.source.Name, to, nil, p.Digest)
	if err != nil || run.Action != "destroy" {
		t.Fatalf("promote: %+v %v", run, err)
	}
	var chained bool
	a.Store.Pool.QueryRow(ctx, `SELECT (detail->>'chained')::boolean FROM audit_log WHERE action = 'promote' AND detail->>'run' = $1`, run.ID.String()).Scan(&chained)
	if list, _ := a.Store.WorkspaceTemplates(ctx, f.tgt.ID); !chained || len(list) != 3 {
		t.Fatalf("chained %v, templates %+v", chained, list)
	}
}

// Who may promote where.
func TestPromoteAccess(t *testing.T) {
	a, ctx := promoteApp(t), context.Background()
	f := newPromoteFixture(t, a)
	g := uniq("g-")
	if err := a.SetProjectGrant(ctx, admin, f.dev, g+"-dev", "read"); err != nil {
		t.Fatal(err)
	}
	reader, both, stranger := user(t, a, g+"-dev"), user(t, a, g+"-dev", g+"-prd"), user(t, a)
	if err := a.SetProjectGrant(ctx, admin, f.prd, g+"-prd", "operate"); err != nil {
		t.Fatal(err)
	}
	to := PromoteTarget{Name: f.tgt.Name}
	if _, err := a.PreviewPromotion(ctx, stranger, f.source.Name, to, nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("no access to the source: %v", err)
	}
	if _, err := a.PreviewPromotion(ctx, reader, f.source.Name, to, nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("no access to the target: %v", err)
	}
	if _, err := a.PreviewPromotion(ctx, both, f.source.Name, to, nil); err != nil {
		t.Errorf("read the source, operate the target: %v", err)
	}
	if _, err := a.PreviewPromotion(ctx, both, f.source.Name, PromoteTarget{Name: uniq("n-"), New: true, Project: f.prd}, nil); err != nil {
		t.Errorf("a new target where the user creates workspaces: %v", err)
	}
	_, err := a.PreviewPromotion(ctx, both, f.source.Name, PromoteTarget{Name: uniq("n-"), New: true, Project: f.dev}, nil)
	if ve, ok := errors.AsType[*ValidationError](err); !ok || ve.Fields[FieldProject] == "" {
		t.Errorf("a new target where the user may not create workspaces: %v", err)
	}
	if _, err := a.PreviewPromotion(ctx, both, f.source.Name, PromoteTarget{Name: f.source.Name}, nil); !errors.Is(err, ErrForbidden) {
		t.Errorf("a target the user only reads: %v", err)
	}
	targets, err := a.PromoteTargets(ctx, both, f.source.Name)
	if err != nil || len(targets.Workspaces) != 1 || targets.Workspaces[0].ID != f.tgt.ID || len(targets.Projects) != 1 {
		t.Errorf("targets: %+v %v", targets, err)
	}
	if _, err := a.PreviewPromotion(ctx, admin, f.source.Name, PromoteTarget{Name: f.source.Name}, nil); !isValidation(err) {
		t.Errorf("to itself: %v", err)
	}
	if err := a.Store.SaveGeneral(ctx, f.tgt.ID, storage.General{Name: f.tgt.Name, ExecutionMode: "local"}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.PreviewPromotion(ctx, admin, f.source.Name, to, nil); !errors.Is(err, storage.ErrLocalExecution) || !IsConflict(err) {
		t.Errorf("local execution: %v", err)
	}
	if err := a.Store.SaveGeneral(ctx, f.tgt.ID, storage.General{Name: f.tgt.Name, ExecutionMode: "remote"}); err != nil {
		t.Fatal(err)
	}
	p, err := a.PreviewPromotion(ctx, admin, f.source.Name, to, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Store.CreateRun(ctx, f.tgt.ID, "apply", uuid.Nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.Promote(ctx, admin, f.source.Name, to, nil, p.Digest); !errors.Is(err, storage.ErrRunActive) || !IsConflict(err) {
		t.Errorf("an active run: %v", err)
	}
}

// step runs one step of a scenario test as a subtest; a failed step ends the scenario, a filtered
// out one skips the rest. (A copy of httpserver's.)
func step(t *testing.T, name string, f func(t *testing.T)) {
	t.Helper()
	ran := false
	if !t.Run(name, func(t *testing.T) { ran = true; f(t) }) {
		t.Fatalf("step %q failed: the steps after it need its state", name)
	}
	if !ran {
		t.Skipf("step %q was filtered out: the steps after it need its state", name)
	}
}
