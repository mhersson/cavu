package push_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/mhersson/cavu/internal/push"
)

func TestFocus(t *testing.T) {
	var f push.Focus

	assert.False(t, f.Looking(1), "an unknown device")

	f.Set(1, true)
	assert.False(t, f.Looking(1), "focused with no stream open")

	first := f.Connect(1)
	assert.True(t, f.Looking(1), "focused and connected")
	assert.False(t, f.Looking(2), "another device")

	f.Set(1, false)
	assert.False(t, f.Looking(1), "unfocused")

	f.Set(1, true)
	second := f.Connect(1)

	first()
	assert.True(t, f.Looking(1), "one of two streams ended")

	second()
	assert.False(t, f.Looking(1), "both streams ended")
}
