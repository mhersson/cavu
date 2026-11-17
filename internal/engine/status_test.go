package engine_test

import (
	"testing"

	"github.com/mhersson/cavu/internal/engine"
	"github.com/stretchr/testify/assert"
)

func TestStatusOf(t *testing.T) {
	tests := []struct {
		name  string
		facts engine.Facts
		want  engine.Status
	}{
		{"not started", engine.Facts{}, engine.StatusDraft},
		{"cancelled before it started", engine.Facts{Cancelled: true}, engine.StatusCancelled},
		{
			"cancelled while running",
			engine.Facts{Started: true, Cancelled: true, RunOpen: true, OpenStop: &engine.StopFact{Reason: engine.StopCancelled}},
			engine.StatusCancelled,
		},
		{"cancelled after it was merged", engine.Facts{Started: true, Exported: true, Merged: true, Cancelled: true}, engine.StatusCancelled},
		{"merged", engine.Facts{Started: true, Exported: true, Merged: true}, engine.StatusDone},
		{"stopped", engine.Facts{Started: true, RunOpen: true, OpenStop: &engine.StopFact{Reason: engine.StopQuestion}}, engine.StatusBlocked},
		{"stopped with no run", engine.Facts{Started: true, OpenStop: &engine.StopFact{Reason: engine.StopDependency}}, engine.StatusBlocked},
		{"exported", engine.Facts{Started: true, Exported: true}, engine.StatusInReview},
		{"started with no run", engine.Facts{Started: true}, engine.StatusQueued},
		{"running", engine.Facts{Started: true, RunOpen: true}, engine.StatusRunning},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, engine.StatusOf(tc.facts))
		})
	}
}
