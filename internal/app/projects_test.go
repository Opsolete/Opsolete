package app

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

func TestProjects(t *testing.T) {
	a, ctx := testApp(t), context.Background()
	group := uniq("grp-")
	member, stranger := user(t, a, group), user(t, a)
	name := uniq("prj-")
	t.Cleanup(func() { a.Store.Pool.Exec(context.Background(), `DELETE FROM projects WHERE name = $1`, name) })
	if _, err := a.CreateProject(ctx, member, name, ""); !errors.Is(err, ErrForbidden) {
		t.Errorf("a user creates a project: %v", err)
	}
	if _, err := a.CreateProject(ctx, admin, "Not Valid", ""); !isValidation(err) {
		t.Errorf("an invalid name: %v", err)
	}
	if _, err := a.CreateProject(ctx, admin, name, ""); err != nil {
		t.Fatal(err)
	}
	_, err := a.CreateProject(ctx, admin, name, "")
	if ve, ok := errors.AsType[*ValidationError](err); !ok || ve.Fields["name"] == "" {
		t.Errorf("a taken name: %v", err)
	}
	ws := emptyWorkspace(t, a, admin)
	if err := a.MoveWorkspace(ctx, member, ws.Name, name); !errors.Is(err, ErrForbidden) {
		t.Errorf("a user moves a workspace: %v", err)
	}
	if err := a.MoveWorkspace(ctx, admin, ws.Name, uniq("nope-")); !isValidation(err) {
		t.Errorf("move to an unknown project: %v", err)
	}
	if err := a.MoveWorkspace(ctx, admin, ws.Name, name); err != nil {
		t.Fatal(err)
	}

	// a project grant reveals its workspaces; revoked, they're gone again
	if _, err := a.Project(ctx, member, name); !errors.Is(err, ErrNotFound) {
		t.Errorf("a project without a grant: %v", err)
	}
	if _, _, err := a.Workspace(ctx, member, ws.Name, LevelRead); !errors.Is(err, ErrNotFound) {
		t.Errorf("its workspace without a grant: %v", err)
	}
	if err := a.SetProjectGrant(ctx, member, name, group, "read"); !errors.Is(err, ErrForbidden) {
		t.Errorf("a user grants: %v", err)
	}
	if err := a.SetProjectGrant(ctx, admin, name, group, "read"); err != nil {
		t.Fatal(err)
	}
	if _, level, err := a.Workspace(ctx, member, ws.Name, LevelRead); err != nil || level != LevelRead {
		t.Errorf("its workspace through a read grant: %v %v", level, err)
	}
	if _, err := a.Project(ctx, member, name); err != nil {
		t.Errorf("the granted project: %v", err)
	}
	if _, _, err := a.Workspace(ctx, stranger, ws.Name, LevelRead); !errors.Is(err, ErrNotFound) {
		t.Errorf("a stranger: %v", err)
	}
	if err := a.SetProjectGrant(ctx, admin, name, group, ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.Workspace(ctx, member, ws.Name, LevelRead); !errors.Is(err, ErrNotFound) {
		t.Errorf("revoked: %v", err)
	}

	// only an empty project that isn't the default is deleted
	if err := a.DeleteProject(ctx, admin, name); !IsConflict(err) {
		t.Errorf("delete a non-empty project: %v", err)
	}
	def, err := a.Store.DefaultProject(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.DeleteProject(ctx, admin, def.Name); !IsConflict(err) {
		t.Errorf("delete the default project: %v", err)
	}
	if err := a.MoveWorkspace(ctx, admin, ws.Name, def.Name); err != nil {
		t.Fatal(err)
	}
	if err := a.DeleteProject(ctx, member, name); !errors.Is(err, ErrForbidden) {
		t.Errorf("a user deletes a project: %v", err)
	}
	if err := a.DeleteProject(ctx, admin, name); err != nil {
		t.Errorf("delete the emptied project: %v", err)
	}
}

// Admins create workspaces in every project, others only where one of their groups has operate: the
// default project too, named or not. Refusals don't tell whether the project exists.
func TestNewWorkspaceProject(t *testing.T) {
	a, ctx := testApp(t), context.Background()
	readers, operators := uniq("grp-"), uniq("grp-")
	reader, operator := user(t, a, readers), user(t, a, operators)
	name := uniq("prj-")
	t.Cleanup(func() { a.Store.Pool.Exec(context.Background(), `DELETE FROM projects WHERE name = $1`, name) })
	if _, err := a.CreateProject(ctx, admin, name, ""); err != nil {
		t.Fatal(err)
	}
	def, err := a.Store.DefaultProject(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.SetProjectGrant(ctx, admin, name, readers, "read"); err != nil {
		t.Fatal(err)
	}
	if err := a.SetProjectGrant(ctx, admin, name, operators, "operate"); err != nil {
		t.Fatal(err)
	}
	refused := func(what string, actor Actor, project string) {
		t.Helper()
		_, err := a.NewWorkspaceProject(ctx, actor, project, uuid.Nil)
		if ve, ok := errors.AsType[*ValidationError](err); !ok || ve.Fields[FieldProject] != notCreatable {
			t.Errorf("%s: %v", what, err)
		}
	}
	allowed := func(what string, actor Actor, project, want string) {
		t.Helper()
		if p, err := a.NewWorkspaceProject(ctx, actor, project, uuid.Nil); err != nil || p.Name != want {
			t.Errorf("%s: %q %v", what, p.Name, err)
		}
	}
	refused("the default project, unnamed", operator, "")
	refused("the default project, named", operator, def.Name)
	refused("a project with read", reader, name)
	refused("an unknown project", operator, uniq("nope-"))
	allowed("a project with operate", operator, name, name)
	allowed("an admin, unnamed", admin, "", def.Name)
	allowed("an admin, another project", admin, name, name)
	if _, err := a.CreateEmptyWorkspace(ctx, operator, EmptyWorkspace{Name: uniq("app-")}); !isValidation(err) {
		t.Errorf("an empty workspace in the default project: %v", err)
	}

	// an explicit operate grant on the default project: like any other project
	if err := a.SetProjectGrant(ctx, admin, def.Name, operators, "operate"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.SetProjectGrant(context.Background(), admin, def.Name, operators, "") })
	allowed("the default project with operate, unnamed", operator, "", def.Name)
	allowed("the default project with operate, named", operator, def.Name, def.Name)
	refused("the default project, someone else", reader, "")
}
