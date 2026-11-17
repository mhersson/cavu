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

// The verify command runs as a command of its own in the agent container,
// never through the agent.
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

	assert.Equal(t, []string{"volumes", "up/agent", "run/make test"}, f.calls())
	assert.Empty(t, d.specs, "the driver runs nothing")

	run := f.run(t, "make test")
	assert.Equal(t, sandbox.PhaseAgent, run.phase)
	assert.Equal(t, 3*time.Minute, run.timeout)
	assert.Equal(t, verify.Argv("make test"), run.cmd.Argv)
	assert.Equal(t, withProjectVars(agentEnv([]string{"go"}, baseCommit), c), run.cmd.Env)
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

// Verify and export run in an agent container nothing else ran in: one the
// action finds up is restarted, and one it brings up is fresh already.
func TestVerifyAndExportStartTheAgentPhaseOnce(t *testing.T) {
	c := workConfig()

	for _, a := range []struct {
		name string
		do   func(r *Runner) error
		ran  string
	}{
		{
			name: "verify", ran: "run/make test",
			do: func(r *Runner) error {
				_, err := r.Verify(t.Context(), c)

				return err
			},
		},
		{
			name: "export", ran: "run/" + exportName(c, headCommit),
			do: func(r *Runner) error {
				_, err := r.Export(t.Context(), c, headCommit)

				return err
			},
		},
	} {
		for _, tt := range []struct {
			name   string
			before func(t *testing.T, f *fakeSandbox, r *Runner)
			want   []string
		}{
			{
				name: "after a work session",
				before: func(t *testing.T, _ *fakeSandbox, r *Runner) {
					_, err := r.Work(t.Context(), c, startWork, noProgress)
					require.NoError(t, err)
				},
				want: []string{"restart", a.ran},
			},
			{
				name: "after its sidecar ended",
				before: func(t *testing.T, f *fakeSandbox, r *Runner) {
					_, err := r.Work(t.Context(), c, startWork, noProgress)
					require.NoError(t, err)
					f.fail("sandbox: the sidecar does not answer: it exited: killed")
				},
				want: []string{"up/agent", "restart", a.ran},
			},
			{
				name: "after a review",
				before: func(t *testing.T, _ *fakeSandbox, r *Runner) {
					_, err := r.Review(t.Context(), c, fullReview(), noProgress)
					require.NoError(t, err)
				},
				want: []string{"up/agent", a.ran},
			},
			{name: "with nothing up", before: func(*testing.T, *fakeSandbox, *Runner) {}, want: []string{"volumes", "up/agent", a.ran}},
		} {
			t.Run(a.name+" "+tt.name, func(t *testing.T) {
				f := newFake()
				r := f.workRunner(&fakeAgent{results: []driver.Result{{Reason: driver.ReasonDone}}})

				tt.before(t, f, r)
				n := len(f.calls())

				require.NoError(t, a.do(r))

				got := f.calls()[n:]
				require.GreaterOrEqual(t, len(got), len(tt.want))
				assert.Equal(t, tt.want, got[:len(tt.want)])
			})
		}
	}
}
