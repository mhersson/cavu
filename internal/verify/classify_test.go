package verify

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestClassify(t *testing.T) {
	tests := []struct {
		name     string
		code     int
		timedOut bool
		want     string
	}{
		{"zero", 0, false, Passed},
		{"one", 1, false, Failed},
		{"missing tool", 127, false, Failed},
		{"timeout with zero", 0, true, Timeout},
		{"timeout with code", 137, true, Timeout},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, Classify(tt.code, tt.timedOut))
		})
	}

	// The engine records these exact strings.
	assert.Equal(t, "passed", Passed)
	assert.Equal(t, "failed", Failed)
	assert.Equal(t, "timeout", Timeout)
}

func TestClampTimeout(t *testing.T) {
	tests := []struct {
		name string
		in   time.Duration
		want time.Duration
	}{
		{"zero", 0, 10 * time.Minute},
		{"negative", -time.Second, 10 * time.Minute},
		{"below floor", time.Second, 30 * time.Second},
		{"sub-second", time.Millisecond, 30 * time.Second},
		{"in range", 5 * time.Minute, 5 * time.Minute},
		{"above cap", 5 * time.Hour, 2 * time.Hour},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, ClampTimeout(tt.in))
		})
	}
}
