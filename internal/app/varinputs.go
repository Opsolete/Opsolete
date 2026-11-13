package app

import (
	"context"
	"fmt"
	"slices"

	"github.com/google/uuid"

	"github.com/opsolete/opsolete/internal/storage"
	"github.com/opsolete/opsolete/internal/templates"
)

// Terraform variables fill the inputs a template doesn't ask: a variable of its registry module that the template's
// JSON doesn't list takes the value of the workspace's Terraform variable of that name (its own, or a variable set's),
// such as a subscription's standard network set once per project. The form shows such an input read-only and
// doesn't store it (save); the run gets the variable (withWorkspaceVars).

// VarInputs is what Terraform variables fill of t's inputs (FormState.FromVars): by input name, the value as the form
// shows it, "(sensitive)" for a sensitive variable. workspace: an existing workspace's variables (RunVariables);
// uuid.Nil: those of a new workspace in project (by name), which gets the global sets and the project's.
func (a *App) VarInputs(ctx context.Context, t templates.Template, workspace uuid.UUID, project string) (map[string]string, error) {
	listed := a.listedInputs(t.ID)
	var free []string
	for _, in := range t.Inputs {
		if !listed[in.Name] {
			free = append(free, in.Name)
		}
	}
	if len(free) == 0 {
		return nil, nil
	}
	var vars []storage.Variable
	var err error
	if workspace != uuid.Nil {
		vars, err = a.Store.RunVariables(ctx, workspace)
	} else {
		vars, err = a.newWorkspaceVars(ctx, project)
	}
	if err != nil {
		return nil, fmt.Errorf("load variables: %w", err)
	}
	out := map[string]string{}
	for _, v := range vars {
		if _, done := out[v.Key]; done || v.Category != "terraform" || !slices.Contains(free, v.Key) {
			continue // the first of a key wins: newWorkspaceVars' order (RunVariables has one per key)
		}
		if v.Sensitive {
			out[v.Key] = "(sensitive)"
			continue
		}
		val, err := a.Secrets.Open(v.Value, VariableAD(v))
		if err != nil {
			return nil, fmt.Errorf("can't decrypt variable %s: SECRET_KEY missing or changed? Set the variable again", v.Key)
		}
		out[v.Key] = val
	}
	return out, nil
}

// AsksAll: t's JSON lists all its inputs, so no Terraform variable fills one (VarInputs).
func (a *App) AsksAll(t templates.Template) bool {
	listed := a.listedInputs(t.ID)
	return !slices.ContainsFunc(t.Inputs, func(in templates.Input) bool { return !listed[in.Name] })
}

// listedInputs: the inputs catalog template id's JSON lists, which its form asks; a registry module's other variables
// only appear in the form because the module declares them. None when the template isn't in the catalog.
func (a *App) listedInputs(id string) map[string]bool {
	out := map[string]bool{}
	if a.Templates == nil {
		return out
	}
	if t, ok := a.Templates.Get(id); ok {
		for _, in := range t.Inputs {
			out[in.Name] = true
		}
	}
	return out
}

// newWorkspaceVars are the set variables a new workspace in project gets, in RunVariables' order: priority sets,
// then the project's, then global ones, by name within each (variable_set_rank; a new workspace has no own
// variables, attached sets or repository yet).
func (a *App) newWorkspaceVars(ctx context.Context, project string) ([]storage.Variable, error) {
	sets, err := a.Store.VariableSets(ctx) // by name
	if err != nil {
		return nil, err
	}
	rank := func(s storage.VariableSet) int {
		switch {
		case !s.Global && !slices.Contains(s.Projects, project):
			return -1 // doesn't apply
		case s.Priority:
			return 0
		case slices.Contains(s.Projects, project):
			return 1
		}
		return 2
	}
	sets = slices.DeleteFunc(sets, func(s storage.VariableSet) bool { return rank(s) < 0 })
	slices.SortStableFunc(sets, func(x, y storage.VariableSet) int { return rank(x) - rank(y) })
	var out []storage.Variable
	for _, s := range sets {
		vs, err := a.Store.Variables(ctx, storage.VarOwner{SetID: s.ID})
		if err != nil {
			return nil, err
		}
		out = append(out, vs...)
	}
	return out, nil
}
