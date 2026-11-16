package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// PutSecret stores the sealed bytes under name verbatim, replacing any previous value.
func (s *Store) PutSecret(ctx context.Context, name string, sealed []byte) error {
	if sealed == nil {
		sealed = []byte{}
	}

	if _, err := s.db.ExecContext(ctx, `INSERT INTO secrets (name, sealed) VALUES (?, ?)
		ON CONFLICT(name) DO UPDATE SET sealed = excluded.sealed`, name, sealed); err != nil {
		return fmt.Errorf("write secret: %w", err)
	}

	return nil
}

// Secret returns the sealed bytes for name; false when unset.
func (s *Store) Secret(ctx context.Context, name string) ([]byte, bool, error) {
	var b []byte

	err := s.db.QueryRowContext(ctx, `SELECT sealed FROM secrets WHERE name = ?`, name).Scan(&b)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}

	if err != nil {
		return nil, false, fmt.Errorf("read secret: %w", err)
	}

	return b, true, nil
}

// SecretNames returns the names of all stored secrets, sorted.
func (s *Store) SecretNames(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name FROM secrets ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list secrets: %w", err)
	}
	defer rows.Close()

	var out []string

	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, fmt.Errorf("list secrets: %w", err)
		}

		out = append(out, n)
	}

	return out, rows.Err()
}
