package storage

import (
	"context"
	"io/fs"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/opsolete/opsolete/internal/testdb"
)

// TestConfigVersionsUpgrade: migration 010 turns each workspace's templates into its current version. An active
// run that changed them (config_changes) gets the templates as they are; the current version has its befores put
// back, as its restore would have: a changed module as it was, an added one gone, a removed one there again.
func TestConfigVersionsUpgrade(t *testing.T) {
	ctx := context.Background()
	conn, apply := migratedTo(t, "010_configuration_versions.sql")
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := conn.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	project := `(SELECT default_project_id FROM organization)`
	exec(`INSERT INTO workspaces (id, name, project_id) VALUES
		('00000000-0000-0000-0000-00000000000a', 'idle', ` + project + `),
		('00000000-0000-0000-0000-00000000000b', 'busy', ` + project + `),
		('00000000-0000-0000-0000-00000000000c', 'empty', ` + project + `)`)
	tpl := func(ws, name, ref string) {
		exec(`INSERT INTO workspace_templates (workspace_id, name, template_id, module_source, module_ref, inputs)
			VALUES ($1, $2, 't', 'git::x', $3, '{"n": 1}')`, ws, name, ref)
	}
	tpl("00000000-0000-0000-0000-00000000000a", "main", "v1")
	// busy's run changed main v1 → v2, added new, removed old (a promotion)
	tpl("00000000-0000-0000-0000-00000000000b", "main", "v2")
	tpl("00000000-0000-0000-0000-00000000000b", "new", "v1")
	tpl("00000000-0000-0000-0000-00000000000b", "kept", "v1")
	exec(`INSERT INTO runs (id, workspace_id, action, status, config_changes) VALUES
		('00000000-0000-0000-0000-0000000000f1', '00000000-0000-0000-0000-00000000000b', 'apply', 'needs_confirmation', $1),
		('00000000-0000-0000-0000-0000000000f2', '00000000-0000-0000-0000-00000000000a', 'apply', 'applied', NULL)`,
		`[{"module": "main", "before": {"template_id": "t", "module_source": "git::x", "module_ref": "v1", "inputs": {"n": 1}},
		   "after": {"template_id": "t", "module_source": "git::x", "module_ref": "v2", "inputs": {"n": 1}}},
		  {"module": "new", "before": null, "after": {"template_id": "t", "module_source": "git::x", "module_ref": "v1", "inputs": {"n": 1}}},
		  {"module": "old", "before": {"template_id": "t", "module_source": "git::x", "module_ref": "v0", "inputs": {}}, "after": null}]`)

	apply("010_configuration_versions.sql")

	refs := func(sql string, args ...any) map[string]string {
		t.Helper()
		rows, err := conn.Query(ctx, `SELECT m.name, m.module_ref FROM configuration_modules m WHERE m.cv_id = (`+sql+`)`, args...)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]string{}
		for rows.Next() {
			var n, r string
			rows.Scan(&n, &r)
			out[n] = r
		}
		return out
	}
	current := `SELECT current_cv_id FROM workspaces WHERE name = $1`
	if got := refs(current, "idle"); len(got) != 1 || got["main"] != "v1" {
		t.Fatalf("idle: %v", got)
	}
	if got := refs(current, "busy"); len(got) != 3 || got["main"] != "v1" || got["kept"] != "v1" || got["old"] != "v0" {
		t.Fatalf("busy, current (the run's change undone): %v", got)
	}
	if got := refs(`SELECT cv_id FROM runs WHERE id = '00000000-0000-0000-0000-0000000000f1'`); len(got) != 3 || got["main"] != "v2" || got["new"] != "v1" || got["kept"] != "v1" {
		t.Fatalf("busy, its run's version: %v", got)
	}
	var parentIsCurrent, emptyHasNone bool
	conn.QueryRow(ctx, `SELECT cv.parent_id = w.current_cv_id FROM runs r JOIN configuration_versions cv ON cv.id = r.cv_id
		JOIN workspaces w ON w.id = r.workspace_id WHERE r.id = '00000000-0000-0000-0000-0000000000f1'`).Scan(&parentIsCurrent)
	conn.QueryRow(ctx, `SELECT current_cv_id IS NULL FROM workspaces WHERE name = 'empty'`).Scan(&emptyHasNone)
	if !parentIsCurrent || !emptyHasNone {
		t.Fatalf("the run's version is made from the current one: %v; a workspace without templates has none: %v", parentIsCurrent, emptyHasNone)
	}
	// the view shows what the workspace is getting while the run is active
	var n int
	conn.QueryRow(ctx, `SELECT count(*) FROM workspace_templates t JOIN workspaces w ON w.id = t.workspace_id WHERE w.name = 'busy' AND t.module_ref = 'v2'`).Scan(&n)
	if n != 1 {
		t.Fatal("the view doesn't show the active run's version")
	}
}

