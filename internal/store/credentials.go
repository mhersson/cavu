package store

import (
	"context"
	"fmt"
)

// Credential is a named GitHub credential; its secret is in the secrets table.
type Credential struct {
	Name, Kind string // Kind is "token" or "app"
	AppID      int64  // an App's id; 0 for a token
}

// Credentials returns every named credential, ordered by name.
func (s *Store) Credentials(ctx context.Context) ([]Credential, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, kind, app_id FROM forge_credentials ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list credentials: %w", err)
	}
	defer rows.Close()

	var out []Credential

	for rows.Next() {
		var c Credential
		if err := rows.Scan(&c.Name, &c.Kind, &c.AppID); err != nil {
			return nil, fmt.Errorf("list credentials: %w", err)
		}

		out = append(out, c)
	}

	return out, rows.Err()
}

// PutCredential stores c, replacing the row of the same name in place.
func (s *Store) PutCredential(ctx context.Context, c Credential) error {
	if _, err := s.db.ExecContext(ctx, `INSERT INTO forge_credentials (name, kind, app_id) VALUES (?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET kind = excluded.kind, app_id = excluded.app_id`, c.Name, c.Kind, c.AppID); err != nil {
		return fmt.Errorf("write credential: %w", err)
	}

	return nil
}

// RemoveCredential deletes the credential name and every secret whose name
// starts with secretPrefix, in one transaction. An unknown name returns
// ErrNotFound; one a project names fails on the foreign key, removing nothing.
func (s *Store) RemoveCredential(ctx context.Context, name, secretPrefix string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("remove credential: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, `DELETE FROM forge_credentials WHERE name = ?`, name)
	if err != nil {
		return fmt.Errorf("remove credential: %w", err)
	}

	if err := rowsAffectedOrNotFound(res); err != nil {
		return err
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM secrets WHERE substr(name, 1, length(?1)) = ?1`, secretPrefix); err != nil {
		return fmt.Errorf("remove credential secrets: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("remove credential: %w", err)
	}

	return nil
}
