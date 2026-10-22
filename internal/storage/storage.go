// Package storage provides PostgreSQL access, migrations and repositories.
package storage

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver
)

//go:embed migrations/*.sql
var migrations embed.FS

type Store struct{ DB *sql.DB }

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

type Deployment struct {
	ID           string
	TemplateID   string
	ModuleSource string
	ModuleRef    string
	Inputs       map[string]any
	Status       string
	Action       string // apply | destroy: what the current/last run does
	StateKey     string
	Runner       *string
	Outputs      map[string]any // non-sensitive terraform outputs; sensitive ones are redacted by the runner
	CreatedBy    string         // email; empty for deployments from before login
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

type StatusEntry struct {
	Status    string
	Message   *string
	CreatedAt time.Time
}

const deploymentCols = `id, template_id, COALESCE(module_source, ''), module_ref, inputs, status, action, state_key,
	runner, outputs, COALESCE((SELECT email FROM users u WHERE u.id = created_by), ''), created_at, updated_at`

func scanDeployment(row interface{ Scan(...any) error }) (Deployment, error) {
	var d Deployment
	var raw, outputs []byte
	if err := row.Scan(&d.ID, &d.TemplateID, &d.ModuleSource, &d.ModuleRef, &raw, &d.Status, &d.Action, &d.StateKey,
		&d.Runner, &outputs, &d.CreatedBy, &d.CreatedAt, &d.UpdatedAt); err != nil {
		return d, err
	}
	if outputs != nil {
		if err := json.Unmarshal(outputs, &d.Outputs); err != nil {
			return d, err
		}
	}
	return d, json.Unmarshal(raw, &d.Inputs)
}

// CreateDeployment queues a deployment for the runners and records it in the status history.
// createdBy is a user id, or empty.
func (s *Store) CreateDeployment(ctx context.Context, templateID, moduleSource, moduleRef string, inputs map[string]any, createdBy string) (Deployment, error) {
	raw, err := json.Marshal(inputs)
	if err != nil {
		return Deployment{}, err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Deployment{}, err
	}
	defer tx.Rollback()

	d := Deployment{TemplateID: templateID, ModuleSource: moduleSource, ModuleRef: moduleRef, Inputs: inputs, Action: "apply"}
	err = tx.QueryRowContext(ctx, `
		WITH id AS (SELECT gen_random_uuid() AS v)
		INSERT INTO deployments (id, template_id, module_source, module_ref, inputs, state_key, status, created_by)
		SELECT v, $1, $2, $3, $4, 'form2stack/' || $1 || '/' || v || '.tfstate', 'queued', NULLIF($5, '')::uuid FROM id
		RETURNING id, status, state_key, created_at`,
		templateID, moduleSource, moduleRef, raw, createdBy).Scan(&d.ID, &d.Status, &d.StateKey, &d.CreatedAt)
	if err != nil {
		return Deployment{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO deployment_status_history (deployment_id, status, message)
		VALUES ($1, $2, 'waiting for a runner')`, d.ID, d.Status); err != nil {
		return Deployment{}, err
	}
	return d, tx.Commit()
}

// ErrNotDestroyable means the deployment is queued, running or already destroyed.
var ErrNotDestroyable = errors.New("deployment can't be destroyed in its current state")

// RequestDestroy re-queues a finished deployment to tear down its resources (same state key).
func (s *Store) RequestDestroy(ctx context.Context, id, by string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE deployments SET status = 'queued', action = 'destroy', updated_at = now()
		WHERE id::text = $1 AND status IN ('succeeded', 'failed', 'canceled')`, id)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return ErrNotDestroyable
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO deployment_status_history (deployment_id, status, message)
		VALUES ($1::uuid, 'queued', 'destroy requested by ' || $2)`, id, by); err != nil {
		return err
	}
	return tx.Commit()
}

// UpsertUser records a login; the role follows ADMIN_EMAILS on every login.
func (s *Store) UpsertUser(ctx context.Context, email, role string) (string, error) {
	var id string
	err := s.DB.QueryRowContext(ctx, `INSERT INTO users (email, role) VALUES ($1, $2)
		ON CONFLICT (email) DO UPDATE SET role = EXCLUDED.role RETURNING id`, email, role).Scan(&id)
	return id, err
}

func (s *Store) ListDeployments(ctx context.Context) ([]Deployment, error) {
	return s.queryDeployments(ctx, `SELECT `+deploymentCols+` FROM deployments ORDER BY created_at DESC LIMIT 100`)
}

func (s *Store) queryDeployments(ctx context.Context, q string, args ...any) ([]Deployment, error) {
	rows, err := s.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Deployment
	for rows.Next() {
		d, err := scanDeployment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// GetDeployment returns sql.ErrNoRows if the id is unknown or not a valid uuid.
func (s *Store) GetDeployment(ctx context.Context, id string) (Deployment, []StatusEntry, error) {
	d, err := scanDeployment(s.DB.QueryRowContext(ctx, `SELECT `+deploymentCols+` FROM deployments WHERE id::text = $1`, id))
	if err != nil {
		return d, nil, err
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT status, message, created_at FROM deployment_status_history
		WHERE deployment_id = $1 ORDER BY id`, d.ID)
	if err != nil {
		return d, nil, err
	}
	defer rows.Close()
	var hist []StatusEntry
	for rows.Next() {
		var e StatusEntry
		if err := rows.Scan(&e.Status, &e.Message, &e.CreatedAt); err != nil {
			return d, nil, err
		}
		hist = append(hist, e)
	}
	return d, hist, rows.Err()
}
