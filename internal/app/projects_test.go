package app

import (
	"context"
	"errors"
	"testing"
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
