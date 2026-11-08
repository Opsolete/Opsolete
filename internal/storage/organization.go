package storage

import (
	"context"
	"crypto/rand"
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

// SigningKey returns the registry's HMAC key, creating it on first use (concurrent first starts
// agree: the first one set wins).
func (s *Store) SigningKey(ctx context.Context) ([]byte, error) {
	return s.q.SigningKey(ctx, []byte(rand.Text()+rand.Text()))
}
