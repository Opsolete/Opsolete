package storage

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"strings"
	"testing"
)

func TestProjects(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	w, _ := newWorkspace(t, s, false)
	if w.ProjectName != "default" {
		t.Fatalf("new workspace lands in the default project: %q", w.ProjectName)
	}
	var orphans int
	if err := s.DB.QueryRowContext(ctx, `SELECT count(*) FROM workspaces WHERE project_id IS DISTINCT FROM
		(SELECT id FROM projects WHERE name = 'default')`).Scan(&orphans); err != nil || orphans != 0 {
		t.Fatalf("existing workspaces outside the default project: %d %v", orphans, err)
	}
	def, err := s.Project(ctx, "default")
	if err != nil || !def.Default || def.Workspaces == 0 || def.ID != w.ProjectID {
		t.Fatalf("default project: %+v %v", def, err)
	}
	if err := s.DeleteProject(ctx, def.ID); !errors.Is(err, ErrDefaultProject) {
		t.Fatalf("deleting the default project: %v", err)
	}

	name := "p-" + strings.ToLower(rand.Text()[:10]) // the test DB is reused
	p, err := s.CreateProject(ctx, name, "d")
	if err != nil || p.Name != name || p.Description != "d" || p.Default || p.Workspaces != 0 {
		t.Fatalf("create: %+v %v", p, err)
	}
	t.Cleanup(func() { s.DB.Exec(`DELETE FROM projects WHERE id = $1`, p.ID) })
	if _, err := s.CreateProject(ctx, name, ""); !errors.Is(err, ErrProjectNameTaken) {
		t.Fatalf("duplicate name: %v", err)
	}
	if err := s.UpdateProject(ctx, p.ID, name+"-x", "e"); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateProject(ctx, p.ID, "default", ""); !errors.Is(err, ErrProjectNameTaken) {
		t.Fatalf("rename onto a taken name: %v", err)
	}

	if err := s.MoveWorkspace(ctx, w.ID, p.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.MoveWorkspace(ctx, w.ID, "nope"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("move to an unknown project: %v", err)
	}
	if w, err = s.GetWorkspaceByID(ctx, w.ID); err != nil || w.ProjectName != name+"-x" {
		t.Fatalf("moved workspace: %q %v", w.ProjectName, err)
	}
	if err := s.DeleteProject(ctx, p.ID); !errors.Is(err, ErrProjectNotEmpty) {
		t.Fatalf("deleting a non-empty project: %v", err)
	}

	if list, err := s.Projects(ctx, Viewer{UserID: "00000000-0000-0000-0000-000000000000"}); err != nil {
		t.Fatal(err)
	} else {
		for _, x := range list {
			if x.ID == p.ID {
				t.Fatal("a stranger sees the project")
			}
		}
	}

	if err := s.MoveWorkspace(ctx, w.ID, def.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteProject(ctx, p.ID); err != nil {
		t.Fatalf("deleting an empty project: %v", err)
	}
	if err := s.DeleteProject(ctx, p.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("deleting it again: %v", err)
	}
}
