package storage

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/opsolete/opsolete/internal/storage/db"
)

var (
	ErrProjectNameTaken = errors.New("a project with this name already exists")
	ErrProjectNotEmpty  = errors.New("the project still holds workspaces: move or delete them first")
	ErrDefaultProject   = errors.New("the default project can't be deleted: make another project the default first")
)

// Project is a folder of workspaces; its grants and variable sets reach all of them.
type Project struct {
	ID          uuid.UUID
	Name        string
	Description string
	Default     bool // new workspaces land here unless another project is picked
	Workspaces  int  // all of them, also those the viewer can't see
	Grants      int  // group grants on the project
	CreatedAt   time.Time
	RunnerPool  string // only runners of this pool get its workspaces' jobs
}

// toProject maps a project_list row (001_schema.sql).
func toProject(p db.ProjectList) Project {
	return Project{ID: p.ID, Name: p.Name, Description: p.Description, Default: p.IsDefault, Workspaces: int(p.Workspaces),
		Grants: int(p.Grants), CreatedAt: p.CreatedAt, RunnerPool: p.RunnerPool}
}

func toProjects(rows []db.ProjectList, err error) ([]Project, error) {
	out := make([]Project, 0, len(rows))
	for _, p := range rows {
		out = append(out, toProject(p))
	}
	return out, err
}

// Projects lists, by name, the projects v may see: all for admins, else the default project, those
// with a workspace v sees, and those granted to one of v's groups.
func (s *Store) Projects(ctx context.Context, v Viewer) ([]Project, error) {
	return toProjects(s.q.ListProjects(ctx, db.ListProjectsParams{Admin: v.Admin, Viewer: optID(v.UserID)}))
}

// CreatableProjects lists, by name, the projects v may create workspaces in: all for admins, else
// those granted operate to one of v's groups (the default project too: only admins create workspaces
// there without such a grant). A workspace gets its project's variable sets, so a read grant mustn't be enough.
func (s *Store) CreatableProjects(ctx context.Context, v Viewer) ([]Project, error) {
	return toProjects(s.q.CreatableProjects(ctx, db.CreatableProjectsParams{Admin: v.Admin, Viewer: optID(v.UserID)}))
}

// Project returns ErrNotFound for an unknown name. Callers check visibility.
func (s *Store) Project(ctx context.Context, name string) (Project, error) {
	p, err := s.q.GetProjectByName(ctx, name)
	return toProject(p), notFound(err)
}

// ProjectsExist reports whether there is a project for each name.
func (s *Store) ProjectsExist(ctx context.Context, names []string) (bool, error) {
	return s.q.ProjectsExist(ctx, names)
}

// DefaultProject is the project new workspaces go into unless another is picked.
func (s *Store) DefaultProject(ctx context.Context) (Project, error) {
	p, err := s.q.DefaultProject(ctx)
	return toProject(p), err
}

// CreateProject returns ErrProjectNameTaken if the name is taken.
func (s *Store) CreateProject(ctx context.Context, name, description string) (Project, error) {
	id, err := s.q.CreateProject(ctx, db.CreateProjectParams{Name: name, Description: description})
	if isUnique(err) {
		return Project{}, ErrProjectNameTaken
	}
	if err != nil {
		return Project{}, err
	}
	p, err := s.q.GetProjectByID(ctx, id)
	return toProject(p), err
}

// UpdateProject renames a project and sets its description and runner pool; ErrNotFound for an unknown id.
func (s *Store) UpdateProject(ctx context.Context, id uuid.UUID, name, description, runnerPool string) error {
	err := affected(s.q.UpdateProject(ctx, db.UpdateProjectParams{ID: id, Name: name, Description: description, RunnerPool: runnerPool}))
	if isUnique(err) {
		return ErrProjectNameTaken
	}
	return err
}

// SetDefaultProject makes a project the one new workspaces land in; ErrNotFound for an unknown id.
func (s *Store) SetDefaultProject(ctx context.Context, id uuid.UUID) error {
	return affected(s.q.SetDefaultProject(ctx, id))
}

// DeleteProject deletes an empty project with its grants and set attachments. It refuses one that
// holds workspaces (ErrProjectNotEmpty) and the default project (ErrDefaultProject): the foreign keys
// decide. ErrNotFound for an unknown id.
func (s *Store) DeleteProject(ctx context.Context, id uuid.UUID) error {
	err := affected(s.q.DeleteProject(ctx, id))
	if constraint, ok := fkViolation(err); ok {
		if constraint == "organization_default_project_id_fkey" {
			return ErrDefaultProject
		}
		return ErrProjectNotEmpty
	}
	return err
}

// MoveWorkspace puts a workspace into another project; ErrNotFound if either is unknown.
// Allowed during a run: project variable sets are resolved when a job is claimed.
func (s *Store) MoveWorkspace(ctx context.Context, workspaceID, projectID uuid.UUID) error {
	return affected(s.q.MoveWorkspace(ctx, db.MoveWorkspaceParams{WorkspaceID: workspaceID, ProjectID: projectID}))
}

// ProjectGrants lists a project's group grants; they reach all of its workspaces.
func (s *Store) ProjectGrants(ctx context.Context, projectID uuid.UUID) ([]Grant, error) {
	rows, err := s.q.ProjectGrants(ctx, projectID)
	out := make([]Grant, 0, len(rows))
	for _, g := range rows {
		out = append(out, Grant{Group: g.GroupName, Level: g.Level})
	}
	return out, err
}

// SetProjectGrant gives a group read or operate access to all of a project's workspaces; level "" removes it.
func (s *Store) SetProjectGrant(ctx context.Context, projectID uuid.UUID, group, level string) error {
	if level == "" {
		return s.q.DeleteProjectGrant(ctx, db.DeleteProjectGrantParams{ProjectID: projectID, GroupName: group})
	}
	return s.q.SetProjectGrant(ctx, db.SetProjectGrantParams{ProjectID: projectID, GroupName: group, Level: level})
}
