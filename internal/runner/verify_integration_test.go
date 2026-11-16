//go:build integration

package runner

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/cavu/internal/engine"
)

// A verify command that runs past its timeout is stopped inside the container
// and gives timeout.
func TestVerifyTimeout(t *testing.T) {
	r, c, _ := fixtureRun(t, map[string]string{"notes.txt": "first line\n"})
	c.Verify.Waived = true

	res, err := r.Prepare(t.Context(), c)
	require.NoError(t, err)
	require.Empty(t, res.FailedStep, res.Output)

	c.Repo.BaseCommit, c.Ecosystems = res.BaseCommit, res.Ecosystems
	c.Verify.Waived, c.Verify.Command, c.Verify.Timeout = false, "sleep 300", 2*time.Second

	started := time.Now()
	vr, err := r.Verify(t.Context(), c)
	require.NoError(t, err)
	assert.Equal(t, engine.ResultTimeout, vr.Result, vr.Output)
	assert.Less(t, time.Since(started), time.Minute)

	left := runIn(t, r.state(c.RunID), "pgrep", "-f", "^sleep 300$")
	assert.Equal(t, 1, left.ExitCode, "a sleep process remains: %s", left.Output)
}