// migratedTo connects to a new test database with the migrations before stop applied, and returns apply, which
// applies one migration file.
func migratedTo(t *testing.T, stop string) (*pgx.Conn, func(string)) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, testdb.URL(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(ctx) })
	apply := func(name string) {
		t.Helper()
		b, err := fs.ReadFile(migrations, "migrations/"+name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Exec(ctx, string(b)); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	names, _ := fs.Glob(migrations, "migrations/*.sql")
	for _, n := range names {
		if n == "migrations/"+stop {
			break
		}
		apply(n[len("migrations/"):])
	}
	return conn, apply
}

// TestRunStateUpgrade: migration 011 makes a promotion's runs records (a pending destroy's templates a
// configuration version, sealed inputs kept), and gives the plans waiting for confirmation their reason.
func TestRunStateUpgrade(t *testing.T) {
	ctx := context.Background()
	conn, apply := migratedTo(t, "011_run_state.sql")
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := conn.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	exec(`INSERT INTO workspaces (id, name, project_id) VALUES ('00000000-0000-0000-0000-00000000000a', 'prd', (SELECT default_project_id FROM organization)),
		('00000000-0000-0000-0000-00000000000b', 'other', (SELECT default_project_id FROM organization))`)
	exec(`INSERT INTO runs (id, workspace_id, action, status, remove_templates, always_confirm, promotion) VALUES
		('00000000-0000-0000-0000-0000000000d1', '00000000-0000-0000-0000-00000000000a', 'destroy', 'needs_confirmation', '{gone}', true, $1)`,
		`{"source": "dev", "actor": "a@example.com", "created_by": "00000000-0000-0000-0000-000000000000",
		  "templates": [{"Name": "web", "TemplateID": "web", "ModuleSource": "git::x", "ModuleRef": "v2", "Inputs": {"pw": {"sealed": "opsolete:1:k:x"}}}]}`)
	exec(`INSERT INTO runs (id, workspace_id, action, status, always_confirm) VALUES
		('00000000-0000-0000-0000-0000000000a1', '00000000-0000-0000-0000-00000000000a', 'apply', 'applied', true),
		('00000000-0000-0000-0000-0000000000a2', '00000000-0000-0000-0000-00000000000b', 'refresh', 'needs_confirmation', false)`)
	apply("011_run_state.sql")

	var source, actor, ref, pw string
	if err := conn.QueryRow(ctx, `SELECT p.source, p.actor, m.module_ref, m.inputs->'pw'->>'sealed' FROM promotions p
		JOIN configuration_modules m ON m.cv_id = p.cv_id WHERE p.destroy_run_id = '00000000-0000-0000-0000-0000000000d1' AND p.apply_run_id IS NULL`).
		Scan(&source, &actor, &ref, &pw); err != nil || source != "dev" || actor != "a@example.com" || ref != "v2" || pw != "opsolete:1:k:x" {
		t.Fatalf("the pending promotion: %q %q %q %q %v", source, actor, ref, pw, err)
	}
	var n int
	conn.QueryRow(ctx, `SELECT count(*) FROM promotions WHERE apply_run_id = '00000000-0000-0000-0000-0000000000a1'`).Scan(&n)
	if n != 1 {
		t.Fatal("a promotion's apply run has no record")
	}
	reasons := map[string]string{}
	rows, _ := conn.Query(ctx, `SELECT id::text, COALESCE(confirm_reason, '') FROM runs`)
	for rows.Next() {
		var id, r string
		rows.Scan(&id, &r)
		reasons[id] = r
	}
	if reasons["00000000-0000-0000-0000-0000000000d1"] != ReasonPromotion || reasons["00000000-0000-0000-0000-0000000000a2"] != ReasonRefresh ||
		reasons["00000000-0000-0000-0000-0000000000a1"] != "" {
		t.Fatalf("reasons: %v", reasons)
	}
}
