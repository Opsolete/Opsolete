package app

// An import is a session of plans (docs/resource-import.md): adding a template that imports what exists, or creating
// a workspace with it, plans it (action plan: never applied, no state lock, the workspace stays usable); its plan
// finds the real values (runnerapi.FinishRequest.Found). PlanImportAgain plans a new version with them; Change
// settings fixes the rest by hand. AdoptImport applies exactly the last planned version: an apply run that always
// asks and refuses to replace or delete anything; applied, the module stops importing (storage.endImport).

import (
	"cmp"
	"context"
	"slices"

	"github.com/google/uuid"

	"github.com/opsolete/opsolete/internal/runnerapi"
	"github.com/opsolete/opsolete/internal/storage"
	"github.com/opsolete/opsolete/internal/templates"
)

// ImportSession is an import's plan round as its run page shows it.
type ImportSession struct {
	Modules   []ImportModule
	Plan      *runnerapi.Plan // nil: no changes, or the plan failed
	CanReplan bool            // what the plan found differs from what it planned with: plan again with it
	CanAdopt  bool            // the plan imports and replaces or deletes nothing: apply it
	Why       string          // why it can't be adopted; "" when it can
}

// ImportModule is one module that imports in the round, with what its plan found.
type ImportModule struct {
	Module, TemplateID string
	Values             []ImportValue
}

// ImportValue is an input whose real value the plan found: as the form shows it, and what the round planned with.
type ImportValue struct {
	Input, Title   string
	Found, Planned string
	Differs        bool
}

// importRound loads run id of the workspace as an import's plan round (operate): ErrNotFound unless it's a plan
// run whose templates include one that imports.
func (a *App) importRound(ctx context.Context, actor Actor, workspace string, id uuid.UUID) (RunDetail, []storage.WorkspaceTemplate, error) {
	d, err := a.Run(ctx, actor, workspace, id, LevelOperate)
	if err != nil {
		return d, nil, err
	}
	mods, err := a.Store.RunTemplates(ctx, id)
	if err != nil {
		return d, nil, err
	}
	if d.Run.Action != "plan" || !slices.ContainsFunc(mods, func(m storage.WorkspaceTemplate) bool { return m.Import }) {
		return d, nil, ErrNotFound
	}
	if applied, err := a.Store.RunVersionApplied(ctx, id); err != nil || applied { // adopted: the import is done
		return d, nil, cmp.Or(err, ErrNotFound)
	}
	return d, mods, nil
}

// ImportRound is run id as an import's plan round: what it found per importing module, and whether it can be
// planned again or adopted. ErrNotFound: not an import's round.
func (a *App) ImportRound(ctx context.Context, actor Actor, workspace string, id uuid.UUID) (ImportSession, error) {
	d, mods, err := a.importRound(ctx, actor, workspace, id)
	if err != nil {
		return ImportSession{}, err
	}
	found, err := a.Store.RunFound(ctx, id)
	if err != nil {
		return ImportSession{}, err
	}
	s := ImportSession{Plan: d.Run.Plan}
	for _, m := range mods {
		if !m.Import {
			continue
		}
		t, _, err := a.TemplateAt(ctx, m.TemplateID, m.ModuleSource, m.ModuleRef)
		if err != nil {
			return s, err
		}
		im := ImportModule{Module: m.Name, TemplateID: m.TemplateID}
		for _, in := range t.Inputs {
			v, ok := found[m.Name].Inputs[in.Name]
			if !ok || in.Sensitive || !fits(in, v) {
				continue
			}
			iv := ImportValue{Input: in.Name, Title: in.Title(), Found: templates.FormatValue(v)}
			if p, ok := m.Inputs[in.Name]; ok && p != nil {
				iv.Planned = templates.FormatValue(p)
			}
			iv.Differs = iv.Found != iv.Planned
			s.CanReplan = s.CanReplan || iv.Differs
			im.Values = append(im.Values, iv)
		}
		s.Modules = append(s.Modules, im)
	}
	s.Why = a.whyNotAdopt(ctx, d)
	s.CanAdopt = s.Why == ""
	s.CanReplan = s.CanReplan && d.Run.Status != storage.Errored && !d.Run.Active()
	return s, nil
}

