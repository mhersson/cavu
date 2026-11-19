package store

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func countRows(t *testing.T, s *Store, table string) int {
	t.Helper()

	var n int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM `+table).Scan(&n))

	return n
}

func appendN(t *testing.T, s *Store, taskID int64, n int) int64 {
	t.Helper()

	var last int64

	for range n {
		var err error

		last, err = s.Append(context.Background(), taskID, -1, []Event{{Type: "note", Actor: "engine", Payload: []byte(`{}`)}}, nil)
		require.NoError(t, err)
	}

	return last
}

func TestAppendCompareAndSwap(t *testing.T) {
	t.Parallel()

	s := openTest(t)
	ctx := context.Background()
	task := seedTask(t, s)
	require.Equal(t, int64(7), appendN(t, s, task.ID, 7))

	ev := []Event{{Type: "note", Actor: "user", Payload: []byte(`{"a":1}`)}}

	_, err := s.Append(ctx, task.ID, 6, ev, nil)
	require.ErrorIs(t, err, ErrConflict)
	require.Equal(t, 7, countRows(t, s, "events"))

	last, err := s.Append(ctx, task.ID, 7, ev, nil)
	require.NoError(t, err)
	require.Equal(t, int64(8), last)

	last, err = s.Append(ctx, task.ID, -1, ev, nil)
	require.NoError(t, err)
	require.Equal(t, int64(9), last)
}

func TestAppendExpectZeroOnEmptyTask(t *testing.T) {
	t.Parallel()

	s := openTest(t)
	ctx := context.Background()
	task := seedTask(t, s)
	other := seedTaskIn(t, s, task.ProjectID)
	appendN(t, s, other.ID, 2)

	ev := []Event{{Type: "note", Actor: "user", Payload: []byte(`{}`)}}

	last, err := s.Append(ctx, task.ID, 0, ev, nil)
	require.NoError(t, err)
	require.Equal(t, int64(3), last)

	_, err = s.Append(ctx, task.ID, 0, ev, nil)
	require.ErrorIs(t, err, ErrConflict)
}

func seedTaskIn(t *testing.T, s *Store, projectID int64) Task {
	t.Helper()

	task, err := s.CreateTask(context.Background(), Task{ProjectID: projectID, Title: "other"})
	require.NoError(t, err)

	return task
}

func TestAppendWritesUsageInSameTransaction(t *testing.T) {
	t.Parallel()

	s := openTest(t)
	ctx := context.Background()
	task := seedTask(t, s)

	events := []Event{{Type: "ok", Actor: "engine", Payload: []byte(`{}`)}}
	usage := []Usage{{Source: "proxy", Model: "m", Input: 1}}

	// A usage row naming a run that does not exist fails after the event insert.
	_, err := s.Append(ctx, task.ID, -1, events, []Usage{{RunID: 9999, Source: "proxy", Model: "m", Input: 1}})
	require.Error(t, err)
	require.Zero(t, countRows(t, s, "events"))
	require.Zero(t, countRows(t, s, "usage"))

	last, err := s.Append(ctx, task.ID, -1, events, usage)
	require.NoError(t, err)

	var eventID int64
	require.NoError(t, s.db.QueryRow(`SELECT event_id FROM usage`).Scan(&eventID))
	require.Equal(t, last, eventID)
}

func TestAppendUsageWithoutEventsHasNoEventID(t *testing.T) {
	t.Parallel()

	s := openTest(t)
	task := seedTask(t, s)

	_, err := s.Append(context.Background(), task.ID, -1, nil, []Usage{{Source: "agent", Model: "m", Output: 2}})
	require.NoError(t, err)

	var n int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM usage WHERE event_id IS NULL`).Scan(&n))
	require.Equal(t, 1, n)
}

