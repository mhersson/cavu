package store

import (
	"context"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func tableExists(t *testing.T, s *Store, name string) bool {
	t.Helper()

	var n int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&n))

	return n > 0
}

func TestMigrateIsAtomic(t *testing.T) {
	t.Parallel()

	s := openTest(t)
	ctx := context.Background()

	bad := []Migration{{Version: 99, SQL: `CREATE TABLE half_done (x INTEGER); INSERT INTO no_such_table VALUES (1);`}}
	require.Error(t, migrate(ctx, s.db, bad))

	require.False(t, tableExists(t, s, "half_done"))

	var n int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version = 99`).Scan(&n))
	require.Zero(t, n)
}

func TestMigrateTwice(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "cavu.db")
	ctx := context.Background()

	first, err := Open(ctx, path)
	require.NoError(t, err)

	var before int
	require.NoError(t, first.db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&before))
	require.NoError(t, first.Close())

	second, err := Open(ctx, path)
	require.NoError(t, err)

	t.Cleanup(func() { _ = second.Close() })

	var after int
	require.NoError(t, second.db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&after))
	require.Equal(t, len(migrations), before)
	require.Equal(t, before, after)
}

func TestMigrateConcurrentFirstOpen(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "cavu.db")
	ctx := context.Background()

	// The extra migration adds a row each time it is applied.
	ms := append(slices.Clone(migrations), Migration{Version: 1000, SQL: `
		CREATE TABLE IF NOT EXISTS applied (version INTEGER NOT NULL);
		INSERT INTO applied VALUES (1000);`})

	const openers = 8

	start := make(chan struct{})
	errs := make(chan error, openers)

	var wg sync.WaitGroup
	for range openers {
		wg.Go(func() {
			<-start

			s, err := open(ctx, path, ms)
			if err == nil {
				_ = s.Close()
			}

			errs <- err
		})
	}

	close(start)
	wg.Wait()
	close(errs)

	for err := range errs {
		require.NoError(t, err)
	}

	s, err := Open(ctx, path)
	require.NoError(t, err)

	t.Cleanup(func() { _ = s.Close() })

	var ledger, applied int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&ledger))
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM applied`).Scan(&applied))
	require.Equal(t, len(ms), ledger)
	require.Equal(t, 1, applied)
}

func TestMigrateAppliesInOrderOnce(t *testing.T) {
	t.Parallel()

	s := openTest(t)
	ctx := context.Background()

	ms := []Migration{
		{Version: 100, SQL: `CREATE TABLE extra (id INTEGER PRIMARY KEY)`},
		{Version: 101, SQL: `ALTER TABLE extra ADD COLUMN note TEXT`},
	}
	require.NoError(t, migrate(ctx, s.db, ms))
	require.NoError(t, migrate(ctx, s.db, ms))

	var n int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version IN (100, 101)`).Scan(&n))
	require.Equal(t, 2, n)
}
