package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Migration is one versioned schema change.
type Migration struct {
	Version int
	SQL     string
}

// migrate applies the migrations not yet in the schema_migrations ledger, all
// with their ledger rows in one transaction so a failure leaves nothing behind.
// The transaction is immediate (see dsn), so a concurrent opener waits for it,
// then finds every version recorded and applies nothing.
func migrate(ctx context.Context, db *sql.DB, ms []Migration) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: migrations: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    INTEGER PRIMARY KEY,
		applied_at INTEGER NOT NULL
	)`); err != nil {
		return fmt.Errorf("store: schema_migrations table: %w", err)
	}

	applied, err := appliedVersions(ctx, tx)
	if err != nil {
		return err
	}

	for _, m := range ms {
		if applied[m.Version] {
			continue
		}

		if err := applyMigration(ctx, tx, m); err != nil {
			return fmt.Errorf("store: migration v%d: %w", m.Version, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: migrations commit: %w", err)
	}

	return nil
}

func appliedVersions(ctx context.Context, tx *sql.Tx) (map[int]bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("store: schema_migrations query: %w", err)
	}
	defer rows.Close()

	applied := map[int]bool{}

	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("store: schema_migrations scan: %w", err)
		}

		applied[v] = true
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: schema_migrations rows: %w", err)
	}

	return applied, nil
}

func applyMigration(ctx context.Context, tx *sql.Tx, m Migration) error {
	if _, err := tx.ExecContext(ctx, m.SQL); err != nil {
		return err
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`,
		m.Version, time.Now().UnixMilli(),
	); err != nil {
		return fmt.Errorf("record: %w", err)
	}

	return nil
}
