package runner

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/cavu/internal/driver"
	"github.com/mhersson/cavu/internal/engine"
	"github.com/mhersson/cavu/internal/sandbox"
	"github.com/mhersson/cavu/internal/sidecar"
	"github.com/mhersson/cavu/internal/verify"
)

func TestVerifyWaived(t *testing.T) {
	f := newFake()
	c := workConfig()
	c.Verify.Waived = true

	res, err := f.runner().Verify(t.Context(), c)
	require.NoError(t, err)
	assert.Equal(t, engine.VerifyResult{Result: engine.ResultWaived}, res)
	assert.Empty(t, f.calls())
}

// The verify command runs as a command of its own in a restarted agent
// container, so nothing the session left running sees it.
func TestVerifyRunsOutsideTheSession(t *testing.T) {
	f := newFake()
	f.results["make test"] = sandbox.Result{ExitCode: 2, Output: "FAIL\n"}
	d := &fakeAgent{results: []driver.Result{{Reason: driver.ReasonDone}}}
	c := workConfig()
	c.Verify.Timeout = 3 * time.Minute

	res, err := f.workRunner(d).Verify(t.Context(), c)
	require.NoError(t, err)
	assert.Equal(t, engine.ResultFailed, res.Result)
	assert.Equal(t, 2, res.ExitCode)
	assert.Equal(t, "FAIL\n", res.Output)
	assert.Equal(t, "make test", res.Command)

	assert.Equal(t, []string{"volumes", "up/agent", "restart", "run/make test"}, f.calls())
	assert.Empty(t, d.specs, "the driver runs nothing")

	run := f.run(t, "make test")
	assert.Equal(t, sandbox.PhaseAgent, run.phase)
	assert.Equal(t, 3*time.Minute, run.timeout)
	assert.Equal(t, verify.Argv("make test"), run.cmd.Argv)
	assert.Equal(t, agentEnv([]string{"go"}, baseCommit), run.cmd.Env)
	assert.Contains(t, run.cmd.Env, "CAVU_BASE_COMMIT="+baseCommit)
	assert.Equal(t, workspace, run.cmd.Dir)
}

func TestVerifyOutputIsRedacted(t *testing.T) {
	f := newFake()
	f.results["make test"] = sandbox.Result{ExitCode: 1, Output: "token " + plantedSetup + " key " + plantedEndpoint + " forge " + plantedForge}

	res, err := f.runner().Verify(t.Context(), workConfig())
	require.NoError(t, err)
	assertMasked(t, res.Output)
}

func TestVerifyNamesDeniedHostsOnlyWhenItFails(t *testing.T) {
	deny := func(f *fakeSandbox) {
		f.onRun = func(name string) {
			if name == "make test" {
				f.send(sidecar.Line{Type: "denied", Host: "registry.example.test"})
				f.send(sidecar.Line{Type: "denied", Host: "registry.example.test"})
			}
		}
	}

	f := newFake()
	f.results["make test"] = sandbox.Result{ExitCode: 1, Output: "FAIL\n"}
	deny(f)

	res, err := f.runner().Verify(t.Context(), workConfig())
	require.NoError(t, err)
	assert.Equal(t, 1, strings.Count(res.Output, "registry.example.test"))

	f = newFake()
	deny(f)

	res, err = f.runner().Verify(t.Context(), workConfig())
	require.NoError(t, err)
	assert.NotContains(t, res.Output, "example.test")
}
