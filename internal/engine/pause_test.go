package engine_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/mhersson/cavu/internal/engine"
	"github.com/mhersson/cavu/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// readsStore records the task of each EventsOmitting call.
type readsStore struct {
	engine.Store

	reads []int64
}

func (s *readsStore) EventsOmitting(ctx context.Context, taskID int64, omit ...string) ([]store.Event, error) {
	s.reads = append(s.reads, taskID)

	return s.Store.EventsOmitting(ctx, taskID, omit...)
}

// stopTask records a stop on the task as the engine would.
func (h *harness) stopTask(id int64, p engine.StoppedPayload) {
	h.t.Helper()

	raw, err := json.Marshal(p)
	require.NoError(h.t, err)

	_, err = h.store.Append(h.t.Context(), id, -1, []store.Event{{Type: engine.Stopped, Actor: "engine", Payload: raw}}, nil)
	require.NoError(h.t, err)
}

func TestPausesFollowTheStore(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	endpoint, limited, alsoLimited := h.started(), h.started(), h.started()
	resets := now.Add(time.Hour)

	h.stopTask(endpoint.ID, engine.StoppedPayload{Reason: engine.StopEndpoint, Action: engine.ClearRetry})
	h.stopTask(alsoLimited.ID, engine.StoppedPayload{Reason: engine.StopUsageLimit, Action: engine.ClearNone, ResetsAt: resets})
	h.stopTask(limited.ID, engine.StoppedPayload{Reason: engine.StopUsageLimit, Action: engine.ClearNone, ResetsAt: resets})

	s := &readsStore{Store: h.store}

	var p engine.Pauses

	require.NoError(t, p.Read(ctx, s))
	assert.ElementsMatch(t, []int64{h.task.ID, endpoint.ID, limited.ID, alsoLimited.ID}, s.reads, "the first read reads every task")

	got := p.Of(h.task.ID)
	assert.Equal(t, endpoint.ID, got.EndpointTask)
	assert.True(t, resets.Equal(got.LimitedUntil), got.LimitedUntil)
	assert.Equal(t, limited.ID, got.LimitedBy, "of two limits that reset together, the lower task names the pause")
	assert.Zero(t, p.Of(endpoint.ID).EndpointTask, "a task's own stop does not pause it")
	assert.Equal(t, alsoLimited.ID, p.Of(limited.ID).LimitedBy, "a task's own stop does not pause it")

	s.reads = nil
	require.NoError(t, p.Read(ctx, s))
	assert.Empty(t, s.reads, "a task with no new event is not read again")

	require.NoError(t, h.loop.Retry(ctx, endpoint.ID))
	require.NoError(t, p.Read(ctx, s))
	assert.Equal(t, []int64{endpoint.ID}, s.reads, "only the task with a new event is read")
	assert.Zero(t, p.Of(h.task.ID).EndpointTask, "a retry lifts the pause")
}
