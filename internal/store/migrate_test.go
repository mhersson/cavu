package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
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

func TestMigrationSixKeepsProjects(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "cavu.db")
	ctx := context.Background()

	old, err := open(ctx, path, migrations[:5])
	require.NoError(t, err)

	_, err = old.db.Exec(`INSERT INTO projects (name, repo_url, base_branch, setup_command, verify_command,
		verify_waived, tool_overrides, egress_hosts, env, secret_vars, switches)
		VALUES ('p', 'r', 'main', '', '', 0, '{}', '[]', '{}', '[]', '{}')`)
	require.NoError(t, err)
	require.NoError(t, old.Close())

	s, err := Open(ctx, path)
	require.NoError(t, err)

	t.Cleanup(func() { _ = s.Close() })

	p, err := s.ProjectByName(ctx, "p")
	require.NoError(t, err)
	require.Equal(t, "r", p.RepoURL)
	require.Equal(t, ForgePolicy{}, p.Forge)
}

func TestMigrationEightKeepsProjectsOnTheDefault(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "cavu.db")
	ctx := context.Background()

	old, err := open(ctx, path, migrations[:7])
	require.NoError(t, err)

	_, err = old.db.Exec(`INSERT INTO projects (name, repo_url, base_branch, setup_command, verify_command,
		verify_waived, tool_overrides, egress_hosts, env, secret_vars, switches, removed_at)
		VALUES ('live', 'r', 'main', '', '', 0, '{}', '[]', '{}', '[]', '{}', NULL),
		       ('gone', 'r', 'main', '', '', 0, '{}', '[]', '{}', '[]', '{}', 5)`)
	require.NoError(t, err)
	require.NoError(t, old.Close())

	s, err := Open(ctx, path)
	require.NoError(t, err)

	t.Cleanup(func() { _ = s.Close() })

	live, err := s.ProjectByName(ctx, "live")
	require.NoError(t, err)
	require.Empty(t, live.ForgeCredential)

	var gone Project

	rows, err := s.db.Query(`SELECT id FROM projects WHERE name = 'gone'`)
	require.NoError(t, err)

	defer rows.Close()

	require.True(t, rows.Next())
	require.NoError(t, rows.Scan(&gone.ID))

	gone, err = s.Project(ctx, gone.ID)
	require.NoError(t, err)
	require.NotNil(t, gone.RemovedAt)
	require.Empty(t, gone.ForgeCredential)

	creds, err := s.Credentials(ctx)
	require.NoError(t, err)
	require.Empty(t, creds)
}

func TestMigrationElevenKeepsData(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "cavu.db")
	ctx := context.Background()

	old, err := open(ctx, path, migrations[:10])
	require.NoError(t, err)

	_, err = old.db.Exec(`INSERT INTO projects (name, repo_url, base_branch, setup_command, verify_command,
		verify_waived, tool_overrides, egress_hosts, env, secret_vars, switches)
		VALUES ('p', 'r', 'main', '', '', 0, '{}', '[]', '{}', '[]', '{}')`)
	require.NoError(t, err)

	for range 2 {
		_, err = old.db.Exec(`INSERT INTO tasks (project_id, title, body, requester, base_commit, reference,
			replay_id, overrides, created_at) VALUES (1, 't', '', 'cli', 'abc', 0, 0, '{}', 1)`)
		require.NoError(t, err)
	}

	_, err = old.db.Exec(`INSERT INTO events (task_id, at, type, actor, payload) VALUES (1, 1, 'x', 'user', '{}')`)
	require.NoError(t, err)
	require.NoError(t, old.Close())

	s, err := Open(ctx, path)
	require.NoError(t, err)

	t.Cleanup(func() { _ = s.Close() })

	task, err := s.Task(ctx, 2)
	require.NoError(t, err)
	require.Equal(t, "abc", task.BaseCommit)

	events, err := s.Events(ctx, 1, 0, 0)
	require.NoError(t, err)
	require.Len(t, events, 1)

	_, err = s.db.Exec(`INSERT INTO batches (project_id, name, version, base_commit, created_at) VALUES (1, 'b', 1, '', 1)`)
	require.NoError(t, err)
	_, err = s.db.Exec(`INSERT INTO batch_tasks (batch_id, task_id, position) VALUES (1, 2, 1), (1, 1, 2)`)
	require.NoError(t, err, "the tasks kept join a batch")
}

type columnInfo struct {
	Name, Type string
	NotNull    bool
	Default    string
	PK         int
}

