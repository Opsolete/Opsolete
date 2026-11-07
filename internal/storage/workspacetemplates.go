package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
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
	return s.changeTemplates(ctx, workspaceID, t.Name, createdBy, q, workspaceID, t.Name, t.TemplateID, t.ModuleSource, t.ModuleRef, raw)
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

// changeTemplates runs q (which must change one row, added template name's, else sql.ErrNoRows) and
// queues a plan, atomically; if the run ends before its apply, the change is undone (configChange).
func (s *Store) changeTemplates(ctx context.Context, workspaceID, name, createdBy, q string, args ...any) (Run, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Run{}, err
	}
	defer tx.Rollback()
	before, err := configSnapshot(ctx, tx, workspaceID, name)
	if err != nil {
		return Run{}, err
	}
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
	if err := recordConfigChanges(ctx, tx, r.ID, workspaceID, []configChange{{Module: name, Before: before}}); err != nil {
		return Run{}, err
	}
	return r, tx.Commit()
}
