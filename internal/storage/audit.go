package storage

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/opsolete/opsolete/internal/storage/db"
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
	var e AuditEntry
	if err := s.tx(ctx, func(q *db.Queries) (err error) {
		e, err = s.audit(ctx, q, actor, action, detail)
		return err
	}); err != nil {
		return err
	}
	e.stream()
	return nil
}

// audit appends an entry in q's transaction; the caller commits, then streams it. The lock orders
// writers, so ids follow the chain without gaps.
func (s *Store) audit(ctx context.Context, q *db.Queries, actor, action string, detail any) (AuditEntry, error) {
	if len(s.AuditKey) == 0 {
		return AuditEntry{}, errors.New("audit log: no key (SECRET_KEY)")
	}
	e := AuditEntry{Actor: actor, Action: action, At: time.Now().UTC().Truncate(time.Microsecond)} // Postgres keeps µs
	var err error
	if e.Detail, err = json.Marshal(detail); err != nil {
		return e, err
	}
	if err := q.LockAudit(ctx); err != nil {
		return e, err
	}
	last, err := q.LastAuditEntry(ctx)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return e, err
	}
	prev := last.Hash
	if prev == nil {
		prev = []byte{}
	}
	e.ID = last.ID + 1
	e.Hash = auditMAC(s.AuditKey, prev, e)
	return e, q.InsertAuditEntry(ctx, db.InsertAuditEntryParams{ID: e.ID, At: e.At, Actor: e.Actor, Action: e.Action,
		Detail: e.Detail, PrevHash: prev, Hash: e.Hash})
}

// auditMAC: HMAC-SHA256 under SECRET_KEY of the previous entry's hash and this entry's content.
func auditMAC(key, prev []byte, e AuditEntry) []byte {
	b, _ := json.Marshal([]any{e.ID, e.At.UTC().Format(time.RFC3339Nano), e.Actor, e.Action, e.Detail})
	m := hmac.New(sha256.New, key)
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
	rows, err := s.q.AuditLog(ctx, db.AuditLogParams{Lim: int32(limit), Access: access})
	out := make([]AuditEntry, 0, len(rows))
	for _, e := range rows {
		out = append(out, AuditEntry{ID: e.ID, At: e.At, Actor: e.Actor, Action: e.Action, Detail: e.Detail, Hash: e.Hash})
	}
	return out, err
}

// VerifyAuditLog recomputes the chain and returns the id of the first entry that was changed, or
// that follows a removed one; 0 if the chain is intact. Removing the newest entries can't be seen
// here: compare with the streamed copy for that.
// shortcut: reads the whole log into memory on every call; verify from a checkpoint once it's millions of rows.
func (s *Store) VerifyAuditLog(ctx context.Context) (int64, error) {
	return s.verifyAudit(ctx, s.q)
}

func (s *Store) verifyAudit(ctx context.Context, q *db.Queries) (int64, error) {
	rows, err := q.AuditChain(ctx)
	if err != nil {
		return 0, err
	}
	want := struct {
		id   int64
		hash []byte
	}{1, []byte{}}
	// Entries before the first one signed with SECRET_KEY were signed with the key it replaced: one
	// signed with SECRET_KEY_PREVIOUS after that is a forgery by whoever had the old key.
	previous := len(s.AuditPreviousKey) > 0
	for _, r := range rows {
		e := AuditEntry{ID: r.ID, At: r.At, Actor: r.Actor, Action: r.Action, Detail: r.Detail, Hash: r.Hash}
		if e.ID != want.id || !bytes.Equal(r.PrevHash, want.hash) {
			return e.ID, nil
		}
		switch {
		case hmac.Equal(e.Hash, auditMAC(s.AuditKey, r.PrevHash, e)):
			previous = false
		case previous && hmac.Equal(e.Hash, auditMAC(s.AuditPreviousKey, r.PrevHash, e)):
		default:
			return e.ID, nil
		}
		want.id, want.hash = e.ID+1, e.Hash
	}
	return 0, nil
}
