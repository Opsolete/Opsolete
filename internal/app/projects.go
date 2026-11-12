package app

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/opsolete/opsolete/internal/storage"
)

// Projects group workspaces; their grants and variable sets reach all of them. Everyone sees the
// projects storage.Projects lists for them; admins create, change and delete them and move workspaces.

// Projects lists, by name, the projects actor may see (see storage.Projects).
func (a *App) Projects(ctx context.Context, actor Actor) ([]storage.Project, error) {
	return a.Store.Projects(ctx, actor.viewer())
}

// Project loads the named project if actor may see it, else ErrNotFound.
func (a *App) Project(ctx context.Context, actor Actor, name string) (storage.Project, error) {
	list, err := a.Projects(ctx, actor)
	if err != nil {
		return storage.Project{}, err
	}
	i := slices.IndexFunc(list, func(p storage.Project) bool { return p.Name == name })
	if i < 0 {
		return storage.Project{}, ErrNotFound
	}
	return list[i], nil
}

// ProjectForSettings is Project for its settings pages: ErrForbidden unless actor is an admin, as
// only admins change them (ErrNotFound first if actor can't see it).
func (a *App) ProjectForSettings(ctx context.Context, actor Actor, name string) (storage.Project, error) {
	p, err := a.Project(ctx, actor, name)
	if err == nil && !actor.Admin {
		return storage.Project{}, errAdminsOnly
	}
	return p, err
}

// adminProject is Project for an admin: ErrForbidden for anyone else.
func (a *App) adminProject(ctx context.Context, actor Actor, name string) (storage.Project, error) {
	if !actor.Admin {
		return storage.Project{}, errAdminsOnly
	}
	return a.Project(ctx, actor, name)
}

// checkProject is the rule for a project's name (the workspace name rule: both are URL keys) and description.
func checkProject(name, description string) error {
	if !ValidName(name) {
		return invalid("name", "a project name is "+NameRule)
	}
	if utf8.RuneCountInString(description) > 1000 {
		return invalid("description", "the description is at most 1000 characters")
	}
	return nil
}

// CreateProject creates a project. Admins only; ValidationError: name or description; conflict: the name is taken.
func (a *App) CreateProject(ctx context.Context, actor Actor, name, description string) (storage.Project, error) {
	if !actor.Admin {
		return storage.Project{}, errAdminsOnly
	}
	name, description = strings.TrimSpace(name), strings.TrimSpace(description)
	if err := checkProject(name, description); err != nil {
		return storage.Project{}, err
	}
	p, err := a.Store.CreateProject(ctx, name, description)
	if errors.Is(err, storage.ErrProjectNameTaken) {
		return p, invalid("name", err.Error())
	}
	if err != nil {
		return p, err
	}
	a.Log.Info("project created", "project", p.Name, "user", actor.Email)
	return p, nil
}

// ProjectSettings is a project's General page.
type ProjectSettings struct {
	Name, Description string
	Default           bool   // make it the default project (unsetting one isn't possible: pick another)
	RunnerPool        string // only its runners get the project's jobs; "": storage.DefaultPool
}

// UpdateProject renames a project, sets its description and maybe makes it the default. Admins only.
func (a *App) UpdateProject(ctx context.Context, actor Actor, project string, set ProjectSettings) error {
	p, err := a.adminProject(ctx, actor, project)
	if err != nil {
		return err
	}
	name, description := strings.TrimSpace(set.Name), strings.TrimSpace(set.Description)
	if err := checkProject(name, description); err != nil {
		return err
	}
	pool := cmp.Or(strings.TrimSpace(set.RunnerPool), storage.DefaultPool)
	if !ValidName(pool) {
		return invalid("runner_pool", "a runner pool is "+NameRule)
	}
	if err := a.Store.UpdateProject(ctx, p.ID, name, description, pool); errors.Is(err, storage.ErrProjectNameTaken) {
		return invalid("name", err.Error())
	} else if err != nil {
		return err
	}
	if set.Default && !p.Default {
		if err := a.Store.SetDefaultProject(ctx, p.ID); err != nil {
			return err
		}
	}
	a.Log.Info("project updated", "project", p.Name, "name", name, "default", set.Default, "runner_pool", pool, "user", actor.Email)
	return nil
}

// DeleteProject deletes an empty project that isn't the default (conflicts otherwise). Admins only.
func (a *App) DeleteProject(ctx context.Context, actor Actor, project string) error {
	p, err := a.adminProject(ctx, actor, project)
	if err != nil {
		return err
	}
	if err := a.Store.DeleteProject(ctx, p.ID); err != nil && !errors.Is(err, storage.ErrNotFound) { // gone meanwhile: deleted all the same
		return err
	}
	a.Log.Info("project deleted", "project", p.Name, "user", actor.Email)
	return nil
}

// ProjectAccess lists a project's grants (ProjectGrants) and, for admins, the groups to grant;
// anyone who sees the project may read them.
func (a *App) ProjectAccess(ctx context.Context, actor Actor, project string) (Access, error) {
	var acc Access
	p, err := a.Project(ctx, actor, project)
	if err != nil {
		return acc, err
	}
	if acc.ProjectGrants, err = a.Store.ProjectGrants(ctx, p.ID); err != nil {
		return acc, err
	}
	if actor.Admin {
		acc.Groups, err = a.Store.KnownGroups(ctx)
	}
	return acc, err
}

// SetProjectGrant gives an IdP group read or operate access to all of a project's workspaces; level ""
// revokes (see ParseGrant). Admins only.
func (a *App) SetProjectGrant(ctx context.Context, actor Actor, project, group, level string) error {
	p, err := a.adminProject(ctx, actor, project)
	if err != nil {
		return err
	}
	group, l, err := ParseGrant(group, level)
	if err != nil {
		return err
	}
	if err := a.Store.SetProjectGrant(ctx, p.ID, group, l.String()); err != nil {
		return err
	}
	a.Log.Info("project access", "project", p.Name, "group", group, "level", l.String(), "user", actor.Email)
	return nil
}

// ProjectVariableSets lists the sets attached to a project; anyone who sees the project may read them.
func (a *App) ProjectVariableSets(ctx context.Context, actor Actor, project string) ([]storage.VariableSet, error) {
	p, err := a.Project(ctx, actor, project)
	if err != nil {
		return nil, err
	}
	return a.Store.ProjectVariableSets(ctx, p.ID)
}

// MoveWorkspace puts a workspace into another project, also during a run (see storage.MoveWorkspace).
// Admins only; ValidationError: no such project.
func (a *App) MoveWorkspace(ctx context.Context, actor Actor, workspace, project string) error {
	if !actor.Admin {
		return errAdminsOnly
	}
	ws, _, err := a.Workspace(ctx, actor, workspace, LevelOperate)
	if err != nil {
		return err
	}
	p, err := a.Store.Project(ctx, project)
	if errors.Is(err, storage.ErrNotFound) {
		return invalid("project", "no such project")
	}
	if err != nil {
		return err
	}
	if err := a.Store.MoveWorkspace(ctx, ws.ID, p.ID); err != nil && !errors.Is(err, storage.ErrNotFound) {
		return err
	}
	a.Log.Info("workspace moved", "workspace", ws.Name, "from", ws.ProjectName, "to", p.Name, "user", actor.Email)
	return nil
}
