package engine_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/mhersson/cavu/internal/engine"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPayloadTimes(t *testing.T) {
	fields := func(p any) map[string]any {
		raw, err := json.Marshal(p)
		require.NoError(t, err)

		var m map[string]any
		require.NoError(t, json.Unmarshal(raw, &m))

		return m
	}

	assert.NotContains(t, fields(engine.StoppedPayload{Reason: engine.StopQuestion}), "resets_at")

	at := time.Date(2030, 1, 1, 12, 30, 0, 0, time.UTC)
	assert.Equal(t, "2030-01-01T12:30:00Z", fields(engine.StoppedPayload{Reason: engine.StopUsageLimit, ResetsAt: at})["resets_at"])
	assert.Equal(t, "2030-01-01T12:30:00Z", fields(engine.SessionEndedPayload{Outcome: engine.OutcomeUsageLimit, ResetsAt: at})["resets_at"])
}
