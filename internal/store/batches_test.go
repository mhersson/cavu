package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func seedTasks(t *testing.T, s *Store, n int) []Task {
	t.Helper()

	tasks := []Task{seedTask(t, s)}
	for len(tasks) < n {
		tasks = append(tasks, seedTaskIn(t, s, tasks[0].ProjectID))
	}

	return tasks
}

func taskIDs(tasks ...Task) []int64 {
	out := make([]int64, len(tasks))
	for i, task := range tasks {
		out[i] = task.ID
	}

	return out
}

func createBatch(t *testing.T, s *Store, tasks ...Task) Batch {
	t.Helper()

	b, err := s.CreateBatch(context.Background(), Batch{ProjectID: tasks[0].ProjectID, Name: "b", Tasks: taskIDs(tasks...)}, nil)
	require.NoError(t, err)

	return b
}

func batchIDs(t *testing.T, s *Store, f BatchFilter) []int64 {
	t.Helper()

	bs, err := s.Batches(context.Background(), f)
	require.NoError(t, err)

	out := []int64{}
	for _, b := range bs {
		out = append(out, b.ID)
	}

	return out
}

var note = []Event{{Type: "note", Actor: "user", Payload: []byte(`{}`)}}

func TestBatchCreate(t *testing.T) {
	t.Parallel()

	s := openTest(t)
	ctx := context.Background()
	tasks := seedTasks(t, s, 4)
	started := time.UnixMilli(1_700_000_000_000)

	b, err := s.CreateBatch(ctx, Batch{
		ProjectID: tasks[0].ProjectID, Name: "ship it", StartedAt: &started,
		Tasks: taskIDs(tasks[2], tasks[0], tasks[1]),
	}, []TaskAppend{{TaskID: tasks[0].ID, ExpectLast: 0, Events: note}})
	require.NoError(t, err)
	require.NotZero(t, b.ID)
	require.Equal(t, int64(1), b.Version)
	require.False(t, b.CreatedAt.IsZero())

	events, err := s.Events(ctx, tasks[0].ID, 0, 0)
	require.NoError(t, err)
	require.Len(t, events, 1, "the create's append")

	got, err := s.Batch(ctx, b.ID)
	require.NoError(t, err)
	require.Equal(t, b, got)
	require.Equal(t, taskIDs(tasks[2], tasks[0], tasks[1]), got.Tasks)
	require.True(t, started.Equal(*got.StartedAt))
	require.Nil(t, got.FinishedAt)
	require.Nil(t, got.CancelledAt)
	require.Nil(t, got.EndedAt)
	require.Empty(t, got.BaseCommit)

	appendN(t, s, tasks[3].ID, 1)

	_, err = s.CreateBatch(ctx, Batch{ProjectID: tasks[0].ProjectID, Name: "late", Tasks: taskIDs(tasks[3])},
		[]TaskAppend{{TaskID: tasks[3].ID, ExpectLast: 0, Events: note}})
	require.ErrorIs(t, err, ErrConflict, "a task whose newest event moved")
	require.Equal(t, 2, countRows(t, s, "events"), "the create's append and the event it lost to")

	gone, err := s.CreateProject(ctx, Project{Name: "gone", RepoURL: "r", BaseBranch: "main"})
	require.NoError(t, err)
	require.NoError(t, s.RemoveProject(ctx, gone.ID, time.Now(), "gone/"))

	for _, project := range []int64{gone.ID, 999} {
		_, err = s.CreateBatch(ctx, Batch{ProjectID: project, Name: "x", Tasks: taskIDs(tasks[3])}, nil)
		require.ErrorIs(t, err, ErrNotFound, "project %d", project)
	}

	require.Equal(t, []int64{b.ID}, batchIDs(t, s, BatchFilter{}), "a refused create writes nothing")
}

