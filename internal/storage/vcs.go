package storage

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

// VCSConnection is a VCS provider account; Token is sealed (see internal/vcs).
type VCSConnection struct {
	ID, Name, Provider, BaseURL string
	Token                       []byte
	WorkspaceScope              string   // workspaces that may link its repositories: "" none, "all" or "selected"
	Workspaces                  []string // names of the selected workspaces
	Projects                    []string // names of the selected projects: all their workspaces may link
	CreatedAt                   time.Time
	// what uses it, for the VCS providers page
	LinkedWorkspaces, LinkedModules int
	Catalog                         bool // the template catalog's repository
}

// Allows reports whether the named workspace ("" for one being created) in the named project may
// link the connection's repositories.
func (c VCSConnection) Allows(workspace, project string) bool {
	return c.WorkspaceScope == "all" || c.WorkspaceScope == "selected" &&
		(workspace != "" && slices.Contains(c.Workspaces, workspace) || slices.Contains(c.Projects, project))
}

// ResealVCSToken replaces a connection's sealed token, unless it changed since it was read
// (sql.ErrNoRows, also for a deleted connection).
func (s *Store) ResealVCSToken(ctx context.Context, id string, old, sealed []byte) error {
	return affected(s.DB.ExecContext(ctx, `UPDATE vcs_connections SET token = $3 WHERE id = $1::uuid AND token = $2`, id, old, sealed))
}

// CreateVCSConnection returns the new connection's id.
func (s *Store) CreateVCSConnection(ctx context.Context, c VCSConnection) (string, error) {
	var id string
	err := s.DB.QueryRowContext(ctx, `INSERT INTO vcs_connections (name, provider, base_url, token)
		VALUES ($1, $2, $3, $4) RETURNING id`, c.Name, c.Provider, c.BaseURL, c.Token).Scan(&id)
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && pgErr.Code == "23505" {
		return "", ErrExists
	}
	return id, err
}

const connCols = `c.id, c.name, c.provider, c.base_url, c.token, c.workspace_scope, c.created_at,
	ARRAY(SELECT w.name FROM vcs_connection_workspaces a JOIN workspaces w ON w.id = a.workspace_id
		WHERE a.connection_id = c.id ORDER BY w.name),
	ARRAY(SELECT p.name FROM vcs_connection_projects a JOIN projects p ON p.id = a.project_id
		WHERE a.connection_id = c.id ORDER BY p.name),
	(SELECT count(*) FROM workspace_vcs w WHERE w.connection_id = c.id),
	(SELECT count(*) FROM vcs_modules m WHERE m.connection_id = c.id),
	EXISTS (SELECT 1 FROM template_catalog t WHERE t.connection_id = c.id)`

func scanConns(rows *sql.Rows, err error) ([]VCSConnection, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []VCSConnection
	types := pgtype.NewMap()
	for rows.Next() {
		var c VCSConnection
		if err := rows.Scan(&c.ID, &c.Name, &c.Provider, &c.BaseURL, &c.Token, &c.WorkspaceScope, &c.CreatedAt,
			types.SQLScanner(&c.Workspaces), types.SQLScanner(&c.Projects), &c.LinkedWorkspaces, &c.LinkedModules, &c.Catalog); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) VCSConnections(ctx context.Context) ([]VCSConnection, error) {
	return scanConns(s.DB.QueryContext(ctx, `SELECT `+connCols+` FROM vcs_connections c ORDER BY c.name`))
}

// GetVCSConnection returns sql.ErrNoRows if the id is unknown or not a uuid.
func (s *Store) GetVCSConnection(ctx context.Context, id string) (VCSConnection, error) {
	conns, err := scanConns(s.DB.QueryContext(ctx, `SELECT `+connCols+` FROM vcs_connections c WHERE c.id = ops_uuid($1)`, id))
	if err == nil && len(conns) == 0 {
		err = sql.ErrNoRows
	}
	if err != nil {
		return VCSConnection{}, err
	}
	return conns[0], nil
}

// SetVCSScope sets the workspaces that may link a connection's repositories: "", "all" or "selected".
func (s *Store) SetVCSScope(ctx context.Context, id, workspaceScope string) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE vcs_connections SET workspace_scope = $2 WHERE id = ops_uuid($1)`, id, workspaceScope)
	return err
}

// DeleteVCSConnection also unlinks its modules (their published versions stay). It returns
// ErrInUse while the template catalog uses the connection.
func (s *Store) DeleteVCSConnection(ctx context.Context, id string) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM vcs_connections WHERE id = ops_uuid($1)`, id)
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && pgErr.Code == "23503" {
		return ErrInUse
	}
	return err
}

// VCSModule links a registry module to a repository whose semver tags become its versions.
type VCSModule struct {
	Namespace, Name, System string
	ConnectionID            string
	Connection, Provider    string // name and provider of the connection, for display
	Repo                    string
	Dir                     string // folder of the module in the repository; "" = the root
	SyncedAt                *time.Time
	SyncError               string
}

// LinkVCSModule returns ErrExists if the module is already linked to a repo.
func (s *Store) LinkVCSModule(ctx context.Context, m VCSModule) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO vcs_modules (namespace, name, system, connection_id, repo, dir) VALUES ($1, $2, $3, $4::uuid, $5, $6)`,
		m.Namespace, m.Name, m.System, m.ConnectionID, m.Repo, m.Dir)
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && pgErr.Code == "23505" {
		return ErrExists
	}
	return err
}

const vcsModuleQuery = `SELECT m.namespace, m.name, m.system, m.connection_id, c.name, c.provider, m.repo, m.dir, m.synced_at, m.sync_error
	FROM vcs_modules m JOIN vcs_connections c ON c.id = m.connection_id`

func scanVCSModule(row interface{ Scan(...any) error }) (VCSModule, error) {
	var m VCSModule
	err := row.Scan(&m.Namespace, &m.Name, &m.System, &m.ConnectionID, &m.Connection, &m.Provider, &m.Repo, &m.Dir, &m.SyncedAt, &m.SyncError)
	return m, err
}

func (s *Store) VCSModules(ctx context.Context) ([]VCSModule, error) {
	rows, err := s.DB.QueryContext(ctx, vcsModuleQuery+` ORDER BY m.namespace, m.name, m.system`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []VCSModule
	for rows.Next() {
		m, err := scanVCSModule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// GetVCSModule returns sql.ErrNoRows if the module isn't linked to a repo.
func (s *Store) GetVCSModule(ctx context.Context, namespace, name, system string) (VCSModule, error) {
	return scanVCSModule(s.DB.QueryRowContext(ctx, vcsModuleQuery+` WHERE m.namespace=$1 AND m.name=$2 AND m.system=$3`,
		namespace, name, system))
}

// UnlinkVCSModule stops publishing a module from its repository; published versions stay.
// It returns sql.ErrNoRows if the module isn't linked.
func (s *Store) UnlinkVCSModule(ctx context.Context, namespace, name, system string) error {
	res, err := s.DB.ExecContext(ctx, `DELETE FROM vcs_modules WHERE namespace=$1 AND name=$2 AND system=$3`, namespace, name, system)
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

// SetVCSSync records the outcome of a sync; syncErr "" means it succeeded.
func (s *Store) SetVCSSync(ctx context.Context, namespace, name, system, syncErr string) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE vcs_modules SET synced_at = now(), sync_error = $4
		WHERE namespace=$1 AND name=$2 AND system=$3`, namespace, name, system, syncErr)
	return err
}
