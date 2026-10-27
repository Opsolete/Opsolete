package storage

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"time"
)

type AuditEntry struct {
	ID     int64
	At     time.Time
	Actor  string
	Action string
	Detail json.RawMessage
	Hash   []byte
}

// Audit appends an entry to the audit log. detail is any JSON-encodable value.
func (s *Store) Audit(ctx context.Context, actor, action string, detail any) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	e, err := s.audit(ctx, tx, actor, action, detail)
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	e.stream()
	return nil
}

// audit appends an entry in tx; the caller commits, then streams it. The lock orders writers,
// so ids follow the chain without gaps.
func (s *Store) audit(ctx context.Context, tx *sql.Tx, actor, action string, detail any) (AuditEntry, error) {
	if len(s.AuditKey) == 0 {
		return AuditEntry{}, errors.New("audit log: no key (SECRET_KEY)")
	}
	e := AuditEntry{Actor: actor, Action: action, At: time.Now().UTC().Truncate(time.Microsecond)} // Postgres keeps µs
	var err error
	if e.Detail, err = json.Marshal(detail); err != nil {
		return e, err
	}
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(4242424243)`); err != nil {
		return e, err
	}
	prev := []byte{}
	err = tx.QueryRowContext(ctx, `SELECT id, hash FROM audit_log ORDER BY id DESC LIMIT 1`).Scan(&e.ID, &prev)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return e, err
	}
	e.ID++
	e.Hash = s.auditMAC(prev, e)
	_, err = tx.ExecContext(ctx, `INSERT INTO audit_log (id, at, actor, action, detail, prev_hash, hash)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`, e.ID, e.At, e.Actor, e.Action, string(e.Detail), prev, e.Hash)
	return e, err
}

// auditMAC: HMAC-SHA256 under SECRET_KEY of the previous entry's hash and this entry's content.
func (s *Store) auditMAC(prev []byte, e AuditEntry) []byte {
	b, _ := json.Marshal([]any{e.ID, e.At.UTC().Format(time.RFC3339Nano), e.Actor, e.Action, e.Detail})
	m := hmac.New(sha256.New, s.AuditKey)
	m.Write([]byte("opsolete audit log\x00"))
	m.Write(prev)
	m.Write(b)
	return m.Sum(nil)
}

// stream writes a committed entry to the server's log (stdout), so a copy lives outside the
// database: compare its hashes with the table to catch removed newest entries.
func (e AuditEntry) stream() {
	slog.Info("audit", "audit_id", e.ID, "at", e.At, "actor", e.Actor, "action", e.Action,
		"detail", e.Detail, "hash", hex.EncodeToString(e.Hash))
}

// AuditLog returns the newest entries first: the access events (sign-ins and requests refused with
// 401/403) when access is set, else everything else (the changes).
func (s *Store) AuditLog(ctx context.Context, limit int, access bool) ([]AuditEntry, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id, at, actor, action, detail, hash FROM audit_log
		WHERE (action = 'GET /auth/callback' OR coalesce(detail->'request'->>'status', '') IN ('401', '403')) = $2
		ORDER BY id DESC LIMIT $1`, limit, access)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		if err := rows.Scan(&e.ID, &e.At, &e.Actor, &e.Action, (*[]byte)(&e.Detail), &e.Hash); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// VerifyAuditLog recomputes the chain and returns the id of the first entry that was changed, or
// that follows a removed one; 0 if the chain is intact. Removing the newest entries can't be seen
// here: compare with the streamed copy for that.
// shortcut: scans the whole log on every call; verify from a checkpoint once it's millions of rows.
func (s *Store) VerifyAuditLog(ctx context.Context) (int64, error) {
	return s.verifyAudit(ctx, s.DB)
}

func (s *Store) verifyAudit(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}) (int64, error) {
	rows, err := q.QueryContext(ctx, `SELECT id, at, actor, action, detail, prev_hash, hash FROM audit_log ORDER BY id`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	want := struct {
		id   int64
		hash []byte
	}{1, []byte{}}
	for rows.Next() {
		var e AuditEntry
		var prev []byte
		if err := rows.Scan(&e.ID, &e.At, &e.Actor, &e.Action, (*[]byte)(&e.Detail), &prev, &e.Hash); err != nil {
			return 0, err
		}
		if e.ID != want.id || !bytes.Equal(prev, want.hash) || !hmac.Equal(e.Hash, s.auditMAC(prev, e)) {
			return e.ID, nil
		}
		want.id, want.hash = e.ID+1, e.Hash
	}
	return 0, rows.Err()
}
