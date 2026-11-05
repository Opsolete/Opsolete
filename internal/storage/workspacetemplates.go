package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/opsolete/opsolete/internal/runnerapi"
)

// WorkspaceTemplate is a template added to a workspace: the module block module.<Name>, pinned to
// a module version, with its inputs.
type WorkspaceTemplate struct {
	Name         string
	TemplateID   string
	ModuleSource string
	ModuleRef    string
	Inputs       map[string]any
}

// WorkspaceTemplates lists a workspace's added templates by name.
func (s *Store) WorkspaceTemplates(ctx context.Context, workspaceID string) ([]WorkspaceTemplate, error) {
	return workspaceTemplates(ctx, s.DB, workspaceID)
}

func workspaceTemplates(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, workspaceID string) ([]WorkspaceTemplate, error) {
	rows, err := q.QueryContext(ctx, `SELECT name, template_id, module_source, module_ref, inputs FROM workspace_templates
		WHERE workspace_id = ops_uuid($1) ORDER BY name`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WorkspaceTemplate
	for rows.Next() {
		var t WorkspaceTemplate
		var raw []byte
		if err := rows.Scan(&t.Name, &t.TemplateID, &t.ModuleSource, &t.ModuleRef, &raw); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, &t.Inputs); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// SetWorkspaceTemplate adds a template to a workspace (add: ErrExists if the name is taken), or
// changes an added one's module version and inputs, and queues the plan that applies it. Without a
// free workspace (ErrRunActive) nothing changes.
func (s *Store) SetWorkspaceTemplate(ctx context.Context, workspaceID string, t WorkspaceTemplate, add bool, createdBy string) (Run, error) {
	raw, err := json.Marshal(t.Inputs)
	if err != nil {
		return Run{}, err
	}
	q := `UPDATE workspace_templates SET template_id = $3, module_source = $4, module_ref = $5, inputs = $6
		WHERE workspace_id = $1::uuid AND name = $2`
	if add {
		q = `INSERT INTO workspace_templates (workspace_id, name, template_id, module_source, module_ref, inputs)
			VALUES ($1::uuid, $2, $3, $4, $5, $6)`
	}
	return s.changeTemplates(ctx, workspaceID, createdBy, q, workspaceID, t.Name, t.TemplateID, t.ModuleSource, t.ModuleRef, raw)
}

// RemoveWorkspaceTemplate removes an added template. A template's module may configure its own
// providers, so dropping its block before its resources are gone leaves Terraform unable to destroy
// them ("Provider configuration not present"): if the state manages resources, a destroy run
// targeting module.<name> is queued that removes the template once applied; if not, it is removed
// right away and no run is queued (Run.ID "").
func (s *Store) RemoveWorkspaceTemplate(ctx context.Context, workspaceID, name, createdBy string) (Run, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Run{}, err
	}
	defer tx.Rollback()
	// FOR UPDATE: templates can't be added meanwhile (their FK check waits for the lock).
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT
			EXISTS (SELECT 1 FROM workspace_templates t WHERE t.workspace_id = w.id AND t.name = $2)
		FROM workspaces w WHERE w.id = $1::uuid FOR UPDATE`, workspaceID, name).Scan(&exists); err != nil {
		return Run{}, err
	}
	switch err := requireIdle(ctx, tx, workspaceID); {
	case !exists:
		return Run{}, sql.ErrNoRows
	case errors.Is(err, ErrHasResources):
		r, err := createRun(ctx, tx, workspaceID, "destroy", createdBy)
		if err != nil {
			return Run{}, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE runs SET remove_template = $2 WHERE id = $1`, r.ID, name); err != nil {
			return Run{}, err
		}
		return r, tx.Commit()
	case err != nil:
		return Run{}, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM workspace_templates WHERE workspace_id = $1::uuid AND name = $2`,
		workspaceID, name); err != nil {
		return Run{}, err
	}
	return Run{}, tx.Commit()
}

// ErrNotIndexed: the current state version's resources aren't known, so a destroy can't be limited to some of them.
var ErrNotIndexed = errors.New("the current state isn't indexed, so the root template's resources can't be told from the added templates': run a plan first")

const clearRoot = `template_id = NULL, module_source = NULL, module_ref = NULL, inputs = '{}'`

// RemoveRootTemplate removes the template a workspace was created from (its root configuration), like
// RemoveWorkspaceTemplate: if the state manages resources, it queues a destroy run that clears the root
// once applied. With added templates next to it, that destroy targets only the root's own resources,
// so the added ones stay (they need their own provider configuration then). sql.ErrNoRows: no root template.
func (s *Store) RemoveRootTemplate(ctx context.Context, workspaceID, createdBy string) (Run, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Run{}, err
	}
	defer tx.Rollback()
	var root bool
	if err := tx.QueryRowContext(ctx, `SELECT template_id IS NOT NULL FROM workspaces WHERE id = $1::uuid FOR UPDATE`,
		workspaceID).Scan(&root); err != nil {
		return Run{}, err
	}
	if !root {
		return Run{}, sql.ErrNoRows
	}
	switch err := requireIdle(ctx, tx, workspaceID); {
	case errors.Is(err, ErrHasResources):
		added, err := workspaceTemplates(ctx, tx, workspaceID)
		if err != nil {
			return Run{}, err
		}
		var target []string // nil: destroy everything, all of it is the root's
		if len(added) > 0 {
			if target, err = rootTargets(ctx, tx, workspaceID, added); err != nil {
				return Run{}, err
			}
			if len(target) == 0 {
				break // only the added templates have resources: nothing of the root's to destroy
			}
		}
		r, err := createRun(ctx, tx, workspaceID, "destroy", createdBy)
		if err != nil {
			return Run{}, err
		}
		var raw []byte
		if target != nil {
			raw, _ = json.Marshal(target)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE runs SET remove_root = true, target = $2 WHERE id = $1`, r.ID, nullJSON(raw)); err != nil {
			return Run{}, err
		}
		return r, tx.Commit()
	case err != nil:
		return Run{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE workspaces SET `+clearRoot+` WHERE id = $1::uuid`, workspaceID); err != nil {
		return Run{}, err
	}
	return Run{}, tx.Commit()
}

// rootTargets returns the -target addresses of the root configuration's resources in the current state.
func rootTargets(ctx context.Context, tx *sql.Tx, workspaceID string, added []WorkspaceTemplate) ([]string, error) {
	var version int64
	var indexed bool
	if err := tx.QueryRowContext(ctx, `SELECT id, indexed FROM state_versions WHERE workspace_id = $1::uuid ORDER BY id DESC LIMIT 1`,
		workspaceID).Scan(&version, &indexed); err != nil {
		return nil, err
	}
	if !indexed {
		return nil, ErrNotIndexed
	}
	rows, err := tx.QueryContext(ctx, `SELECT address, module FROM state_version_resources WHERE state_version_id = $1 ORDER BY address`, version)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var res []runnerapi.Resource
	for rows.Next() {
		var r runnerapi.Resource
		if err := rows.Scan(&r.Address, &r.Module); err != nil {
			return nil, err
		}
		res = append(res, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	names := make([]string, len(added))
	for i, a := range added {
		names[i] = a.Name
	}
	return rootAddresses(res, names), nil
}

// rootAddresses picks the resources that aren't in an added template's module: root resources by
// address, and the root's own module calls whole (module.net["a"].x is module.net).
func rootAddresses(res []runnerapi.Resource, added []string) []string {
	var out []string
	for _, r := range res {
		t := r.Address
		if r.Module != "" {
			top, _, _ := strings.Cut(r.Module, ".module.")
			call, _, _ := strings.Cut(strings.TrimPrefix(top, "module."), "[")
			if slices.Contains(added, call) {
				continue
			}
			t = "module." + call
		}
		if !slices.Contains(out, t) {
			out = append(out, t)
		}
	}
	return out
}

// changeTemplates runs q (which must change one row, else sql.ErrNoRows) and queues a plan, atomically.
func (s *Store) changeTemplates(ctx context.Context, workspaceID, createdBy, q string, args ...any) (Run, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Run{}, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, q, args...)
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && pgErr.Code == "23505" {
		return Run{}, ErrExists
	}
	if err != nil {
		return Run{}, err
	}
	if n, err := res.RowsAffected(); err != nil {
		return Run{}, err
	} else if n == 0 {
		return Run{}, sql.ErrNoRows
	}
	r, err := createRun(ctx, tx, workspaceID, "apply", createdBy)
	if err != nil {
		return Run{}, err
	}
	return r, tx.Commit()
}
