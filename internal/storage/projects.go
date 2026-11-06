package storage

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

var (
	ErrProjectNameTaken = errors.New("a project with this name already exists")
	ErrProjectNotEmpty  = errors.New("the project still holds workspaces: move or delete them first")
	ErrDefaultProject   = errors.New("the default project can't be deleted: make another project the default first")
)

// Project is a folder of workspaces; its grants and variable sets reach all of them.
type Project struct {
	ID          string
	Name        string
	Description string
	Default     bool // new workspaces land here unless another project is picked
	Workspaces  int  // all of them, also those the viewer can't see
	Grants      int  // group grants on the project
	CreatedAt   time.Time
}

const projectCols = `p.id, p.name, p.description, p.id = (SELECT default_project_id FROM organization),
	(SELECT count(*) FROM workspaces w WHERE w.project_id = p.id),
	(SELECT count(*) FROM project_access a WHERE a.project_id = p.id), p.created_at`

func scanProject(row interface{ Scan(...any) error }) (Project, error) {
	var p Project
	err := row.Scan(&p.ID, &p.Name, &p.Description, &p.Default, &p.Workspaces, &p.Grants, &p.CreatedAt)
	return p, err
}

// Projects lists, by name, the projects v may see: those with a workspace v sees, or a grant to one of v's groups.
func (s *Store) Projects(ctx context.Context, v Viewer) ([]Project, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT `+projectCols+` FROM projects p
		WHERE $2 OR EXISTS (SELECT 1 FROM workspaces w WHERE w.project_id = p.id AND `+visible+`)
			OR EXISTS (SELECT 1 FROM project_access a JOIN users u ON u.id = NULLIF($1, '')::uuid
				WHERE a.project_id = p.id AND a.group_name = ANY(u.groups))
		ORDER BY p.name`, v.UserID, v.Admin)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Project
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Project returns sql.ErrNoRows for an unknown name. Callers check visibility.
func (s *Store) Project(ctx context.Context, name string) (Project, error) {
	return scanProject(s.DB.QueryRowContext(ctx, `SELECT `+projectCols+` FROM projects p WHERE p.name = $1`, name))
}

func (s *Store) CreateProject(ctx context.Context, name, description string) (Project, error) {
	var id string
	err := s.DB.QueryRowContext(ctx, `INSERT INTO projects (name, description) VALUES ($1, $2) RETURNING id`, name, description).Scan(&id)
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && pgErr.Code == "23505" {
		return Project{}, ErrProjectNameTaken
	}
	if err != nil {
		return Project{}, err
	}
	return scanProject(s.DB.QueryRowContext(ctx, `SELECT `+projectCols+` FROM projects p WHERE p.id = $1`, id))
}

// UpdateProject renames a project and sets its description; sql.ErrNoRows for an unknown id.
func (s *Store) UpdateProject(ctx context.Context, id, name, description string) error {
	res, err := s.DB.ExecContext(ctx, `UPDATE projects SET name = $2, description = $3 WHERE id = ops_uuid($1)`, id, name, description)
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && pgErr.Code == "23505" {
		return ErrProjectNameTaken
	}
	return affected(res, err)
}

// DeleteProject deletes an empty project with its grants and set attachments. It refuses one that
// holds workspaces (ErrProjectNotEmpty) and the default project (ErrDefaultProject): the foreign keys decide.
func (s *Store) DeleteProject(ctx context.Context, id string) error {
	res, err := s.DB.ExecContext(ctx, `DELETE FROM projects WHERE id = ops_uuid($1)`, id)
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && pgErr.Code == "23503" {
		if pgErr.ConstraintName == "organization_default_project_id_fkey" {
			return ErrDefaultProject
		}
		return ErrProjectNotEmpty
	}
	return affected(res, err)
}

// MoveWorkspace puts a workspace into another project; sql.ErrNoRows if either is unknown.
// Allowed during a run: project variable sets are resolved when a job is claimed.
func (s *Store) MoveWorkspace(ctx context.Context, workspaceID, projectID string) error {
	res, err := s.DB.ExecContext(ctx, `UPDATE workspaces w SET project_id = p.id, updated_at = now()
		FROM projects p WHERE w.id = ops_uuid($1) AND p.id = ops_uuid($2)`, workspaceID, projectID)
	return affected(res, err)
}

// affected turns "no row matched" into sql.ErrNoRows.
func affected(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}
