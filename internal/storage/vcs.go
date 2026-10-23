package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// VCSConnection is a VCS provider account; Token is sealed (see internal/vcs).
type VCSConnection struct {
	ID, Name, Provider, BaseURL string
	Token                       []byte
	CreatedAt                   time.Time
}

func (s *Store) CreateVCSConnection(ctx context.Context, c VCSConnection) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO vcs_connections (name, provider, base_url, token) VALUES ($1, $2, $3, $4)`,
		c.Name, c.Provider, c.BaseURL, c.Token)
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && pgErr.Code == "23505" {
		return ErrExists
	}
	return err
}

func (s *Store) VCSConnections(ctx context.Context) ([]VCSConnection, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id, name, provider, base_url, token, created_at FROM vcs_connections ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []VCSConnection
	for rows.Next() {
		var c VCSConnection
		if err := rows.Scan(&c.ID, &c.Name, &c.Provider, &c.BaseURL, &c.Token, &c.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// GetVCSConnection returns sql.ErrNoRows if the id is unknown or not a uuid.
func (s *Store) GetVCSConnection(ctx context.Context, id string) (VCSConnection, error) {
	var c VCSConnection
	err := s.DB.QueryRowContext(ctx, `SELECT id, name, provider, base_url, token, created_at FROM vcs_connections WHERE id::text = $1`,
		id).Scan(&c.ID, &c.Name, &c.Provider, &c.BaseURL, &c.Token, &c.CreatedAt)
	return c, err
}

// DeleteVCSConnection also unlinks its modules (their published versions stay).
func (s *Store) DeleteVCSConnection(ctx context.Context, id string) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM vcs_connections WHERE id::text = $1`, id)
	return err
}

// VCSModule links a registry module to a repository whose semver tags become its versions.
type VCSModule struct {
	Namespace, Name, System string
	ConnectionID            string
	Connection, Provider    string // name and provider of the connection, for display
	Repo                    string
	SyncedAt                *time.Time
	SyncError               string
}

// LinkVCSModule returns ErrExists if the module is already linked to a repo.
func (s *Store) LinkVCSModule(ctx context.Context, m VCSModule) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO vcs_modules (namespace, name, system, connection_id, repo) VALUES ($1, $2, $3, $4::uuid, $5)`,
		m.Namespace, m.Name, m.System, m.ConnectionID, m.Repo)
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && pgErr.Code == "23505" {
		return ErrExists
	}
	return err
}

const vcsModuleQuery = `SELECT m.namespace, m.name, m.system, m.connection_id, c.name, c.provider, m.repo, m.synced_at, m.sync_error
	FROM vcs_modules m JOIN vcs_connections c ON c.id = m.connection_id`

func scanVCSModule(row interface{ Scan(...any) error }) (VCSModule, error) {
	var m VCSModule
	err := row.Scan(&m.Namespace, &m.Name, &m.System, &m.ConnectionID, &m.Connection, &m.Provider, &m.Repo, &m.SyncedAt, &m.SyncError)
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

// SetVCSSync records the outcome of a sync; syncErr "" means it succeeded.
func (s *Store) SetVCSSync(ctx context.Context, namespace, name, system, syncErr string) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE vcs_modules SET synced_at = now(), sync_error = $4
		WHERE namespace=$1 AND name=$2 AND system=$3`, namespace, name, system, syncErr)
	return err
}

// ConfigureWorkspace moves a workspace to a module version and inputs and queues the plan that
// applies them. Without a free workspace (ErrRunActive) nothing changes.
func (s *Store) ConfigureWorkspace(ctx context.Context, workspaceID, source, ref string, inputs map[string]any, createdBy string) (Run, error) {
	raw, err := json.Marshal(inputs)
	if err != nil {
		return Run{}, err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Run{}, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE workspaces SET module_source = $2, module_ref = $3, inputs = $4, updated_at = now()
		WHERE id::text = $1`, workspaceID, source, ref, raw)
	if err != nil {
		return Run{}, err
	}
	if n, err := res.RowsAffected(); err != nil {
		return Run{}, err
	} else if n == 0 {
		return Run{}, sql.ErrNoRows
	}
	r, err := createRun(ctx, tx, workspaceID, "apply", createdBy)
	if err != nil {
		return Run{}, err
	}
	return r, tx.Commit()
}
