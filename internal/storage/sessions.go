package storage

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/opsolete/opsolete/internal/auth"
	"github.com/opsolete/opsolete/internal/storage/db"
)

// CreateSession stores a new browser session and returns its token (256 random bits), the cookie's value.
func (s *Store) CreateSession(ctx context.Context, n auth.NewSession) (string, error) {
	b := make([]byte, 32)
	rand.Read(b) // never fails (crypto/rand since Go 1.24)
	token := base64.RawURLEncoding.EncodeToString(b)
	groups := n.AdminGroups
	if groups == nil {
		groups = []string{}
	}
	return token, s.q.CreateSession(ctx, db.CreateSessionParams{TokenHash: tokenHash(token), UserID: n.UserID, AdminEmail: n.AdminEmail,
		AdminGroups: groups, UserAgent: n.UserAgent, Ip: n.IP, TtlSecs: n.TTL.Seconds()})
}

// Session returns the unexpired session of token with its user; auth.ErrNoSession if there is none.
func (s *Store) Session(ctx context.Context, token string) (auth.Session, error) {
	r, err := s.q.SessionUser(ctx, tokenHash(token))
	if errors.Is(err, pgx.ErrNoRows) {
		return auth.Session{}, auth.ErrNoSession
	}
	return auth.Session{ID: r.ID, UserID: r.UserID, Email: r.Email, AdminEmail: r.AdminEmail, AdminGroups: r.AdminGroups}, err
}

// DeleteSession ends the session of token (logout); an unknown token deletes nothing.
func (s *Store) DeleteSession(ctx context.Context, token string) error {
	return s.q.DeleteSession(ctx, tokenHash(token))
}

// DeleteUserSessions ends all of a user's sessions and returns how many there were.
func (s *Store) DeleteUserSessions(ctx context.Context, userID uuid.UUID) (int64, error) {
	return s.q.DeleteUserSessions(ctx, userID)
}

// UserSession is one of a user's unexpired browser sessions.
type UserSession struct {
	ID                               uuid.UUID
	UserAgent, IP                    string
	CreatedAt, LastSeenAt, ExpiresAt time.Time
}

// UserSessions lists a user's unexpired sessions, most recently seen first.
func (s *Store) UserSessions(ctx context.Context, userID uuid.UUID) ([]UserSession, error) {
	rows, err := s.q.UserSessions(ctx, userID)
	out := make([]UserSession, 0, len(rows))
	for _, r := range rows {
		out = append(out, UserSession{ID: r.ID, UserAgent: r.UserAgent, IP: r.Ip, CreatedAt: r.CreatedAt, LastSeenAt: r.LastSeenAt, ExpiresAt: r.ExpiresAt})
	}
	return out, err
}

// PruneSessions deletes expired sessions.
func (s *Store) PruneSessions(ctx context.Context) (int64, error) {
	return s.q.PruneSessions(ctx)
}
