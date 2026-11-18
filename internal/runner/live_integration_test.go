//go:build integration && live

package runner

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/cavu/internal/engine"
)

// Like the driver's live checks, this spends the operator's subscription, so
// it never runs in CI and skips unless the token is set.
const (
	liveTokenVar = "CAVU_LIVE_SUBSCRIPTION_TOKEN" //nolint:gosec // a variable name, not a credential
	liveModelVar = "CAVU_LIVE_MODEL"              // claude-haiku-5-5 when unset
)

func TestLiveReadinessAsks(t *testing.T) {
	token := os.Getenv(liveTokenVar)
	if token == "" {
		t.Skipf("%s is not set", liveTokenVar)
	}

	model := os.Getenv(liveModelVar)
	if model == "" {
		model = "claude-haiku-5-5"
	}

	r, c, _ := fixtureRun(t, map[string]string{"README.md": "# Plover\n\nPlover is a small library for counting birds.\n"})

	c.Endpoint.Credential = token
	c.Agent.Model, c.Agent.MaxTurns, c.Agent.Timeout = model, 10, 5*time.Minute
	c.Task.Title = "Add a security policy"
	c.Task.Body = "Add a SECURITY.md that tells people where to send reports of vulnerabilities."
	c.Verify.Command, c.Verify.Timeout = "true", time.Minute

	prep, err := r.Prepare(t.Context(), c)
	require.NoError(t, err)
	require.Empty(t, prep.FailedStep)

	c.Repo.BaseCommit, c.Ecosystems = prep.BaseCommit, prep.Ecosystems

	w := newSession(t)
	w.Mode = engine.ModeReadiness

	res := work(t, r, c, w)
	require.Equal(t, engine.OutcomeBlocked, res.Outcome, "detail: %s", res.Detail)
	assert.NotEmpty(t, res.Questions)
	assert.NotEmpty(t, res.SuggestedAnswers)
}