func TestBatchesFilter(t *testing.T) {
	t.Parallel()

	s := openTest(t)
	ctx := context.Background()
	tasks := seedTasks(t, s, 2)

	q, err := s.CreateProject(ctx, Project{Name: "q", RepoURL: "r", BaseBranch: "main"})
	require.NoError(t, err)

	a := createBatch(t, s, tasks[0])
	ended := createBatch(t, s, tasks[1])
	other := createBatch(t, s, seedTaskIn(t, s, q.ID))

	_, err = s.WriteBatch(ctx, ended.ID, BatchWrite{Version: ended.Version, EndedAt: new(time.Now())})
	require.NoError(t, err)

	for _, tc := range []struct {
		name string
		f    BatchFilter
		want []int64
	}{
		{"all", BatchFilter{}, []int64{a.ID, ended.ID, other.ID}},
		{"one project", BatchFilter{ProjectID: tasks[0].ProjectID}, []int64{a.ID, ended.ID}},
		{"not ended", BatchFilter{Ended: new(false)}, []int64{a.ID, other.ID}},
		{"ended", BatchFilter{Ended: new(true)}, []int64{ended.ID}},
		{"one project, not ended", BatchFilter{ProjectID: tasks[0].ProjectID, Ended: new(false)}, []int64{a.ID}},
	} {
		require.Equal(t, tc.want, batchIDs(t, s, tc.f), tc.name)
	}

	bs, err := s.Batches(ctx, BatchFilter{ProjectID: q.ID})
	require.NoError(t, err)
	require.Equal(t, []Batch{other}, bs, "a listed batch carries its tasks")
}

func TestTaskBatch(t *testing.T) {
	t.Parallel()

	s := openTest(t)
	ctx := context.Background()
	tasks := seedTasks(t, s, 3)
	b := createBatch(t, s, tasks[0], tasks[1])
	createBatch(t, s, seedTaskIn(t, s, tasks[0].ProjectID)) // whose task no size of b counts

	place, err := s.TaskBatch(ctx, tasks[1].ID)
	require.NoError(t, err)
	require.Equal(t, BatchPlace{BatchID: b.ID, Name: "b", Position: 2, Size: 2}, place)

	place, err = s.TaskBatch(ctx, tasks[2].ID)
	require.NoError(t, err)
	require.Equal(t, BatchPlace{}, place, "a task in no batch")

	_, err = s.WriteBatch(ctx, b.ID, BatchWrite{Version: b.Version, Tasks: new(taskIDs(tasks[1], tasks[2], tasks[0]))})
	require.NoError(t, err)

	for i, task := range []Task{tasks[1], tasks[2], tasks[0]} {
		place, err = s.TaskBatch(ctx, task.ID)
		require.NoError(t, err)
		require.Equal(t, BatchPlace{BatchID: b.ID, Name: "b", Position: i + 1, Size: 3}, place)
	}
}

func TestBatchWriteStaleVersion(t *testing.T) {
	t.Parallel()

	s := openTest(t)
	ctx := context.Background()
	tasks := seedTasks(t, s, 2)
	b := createBatch(t, s, tasks[0])

	v, err := s.WriteBatch(ctx, b.ID, BatchWrite{Version: b.Version, Name: new("first")})
	require.NoError(t, err)
	require.Equal(t, b.Version+1, v)

	for _, w := range []BatchWrite{
		{
			Version: b.Version, Name: new("second"), Tasks: new(taskIDs(tasks[1], tasks[0])),
			Appends: []TaskAppend{{TaskID: tasks[0].ID, ExpectLast: -1, Events: note}},
		},
		{Version: b.Version, Delete: true},
	} {
		_, err = s.WriteBatch(ctx, b.ID, w)
		require.ErrorIs(t, err, ErrConflict)
	}

	got, err := s.Batch(ctx, b.ID)
	require.NoError(t, err)
	require.Equal(t, "first", got.Name)
	require.Equal(t, v, got.Version)
	require.Equal(t, taskIDs(tasks[0]), got.Tasks)
	require.Zero(t, countRows(t, s, "events"))

	skipped, err := s.WriteBatch(ctx, b.ID, BatchWrite{Version: -1, Name: new("third")})
	require.NoError(t, err, "-1 skips the check")
	require.Equal(t, v+1, skipped)

	_, err = s.WriteBatch(ctx, 999, BatchWrite{Version: -1})
	require.ErrorIs(t, err, ErrNotFound)
}

