package storage

import (
	"context"
	"database/sql"
	"testing"
)

func TestAuditLogAppendOnly(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.Audit(ctx, "a@example.com", "POST /x", map[string]any{"status": 303, "html": "<&>"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Audit(ctx, "b@example.com", "POST /y", nil); err != nil {
		t.Fatal(err)
	}
	if broken, err := s.VerifyAuditLog(ctx); err != nil || broken != 0 {
		t.Fatalf("fresh chain: broken at %d, %v", broken, err)
	}
	for _, q := range []string{`UPDATE audit_log SET actor = 'x'`, `DELETE FROM audit_log`, `TRUNCATE audit_log`} {
		if _, err := s.DB.ExecContext(ctx, q); err == nil {
			t.Fatalf("%s succeeded", q)
		}
	}
	// Around the triggers (as the table owner could), in transactions that are rolled back
	// (DDL is transactional: the triggers come back).
	tamper := func(check func(tx *sql.Tx, last int64)) {
		t.Helper()
		tx, err := s.DB.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		var last int64
		if err := tx.QueryRowContext(ctx, `SELECT max(id) FROM audit_log`).Scan(&last); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(ctx, `ALTER TABLE audit_log DISABLE TRIGGER audit_log_no_update`); err != nil {
			t.Fatal(err)
		}
		check(tx, last)
	}
	verify := func(tx *sql.Tx, want int64, what string) {
		t.Helper()
		if broken, err := s.verifyAudit(ctx, tx); err != nil || broken != want {
			t.Fatalf("%s: verify says %d, want %d (%v)", what, broken, want, err)
		}
	}
	tamper(func(tx *sql.Tx, last int64) {
		tx.ExecContext(ctx, `UPDATE audit_log SET actor = 'mallory' WHERE id = $1`, last-1)
		verify(tx, last-1, "edited entry")
	})
	tamper(func(tx *sql.Tx, last int64) {
		tx.ExecContext(ctx, `DELETE FROM audit_log WHERE id = $1`, last-1)
		verify(tx, last, "entry before removed")
	})
	tamper(func(tx *sql.Tx, last int64) {
		// Replace the newest entry with one chained correctly but signed without SECRET_KEY.
		tx.ExecContext(ctx, `DELETE FROM audit_log WHERE id = $1`, last)
		forger := &Store{DB: s.DB, AuditKey: []byte("not-the-key")}
		if _, err := forger.audit(ctx, tx, "mallory", "POST /x", nil); err != nil {
			t.Fatal(err)
		}
		verify(tx, last, "forged entry")
	})
}
