package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// User is the one account that logs in.
type User struct {
	Name, PasswordHash string
	CreatedAt          time.Time
}

// PutUser creates the one user, or replaces its name and hash and keeps the
// first creation time.
func (s *Store) PutUser(ctx context.Context, u User) error {
	if _, err := s.db.ExecContext(ctx, `INSERT INTO users (id, name, password_hash, created_at) VALUES (1, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET name = excluded.name, password_hash = excluded.password_hash`,
		u.Name, u.PasswordHash, u.CreatedAt.UnixMilli()); err != nil {
		return fmt.Errorf("write user: %w", err)
	}

	return nil
}

// User returns the user; false when none is set.
func (s *Store) User(ctx context.Context) (User, bool, error) {
	var (
		u  User
		ms int64
	)

	err := s.db.QueryRowContext(ctx, `SELECT name, password_hash, created_at FROM users WHERE id = 1`).
		Scan(&u.Name, &u.PasswordHash, &ms)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, false, nil
	}

	if err != nil {
		return User{}, false, fmt.Errorf("read user: %w", err)
	}

	u.CreatedAt = time.UnixMilli(ms)

	return u, true, nil
}

// CreateSession stores a session by its token hash.
func (s *Store) CreateSession(ctx context.Context, tokenHash string, createdAt, expiresAt time.Time) error {
	if _, err := s.db.ExecContext(ctx, `INSERT INTO sessions (token_hash, created_at, expires_at) VALUES (?, ?, ?)`,
		tokenHash, createdAt.UnixMilli(), expiresAt.UnixMilli()); err != nil {
		return fmt.Errorf("create session: %w", err)
	}

	return nil
}

// SessionExpiry returns when the session expires; false when it does not exist.
func (s *Store) SessionExpiry(ctx context.Context, tokenHash string) (time.Time, bool, error) {
	var ms int64

	err := s.db.QueryRowContext(ctx, `SELECT expires_at FROM sessions WHERE token_hash = ?`, tokenHash).Scan(&ms)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}

	if err != nil {
		return time.Time{}, false, fmt.Errorf("read session: %w", err)
	}

	return time.UnixMilli(ms), true, nil
}

// ExtendSession moves a session's expiry; an unknown session is a no-op.
func (s *Store) ExtendSession(ctx context.Context, tokenHash string, expiresAt time.Time) error {
	if _, err := s.db.ExecContext(ctx, `UPDATE sessions SET expires_at = ? WHERE token_hash = ?`,
		expiresAt.UnixMilli(), tokenHash); err != nil {
		return fmt.Errorf("extend session: %w", err)
	}

	return nil
}

// DeleteSession removes a session; an unknown session is a no-op.
func (s *Store) DeleteSession(ctx context.Context, tokenHash string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, tokenHash); err != nil {
		return fmt.Errorf("delete session: %w", err)
	}

	return nil
}

// DeleteSessions removes every session that expires before t; a zero t removes all.
func (s *Store) DeleteSessions(ctx context.Context, t time.Time) error {
	query, args := `DELETE FROM sessions`, []any{}
	if !t.IsZero() {
		query, args = query+` WHERE expires_at < ?`, []any{t.UnixMilli()}
	}

	if _, err := s.db.ExecContext(ctx, query, args...); err != nil {
		return fmt.Errorf("delete sessions: %w", err)
	}

	return nil
}

// APIToken is a token's listing; the hash is never read back.
type APIToken struct {
	Name       string
	CreatedAt  time.Time
	LastUsedAt *time.Time
}

// CreateAPIToken stores a token by its hash and returns ErrConflict for a name that exists.
func (s *Store) CreateAPIToken(ctx context.Context, name, tokenHash string, createdAt time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO api_tokens (name, token_hash, created_at) VALUES (?, ?, ?)`,
		name, tokenHash, createdAt.UnixMilli())
	if isUniqueViolation(err) {
		return ErrConflict
	}

	if err != nil {
		return fmt.Errorf("create api token: %w", err)
	}

	return nil
}

// UseAPIToken marks the token used at t and reports whether it exists.
func (s *Store) UseAPIToken(ctx context.Context, tokenHash string, t time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE api_tokens SET last_used_at = ? WHERE token_hash = ?`,
		t.UnixMilli(), tokenHash)
	if err != nil {
		return false, fmt.Errorf("use api token: %w", err)
	}

	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("use api token: %w", err)
	}

	return n > 0, nil
}

// APITokens lists the tokens by name.
func (s *Store) APITokens(ctx context.Context) ([]APIToken, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, created_at, last_used_at FROM api_tokens ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list api tokens: %w", err)
	}
	defer rows.Close()

	var out []APIToken

	for rows.Next() {
		var (
			t        APIToken
			created  int64
			lastUsed sql.NullInt64
		)

		if err := rows.Scan(&t.Name, &created, &lastUsed); err != nil {
			return nil, fmt.Errorf("list api tokens: %w", err)
		}

		t.CreatedAt = time.UnixMilli(created)

		if lastUsed.Valid {
			at := time.UnixMilli(lastUsed.Int64)
			t.LastUsedAt = &at
		}

		out = append(out, t)
	}

	return out, rows.Err()
}

// DeleteAPIToken removes a token and returns ErrNotFound for a name that does not exist.
func (s *Store) DeleteAPIToken(ctx context.Context, name string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM api_tokens WHERE name = ?`, name)
	if err != nil {
		return fmt.Errorf("delete api token: %w", err)
	}

	return rowsAffectedOrNotFound(res)
}
