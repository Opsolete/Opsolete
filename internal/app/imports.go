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
// Fixed: the input can't take the found value from here (its project or a Terraform variable sets it, or it's
// hidden), so it never asks for another round.
type ImportValue struct {
	Input, Title   string
	Found, Planned string
	Differs        bool
	Fixed          string
}

// round is an import's plan round, loaded once: the run, its templates, what its plan found.
type round struct {
	d          RunDetail
	superseded bool // a later run plans another configuration
	mods       []storage.WorkspaceTemplate
	found      map[string]storage.Found
	env        EnvironmentMap
}

// importRound loads run id of the workspace as an import's plan round (operate): ErrNotFound unless it's a plan
// run whose templates include one that imports, and still pending (storage.RunPending: not applied, and nothing
// else applied since, so going on from it undoes nothing).
func (a *App) importRound(ctx context.Context, actor Actor, workspace string, id uuid.UUID) (round, error) {
	var rd round
	var err error
	if rd.d, err = a.Run(ctx, actor, workspace, id, LevelOperate); err != nil {
		return rd, err
	}
	if rd.mods, err = a.Store.RunTemplates(ctx, id); err != nil {
		return rd, err
	}
	if rd.d.Run.Action != "plan" || !slices.ContainsFunc(rd.mods, func(m storage.WorkspaceTemplate) bool { return m.Import }) {
		return rd, ErrNotFound
	}
	if pending, err := a.Store.RunPending(ctx, id); err != nil || !pending {
		return rd, cmp.Or(err, ErrNotFound)
	}
	if rd.superseded, err = a.Store.RunSuperseded(ctx, id); err != nil {
		return rd, err
	}
	if rd.found, err = a.Store.RunFound(ctx, id); err != nil {
		return rd, err
	}
	rd.env, err = a.ProjectEnvironment(ctx, rd.d.Workspace.ProjectID)
	return rd, err
}

// session is what round rd found per importing module, and whether it can be planned again or adopted.
func (a *App) session(ctx context.Context, rd round) (ImportSession, error) {
	s := ImportSession{Plan: rd.d.Run.Plan}
	for _, m := range rd.mods {
		if !m.Import {
			continue
		}
		t, _, err := a.TemplateAt(ctx, m.TemplateID, m.ModuleSource, m.ModuleRef)
		if err != nil {
			return s, err
		}
		found := rd.found[m.Name].Inputs
		locked := rd.env.LockedForm(t, m.Inputs, m.Follows, found)
		vars, err := a.VarInputs(ctx, t, rd.d.Workspace.ID, "")
		if err != nil {
			return s, err
		}
		shown, _ := FormDefaults(t, m.Inputs)
		im := ImportModule{Module: m.Name, TemplateID: m.TemplateID}
		for _, in := range t.Inputs {
			v, ok := found[in.Name]
			if !ok || in.Sensitive || !fits(in, v) {
				continue
			}
			iv := ImportValue{Input: in.Name, Title: in.Title(), Found: templates.FormatValue(v)}
			if p, ok := m.Inputs[in.Name]; ok && p != nil {
				iv.Planned = templates.FormatValue(p)
			}
			pv, isLocked := locked[in.Name]
			vv, isVar := vars[in.Name]
			switch {
			case isLocked:
				iv.Planned, iv.Fixed = pv, "set by the project" // the run planned with the project's value, not the workspace's own
			case isVar:
				iv.Planned, iv.Fixed = vv, "set by a Terraform variable"
			case !t.Match(in.ShowIf, shown):
				iv.Fixed = "hidden by the other settings"
			}
			iv.Differs = iv.Found != iv.Planned
			s.CanReplan = s.CanReplan || iv.Differs && iv.Fixed == ""
			im.Values = append(im.Values, iv)
		}
		s.Modules = append(s.Modules, im)
	}
	s.Why = whyNotAdopt(rd)
	s.CanAdopt = s.Why == ""
	s.CanReplan = s.CanReplan && rd.d.Run.Status != storage.Errored && !rd.d.Run.Active()
	return s, nil
}

// ImportRound is run id as an import's plan round: what it found per importing module, and whether it can be
// planned again or adopted. ErrNotFound: not an import's round, or not pending anymore.
func (a *App) ImportRound(ctx context.Context, actor Actor, workspace string, id uuid.UUID) (ImportSession, error) {
	rd, err := a.importRound(ctx, actor, workspace, id)
	if err != nil {
		return ImportSession{}, err
	}
	return a.session(ctx, rd)
}