func projectColumnInfo(t *testing.T, s *Store) []columnInfo {
	t.Helper()

	rows, err := s.db.Query(`SELECT name, type, "notnull", COALESCE(dflt_value, ''), pk FROM pragma_table_info('projects') ORDER BY cid`)
	require.NoError(t, err)

	defer rows.Close()

	var out []columnInfo

	for rows.Next() {
		var c columnInfo
		require.NoError(t, rows.Scan(&c.Name, &c.Type, &c.NotNull, &c.Default, &c.PK))

		out = append(out, c)
	}

	require.NoError(t, rows.Err())

	return out
}

func projectRows(t *testing.T, s *Store) [][]any {
	t.Helper()

	rows, err := s.db.Query(`SELECT * FROM projects ORDER BY id`)
	require.NoError(t, err)

	defer rows.Close()

	cols, err := rows.Columns()
	require.NoError(t, err)

	var out [][]any

	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))

		for i := range vals {
			ptrs[i] = &vals[i]
		}

		require.NoError(t, rows.Scan(ptrs...))

		out = append(out, vals)
	}

	require.NoError(t, rows.Err())

	return out
}

func TestMigrationSevenKeepsEverythingAndFreesNames(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "cavu.db")
	ctx := context.Background()

	old, err := open(ctx, path, migrations[:6])
	require.NoError(t, err)

	for _, name := range []string{"alpha", "beta"} {
		_, err = old.db.Exec(`INSERT INTO projects (name, repo_url, base_branch, setup_command, verify_command,
			verify_waived, tool_overrides, egress_hosts, env, secret_vars, switches, caps, forge)
			VALUES (?, 'r', 'main', 'setup', 'verify', 1, '{"go":"1"}', '["h"]', '{"A":"b"}', '["V"]', '{"s":true}', '{"c":"1"}', '{"NoCI":true}')`, name)
		require.NoError(t, err)
	}

	_, err = old.db.Exec(`UPDATE projects SET baseline_commit = 'abc', baseline_passed = 1, baseline_at = 5, baseline_output = 'out',
		check_requested_at = 7 WHERE name = 'beta'`)
	require.NoError(t, err)

	for _, project := range []int{1, 2, 2} {
		_, err = old.db.Exec(`INSERT INTO tasks (project_id, title, body, requester, base_commit, reference,
			replay_id, overrides, created_at) VALUES (?, 't', '', 'cli', '', 0, 0, '{}', 1)`, project)
		require.NoError(t, err)
	}

	_, err = old.db.Exec(`INSERT INTO events (task_id, at, type, actor, payload) VALUES (1, 1, 'x', 'user', '{}'), (3, 2, 'y', 'user', '{}')`)
	require.NoError(t, err)

	before := projectColumnInfo(t, old)

	oldRows := projectRows(t, old)

	require.NoError(t, old.Close())

	s, err := Open(ctx, path)
	require.NoError(t, err)

	t.Cleanup(func() { _ = s.Close() })

	assert.Equal(t, append(before, columnInfo{Name: "removed_at", Type: "INTEGER"}, columnInfo{Name: "forge_credential", Type: "TEXT"}),
		projectColumnInfo(t, s), "the rebuilt table is the old one plus removed_at and the later forge_credential")

	var uniques []string

	rows, err := s.db.Query(`SELECT name FROM pragma_index_list('projects') WHERE "unique" = 1 AND partial = 1`)
	require.NoError(t, err)

	defer rows.Close()

	for rows.Next() {
		var n string
		require.NoError(t, rows.Scan(&n))

		uniques = append(uniques, n)
	}

	require.NoError(t, rows.Err())
	require.Equal(t, []string{"projects_name"}, uniques)

	newRows := projectRows(t, s)
	require.Len(t, newRows, len(oldRows))

	for i, want := range oldRows {
		require.Equal(t, append(want, nil, nil), newRows[i], "every column and id kept, removed_at and forge_credential empty")
	}

	var tasks, events, bad int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM tasks`).Scan(&tasks))
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM events`).Scan(&events))
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM pragma_foreign_key_check`).Scan(&bad))
	require.Equal(t, 3, tasks)
	require.Equal(t, 2, events)
	require.Zero(t, bad)

	fresh, err := s.CreateProject(ctx, Project{Name: "gamma"})
	require.NoError(t, err)
	require.Equal(t, int64(3), fresh.ID, "the id sequence goes on above every old id")

	_, err = s.CreateProject(ctx, Project{Name: "alpha"})
	require.ErrorIs(t, err, ErrConflict, "a live name stays unique")
}
