package storage

import "context"

// Currency is the ISO 4217 code cost estimates are in: USD or EUR.
func (s *Store) Currency(ctx context.Context) (string, error) {
	var c string
	err := s.DB.QueryRowContext(ctx, `SELECT currency FROM organization`).Scan(&c)
	return c, err
}

func (s *Store) SetCurrency(ctx context.Context, c string) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE organization SET currency = $1`, c)
	return err
}