func TestBatchWriteKeepsWhatItLeavesOut(t *testing.T) {
	t.Parallel()

	s := openTest(t)
	ctx := context.Background()
	tasks := seedTasks(t, s, 2)
	b := createBatch(t, s, tasks...)
	at := time.UnixMilli(1_700_000_000_000)

	v, err := s.WriteBatch(ctx, b.ID, BatchWrite{Version: b.Version, BaseCommit: new("abc"), StartedAt: &at, FinishedAt: &at})
	require.NoError(t, err)

	v, err = s.WriteBatch(ctx, b.ID, BatchWrite{Version: v, Name: new("renamed")})
	require.NoError(t, err)

	v, err = s.WriteBatch(ctx, b.ID, BatchWrite{Version: v})
	require.NoError(t, err, "a write of nothing")
	require.Equal(t, b.Version+3, v, "every write raises the version")

	got, err := s.Batch(ctx, b.ID)
	require.NoError(t, err)
	require.Equal(t, Batch{
		ID: b.ID, ProjectID: b.ProjectID, Name: "renamed", Version: v, BaseCommit: "abc",
		Tasks: b.Tasks, CreatedAt: b.CreatedAt, StartedAt: &at, FinishedAt: &at,
	}, got)
}

func TestBatchWriteLosesToATaskEvent(t *testing.T) {
	t.Parallel()

	s := openTest(t)
	ctx := context.Background()
	tasks := seedTasks(t, s, 2)
	b := createBatch(t, s, tasks...)

	// The writer saw both tasks with no event; then the second got one.
	moved := appendN(t, s, tasks[1].ID, 1)

	w := BatchWrite{
		Version: b.Version,
		Tasks:   new(taskIDs(tasks[1], tasks[0])),
		Appends: []TaskAppend{
			{TaskID: tasks[0].ID, ExpectLast: 0, Events: note},
			{TaskID: tasks[1].ID, ExpectLast: 0},
		},
	}

	_, err := s.WriteBatch(ctx, b.ID, w)
	require.ErrorIs(t, err, ErrConflict)

	got, err := s.Batch(ctx, b.ID)
	require.NoError(t, err)
	require.Equal(t, b, got)
	require.Equal(t, 1, countRows(t, s, "events"), "only the event the write lost to")

	w.Appends[1].ExpectLast = moved

	_, err = s.WriteBatch(ctx, b.ID, w)
	require.NoError(t, err)

	got, err = s.Batch(ctx, b.ID)
	require.NoError(t, err)
	require.Equal(t, taskIDs(tasks[1], tasks[0]), got.Tasks)
}

func TestBatchWriteAppendsToSeveralTasks(t *testing.T) {
	t.Parallel()

	s := openTest(t)
	ctx := context.Background()
	tasks := seedTasks(t, s, 3)
	b := createBatch(t, s, tasks...)
	at := time.UnixMilli(1_700_000_000_000)
	cancel := []Event{{Type: "task_cancelled", Actor: "user", Payload: []byte(`{}`)}}

	w := BatchWrite{
		Version: b.Version,
		Appends: []TaskAppend{
			{TaskID: tasks[0].ID, ExpectLast: 0, Events: cancel, Usage: []Usage{{Source: "proxy", Model: "m", Input: 3}}},
			{TaskID: tasks[1].ID, ExpectLast: 0, Events: cancel},
			// A usage row naming a run that does not exist fails the third append.
			{TaskID: tasks[2].ID, ExpectLast: 0, Events: cancel, Usage: []Usage{{RunID: 9999, Source: "proxy", Model: "m"}}},
		},
		CancelledAt: &at,
		EndedAt:     &at,
		Tasks:       new(taskIDs(tasks[2], tasks[0])),
	}

	_, err := s.WriteBatch(ctx, b.ID, w)
	require.Error(t, err)
	require.Zero(t, countRows(t, s, "events"))
	require.Zero(t, countRows(t, s, "usage"))

	got, err := s.Batch(ctx, b.ID)
	require.NoError(t, err)
	require.Equal(t, b, got, "the row and its tasks unchanged")

	w.Appends[2].Usage = nil

	v, err := s.WriteBatch(ctx, b.ID, w)
	require.NoError(t, err)
	require.Equal(t, b.Version+1, v)

	for _, task := range tasks {
		events, err := s.Events(ctx, task.ID, 0, 0)
		require.NoError(t, err)
		require.Len(t, events, 1)
		require.Equal(t, "task_cancelled", events[0].Type)
	}

	var usageTask, usageEvent int64
	require.NoError(t, s.db.QueryRow(`SELECT task_id, event_id FROM usage`).Scan(&usageTask, &usageEvent))

	last, err := s.LastEventID(ctx, tasks[0].ID)
	require.NoError(t, err)
	require.Equal(t, []int64{tasks[0].ID, last}, []int64{usageTask, usageEvent}, "usage hangs on its task's event")

	got, err = s.Batch(ctx, b.ID)
	require.NoError(t, err)
	require.Equal(t, taskIDs(tasks[2], tasks[0]), got.Tasks)
	require.True(t, at.Equal(*got.CancelledAt))
	require.True(t, at.Equal(*got.EndedAt))
}

