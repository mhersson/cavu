package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Setting returns the value for key; false when unset.
func (s *Store) Setting(ctx context.Context, key string) (string, bool, error) {
	var v string

	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}

	if err != nil {
		return "", false, fmt.Errorf("read setting: %w", err)
	}

	return v, true, nil
}

// SetSetting stores value under key, replacing any previous value.
func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	if _, err := s.db.ExecContext(ctx, `INSERT INTO settings (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value); err != nil {
		return fmt.Errorf("write setting: %w", err)
	}

	return nil
}

// Settings returns every setting.
func (s *Store) Settings(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key, value FROM settings`)
	if err != nil {
		return nil, fmt.Errorf("list settings: %w", err)
	}
	defer rows.Close()

	out := map[string]string{}

	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, fmt.Errorf("list settings: %w", err)
		}

		out[k] = v
	}

	return out, rows.Err()
}
