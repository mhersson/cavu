package store

import (
	"context"
	"path/filepath"
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

func TestMigrationThreeKeepsData(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "cavu.db")
	ctx := context.Background()

	old, err := open(ctx, path, migrations[:2])
	require.NoError(t, err)

	_, err = old.db.Exec(`INSERT INTO projects (name, repo_url, base_branch, setup_command, verify_command,
		verify_waived, tool_overrides, egress_hosts, env, secret_vars, switches)
		VALUES ('p', 'r', 'main', '', '', 0, '{}', '[]', '{}', '[]', '{}')`)
	require.NoError(t, err)
	_, err = old.db.Exec(`INSERT INTO tasks (project_id, title, body, requester, base_commit, reference,
		replay_id, overrides, created_at) VALUES (1, 't', '', 'cli', '', 0, 0, '{}', 1)`)
	require.NoError(t, err)
	require.NoError(t, old.Close())

	s, err := Open(ctx, path)
	require.NoError(t, err)

	t.Cleanup(func() { _ = s.Close() })

	p, err := s.ProjectByName(ctx, "p")
	require.NoError(t, err)
	require.Nil(t, p.CheckRequestedAt)
	require.Equal(t, CheckResult{}, p.CheckResult)

	task, err := s.Task(ctx, 1)
	require.NoError(t, err)
	require.Equal(t, "t", task.Title)
}

func TestMigrationFiveRenamesTheCaps(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "cavu.db")
	ctx := context.Background()

	old, err := open(ctx, path, migrations[:4])
	require.NoError(t, err)

	_, err = old.db.Exec(`INSERT INTO settings (key, value) VALUES ('work_timeout_minutes', '30'), ('work_max_turns', '50')`)
	require.NoError(t, err)
	_, err = old.db.Exec(`INSERT INTO projects (name, repo_url, base_branch, setup_command, verify_command,
		verify_waived, tool_overrides, egress_hosts, env, secret_vars, switches)
		VALUES ('p', 'r', 'main', '', '', 0, '{}', '[]', '{}', '[]', '{}')`)
	require.NoError(t, err)
	require.NoError(t, old.Close())

	s, err := Open(ctx, path)
	require.NoError(t, err)

	t.Cleanup(func() { _ = s.Close() })

	settings, err := s.Settings(ctx)
	require.NoError(t, err)
	require.Equal(t, map[string]string{"task_timeout_minutes": "30", "task_max_turns": "50"}, settings)

	p, err := s.ProjectByName(ctx, "p")
	require.NoError(t, err)
	require.Empty(t, p.Caps)
}