func TestEventsPaging(t *testing.T) {
	t.Parallel()

	s := openTest(t)
	ctx := context.Background()
	task := seedTask(t, s)
	run, err := s.CreateRun(ctx, Run{TaskID: task.ID, Reason: "initial"})
	require.NoError(t, err)

	at := time.UnixMilli(1_700_000_000_000)
	_, err = s.Append(ctx, task.ID, -1, []Event{
		{RunID: run.ID, At: at, Type: "a", Actor: "agent", Payload: []byte(`{"k":"v"}`)},
		{Type: "b", Actor: "user"},
		{Type: "c", Actor: "user", Payload: []byte(`[1]`)},
	}, nil)
	require.NoError(t, err)

	all, err := s.Events(ctx, task.ID, 0, 0)
	require.NoError(t, err)
	require.Len(t, all, 3)
	require.Equal(t, run.ID, all[0].RunID)
	require.True(t, at.Equal(all[0].At))
	require.JSONEq(t, `{"k":"v"}`, string(all[0].Payload))
	require.Zero(t, all[1].RunID)
	require.False(t, all[1].At.IsZero())
	require.Equal(t, task.ID, all[1].TaskID)

	tests := []struct {
		name  string
		after int64
		limit int
		want  []string
	}{
		{"limit", 0, 2, []string{"a", "b"}},
		{"after", all[0].ID, 0, []string{"b", "c"}},
		{"after and limit", all[0].ID, 1, []string{"b"}},
		{"past end", all[2].ID, 0, nil},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := s.Events(ctx, task.ID, tc.after, tc.limit)
			require.NoError(t, err)

			var types []string
			for _, e := range got {
				types = append(types, e.Type)
			}

			require.Equal(t, tc.want, types)
		})
	}
}

func TestUsageTotals(t *testing.T) {
	t.Parallel()

	s := openTest(t)
	ctx := context.Background()
	task := seedTask(t, s)

	_, err := s.Append(ctx, task.ID, -1, nil, []Usage{
		{Source: "proxy", Model: "a", Input: 10, Output: 1, CacheRead: 2, CacheWrite: 3, CostEstimateUSD: 0.5},
		{Source: "proxy", Model: "a", Input: 5, Output: 4, CacheRead: 1, CacheWrite: 1, CostEstimateUSD: 0.25},
		{Source: "agent", Model: "b", Input: 7, CostEstimateUSD: 1},
	})
	require.NoError(t, err)

	got, err := s.UsageTotals(ctx, task.ID)
	require.NoError(t, err)
	require.Equal(t, []Usage{
		{Source: "agent", Model: "b", Input: 7, CostEstimateUSD: 1},
		{Source: "proxy", Model: "a", Input: 15, Output: 5, CacheRead: 3, CacheWrite: 4, CostEstimateUSD: 0.75},
	}, got)
}

func TestUsageSince(t *testing.T) {
	t.Parallel()

	s := openTest(t)
	ctx := context.Background()
	task := seedTask(t, s)
	other := seedTaskIn(t, s, task.ProjectID)
	note := []Event{{Type: "note", Actor: "engine", Payload: []byte(`{}`)}}

	_, err := s.Append(ctx, task.ID, -1, note, []Usage{{Source: "proxy", Model: "a", Input: 100}})
	require.NoError(t, err)

	after, err := s.Append(ctx, task.ID, -1, note, []Usage{{Source: "proxy", Model: "a", Input: 200}})
	require.NoError(t, err)

	for _, u := range [][]Usage{
		{{Source: "proxy", Model: "a", Input: 10, Output: 1, CacheRead: 2, CacheWrite: 3}, {Source: "agent", Model: "b", Input: 7}},
		{{Source: "proxy", Model: "a", Input: 5, Output: 4, CacheRead: 1, CacheWrite: 1}},
	} {
		_, err := s.Append(ctx, task.ID, -1, note, u)
		require.NoError(t, err)
	}

	_, err = s.Append(ctx, other.ID, -1, note, []Usage{{Source: "proxy", Model: "a", Input: 1000}})
	require.NoError(t, err)

	got, err := s.UsageSince(ctx, task.ID, after)
	require.NoError(t, err)
	require.Equal(t, []Usage{
		{Source: "agent", Model: "b", Input: 7},
		{Source: "proxy", Model: "a", Input: 15, Output: 5, CacheRead: 3, CacheWrite: 4},
	}, got)
}

func TestUsageBetween(t *testing.T) {
	t.Parallel()

	s := openTest(t)
	ctx := context.Background()
	task := seedTask(t, s)
	other := seedTaskIn(t, s, task.ProjectID)
	from := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	to := from.Add(24 * time.Hour)

	for _, row := range []struct {
		taskID int64
		at     time.Time
		usage  Usage
	}{
		{task.ID, from.Add(-time.Millisecond), Usage{Source: "proxy", Model: "a", Input: 100}},
		{task.ID, from, Usage{Source: "proxy", Model: "a", Input: 10, Output: 1}},
		{other.ID, from.Add(12 * time.Hour), Usage{Source: "proxy", Model: "a", Input: 5, CacheRead: 2}},
		{other.ID, to.Add(-time.Millisecond), Usage{Source: "agent", Model: "b", Output: 3}},
		{task.ID, to, Usage{Source: "proxy", Model: "a", Input: 1000}},
	} {
		_, err := s.Append(ctx, row.taskID, -1, []Event{{At: row.at, Type: "note", Actor: "engine", Payload: []byte(`{}`)}}, []Usage{row.usage})
		require.NoError(t, err)
	}

	got, err := s.UsageBetween(ctx, from, to)
	require.NoError(t, err)
	require.Equal(t, []Usage{
		{Source: "agent", Model: "b", Output: 3},
		{Source: "proxy", Model: "a", Input: 15, Output: 1, CacheRead: 2},
	}, got)
}

