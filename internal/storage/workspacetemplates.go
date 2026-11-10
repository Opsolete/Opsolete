package storage

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/google/uuid"

	"github.com/opsolete/opsolete/internal/runnerapi"
	"github.com/opsolete/opsolete/internal/storage/db"
)

// WorkspaceTemplate is a template added to a workspace: the module block module.<Name>, pinned to
// a module version, with its inputs.
type WorkspaceTemplate struct {
	Name         string
	TemplateID   string
	ModuleSource string
	ModuleRef    string
	Inputs       map[string]any
	// Imports: the run queued with this change adopts these existing resources (setImports). Not stored with
	// the template, and empty when read back.
	Imports []runnerapi.Import
}

// setImports makes run r adopt imports (none: nothing to do).
func setImports(ctx context.Context, q *db.Queries, r Run, imports []runnerapi.Import) error {
	if len(imports) == 0 {
		return nil
	}
	b, err := json.Marshal(imports)
	if err != nil {
		return err
	}
	return q.SetRunImports(ctx, db.SetRunImportsParams{ID: r.ID, Imports: b})
}

// TemplateInputs is one added template's inputs as stored, for re-sealing them under a new SECRET_KEY.
type TemplateInputs struct {
	WorkspaceID uuid.UUID
	Name        string
	Inputs      []byte
}

func (s *Store) AllTemplateInputs(ctx context.Context) ([]TemplateInputs, error) {
	rows, err := s.q.AllTemplateInputs(ctx)
	out := make([]TemplateInputs, 0, len(rows))
	for _, t := range rows {
		out = append(out, TemplateInputs{WorkspaceID: t.WorkspaceID, Name: t.Name, Inputs: t.Inputs})
	}
	return out, err
}

// ResealInputs replaces an added template's inputs, unless they changed since they were read
// (ErrNotFound).
func (s *Store) ResealInputs(ctx context.Context, workspaceID uuid.UUID, name string, old, inputs []byte) error {
	return affected(s.q.ResealInputs(ctx, db.ResealInputsParams{WorkspaceID: workspaceID, Name: name, Old: old, Inputs: inputs}))
}

// WorkspaceTemplates lists a workspace's added templates by name.
func (s *Store) WorkspaceTemplates(ctx context.Context, workspaceID uuid.UUID) ([]WorkspaceTemplate, error) {
	return workspaceTemplates(ctx, s.q, workspaceID)
}

func workspaceTemplates(ctx context.Context, q *db.Queries, workspaceID uuid.UUID) ([]WorkspaceTemplate, error) {
	rows, err := q.ListWorkspaceTemplates(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	out := make([]WorkspaceTemplate, 0, len(rows))
	for _, r := range rows {
		t := WorkspaceTemplate{Name: r.Name, TemplateID: r.TemplateID, ModuleSource: r.ModuleSource, ModuleRef: r.ModuleRef}
		if err := json.Unmarshal(r.Inputs, &t.Inputs); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
}

// SetWorkspaceTemplate adds a template to a workspace (add: ErrExists if the name is taken), or
// changes an added one's module version and inputs (ErrNotFound if there is none), and queues the
// plan that applies it. Without a free workspace (ErrRunActive) nothing changes.
func (s *Store) SetWorkspaceTemplate(ctx context.Context, workspaceID uuid.UUID, t WorkspaceTemplate, add bool, createdBy uuid.UUID) (Run, error) {
	raw, err := json.Marshal(t.Inputs)
	if err != nil {
		return Run{}, err
	}
	change := func(q *db.Queries) (int64, error) {
		if add {
			return q.InsertWorkspaceTemplate(ctx, db.InsertWorkspaceTemplateParams{WorkspaceID: workspaceID, Name: t.Name,
				TemplateID: t.TemplateID, ModuleSource: t.ModuleSource, ModuleRef: t.ModuleRef, Inputs: raw})
		}
		return q.UpdateWorkspaceTemplate(ctx, db.UpdateWorkspaceTemplateParams{WorkspaceID: workspaceID, Name: t.Name,
			TemplateID: t.TemplateID, ModuleSource: t.ModuleSource, ModuleRef: t.ModuleRef, Inputs: raw})
	}
	return s.changeTemplates(ctx, workspaceID, t.Name, t.Imports, createdBy, change)
}

// RemoveWorkspaceTemplate removes an added template. A template's module may configure its own
// providers, so dropping its block before its resources are gone leaves Terraform unable to destroy
// them ("Provider configuration not present"): if the state manages resources, a destroy run
// targeting module.<name> is queued that removes the template once applied; if not, it is removed
// right away and no run is queued (Run.ID uuid.Nil). ErrNotFound: no such template.
func (s *Store) RemoveWorkspaceTemplate(ctx context.Context, workspaceID uuid.UUID, name string, createdBy uuid.UUID) (Run, error) {
	var r Run
	err := s.tx(ctx, func(q *db.Queries) error {
		exists, err := q.LockWorkspaceTemplate(ctx, db.LockWorkspaceTemplateParams{WorkspaceID: workspaceID, Name: name})
		if err != nil {
			return notFound(err)
		}
		switch err := requireIdle(ctx, q, workspaceID); {
		case !exists:
			return ErrNotFound
		case errors.Is(err, ErrHasResources):
			if r, err = createRun(ctx, q, workspaceID, "destroy", createdBy); err != nil {
				return err
			}
			return q.SetRunRemoveTemplates(ctx, db.SetRunRemoveTemplatesParams{ID: r.ID, RemoveTemplates: []string{name}})
		case err != nil:
			return err
		}
		return q.DeleteWorkspaceTemplate(ctx, db.DeleteWorkspaceTemplateParams{WorkspaceID: workspaceID, Name: name})
	})
	if err != nil {
		return Run{}, err
	}
	return r, nil
}

// changeTemplates runs change (which must change one row, added template name's, else ErrNotFound)
// and queues a plan, atomically; if the run ends before its apply, the change is undone (configChange).
func (s *Store) changeTemplates(ctx context.Context, workspaceID uuid.UUID, name string, imports []runnerapi.Import, createdBy uuid.UUID,
	change func(q *db.Queries) (int64, error)) (Run, error) {
	var r Run
	err := s.tx(ctx, func(q *db.Queries) error {
		before, err := configSnapshot(ctx, q, workspaceID, name)
		if err != nil {
			return err
		}
		err = affected(change(q))
		if isUnique(err) {
			return ErrExists
		}
		if err != nil {
			return err
		}
		if r, err = createRun(ctx, q, workspaceID, "apply", createdBy); err != nil {
			return err
		}
		if err := setImports(ctx, q, r, imports); err != nil {
			return err
		}
		return recordConfigChanges(ctx, q, r.ID, workspaceID, []configChange{{Module: name, Before: before}})
	})
	if err != nil {
		return Run{}, err
	}
	return r, nil
}
