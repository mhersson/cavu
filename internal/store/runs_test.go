package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestOpenRun(t *testing.T) {
	t.Parallel()

	s := openTest(t)
	ctx := context.Background()
	task := seedTask(t, s)

	_, ok, err := s.OpenRun(ctx, task.ID)
	require.NoError(t, err)
	require.False(t, ok)

	first, err := s.CreateRun(ctx, Run{TaskID: task.ID, Reason: "initial"})
	require.NoError(t, err)

	second, err := s.CreateRun(ctx, Run{TaskID: task.ID, Reason: "retry"})
	require.NoError(t, err)

	ended := time.Now()
	first.EndedAt = &ended
	first.Outcome = "done"
	require.NoError(t, s.UpdateRun(ctx, first))

	got, ok, err := s.OpenRun(ctx, task.ID)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, second.ID, got.ID)
	require.Equal(t, "retry", got.Reason)
	require.Nil(t, got.EndedAt)

	second.EndedAt = &ended
	second.SessionID = "sess"
	require.NoError(t, s.UpdateRun(ctx, second))

	_, ok, err = s.OpenRun(ctx, task.ID)
	require.NoError(t, err)
	require.False(t, ok)
}

func TestUpdateRunKeepsStartedAt(t *testing.T) {
	t.Parallel()

	s := openTest(t)
	ctx := context.Background()
	task := seedTask(t, s)

	run, err := s.CreateRun(ctx, Run{TaskID: task.ID, Reason: "initial"})
	require.NoError(t, err)

	run.StartedAt = time.Time{}
	run.Outcome = "done"
	require.NoError(t, s.UpdateRun(ctx, run))

	got, ok, err := s.OpenRun(ctx, task.ID)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "done", got.Outcome)
	require.WithinDuration(t, time.Now(), got.StartedAt, time.Minute)
}