// whyNotAdopt says why round rd can't be applied as it is; "" when it can.
func whyNotAdopt(rd round) string {
	d := rd.d
	p := d.Run.Plan
	switch {
	case rd.superseded:
		return "a newer run plans another configuration: go on from the latest round"
	case d.Run.Active():
		return "the plan hasn't finished"
	case d.Run.Status == storage.Errored || d.Run.Status == storage.Canceled:
		for i := len(d.History) - 1; i >= 0; i-- { // the reason, e.g. an import conflict
			if m := d.History[i].Message; m != nil && *m != "" {
				return "the plan " + d.Run.Status + ": " + *m
			}
		}
		return "the plan " + d.Run.Status
	case d.Run.Status != storage.Planned || p == nil:
		return "only a plan that finished with changes can be applied"
	case p.Import == 0:
		return "the plan imports nothing: the module found nothing to import"
	case p.Destroy > 0:
		return "the plan would replace or delete existing resources: the inputs don't match them yet"
	}
	return ""
}

// PlanImportAgain plans the round's templates again with what its plan found: a new version, the found values (the
// ones its inputs take) over the importing modules' inputs, as their forms would save them. ValidationError: a
// found value its input rejects (set it in Change settings); conflicts: not a round that can go on, a run active.
func (a *App) PlanImportAgain(ctx context.Context, actor Actor, workspace string, id uuid.UUID) (storage.Run, error) {
	rd, err := a.importRound(ctx, actor, workspace, id)
	if err != nil {
		return storage.Run{}, err
	}
	s, err := a.session(ctx, rd)
	if err != nil {
		return storage.Run{}, err
	}
	if !s.CanReplan {
		return storage.Run{}, reason{ErrNotImport, "the plan found nothing it can take that differs from what it planned with: change the settings by hand instead"}
	}
	mods := slices.Clone(rd.mods)
	for i, m := range mods {
		if !m.Import {
			continue
		}
		t, _, err := a.TemplateAt(ctx, m.TemplateID, m.ModuleSource, m.ModuleRef)
		if err != nil {
			return storage.Run{}, err
		}
		found, ok := foundValues(rd.found[m.Name], t)
		if !ok {
			continue // found nothing for this module: it keeps its inputs
		}
		raw, _ := FormDefaults(t, found)
		vars, err := a.VarInputs(ctx, t, rd.d.Workspace.ID, "")
		if err != nil {
			return storage.Run{}, err
		}
		sv, err := a.save(formSave{T: t, Raw: raw, Old: m.Inputs, Follows: m.Follows, Found: found, Env: rd.env, Vars: vars, Workspace: rd.d.Workspace.ID, Module: m.Name})
		if err != nil {
			return storage.Run{}, err
		}
		if len(sv.Errs) > 0 {
			return storage.Run{}, &ValidationError{Message: "module " + m.Name + ": a value the plan found doesn't pass its input's rules; set it in Change settings", Fields: sv.Errs}
		}
		mods[i].Inputs, mods[i].Follows, mods[i].Import = sv.Vals, sv.Follows, true
	}
	run, err := a.Store.QueueConfiguration(ctx, rd.d.Workspace.ID, id, mods, storage.SourceForm, "plan", actor.UserID)
	if err == nil {
		a.Log.Info("import planned again", "workspace", rd.d.Workspace.Name, "after", id, "run", run.ID, "user", actor.Email)
	}
	return run, err
}

// AdoptImport applies exactly what round id planned: an apply run of its version, which waits for confirmation and
// is refused if its plan replaces or deletes anything. Conflicts: why it can't (ImportSession.Why), a run active,
// ErrChanged: another change was applied meanwhile.
func (a *App) AdoptImport(ctx context.Context, actor Actor, workspace string, id uuid.UUID) (storage.Run, error) {
	rd, err := a.importRound(ctx, actor, workspace, id)
	if err != nil {
		return storage.Run{}, err
	}
	if why := whyNotAdopt(rd); why != "" {
		return storage.Run{}, reason{ErrNotImport, "can't apply this import plan: " + why}
	}
	run, err := a.Store.AdoptRun(ctx, id, actor.UserID)
	if err == nil {
		a.Log.Info("import adopted", "workspace", workspace, "plan", id, "run", run.ID, "user", actor.Email)
	}
	return run, err
}