func TestOneBatchPerTask(t *testing.T) {
	t.Parallel()

	s := openTest(t)
	ctx := context.Background()
	tasks := seedTasks(t, s, 3)
	a := createBatch(t, s, tasks[0], tasks[1])

	_, err := s.CreateBatch(ctx, Batch{ProjectID: a.ProjectID, Name: "b", Tasks: taskIDs(tasks[2], tasks[0])}, nil)
	require.ErrorIs(t, err, ErrConflict)
	require.Equal(t, []int64{a.ID}, batchIDs(t, s, BatchFilter{}))

	b := createBatch(t, s, tasks[2])

	for _, list := range [][]int64{taskIDs(tasks[2], tasks[1]), taskIDs(tasks[2], tasks[2])} {
		_, err = s.WriteBatch(ctx, b.ID, BatchWrite{Version: b.Version, Tasks: &list})
		require.ErrorIs(t, err, ErrConflict, "%v", list)
	}

	got, err := s.Batch(ctx, b.ID)
	require.NoError(t, err)
	require.Equal(t, b, got)

	_, err = s.WriteBatch(ctx, a.ID, BatchWrite{Version: a.Version, Tasks: new(taskIDs(tasks[0]))})
	require.NoError(t, err)

	_, err = s.WriteBatch(ctx, b.ID, BatchWrite{Version: b.Version, Tasks: new(taskIDs(tasks[2], tasks[1]))})
	require.NoError(t, err, "a task taken out of one batch joins another")
}

func TestBatchDeleteRemovesRowsOnly(t *testing.T) {
	t.Parallel()

	s := openTest(t)
	ctx := context.Background()
	tasks := seedTasks(t, s, 3)
	other := createBatch(t, s, tasks[2])
	a := createBatch(t, s, tasks[0], tasks[1]) // the newest, so a reused id would be its
	appendN(t, s, tasks[0].ID, 2)

	v, err := s.WriteBatch(ctx, a.ID, BatchWrite{Version: a.Version, Delete: true})
	require.NoError(t, err)
	require.Zero(t, v)

	_, err = s.Batch(ctx, a.ID)
	require.ErrorIs(t, err, ErrNotFound)

	place, err := s.TaskBatch(ctx, tasks[0].ID)
	require.NoError(t, err)
	require.Equal(t, BatchPlace{}, place)

	for _, task := range tasks {
		_, err := s.Task(ctx, task.ID)
		require.NoError(t, err)
	}

	require.Equal(t, 2, countRows(t, s, "events"))

	got, err := s.Batch(ctx, other.ID)
	require.NoError(t, err)
	require.Equal(t, other, got)

	again := createBatch(t, s, tasks[0], tasks[1])
	require.NotEqual(t, a.ID, again.ID, "a deleted batch's id is never given again")

	_, err = s.WriteBatch(ctx, a.ID, BatchWrite{Version: -1, Delete: true})
	require.ErrorIs(t, err, ErrNotFound)
}

func TestConcurrentBatchWrites(t *testing.T) {
	t.Parallel()

	s := openTest(t)
	ctx := context.Background()
	b := createBatch(t, s, seedTask(t, s))

	const rounds = 20

	for round := range int64(rounds) {
		start := make(chan struct{})
		errs := make(chan error, 2)

		for _, name := range []string{"one", "two"} {
			go func() {
				<-start

				_, err := s.WriteBatch(ctx, b.ID, BatchWrite{Version: b.Version + round, Name: &name})
				errs <- err
			}()
		}

		close(start)

		var won int

		for range 2 {
			if err := <-errs; err == nil {
				won++
			} else {
				require.ErrorIs(t, err, ErrConflict)
			}
		}

		require.Equal(t, 1, won, "round %d", round)
	}

	got, err := s.Batch(ctx, b.ID)
	require.NoError(t, err)
	require.Equal(t, b.Version+rounds, got.Version)
}
