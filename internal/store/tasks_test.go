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

// taskState is what an edit may change of a task: its row, its dependencies and its images.
type taskState struct {
	Task Task
	Deps []int64
	Imgs []Image
}

func stateOf(t *testing.T, s *Store, taskID int64) taskState {
	t.Helper()

	task, err := s.Task(t.Context(), taskID)
	require.NoError(t, err)

	deps, err := s.TaskDeps(t.Context(), taskID)
	require.NoError(t, err)

	return taskState{task, deps, taskImages(t, s, taskID)}
}

func TestEditTask(t *testing.T) {
	t.Parallel()

	s, dir := openWithImages(t)
	task := seedTask(t, s)
	old, dep := otherTask(t, s, task), otherTask(t, s, task)
	require.NoError(t, s.SetTaskDeps(t.Context(), task.ID, []int64{old.ID}))
	gone := putImage(t, s, task.ID, "gone.png", []byte("gone"))
	kept := putImage(t, s, task.ID, "kept.png", []byte("kept"))
	newest := appendN(t, s, task.ID, 1)

	edit := task
	edit.Title, edit.Body, edit.Requester = "Renamed", "A new body.", "someone else"
	require.NoError(t, s.EditTask(t.Context(), edit, []int64{dep.ID}, []int64{gone.ID}, newest))

	got := stateOf(t, s, task.ID)
	require.Equal(t, "Renamed", got.Task.Title)
	require.Equal(t, "A new body.", got.Task.Body)
	require.Equal(t, task.Requester, got.Task.Requester, "only the title and the body are written of the row")
	require.Equal(t, []int64{dep.ID}, got.Deps)
	require.Equal(t, []Image{kept}, got.Imgs)
	require.Equal(t, []string{kept.SHA256}, imageFiles(t, dir), "the removed image's contents are gone")
}

func TestEditTaskWritesNothingOnAFailure(t *testing.T) {
	t.Parallel()

	// edit holds what a case's edit may name: the task with a new title and
	// body, a task it may depend on, its image, another task's image, and the
	// id of an event older than its newest.
	type edit struct {
		task, other    Task
		img, elsewhere Image
		seen           int64
	}

	tests := map[string]struct {
		call func(ctx context.Context, s *Store, e edit) error
		want error // nil: any error
	}{
		"an image the task lacks": {func(ctx context.Context, s *Store, e edit) error {
			return s.EditTask(ctx, e.task, []int64{e.other.ID}, []int64{e.img.ID, e.elsewhere.ID + 1}, -1)
		}, ErrNotFound},
		"another task's image": {func(ctx context.Context, s *Store, e edit) error {
			return s.EditTask(ctx, e.task, []int64{e.other.ID}, []int64{e.img.ID, e.elsewhere.ID}, -1)
		}, ErrNotFound},
		"a dependency that does not exist": {func(ctx context.Context, s *Store, e edit) error {
			return s.EditTask(ctx, e.task, []int64{e.other.ID, 9999}, []int64{e.img.ID}, -1)
		}, nil},
		"an event newer than the one seen": {func(ctx context.Context, s *Store, e edit) error {
			return s.EditTask(ctx, e.task, []int64{e.other.ID}, []int64{e.img.ID}, e.seen)
		}, ErrConflict},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			s, dir := openWithImages(t)
			task := seedTask(t, s)
			dep, other := otherTask(t, s, task), otherTask(t, s, task)
			require.NoError(t, s.SetTaskDeps(t.Context(), task.ID, []int64{dep.ID}))
			img := putImage(t, s, task.ID, "shot.png", []byte("shot"))
			elsewhere := putImage(t, s, other.ID, "other.png", []byte("other"))
			seen := appendN(t, s, task.ID, 1)
			appendN(t, s, task.ID, 1)
			was := stateOf(t, s, task.ID)

			e := edit{task: task, other: other, img: img, elsewhere: elsewhere, seen: seen}
			e.task.Title, e.task.Body = "Renamed", "A new body."

			err := tc.call(t.Context(), s, e)
			if tc.want != nil {
				require.ErrorIs(t, err, tc.want)
			} else {
				require.Error(t, err)
			}

			require.Equal(t, was, stateOf(t, s, task.ID), "nothing is written")
			require.Equal(t, []byte("shot"), readImage(t, s, task.ID, img.ID), "the image keeps its contents")
			require.ElementsMatch(t, []string{img.SHA256, elsewhere.SHA256}, imageFiles(t, dir))
		})
	}
}
