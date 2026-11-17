package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestTaskRoundTrip(t *testing.T) {
	t.Parallel()

	s := openTest(t)
	ctx := context.Background()

	p, err := s.CreateProject(ctx, Project{Name: "p"})
	require.NoError(t, err)

	created, err := s.CreateTask(ctx, Task{
		ProjectID: p.ID, Title: "fix it", Body: "details", Requester: "ada",
		Reference: true, Overrides: map[string]string{"model_work": "m"},
		CreatedAt: time.UnixMilli(1_700_000_000_000),
	})
	require.NoError(t, err)
	require.NotZero(t, created.ID)

	got, err := s.Task(ctx, created.ID)
	require.NoError(t, err)
	require.True(t, created.CreatedAt.Equal(got.CreatedAt))
	created.CreatedAt, got.CreatedAt = time.Time{}, time.Time{}
	require.Equal(t, created, got)

	got.BaseCommit = "deadbeef"
	got.Title = "renamed"
	require.NoError(t, s.UpdateTask(ctx, got))

	again, err := s.Task(ctx, got.ID)
	require.NoError(t, err)
	require.Equal(t, "deadbeef", again.BaseCommit)
	require.Equal(t, "renamed", again.Title)
}

func TestTaskDefaultsCreatedAt(t *testing.T) {
	t.Parallel()

	s := openTest(t)
	task := seedTask(t, s)

	require.WithinDuration(t, time.Now(), task.CreatedAt, time.Minute)
}

func TestTasksReplayFilter(t *testing.T) {
	t.Parallel()

	s := openTest(t)
	ctx := context.Background()

	task := seedTask(t, s)
	shadow, err := s.CreateTask(ctx, Task{ProjectID: task.ProjectID, Title: "shadow", ReplayID: 5})
	require.NoError(t, err)

	tests := []struct {
		name     string
		replays  bool
		wantTask []int64
	}{
		{"without replays", false, []int64{task.ID}},
		{"with replays", true, []int64{task.ID, shadow.ID}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := s.Tasks(ctx, tc.replays)
			require.NoError(t, err)

			var ids []int64
			for _, g := range got {
				ids = append(ids, g.ID)
			}

			require.Equal(t, tc.wantTask, ids)
		})
	}
}

func TestUpdateTaskKeepsCreatedAt(t *testing.T) {
	t.Parallel()

	s := openTest(t)
	ctx := context.Background()
	task := seedTask(t, s)

	task.CreatedAt = time.Time{}
	task.Title = "changed"
	require.NoError(t, s.UpdateTask(ctx, task))

	got, err := s.Task(ctx, task.ID)
	require.NoError(t, err)
	require.Equal(t, "changed", got.Title)
	require.WithinDuration(t, time.Now(), got.CreatedAt, time.Minute)
}

func TestTaskDeps(t *testing.T) {
	t.Parallel()

	s := openTest(t)
	ctx := context.Background()
	first := seedTask(t, s)
	ids := []int64{first.ID}

	for range 3 {
		ids = append(ids, seedTaskIn(t, s, first.ProjectID).ID)
	}

	set := func(deps ...int64) error { return s.SetTaskDeps(ctx, ids[0], deps) }
	read := func() []int64 {
		got, err := s.TaskDeps(ctx, ids[0])
		require.NoError(t, err)

		return got
	}

	require.NoError(t, set(ids[2], ids[1]))
	require.Equal(t, []int64{ids[1], ids[2]}, read())

	require.NoError(t, set(ids[3]))
	require.Equal(t, []int64{ids[3]}, read())

	require.Error(t, set(ids[1], 9999))
	require.Equal(t, []int64{ids[3]}, read())

	require.NoError(t, set())
	require.Empty(t, read())
}
