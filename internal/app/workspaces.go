package app

import (
	"context"
	"errors"
	"regexp"
	"slices"
	"strings"

	"github.com/google/uuid"
	"gocloud.dev/gcerrors"

	"github.com/opsolete/opsolete/internal/storage"
)

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,62}$`)

// NameRule says what ValidName accepts; project names follow it too.
const NameRule = "2–63 lowercase letters, digits and dashes, starting with a letter or digit (not \"new\")"

// ValidName: a workspace or project name, a URL key. "new" is reserved for /workspaces/new.
func ValidName(name string) bool { return nameRe.MatchString(name) && name != "new" }

// ListWorkspaces lists the workspaces actor may see; project (a name) limits them to one, "" lists all.
func (a *App) ListWorkspaces(ctx context.Context, actor Actor, project string) ([]storage.Workspace, error) {
	return a.Store.ListWorkspaces(ctx, actor.viewer(), project)
}

// CreatableProjects lists the projects actor may create workspaces in.
func (a *App) CreatableProjects(ctx context.Context, actor Actor) ([]storage.Project, error) {
	return a.Store.CreatableProjects(ctx, actor.viewer())
}

// NewWorkspaceProject is the project a new workspace of actor goes into: the one named (or with
// id), else the default. One actor may not create workspaces in is a ValidationError (FieldProject).
func (a *App) NewWorkspaceProject(ctx context.Context, actor Actor, name string, id uuid.UUID) (storage.Project, error) {
	if name == "" && id == uuid.Nil {
		return a.Store.DefaultProject(ctx)
	}
	list, err := a.CreatableProjects(ctx, actor)
	if err != nil {
		return storage.Project{}, err
	}
	i := slices.IndexFunc(list, func(p storage.Project) bool { return p.Name == name || id != uuid.Nil && p.ID == id })
	if i < 0 {
		return storage.Project{}, invalid(FieldProject, "no such project, or you may not create workspaces in it: that needs operate access to it")
	}
	return list[i], nil
}

// NewWorkspace is a workspace to create from a template.
type NewWorkspace struct {
	Name       string
	Project    string // by name; "" is the default project
	AutoApply  bool
	TemplateID string
	Inputs     map[string]string // the template's inputs as typed, by input name
}

// CreateWorkspace creates a workspace with the template added to it and queues its first plan.
// ErrNotFound: no such template; ValidationError: the name, project or inputs (Fields by input name).
func (a *App) CreateWorkspace(ctx context.Context, actor Actor, in NewWorkspace) (storage.Workspace, storage.Run, error) {
	t, err := a.Template(ctx, in.TemplateID)
	if err != nil {
		return storage.Workspace{}, storage.Run{}, err
	}
	vals, errs := t.Validate(in.Inputs) // errs is never nil
	if !ValidName(in.Name) {
		errs[FieldName] = NameRule
	}
	id := uuid.New() // sensitive inputs are sealed to the workspace's id and module name
	module := defaultModule(t, in.Inputs, nil)
	if err := a.SealInputs(t, in.Inputs, vals, nil, errs, id, module); err != nil {
		return storage.Workspace{}, storage.Run{}, err
	}
	if len(errs) > 0 {
		return storage.Workspace{}, storage.Run{}, &ValidationError{Fields: errs}
	}
	p, err := a.NewWorkspaceProject(ctx, actor, in.Project, uuid.Nil)
	if err != nil {
		return storage.Workspace{}, storage.Run{}, err
	}
	ws, run, err := a.Store.CreateWorkspace(ctx, id, in.Name, p.Name, storage.WorkspaceTemplate{Name: module, TemplateID: t.ID,
		ModuleSource: t.Module.Source, ModuleRef: t.Module.Ref, Inputs: vals}, in.AutoApply, actor.UserID)
	if errors.Is(err, storage.ErrNameTaken) {
		return ws, run, invalid(FieldName, err.Error())
	}
	if err != nil {
		return ws, run, err
	}
	a.Log.Info("workspace created", "workspace", ws.Name, "template", t.ID, "run", run.ID, "user", actor.Email)
	return ws, run, nil
}

// EmptyWorkspace is a workspace to create without a template and without a run.
type EmptyWorkspace struct {
	Name          string
	Project       string    // by name; "" is the default project
	ProjectID     uuid.UUID // by id instead (the cloud API)
	AutoApply     bool
	ExecutionMode string           // "" is remote
	VCS           *storage.VCSLink // already checked by the caller (connection allowed, branch readable)
}

// CreateEmptyWorkspace creates a workspace with its execution mode and VCS link, all or nothing.
// ValidationError: the name (FieldName, also when taken), project (FieldProject) or execution mode.
func (a *App) CreateEmptyWorkspace(ctx context.Context, actor Actor, in EmptyWorkspace) (storage.Workspace, error) {
	if !ValidName(in.Name) {
		return storage.Workspace{}, invalid(FieldName, "a workspace name is "+NameRule)
	}
	if !slices.Contains([]string{"", "remote", "local"}, in.ExecutionMode) {
		return storage.Workspace{}, invalid("execution_mode", "execution mode is remote or local")
	}
	p, err := a.NewWorkspaceProject(ctx, actor, in.Project, in.ProjectID)
	if err != nil {
		return storage.Workspace{}, err
	}
	ws, err := a.Store.CreateEmptyWorkspace(ctx, storage.EmptyWorkspace{Name: in.Name, Project: p.Name, AutoApply: in.AutoApply,
		ExecutionMode: in.ExecutionMode, VCS: in.VCS, CreatedBy: actor.UserID})
	if errors.Is(err, storage.ErrNameTaken) {
		return ws, invalid(FieldName, err.Error())
	}
	if err != nil {
		return ws, err
	}
	var repo string
	if in.VCS != nil {
		repo = in.VCS.Repo
	}
	a.Log.Info("workspace created", "workspace", ws.Name, "project", ws.ProjectName, "repo", repo, "mode", ws.ExecutionMode, "user", actor.Email)
	return ws, nil
}

// SaveSettings saves a workspace's General or Health page (see storage.Settings), all or nothing, and
// returns the workspace as saved. A new name (set.Name; "" or the current one keeps it) changes the URL;
// state, runs and grants stay (keyed by id). ErrNameTaken: the new name is in use.
func (a *App) SaveSettings(ctx context.Context, actor Actor, workspace string, set storage.Settings) (storage.Workspace, error) {
	ws, _, err := a.Workspace(ctx, actor, workspace, LevelOperate)
	if err != nil {
		return ws, err
	}
	if !slices.Contains([]string{"", "remote", "local"}, set.ExecutionMode) {
		return ws, invalid("execution_mode", "execution mode is remote or local")
	}
	if set.Name = strings.TrimSpace(set.Name); set.Name == ws.Name {
		set.Name = ""
	}
	if set.Name != "" && !ValidName(set.Name) {
		return ws, invalid("name", "a workspace name is "+NameRule)
	}
	if err := a.Store.SaveSettings(ctx, ws.ID, set); err != nil {
		return ws, err
	}
	if set.Name != "" {
		a.Log.Info("workspace renamed", "from", ws.Name, "to", set.Name, "user", actor.Email)
		ws.Name = set.Name
	}
	return ws, nil
}

// DeleteWorkspace deletes a workspace that manages no resources, then its state files; force deletes
// it anyway, leaving any real resources unmanaged. Conflicts: an active run, resources, a lock.
func (a *App) DeleteWorkspace(ctx context.Context, actor Actor, workspace string, force bool) error {
	ws, _, err := a.Workspace(ctx, actor, workspace, LevelOperate)
	if err != nil {
		return err
	}
	keys, err := a.Store.DeleteWorkspace(ctx, ws.ID, force)
	if err != nil {
		return err
	}
	// Committed: from here failures only leave an unreferenced file behind, never fail the delete.
	for _, k := range keys {
		if err := a.Blobs.Delete(context.WithoutCancel(ctx), k); err != nil && gcerrors.Code(err) != gcerrors.NotFound {
			a.Log.Warn("delete state file", "workspace", ws.Name, "key", k, "err", err)
		}
	}
	a.Log.Info("workspace deleted", "workspace", ws.Name, "id", ws.ID, "force", force, "user", actor.Email)
	return nil
}

// ParseGrant checks a group grant as a form or API gives it; level "" (LevelNone) revokes.
func ParseGrant(group, level string) (string, Level, error) {
	l, ok := ParseLevel(level)
	if group = strings.TrimSpace(group); group == "" || len(group) > 256 || !ok {
		return "", LevelNone, &ValidationError{Message: "group is required, and level must be read, operate or empty (revoke)"}
	}
	return group, l, nil
}

// SetGrant gives an IdP group read or operate access to a workspace; level "" revokes (see ParseGrant). Admins only.
func (a *App) SetGrant(ctx context.Context, actor Actor, workspace, group, level string) error {
	if !actor.Admin {
		return errAdminsOnly
	}
	group, l, err := ParseGrant(group, level)
	if err != nil {
		return err
	}
	ws, _, err := a.Workspace(ctx, actor, workspace, LevelOperate)
	if err != nil {
		return err
	}
	if err := a.Store.SetGrant(ctx, ws.ID, group, l.String()); err != nil {
		return err
	}
	a.Log.Info("workspace access", "workspace", ws.Name, "group", group, "level", l.String(), "user", actor.Email)
	return nil
}

// Access is who may use a workspace besides admins and its creator, for its Access page (or a
// project's: then only ProjectGrants and Groups).
type Access struct {
	Grants        []storage.Grant // the workspace's own
	ProjectGrants []storage.Grant // its project's
	Groups        []string        // the groups known from logins, to grant; admins only (the only ones who grant)
}

// Access lists a workspace's grants; anyone who sees the workspace may read them.
func (a *App) Access(ctx context.Context, actor Actor, workspace string) (Access, error) {
	var acc Access
	ws, _, err := a.Workspace(ctx, actor, workspace, LevelRead)
	if err != nil {
		return acc, err
	}
	if acc.Grants, err = a.Store.Grants(ctx, ws.ID); err != nil {
		return acc, err
	}
	if acc.ProjectGrants, err = a.Store.ProjectGrants(ctx, ws.ProjectID); err != nil {
		return acc, err
	}
	if actor.Admin {
		acc.Groups, err = a.Store.KnownGroups(ctx)
	}
	return acc, err
}