func TestAppendRaceAcrossHandles(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "cavu.db")
	ctx := context.Background()

	a, err := Open(ctx, path)
	require.NoError(t, err)

	t.Cleanup(func() { _ = a.Close() })

	b, err := Open(ctx, path)
	require.NoError(t, err)

	t.Cleanup(func() { _ = b.Close() })

	task := seedTask(t, a)
	base := appendN(t, a, task.ID, 3)

	const writers = 8

	errs := make([]error, writers)

	var wg sync.WaitGroup

	for i := range writers {
		wg.Go(func() {
			s := a
			if i%2 == 1 {
				s = b
			}

			_, errs[i] = s.Append(ctx, task.ID, base, []Event{{Type: "race", Actor: "user", Payload: []byte(`{}`)}}, nil)
		})
	}

	wg.Wait()

	wins := 0

	for _, err := range errs {
		if err == nil {
			wins++
		} else {
			require.ErrorIs(t, err, ErrConflict)
		}
	}

	require.Equal(t, 1, wins)
	require.Equal(t, int(base)+1, countRows(t, a, "events"))
}

func appendTyped(t *testing.T, s *Store, taskID int64, types ...string) []int64 {
	t.Helper()

	var ids []int64

	for _, ty := range types {
		id, err := s.Append(context.Background(), taskID, -1, []Event{{Type: ty, Actor: "engine", Payload: []byte(`{}`)}}, nil)
		require.NoError(t, err)

		ids = append(ids, id)
	}

	return ids
}

func eventIDs(events []Event) []int64 {
	var ids []int64
	for _, e := range events {
		ids = append(ids, e.ID)
	}

	return ids
}

func TestEventsOmitting(t *testing.T) {
	t.Parallel()

	s := openTest(t)
	ctx := context.Background()
	task := seedTask(t, s)
	ids := appendTyped(t, s, task.ID, "a", "b", "a", "c")

	got, err := s.EventsOmitting(ctx, task.ID, "a")
	require.NoError(t, err)
	require.Equal(t, []int64{ids[1], ids[3]}, eventIDs(got))

	got, err = s.EventsOmitting(ctx, task.ID)
	require.NoError(t, err)
	require.Equal(t, ids, eventIDs(got))
}

func TestLastEventIDs(t *testing.T) {
	t.Parallel()

	s := openTest(t)
	ctx := context.Background()
	one := seedTask(t, s)
	two := seedTaskIn(t, s, one.ProjectID)
	three := seedTaskIn(t, s, one.ProjectID)

	a := appendTyped(t, s, one.ID, "x", "x")
	b := appendTyped(t, s, two.ID, "x")

	got, err := s.LastEventIDs(ctx)
	require.NoError(t, err)
	require.Equal(t, map[int64]int64{one.ID: a[1], two.ID: b[0]}, got)

	last, err := s.LastEventID(ctx, one.ID)
	require.NoError(t, err)
	require.Equal(t, a[1], last)

	last, err = s.LastEventID(ctx, three.ID)
	require.NoError(t, err)
	require.Zero(t, last)
}

func TestEventsAfter(t *testing.T) {
	t.Parallel()

	s := openTest(t)
	ctx := context.Background()
	one := seedTask(t, s)
	two := seedTaskIn(t, s, one.ProjectID)

	a := appendTyped(t, s, one.ID, "x")
	b := appendTyped(t, s, two.ID, "x", "x")
	c := appendTyped(t, s, one.ID, "x")

	got, err := s.EventsAfter(ctx, a[0], 0, 0)
	require.NoError(t, err)
	require.Equal(t, []int64{b[0], b[1], c[0]}, eventIDs(got))

	got, err = s.EventsAfter(ctx, 0, one.ID, 0)
	require.NoError(t, err)
	require.Equal(t, []int64{a[0], c[0]}, eventIDs(got))

	got, err = s.EventsAfter(ctx, a[0], one.ID, 0)
	require.NoError(t, err)
	require.Equal(t, []int64{c[0]}, eventIDs(got))

	got, err = s.EventsAfter(ctx, 0, 0, 2)
	require.NoError(t, err)
	require.Equal(t, []int64{a[0], b[0]}, eventIDs(got))
}
