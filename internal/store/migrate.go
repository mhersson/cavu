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

// migrate applies the migrations not yet in the schema_migrations ledger,
// each with its ledger row in one transaction so a failure leaves nothing behind.
func migrate(ctx context.Context, db *sql.DB, ms []Migration) error {
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    INTEGER PRIMARY KEY,
		applied_at INTEGER NOT NULL
	)`); err != nil {
		return fmt.Errorf("store: schema_migrations table: %w", err)
	}

	applied, err := appliedVersions(ctx, db)
	if err != nil {
		return err
	}

	for _, m := range ms {
		if applied[m.Version] {
			continue
		}

		if err := applyMigration(ctx, db, m); err != nil {
			return fmt.Errorf("store: migration v%d: %w", m.Version, err)
		}
	}

	return nil
}

func appliedVersions(ctx context.Context, db *sql.DB) (map[int]bool, error) {
	rows, err := db.QueryContext(ctx, `SELECT version FROM schema_migrations`)
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

func applyMigration(ctx context.Context, db *sql.DB, m Migration) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, m.SQL); err != nil {
		return err
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`,
		m.Version, toMillis(time.Now()),
	); err != nil {
		return fmt.Errorf("record: %w", err)
	}

	return tx.Commit()
}
