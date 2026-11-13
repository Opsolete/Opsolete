package app

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"

	"github.com/opsolete/opsolete/internal/storage"
)

// Variable sets: reusable groups of variables applied to all workspaces (global) or to the ones and
// the projects' workspaces they're attached to. Admins only, every operation: variableSet is the rule.

// variableSet loads the named set for an admin: ErrForbidden for anyone else, ErrNotFound if there's none.
func (a *App) variableSet(ctx context.Context, actor Actor, name string) (storage.VariableSet, error) {
	if !actor.Admin {
		return storage.VariableSet{}, errAdminsOnly
	}
	return a.Store.GetVariableSet(ctx, name)
}

// VariableSets lists every variable set. Admins only.
func (a *App) VariableSets(ctx context.Context, actor Actor) ([]storage.VariableSet, error) {
	if !actor.Admin {
		return nil, errAdminsOnly
	}
	return a.Store.VariableSets(ctx)
}

// VariableSet is a set with its variables (sensitive values never returned). Admins only.
func (a *App) VariableSet(ctx context.Context, actor Actor, name string) (SetVariables, error) {
	set, err := a.variableSet(ctx, actor, name)
	if err != nil {
		return SetVariables{}, err
	}
	vars, err := a.variables(ctx, storage.VarOwner{SetID: set.ID})
	return SetVariables{set, vars}, err
}

// NewVariableSet is a set to create; created from a project's or workspace's page (Project or
// Workspace, by name) it's attached there too.
type NewVariableSet struct {
	Name, Description string
	Global            bool
	Project           string
	Workspace         string
}

// CreateVariableSet creates a set, attached to in.Project or in.Workspace if one is named. Admins only.
// ValidationError: the name, or an unknown project or workspace (checked first: no set is left behind).
func (a *App) CreateVariableSet(ctx context.Context, actor Actor, in NewVariableSet) error {
	if !actor.Admin {
		return errAdminsOnly
	}
	name := strings.TrimSpace(in.Name)
	if !ValidName(name) && name != "new" { // the name rule, "new" allowed: no /variable-sets/new page
		return invalid("name", "a variable set name is 2–63 lowercase letters, digits and dashes")
	}
	attach := func(uuid.UUID) error { return nil }
	exists, err := true, error(nil)
	switch {
	case in.Project != "":
		exists, err = a.Store.ProjectsExist(ctx, []string{in.Project})
		attach = func(id uuid.UUID) error { return a.Store.AttachVariableSetProject(ctx, id, in.Project, true) }
	case in.Workspace != "":
		exists, err = a.Store.WorkspacesExist(ctx, []string{in.Workspace})
		attach = func(id uuid.UUID) error { return a.Store.AttachVariableSet(ctx, id, in.Workspace, true) }
	}
	if err != nil {
		return err
	}
	if !exists {
		return &ValidationError{Message: "no such project or workspace"}
	}
	err = a.Store.CreateVariableSet(ctx, name, strings.TrimSpace(in.Description), in.Global)
	if errors.Is(err, storage.ErrExists) {
		return invalid("name", "a variable set with this name already exists")
	}
	if err != nil {
		return err
	}
	set, err := a.Store.GetVariableSet(ctx, name)
	if err == nil {
		err = attach(set.ID)
	}
	if err != nil {
		return err
	}
	a.Log.Info("variable set created", "set", name, "project", in.Project, "workspace", in.Workspace, "user", actor.Email)
	return nil
}

// UpdateVariableSet sets a set's description and whether it applies to all workspaces. Admins only.
func (a *App) UpdateVariableSet(ctx context.Context, actor Actor, name, description string, global, priority bool) error {
	set, err := a.variableSet(ctx, actor, name)
	if err != nil {
		return err
	}
	if err := a.Store.UpdateVariableSet(ctx, set.ID, strings.TrimSpace(description), global, priority); err != nil {
		return err
	}
	a.Log.Info("variable set updated", "set", set.Name, "global", global, "user", actor.Email)
	return nil
}

// DeleteVariableSet deletes a set with its variables. Admins only.
func (a *App) DeleteVariableSet(ctx context.Context, actor Actor, name string) error {
	set, err := a.variableSet(ctx, actor, name)
	if err != nil {
		return err
	}
	if err := a.Store.DeleteVariableSet(ctx, set.ID); err != nil {
		return err
	}
	a.Log.Info("variable set deleted", "set", set.Name, "user", actor.Email)
	return nil
}

// SetSetVariable adds or replaces (by category and key) a set's variable. Admins only.
func (a *App) SetSetVariable(ctx context.Context, actor Actor, name string, in VariableInput) error {
	set, err := a.variableSet(ctx, actor, name)
	if err != nil {
		return err
	}
	o := storage.VarOwner{SetID: set.ID}
	v, err := a.sealVariable(o, in)
	if err != nil {
		return err
	}
	if err := a.Store.SetVariable(ctx, o, v); err != nil {
		return err
	}
	a.Log.Info("variable set", "set", set.Name, "category", v.Category, "key", v.Key, "user", actor.Email)
	return nil
}

// DeleteSetVariable deletes a set's variable; an unknown id deletes nothing. Admins only.
func (a *App) DeleteSetVariable(ctx context.Context, actor Actor, name string, id uuid.UUID) error {
	set, err := a.variableSet(ctx, actor, name)
	if err != nil {
		return err
	}
	if err := a.Store.DeleteVariable(ctx, storage.VarOwner{SetID: set.ID}, id); err != nil {
		return err
	}
	a.Log.Info("variable deleted", "set", set.Name, "id", id, "user", actor.Email)
	return nil
}

// SelectSetWorkspaces applies the set to exactly the named workspaces. Admins only;
// ValidationError: a name isn't a workspace.
func (a *App) SelectSetWorkspaces(ctx context.Context, actor Actor, name string, workspaces []string) error {
	return a.selectSet(ctx, actor, name, storage.Pick{Selection: storage.SetWorkspaces, Names: workspaces}, "workspace", "choose existing workspaces")
}

// SelectSetProjects applies the set to the workspaces of exactly the named projects. Admins only;
// ValidationError: a name isn't a project.
func (a *App) SelectSetProjects(ctx context.Context, actor Actor, name string, projects []string) error {
	return a.selectSet(ctx, actor, name, storage.Pick{Selection: storage.SetProjects, Names: projects}, "project", "choose existing projects")
}

func (a *App) selectSet(ctx context.Context, actor Actor, name string, p storage.Pick, field, unknown string) error {
	set, err := a.variableSet(ctx, actor, name)
	if err != nil {
		return err
	}
	err = a.Store.Select(ctx, set.ID, p)
	if errors.Is(err, storage.ErrNotFound) {
		return invalid(field, unknown)
	}
	if err != nil {
		return err
	}
	a.Log.Info("variable set "+field+"s", "set", set.Name, field+"s", p.Names, "user", actor.Email)
	return nil
}