// whyNotAdopt says why round d can't be applied as it is; "" when it can.
func (a *App) whyNotAdopt(ctx context.Context, d RunDetail) string {
	p := d.Run.Plan
	switch {
	case d.Run.Active():
		return "the plan hasn't finished"
	case d.Run.Status != storage.Planned || p == nil:
		return "only a plan that finished with changes can be applied"
	case p.Import == 0:
		return "the plan imports nothing: the module found nothing to import"
	case p.Destroy > 0:
		return "the plan would replace or delete existing resources: the inputs don't match them yet"
	}
	latest, err := a.Store.Runs(ctx, d.Workspace.ID, 1)
	if err == nil && len(latest) == 1 && latest[0].ID != d.Run.ID {
		return "a newer run exists: adopt the latest plan"
	}
	return ""
}

// PlanImportAgain plans the round's templates again with what its plan found: a new version, the found values (the
// ones its inputs take) over the importing modules' inputs, as their forms would save them. ValidationError: a
// found value its input rejects (set it in Change settings); conflicts: not a round that can go on, a run active.
func (a *App) PlanImportAgain(ctx context.Context, actor Actor, workspace string, id uuid.UUID) (storage.Run, error) {
	s, err := a.ImportRound(ctx, actor, workspace, id)
	if err != nil {
		return storage.Run{}, err
	}
	if !s.CanReplan {
		return storage.Run{}, reason{ErrNotImport, "the plan found nothing that differs from what it planned with: change the settings by hand instead"}
	}
	d, mods, err := a.importRound(ctx, actor, workspace, id)
	if err != nil {
		return storage.Run{}, err
	}
	env, err := a.ProjectEnvironment(ctx, d.Workspace.ProjectID)
	if err != nil {
		return storage.Run{}, err
	}
	for i, m := range mods {
		if !m.Import {
			continue
		}
		t, _, err := a.TemplateAt(ctx, m.TemplateID, m.ModuleSource, m.ModuleRef)
		if err != nil {
			return storage.Run{}, err
		}
		found, err := a.FoundValues(ctx, actor, workspace, id, m.Name, t)
		if err != nil {
			return storage.Run{}, err
		}
		raw, _ := FormDefaults(t, found)
		sv, err := a.save(formSave{T: t, Raw: raw, Old: m.Inputs, Follows: m.Follows, Found: found, Env: env, Workspace: d.Workspace.ID, Module: m.Name})
		if err != nil {
			return storage.Run{}, err
		}
		if len(sv.Errs) > 0 {
			return storage.Run{}, &ValidationError{Message: "module " + m.Name + ": a value the plan found doesn't pass its input's rules; set it in Change settings", Fields: sv.Errs}
		}
		mods[i].Inputs, mods[i].Follows, mods[i].Import = sv.Vals, sv.Follows, true
	}
	run, err := a.Store.QueueConfiguration(ctx, d.Workspace.ID, mods, storage.SourceImport, "plan", actor.UserID)
	if err == nil {
		a.Log.Info("import planned again", "workspace", d.Workspace.Name, "after", id, "run", run.ID, "user", actor.Email)
	}
	return run, err
}

// AdoptImport applies exactly what round id planned: an apply run of its version, which waits for confirmation and
// is refused if its plan replaces or deletes anything. Conflicts: why it can't (ImportSession.Why), a run active.
func (a *App) AdoptImport(ctx context.Context, actor Actor, workspace string, id uuid.UUID) (storage.Run, error) {
	s, err := a.ImportRound(ctx, actor, workspace, id)
	if err != nil {
		return storage.Run{}, err
	}
	if !s.CanAdopt {
		return storage.Run{}, reason{ErrNotImport, "can't apply this import plan: " + s.Why}
	}
	run, err := a.Store.AdoptRun(ctx, id, actor.UserID)
	if err == nil {
		a.Log.Info("import adopted", "workspace", workspace, "plan", id, "run", run.ID, "user", actor.Email)
	}
	return run, err
}
