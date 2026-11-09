// Package storage provides PostgreSQL access, migrations and repositories.
//
// Queries live in queries/*.sql; sqlc generates package db from them (just sqlc). Store methods map
// db's row types to the domain types here, the package's API.
//
// Ids are uuid.UUID. An optional id (a reference that may be absent, or an argument that may be
// left out) is uuid.Nil for none; uuid.NullUUID stays inside this package. Callers parse ids from
// input themselves: an invalid id is a missing row. A lookup of a missing row returns ErrNotFound.
package storage

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"sort"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/opsolete/opsolete/internal/auth"
	"github.com/opsolete/opsolete/internal/storage/db"
)

//go:embed migrations/*.sql
var migrations embed.FS

// ErrNotFound: the row asked for doesn't exist.
var ErrNotFound = errors.New("not found")

type Store struct {
	Pool     *pgxpool.Pool
	q        *db.Queries
	AuditKey []byte // signs the audit log (SECRET_KEY); entries can't be written without it
	// AuditPreviousKey (SECRET_KEY_PREVIOUS) verifies the entries signed before a key rotation.
	AuditPreviousKey []byte
}

func Open(ctx context.Context, url string) (*Store, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return &Store{Pool: pool, q: db.New(pool)}, nil
}

func (s *Store) Close() { s.Pool.Close() }

// tx runs fn in a transaction, committed if fn returns nil.
func (s *Store) tx(ctx context.Context, fn func(q *db.Queries) error) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // a no-op after Commit
	if err := fn(s.q.WithTx(tx)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// notFound turns pgx.ErrNoRows into ErrNotFound.
func notFound(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

// affected turns "no row matched" into ErrNotFound.
func affected(n int64, err error) error {
	if err == nil && n == 0 {
		return ErrNotFound
	}
	return err
}

// isUnique reports a unique violation.
func isUnique(err error) bool {
	pg, ok := errors.AsType[*pgconn.PgError](err)
	return ok && pg.Code == "23505"
}

// fkViolation returns the constraint of a foreign key violation.
func fkViolation(err error) (constraint string, ok bool) {
	pg, ok := errors.AsType[*pgconn.PgError](err)
	if !ok || pg.Code != "23503" {
		return "", false
	}
	return pg.ConstraintName, true
}

// optID is an optional id as SQL sees it: uuid.Nil is NULL.
func optID(id uuid.UUID) uuid.NullUUID { return uuid.NullUUID{UUID: id, Valid: id != uuid.Nil} }

// Migrate applies pending embedded SQL migrations in filename order. Everything runs in one
// transaction under an advisory lock, so concurrent app instances (or test packages) can't race.
func (s *Store) Migrate(ctx context.Context) error {
	files, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(files)
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // a no-op after Commit
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(4242424242)`); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	// A 0.x database recorded migrations 1.0 doesn't have (001_init.sql …): it isn't upgraded.
	var foreign string
	if err := tx.QueryRow(ctx, `SELECT COALESCE(min(version), '') FROM schema_migrations WHERE version <> ALL($1)`,
		files).Scan(&foreign); err != nil {
		return err
	}
	if foreign != "" {
		// every 0.6+ database has the first file (001_schema.sql), no 0.x one does: the rest is from a newer Opsolete
		var newer bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version = $1)`, files[0]).Scan(&newer); err != nil {
			return err
		}
		if newer {
			return fmt.Errorf("this database was migrated by a newer Opsolete (it has migration %s, which this version doesn't know): run that version again; going back to an older one needs the database backup taken before the upgrade", foreign)
		}
		return fmt.Errorf("this database was created by Opsolete 0.5 or earlier (it has migration %s): this version needs a new, empty database (see Upgrading to 0.6)", foreign)
	}
	for _, f := range files {
		var done bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=$1)`, f).Scan(&done); err != nil {
			return err
		}
		if done {
			continue
		}
		body, err := migrations.ReadFile(f)
		if err != nil {
			return err
		}
		// no arguments: the simple protocol runs the file's many statements
		if _, err := tx.Exec(ctx, string(body)); err != nil {
			return fmt.Errorf("migration %s: %w", f, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations(version) VALUES ($1)`, f); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// UpsertUser records a login; the email, the role (from ADMIN_EMAILS) and the IdP groups are refreshed on every login.
// A user is keyed by subject (OIDC issuer + sub) when there is one: a user without a subject yet is bound
// to the first subject that logs in with its email, and a different subject with that email is refused.
// subject "" (the dev user) keys by email.
func (s *Store) UpsertUser(ctx context.Context, subject, email, role string, groups []string) (uuid.UUID, error) {
	if groups == nil {
		groups = []string{}
	}
	if subject == "" {
		return s.q.UpsertUserByEmail(ctx, db.UpsertUserByEmailParams{Email: email, Role: role, Groups: groups})
	}
	id, err := s.q.UpdateUserBySubject(ctx, db.UpdateUserBySubjectParams{Subject: subject, Email: email, Role: role, Groups: groups})
	if errors.Is(err, pgx.ErrNoRows) {
		id, err = s.q.UpsertUserBySubject(ctx, db.UpsertUserBySubjectParams{Subject: subject, Email: email, Role: role, Groups: groups})
	}
	if isUnique(err) { // the subject's new email is another user's
		err = auth.ErrEmailTaken
	}
	return id, err
}

// UserGroups returns the IdP groups recorded at the user's last login, as a JSON array.
func (s *Store) UserGroups(ctx context.Context, id uuid.UUID) (json.RawMessage, error) {
	groups, err := s.q.UserGroups(ctx, id)
	return groups, notFound(err)
}

// KnownGroups lists the IdP groups of everyone who has logged in, for picking a grant.
func (s *Store) KnownGroups(ctx context.Context) ([]string, error) {
	return s.q.KnownGroups(ctx)
}
