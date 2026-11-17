package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// moreTask creates another task in the project of like.
func moreTask(t *testing.T, s *Store, like Task) Task {
	t.Helper()

	task, err := s.CreateTask(context.Background(), Task{ProjectID: like.ProjectID, Title: "other"})
	require.NoError(t, err)

	return task
}

func TestReplayRoundTrip(t *testing.T) {
	t.Parallel()

	s := openTest(t)
	ctx := context.Background()
	src := seedTask(t, s)
	shadow := moreTask(t, s, src)

	r, err := s.CreateReplay(ctx, Replay{Name: "effort low", Config: map[string]string{"effort_work": "low"}})
	require.NoError(t, err)
	require.NotZero(t, r.ID)
	require.False(t, r.StartedAt.IsZero())
	require.Nil(t, r.EndedAt)

	item := ReplayItem{
		ReplayID: r.ID, SourceTaskID: src.ID, TaskID: shadow.ID, Verify: "passed", Stop: "question",
		Asked: 3, Turns: 7, Tokens: 1234, CostEstimateUSD: 0.42, Duration: 90 * time.Second, DiffPath: "diff.patch",
	}
	require.NoError(t, s.PutReplayItem(ctx, item))

	got, err := s.ReplayItems(ctx, r.ID)
	require.NoError(t, err)
	require.Equal(t, []ReplayItem{item}, got)

	item.Verify, item.Turns = "failed", 9
	require.NoError(t, s.PutReplayItem(ctx, item))

	got, err = s.ReplayItems(ctx, r.ID)
	require.NoError(t, err)
	require.Equal(t, []ReplayItem{item}, got)
}

func TestEndReplay(t *testing.T) {
	t.Parallel()

	s := openTest(t)
	ctx := context.Background()

	require.ErrorIs(t, s.EndReplay(ctx, 99, time.Now()), ErrNotFound)

	r, err := s.CreateReplay(ctx, Replay{Name: "x"})
	require.NoError(t, err)

	at := time.UnixMilli(1_700_000_000_000)
	require.NoError(t, s.EndReplay(ctx, r.ID, at))

	got, ok, err := s.PreviousReplay(ctx, r.ID+1)
	require.NoError(t, err)
	require.True(t, ok)
	require.NotNil(t, got.EndedAt)
	require.True(t, at.Equal(*got.EndedAt))
}

func TestReplayItemsOrderedBySource(t *testing.T) {
	t.Parallel()

	s := openTest(t)
	ctx := context.Background()
	a := seedTask(t, s)
	b, shadow := moreTask(t, s, a), moreTask(t, s, a)

	r, err := s.CreateReplay(ctx, Replay{Name: "x"})
	require.NoError(t, err)

	for _, src := range []int64{b.ID, a.ID} {
		require.NoError(t, s.PutReplayItem(ctx, ReplayItem{ReplayID: r.ID, SourceTaskID: src, TaskID: shadow.ID}))
	}

	got, err := s.ReplayItems(ctx, r.ID)
	require.NoError(t, err)
	require.Len(t, got, 2)
	require.Equal(t, a.ID, got[0].SourceTaskID)
	require.Equal(t, b.ID, got[1].SourceTaskID)
}

func TestPreviousReplay(t *testing.T) {
	t.Parallel()

	s := openTest(t)
	ctx := context.Background()

	_, ok, err := s.PreviousReplay(ctx, 100)
	require.NoError(t, err)
	require.False(t, ok)

	first, err := s.CreateReplay(ctx, Replay{Name: "first", Image: "cavu-sandbox:1", Config: map[string]string{"model_work": "m"}})
	require.NoError(t, err)
	require.NoError(t, s.EndReplay(ctx, first.ID, time.Now()))

	second, err := s.CreateReplay(ctx, Replay{Name: "second"})
	require.NoError(t, err)
	require.NoError(t, s.EndReplay(ctx, second.ID, time.Now()))

	cutOff, err := s.CreateReplay(ctx, Replay{Name: "cut off"})
	require.NoError(t, err)

	_, ok, err = s.PreviousReplay(ctx, first.ID)
	require.NoError(t, err)
	require.False(t, ok)

	got, ok, err := s.PreviousReplay(ctx, second.ID)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, first.ID, got.ID)
	require.Equal(t, first.Config, got.Config)
	require.Equal(t, first.Image, got.Image)

	got, ok, err = s.PreviousReplay(ctx, cutOff.ID+1)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, second.ID, got.ID, "an unended replay is skipped")
}

func TestMigrationTwoOnExistingDatabase(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "cavu.db")
	ctx := context.Background()

	old, err := open(ctx, path, migrations[:1])
	require.NoError(t, err)

	_, err = old.db.Exec(`INSERT INTO projects (name, repo_url, base_branch, setup_command, verify_command,
		verify_waived, tool_overrides, egress_hosts, env, secret_vars, switches)
		VALUES ('p', 'r', 'main', '', '', 0, '{}', '[]', '{}', '[]', '{}')`)
	require.NoError(t, err)
	_, err = old.db.Exec(`INSERT INTO tasks (project_id, title, body, requester, base_commit, reference,
		replay_id, overrides, created_at) VALUES (1, 't', '', 'cli', '', 0, 0, '{}', 1)`)
	require.NoError(t, err)
	require.False(t, tableExists(t, old, "replays"))
	require.NoError(t, old.Close())

	s, err := Open(ctx, path)
	require.NoError(t, err)

	t.Cleanup(func() { _ = s.Close() })

	require.True(t, tableExists(t, s, "replays"))
	require.True(t, tableExists(t, s, "replay_items"))

	got, err := s.Task(ctx, 1)
	require.NoError(t, err)
	require.Equal(t, "t", got.Title)

	var versions int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version IN (1, 2)`).Scan(&versions))
	require.Equal(t, 2, versions)
}
