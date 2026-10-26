// Package storage provides PostgreSQL access, migrations and repositories.
package storage

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"sort"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver
)

//go:embed migrations/*.sql
var migrations embed.FS

type Store struct {
	DB       *sql.DB
	AuditKey []byte // signs the audit log (SECRET_KEY); entries can't be written without it
}

func Open(ctx context.Context, url string) (*Store, error) {
	db, err := sql.Open("pgx", url)
	if err != nil {
		return nil, err
	}
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return &Store{DB: db}, nil
}

// Migrate applies pending embedded SQL migrations in filename order. Everything runs in one
// transaction under an advisory lock, so concurrent app instances (or test packages) can't race.
func (s *Store) Migrate(ctx context.Context) error {
	files, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(files)
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(4242424242)`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	for _, f := range files {
		var done bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=$1)`, f).Scan(&done); err != nil {
			return err
		}
		if done {
			continue
		}
		body, err := migrations.ReadFile(f)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, string(body)); err != nil {
			return fmt.Errorf("migration %s: %w", f, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version) VALUES ($1)`, f); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// UpsertUser records a login; the role (from ADMIN_EMAILS) and the IdP groups are refreshed on every login.
func (s *Store) UpsertUser(ctx context.Context, email, role string, groups []string) (string, error) {
	if groups == nil {
		groups = []string{}
	}
	var id string
	err := s.DB.QueryRowContext(ctx, `INSERT INTO users (email, role, groups) VALUES ($1, $2, $3)
		ON CONFLICT (email) DO UPDATE SET role = EXCLUDED.role, groups = EXCLUDED.groups RETURNING id`, email, role, groups).Scan(&id)
	return id, err
}

// UserGroups returns the IdP groups recorded at the user's last login, as a JSON array.
func (s *Store) UserGroups(ctx context.Context, id string) (json.RawMessage, error) {
	var groups []byte
	err := s.DB.QueryRowContext(ctx, `SELECT array_to_json(groups) FROM users WHERE id = ops_uuid($1)`, id).Scan(&groups)
	return groups, err
}
