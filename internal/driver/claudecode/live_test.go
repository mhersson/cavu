//go:build integration && live

package claudecode

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/cavu/internal/driver"
	"github.com/mhersson/cavu/internal/proxy"
	"github.com/mhersson/cavu/internal/redact"
	"github.com/mhersson/cavu/internal/sidecar"
	"github.com/mhersson/cavu/internal/testkit/sandboxtest"
)

// The live checks settle what a mock cannot: they run the pinned claude
// against the real endpoint with the operator's credential, so they never run
// in CI and each skips unless its variables are set.
const (
	liveTokenVar = "CAVU_LIVE_SUBSCRIPTION_TOKEN" //nolint:gosec // a variable name, not a credential
	liveURLVar   = "CAVU_LIVE_ENDPOINT_URL"
	liveKeyVar   = "CAVU_LIVE_ENDPOINT_KEY"
	liveAuthVar  = "CAVU_LIVE_ENDPOINT_AUTH_HEADER" // bearer or x-api-key, x-api-key when unset
	liveModelVar = "CAVU_LIVE_MODEL"                // claude-haiku-5-5 when unset
)

func liveEnv(t *testing.T, names ...string) []string {
	t.Helper()

	out := make([]string, len(names))

	for i, n := range names {
		if out[i] = os.Getenv(n); out[i] == "" {
			t.Skipf("%s is not set", n)
		}
	}

	return out
}

func liveModel() string {
	if m := os.Getenv(liveModelVar); m != "" {
		return m
	}

	return confModel
}

// liveDone fails unless the run ended done. Unlike requireDone it masks the
// operator's secrets in what it prints, and so does every log of a live check.
func liveDone(t *testing.T, o outcome, mask func(string) string) {
	t.Helper()

	if o.res.Reason != driver.ReasonDone {
		t.Fatalf("reason %s, not done\ndetail: %s\nstderr: %s", o.res.Reason, mask(o.res.Detail), mask(o.stderr))
	}
}

// liveBox is a sandbox with an empty workspace whose proxy serves req.
func liveBox(t *testing.T, req sidecar.ProxyRequest) *box {
	t.Helper()

	r := sandboxtest.Runtime(t)
	b := &box{t: t, rt: r}
	b.up(sandboxtest.Image(t, r, Version, SHA512, nil), req)

	return b
}

// liveSubscription is a one-turn subscription session whose allowlist is the
// driver's hosts for the mode and nothing else.
func liveSubscription(t *testing.T) (*box, driver.Spec, func(string) string) {
	t.Helper()

	token := liveEnv(t, liveTokenVar)[0]
	b := liveBox(t, sidecar.ProxyRequest{AllowHosts: conformanceDriver().Hosts(modeSubscription)})

	s := b.spec()
	s.Auth = driver.Auth{Mode: modeSubscription, Token: token}
	s.Model = liveModel()
	s.MaxTurns = 1
	s.Prompt = "Reply with the single word ok."

	return b, s, redact.New(token)
}

// TestLiveSubscriptionHosts records every host a subscription run asks for
// beyond the driver's Hosts; the run must succeed without them.
func TestLiveSubscriptionHosts(t *testing.T) {
	b, s, mask := liveSubscription(t)

	o := b.run(conformanceDriver(), s)

	for _, l := range b.sidecar.of("denied") {
		t.Logf("denied host: %s", mask(l.Host))
	}

	liveDone(t, o, mask)
}

// TestLiveRateLimitEventShape: a subscription run reports its window and the
// window's reset time.
func TestLiveRateLimitEventShape(t *testing.T) {
	b, s, mask := liveSubscription(t)

	o := b.run(conformanceDriver(), s)
	liveDone(t, o, mask)

	for _, raw := range o.lines {
		if lineType(raw) == "rate_limit_event" {
			t.Logf("rate_limit_event: %s", mask(string(raw)))
		}
	}

	var seen bool

	for _, ev := range o.events {
		if ev.Kind == driver.KindRateLimit {
			seen = true

			assert.NotEmpty(t, ev.RateLimit.Window)
			assert.False(t, ev.RateLimit.ResetsAt.IsZero(), "no reset time")
		}
	}

	assert.True(t, seen, "no rate_limit event")
}

// TestLiveGatewayCacheReads: in key mode the second of two turns reads the
// cache the first wrote, as the sidecar's meter counts it.
func TestLiveGatewayCacheReads(t *testing.T) {
	vars := liveEnv(t, liveURLVar, liveKeyVar)
	mask := redact.New(vars[1])

	header := os.Getenv(liveAuthVar)
	if header == "" {
		header = "x-api-key"
	}

	standIn := proxy.NewStandIn()
	b := liveBox(t, sidecar.ProxyRequest{Model: &proxy.ModelRoute{
		BaseURL: vars[0], Credential: vars[1], AuthHeader: header, StandIn: standIn,
	}})
	b.standIn = standIn

	d := conformanceDriver()

	first := b.spec()
	first.Model = liveModel()
	first.MaxTurns = 1
	first.Prompt = "Reply with the single word ok."
	liveDone(t, b.run(d, first), mask)

	before := len(b.sidecar.of("usage"))

	second := first
	second.SessionID, second.Resume = "", first.SessionID
	second.Prompt = "Reply with the single word yes."
	liveDone(t, b.run(d, second), mask)

	usage := b.sidecar.of("usage")[before:]
	require.NotEmpty(t, usage, "the meter counted no call of the second turn")

	var read int64

	for _, l := range usage {
		t.Logf("second turn usage: %+v", *l.Usage)

		read += l.Usage.CacheRead
	}

	assert.Positive(t, read, "the second turn read no cache")
}

// TestLiveBudgetFlagUnderSubscription records whether --max-budget-usd stops
// a subscription run. It is a finding, not a gate: the driver never passes
// the flag.
func TestLiveBudgetFlagUnderSubscription(t *testing.T) {
	b, s, mask := liveSubscription(t)

	d := conformanceDriver()
	d.amend = func(inv *invocation) { inv.argv = append(inv.argv, "--max-budget-usd", "0.000001") }

	o := b.run(d, s)
	t.Logf("reason %s, error %v, detail %q, result %s", o.res.Reason, o.res.IsError, mask(o.res.Detail), mask(rawLine(o, "result")))
}

// lineType is the type of one stdout line, empty for a line that is not
// JSON.
func lineType(raw []byte) string {
	l, _ := decodeLine(raw)

	return l.Type
}
