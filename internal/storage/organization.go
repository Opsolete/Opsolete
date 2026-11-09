package storage

import (
	"context"
	"crypto/rand"

	"github.com/opsolete/opsolete/internal/storage/db"
)

// Currency is the ISO 4217 code cost estimates are in: USD or EUR.
func (s *Store) Currency(ctx context.Context) (string, error) {
	return s.q.Currency(ctx)
}

func (s *Store) SetCurrency(ctx context.Context, c string) error {
	return s.q.SetCurrency(ctx, c)
}

// ModuleUploads reports whether admins may upload registry modules.
func (s *Store) ModuleUploads(ctx context.Context) (bool, error) {
	return s.q.ModuleUploads(ctx)
}

func (s *Store) SetModuleUploads(ctx context.Context, on bool) error {
	return s.q.SetModuleUploads(ctx, on)
}

// SigningKey returns the stored signing key as stored (see app.SigningKey), creating a random one
// on first use (concurrent first starts agree: the first one set wins).
func (s *Store) SigningKey(ctx context.Context) ([]byte, error) {
	return s.q.SigningKey(ctx, []byte(rand.Text()+rand.Text()))
}

// ReplaceSigningKey stores new as the signing key if it is still old; ErrNotFound if it changed meanwhile.
func (s *Store) ReplaceSigningKey(ctx context.Context, old, new []byte) error {
	n, err := s.q.ReplaceSigningKey(ctx, db.ReplaceSigningKeyParams{Old: old, New: new})
	if err == nil && n == 0 {
		err = ErrNotFound
	}
	return err
}
