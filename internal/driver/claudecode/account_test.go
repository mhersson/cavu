package claudecode

import (
	"encoding/json"
	"maps"
	"slices"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/cavu/internal/driver"
	"github.com/mhersson/cavu/internal/testkit/fakeclaude"
)

// requireAccountRefused checks that f ended as an auth result naming what the
// initialize answer reported, before the prompt was written.
func requireAccountRefused(t *testing.T, f *fakeRun, reported ...string) {
	t.Helper()

	res, err := f.run(t.Context())
	require.NoError(t, err, "a wrong credential mode is a result, not a driver failure")
	assert.Equal(t, driver.ReasonAuth, res.Reason)
	assert.True(t, res.IsError)

	for _, v := range reported {
		assert.Contains(t, res.Detail, strconv.Quote(v), "the detail names what claude reported")
	}

	rec := f.recorded(t)
	assert.Empty(t, rec.Stdin, "the prompt reached a process with the wrong credential mode")
	assert.True(t, processGone(rec.PID))
	assert.Empty(t, ofKind(f.events, driver.KindStarted))
}

func TestAccountCheckSubscription(t *testing.T) {
	// The pinned release's answers when the run would not bill the
	// subscription token: a provider switch, an API key alone, an API key
	// beside the token, and no token at all.
	foreign := map[string]string{
		"provider switch": `{"apiProvider":"bedrock"}`,
		"api key only":    `{"tokenSource":"none","apiKeySource":"ANTHROPIC_API_KEY","apiProvider":"firstParty"}`,
		"api key wins":    `{"tokenSource":"CLAUDE_CODE_OAUTH_TOKEN","apiKeySource":"ANTHROPIC_API_KEY","apiProvider":"firstParty"}`,
		"no token":        `{"tokenSource":"none","apiProvider":"firstParty"}`,
	}

	for name, acct := range foreign {
		t.Run(name, func(t *testing.T) {
			f := newFakeRun(t, fakeclaude.Scenario{Replay: fixture(t, "real/t1.jsonl"), Account: json.RawMessage(acct)})

			var seen map[string]string
			require.NoError(t, json.Unmarshal([]byte(acct), &seen))

			requireAccountRefused(t, f, slices.Collect(maps.Values(seen))...)
		})
	}

	for name, acct := range map[string]string{
		"number field":  `{"tokenSource":42,"apiProvider":"firstParty"}`,
		"not an object": `"CLAUDE_CODE_OAUTH_TOKEN"`,
	} {
		t.Run("unreadable "+name, func(t *testing.T) {
			f := newFakeRun(t, fakeclaude.Scenario{Replay: fixture(t, "real/t1.jsonl"), Account: json.RawMessage(acct)})
			requireAccountRefused(t, f)
		})
	}

	t.Run("no account passes", func(t *testing.T) {
		f := newFakeRun(t, fakeclaude.Scenario{Replay: fixture(t, "real/t1.jsonl"), NoAccount: true})
		runFake(t, f)
	})
}

func TestAccountCheckKeyMode(t *testing.T) {
	t.Run("oauth token source", func(t *testing.T) {
		f := newFakeRun(t, fakeclaude.Scenario{
			Replay:  fixture(t, "real/t1.jsonl"),
			Account: json.RawMessage(`{"tokenSource":"CLAUDE_CODE_OAUTH_TOKEN","apiProvider":"firstParty"}`),
		})
		f.spec.Auth = keyAuth()

		requireAccountRefused(t, f, "CLAUDE_CODE_OAUTH_TOKEN")
	})

	t.Run("unreadable", func(t *testing.T) {
		f := newFakeRun(t, fakeclaude.Scenario{Replay: fixture(t, "real/t1.jsonl"), Account: json.RawMessage(`[]`)})
		f.spec.Auth = keyAuth()

		requireAccountRefused(t, f)
	})

	t.Run("no account passes", func(t *testing.T) {
		f := newFakeRun(t, fakeclaude.Scenario{Replay: fixture(t, "real/t1.jsonl"), NoAccount: true})
		f.spec.Auth = keyAuth()

		runFake(t, f)
	})

	t.Run("auth token source passes", func(t *testing.T) {
		f := newFakeRun(t, fakeclaude.Scenario{Replay: fixture(t, "real/t1.jsonl")})
		f.spec.Auth = keyAuth()

		runFake(t, f)
	})
}
