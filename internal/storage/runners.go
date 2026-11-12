package storage

import (
	"context"
	"crypto/rand"
	"time"

	"github.com/google/uuid"

	"github.com/opsolete/opsolete/internal/storage/db"
)

// DefaultPool is the runner pool of a project or runner that names none.
const DefaultPool = "default"

// Runner is a registered runner (009_runners.sql); its token is only stored hashed.
type Runner struct {
	ID        uuid.UUID
	Name      string
	Pool      string
	CreatedAt time.Time
	LastSeen  *time.Time
}

// CreateRunner registers runner name in pool with a new token, returned once. ErrExists: the name is taken.
func (s *Store) CreateRunner(ctx context.Context, name, pool string) (string, error) {
	token := "opsr_" + rand.Text() + rand.Text()
	err := s.q.CreateRunner(ctx, db.CreateRunnerParams{Name: name, Pool: pool, TokenHash: tokenHash(token)})
	if isUnique(err) {
		return "", ErrExists
	}
	return token, err
}

// RegisterRunner registers runner name in pool with token (one the operator generated), replacing the token and
// pool it had. ErrExists: another runner has that token.
func (s *Store) RegisterRunner(ctx context.Context, name, pool, token string) error {
	err := s.q.RegisterRunner(ctx, db.RegisterRunnerParams{Name: name, Pool: pool, TokenHash: tokenHash(token)})
	if isUnique(err) {
		return ErrExists
	}
	return err
}

// RunnerByToken is the runner token belongs to; it records the runner as seen (at most once a minute).
// ErrNotFound: no runner has it (never registered, or deleted).
func (s *Store) RunnerByToken(ctx context.Context, token string) (Runner, error) {
	r, err := s.q.RunnerByToken(ctx, tokenHash(token))
	if err != nil {
		return Runner{}, notFound(err)
	}
	if r.LastSeenAt == nil || time.Since(*r.LastSeenAt) > time.Minute {
		if err := s.q.TouchRunner(ctx, r.ID); err != nil {
			return Runner{}, err
		}
	}
	return Runner{ID: r.ID, Name: r.Name, Pool: r.Pool, LastSeen: r.LastSeenAt}, nil
}

// Runners lists the registered runners by pool and name.
func (s *Store) Runners(ctx context.Context) ([]Runner, error) {
	rows, err := s.q.Runners(ctx)
	out := make([]Runner, 0, len(rows))
	for _, r := range rows {
		out = append(out, Runner{ID: r.ID, Name: r.Name, Pool: r.Pool, CreatedAt: r.CreatedAt, LastSeen: r.LastSeenAt})
	}
	return out, err
}

// DeleteRunner revokes runner id's token and returns its name; ErrNotFound for an unknown id.
func (s *Store) DeleteRunner(ctx context.Context, id uuid.UUID) (string, error) {
	name, err := s.q.DeleteRunner(ctx, id)
	return name, notFound(err)
}

// RunnerPools lists the pools runners or projects use, and the default one.
func (s *Store) RunnerPools(ctx context.Context) ([]string, error) {
	return s.q.RunnerPools(ctx)
}
