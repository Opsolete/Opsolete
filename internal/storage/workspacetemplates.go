package storage

import (
	"context"
	"encoding/json"
	"errors"
	"slices"

	"github.com/google/uuid"

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
	Follows      []string // per-environment inputs that take the project's value although it lets workspaces keep their own
	Import       bool     // the module imports what exists (the runner gets the template's import switch on)
}

// TemplateInputs is one module's inputs in a configuration version as stored, for re-sealing them under a new
// SECRET_KEY (bound to the workspace and module, not the version: versions share them).
type TemplateInputs struct {
	WorkspaceID uuid.UUID
	CVID        uuid.UUID
	Name        string
	Inputs      []byte
}

// AllTemplateInputs lists the inputs of every module of every configuration version.
func (s *Store) AllTemplateInputs(ctx context.Context) ([]TemplateInputs, error) {
	rows, err := s.q.AllConfigInputs(ctx)
	out := make([]TemplateInputs, 0, len(rows))
	for _, t := range rows {
		out = append(out, TemplateInputs{WorkspaceID: t.WorkspaceID, CVID: t.CvID, Name: t.Name, Inputs: t.Inputs})
	}
	return out, err
}

// ResealInputs replaces a version's module's inputs, unless they changed since they were read (ErrNotFound).
func (s *Store) ResealInputs(ctx context.Context, cv uuid.UUID, name string, old, inputs []byte) error {
	return affected(s.q.ResealConfigInputs(ctx, db.ResealConfigInputsParams{CvID: cv, Name: name, Old: old, Inputs: inputs}))
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
		t := WorkspaceTemplate{Name: r.Name, TemplateID: r.TemplateID, ModuleSource: r.ModuleSource, ModuleRef: r.ModuleRef, Follows: nilIfEmpty(r.Follows), Import: r.Importing}
		if err := json.Unmarshal(r.Inputs, &t.Inputs); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
}

// SetWorkspaceTemplate adds a template to a workspace (add: ErrExists if the name is taken), or
// changes an added one's module version and inputs (ErrNotFound if there is none): a new configuration version,
// and the plan that applies it. Without a free workspace (ErrRunActive) nothing changes.
func (s *Store) SetWorkspaceTemplate(ctx context.Context, workspaceID uuid.UUID, t WorkspaceTemplate, add bool, createdBy uuid.UUID) (Run, error) {
	return s.SetWorkspaceTemplateAs(ctx, workspaceID, t, add, "apply", createdBy)
}

// SetWorkspaceTemplateAs is SetWorkspaceTemplate with the run's action: plan for an import's rounds, never applied.
func (s *Store) SetWorkspaceTemplateAs(ctx context.Context, workspaceID uuid.UUID, t WorkspaceTemplate, add bool, action string, createdBy uuid.UUID) (Run, error) {
	var r Run
	err := s.tx(ctx, func(q *db.Queries) error {
		cur, mods, err := currentConfig(ctx, q, workspaceID)
		if err != nil {
			return err
		}
		switch exists := slices.ContainsFunc(mods, func(m WorkspaceTemplate) bool { return m.Name == t.Name }); {
		case add && exists:
			return ErrExists
		case !add && !exists:
			return ErrNotFound
		}
		r, err = queueVersion(ctx, q, workspaceID, cur, withModule(mods, t), SourceForm, action, createdBy)
		return err
	})
	if err != nil {
		return Run{}, err
	}
	return r, nil
}

// RemoveWorkspaceTemplate removes an added template. A template's module may configure its own
// providers, so dropping its block before its resources are gone leaves Terraform unable to destroy
// them ("Provider configuration not present"): if the state manages resources, a destroy run
// targeting module.<name> is queued that removes the template once applied; if not, it is removed
// right away (a version without it) and no run is queued (Run.ID uuid.Nil). ErrNotFound: no such template.
func (s *Store) RemoveWorkspaceTemplate(ctx context.Context, workspaceID uuid.UUID, name string, createdBy uuid.UUID) (Run, error) {
	var r Run
	err := s.tx(ctx, func(q *db.Queries) error {
		cur, mods, err := currentConfig(ctx, q, workspaceID)
		if err != nil {
			return err
		}
		switch err := requireIdle(ctx, q, workspaceID); {
		case !slices.ContainsFunc(mods, func(m WorkspaceTemplate) bool { return m.Name == name }):
			return ErrNotFound
		case errors.Is(err, ErrHasResources):
			if r, err = createRun(ctx, q, workspaceID, "destroy", createdBy); err != nil {
				return err
			}
			return q.SetRunRemoveTemplates(ctx, db.SetRunRemoveTemplatesParams{ID: r.ID, RemoveTemplates: []string{name}})
		case err != nil:
			return err
		}
		cv, err := newVersion(ctx, q, workspaceID, cur, without(mods, []string{name}), SourceRemove, createdBy)
		if err != nil {
			return err
		}
		return setCurrent(ctx, q, workspaceID, cv)
	})
	if err != nil {
		return Run{}, err
	}
	return r, nil
}

// QueueConfiguration makes mods the workspace's templates in a new version (made from the current one) and queues
// an action run of it: an import's next round, with what round after's plan found. ErrRunActive,
// ErrLocalExecution; ErrChanged: after isn't pending anymore (RunPending).
func (s *Store) QueueConfiguration(ctx context.Context, workspaceID, after uuid.UUID, mods []WorkspaceTemplate, source, action string, createdBy uuid.UUID) (Run, error) {
	var r Run
	err := s.tx(ctx, func(q *db.Queries) error {
		cur, _, err := currentConfig(ctx, q, workspaceID)
		if err != nil {
			return err
		}
		if err := requirePending(ctx, q, after); err != nil {
			return err
		}
		r, err = queueVersion(ctx, q, workspaceID, cur, mods, source, action, createdBy)
		return err
	})
	return r, err
}

// AdoptRun queues an apply run of the configuration version run runID planned (an import's plan round): exactly
// what was planned. ErrRunActive, ErrLocalExecution; ErrNotFound: no such run; ErrChanged: not pending anymore.
func (s *Store) AdoptRun(ctx context.Context, runID, createdBy uuid.UUID) (Run, error) {
	var r Run
	err := s.tx(ctx, func(q *db.Queries) error {
		run, err := q.LockRun(ctx, runID)
		if err != nil {
			return notFound(err)
		}
		if _, _, err := currentConfig(ctx, q, run.WorkspaceID); err != nil { // its lock: no change meanwhile
			return err
		}
		if err := requirePending(ctx, q, runID); err != nil {
			return err
		}
		r, err = queueRun(ctx, q, run.WorkspaceID, "apply", createdBy, run.CvID)
		return err
	})
	return r, err
}
