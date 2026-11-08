// Package testdb gives every database test its own empty PostgreSQL database, so tests share no
// rows (job queue, audit log, default project) and test packages may run in parallel.
package testdb

import (
	"context"
	"crypto/rand"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// URL creates an empty database for t on the server TEST_DATABASE_URL points at (a URL, its user
// needs CREATEDB) and drops it when t ends. Without TEST_DATABASE_URL it skips t.
// shortcut: a killed test run leaves its opsolete_t_* databases behind; drop them by hand (or `just db-reset`).
func URL(t testing.TB) string {
	t.Helper()
	base := os.Getenv("TEST_DATABASE_URL")
	if base == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("TEST_DATABASE_URL: %v", err)
	}
	name := "opsolete_t_" + strings.ToLower(rand.Text()[:16])
	if err := exec(base, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create test database: %v", err)
	}
	t.Cleanup(func() {
		// FORCE: connections a test leaves open (a runner goroutine's) must not keep it.
		if err := exec(base, "DROP DATABASE "+name+" WITH (FORCE)"); err != nil {
			t.Errorf("drop test database %s: %v", name, err)
		}
	})
	u.Path = "/" + name
	return u.String()
}

func exec(dbURL, sql string) error {
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dbURL)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	_, err = conn.Exec(ctx, sql)
	return err
}
