package runner

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/cavu/internal/engine"
	"github.com/mhersson/cavu/internal/sandbox"
)

func TestEnsureUpIsIdempotent(t *testing.T) {
	t.Run("after a prepare", func(t *testing.T) {
		f := newFake()
		r := f.runner()

		_, err := r.Prepare(t.Context(), testConfig())
		require.NoError(t, err)

		for range 2 {
			err := r.ensureUp(t.Context(), testConfig())
			require.NoError(t, err)
		}

		assert.Equal(t, fullOrder, f.calls(), "the agent phase the prepare left up is used")
	})

	// A fresh runner on the same sandbox is a process that restarted: the
	// ecosystems come from the config, never from the workspace, whose files
	// here say go.
	t.Run("after a restart", func(t *testing.T) {
		for _, tt := range []struct {
			ecosystems, hosts []string
		}{
			{ecosystems: []string{"node"}, hosts: []string{"registry.example", "registry.npmjs.org"}},
			{ecosystems: nil, hosts: []string{"registry.example"}},
		} {
			f := newFake()
			c := testConfig()
			c.Ecosystems = tt.ecosystems
			r := f.runner()

			for range 2 {
				require.NoError(t, r.ensureUp(t.Context(), c))
			}

			assert.Equal(t, []string{"volumes", "up/agent"}, f.calls())
			assert.Empty(t, f.reads, "nothing is read from the workspace")

			agent := f.requests(sandbox.PhaseAgent)
			require.Len(t, agent, 1)
			assert.Equal(t, tt.hosts, agent[0].AllowHosts)
			require.NotNil(t, agent[0].Model)

			_, err := runVerify(t.Context(), r.state(c.RunID), c)
			require.NoError(t, err)
			assert.Equal(t, agentEnv(tt.ecosystems), f.run(t, "make test").cmd.Env, "the agent phase's environment follows them too")
		}
	})

	t.Run("after a start whose volumes failed", func(t *testing.T) {
		f := newFake()
		r := f.runner()
		c := testConfig()
		c.Ecosystems = []string{"node"}

		f.volErr = errors.New("sandbox: volume busy")
		require.ErrorIs(t, r.ensureUp(t.Context(), c), f.volErr)

		f.volErr = nil

		require.NoError(t, r.ensureUp(t.Context(), c))
		assert.Equal(t, []string{"volumes", "volumes", "up/agent"}, f.calls(), "a failed start keeps no state")
		assert.Equal(t, []string{"registry.example", "registry.npmjs.org"}, f.requests(sandbox.PhaseAgent)[0].AllowHosts)
	})

	t.Run("after the sidecar ended", func(t *testing.T) {
		f := newFake()
		r := f.runner()

		_, err := r.Prepare(t.Context(), testConfig())
		require.NoError(t, err)

		first := f.requests(sandbox.PhaseAgent)[0].Model.StandIn

		f.fail("sandbox: the sidecar does not answer: it exited: killed")

		for range 2 {
			err := r.ensureUp(t.Context(), testConfig())
			require.NoError(t, err)
		}

		assert.Equal(t, append(fullOrder, "up/agent"), f.calls(), "the phase comes up again, once")

		second := f.requests(sandbox.PhaseAgent)[1].Model.StandIn
		assert.NotEqual(t, first, second, "each start of the agent phase has its own stand-in")
	})
}

func TestRelease(t *testing.T) {
	ref := engine.RunRef{InstanceID: "0123456789abcdef", Image: "cavu-sandbox:test", TaskID: 5, RunID: 7, ProjectID: 3}

	t.Run("keeping the volumes", func(t *testing.T) {
		f := newFake()

		require.NoError(t, f.runner().Release(t.Context(), ref, false))
		assert.Equal(t, []string{"down"}, f.calls())
		assert.Equal(t, []sandbox.Options{{
			Runtime: sandbox.Runtime{Bin: "docker", Kind: "docker"}, Image: ref.Image, InstanceID: ref.InstanceID,
			RunID: 7, TaskID: 5, ProjectID: 3,
		}}, f.opened)
	})

	t.Run("removing the volumes", func(t *testing.T) {
		f := newFake()

		require.NoError(t, f.runner().Release(t.Context(), ref, true))
		assert.Equal(t, []string{"destroy"}, f.calls(), "the task's home goes too")
	})

	t.Run("a prepared run", func(t *testing.T) {
		f := newFake()
		r := f.runner()

		_, err := r.Prepare(t.Context(), testConfig())
		require.NoError(t, err)
		require.NoError(t, r.Release(t.Context(), ref, false))

		require.NoError(t, r.ensureUp(t.Context(), testConfig()))
		assert.Equal(t, append(fullOrder, "down", "up/agent"), f.calls())
		assert.Len(t, f.opened, 1)
		assert.Equal(t, f.requests(sandbox.PhaseAgent)[0].AllowHosts, f.requests(sandbox.PhaseAgent)[1].AllowHosts,
			"a release that keeps the volumes keeps what detection found")

		require.NoError(t, r.Release(t.Context(), ref, true))
		require.NoError(t, r.Release(t.Context(), ref, true))
		assert.Len(t, f.opened, 2, "a release that removes the volumes forgets the run")
	})

	// A restart between a session's end and its keep-run stop makes the stop's
	// release the process's first call on the run; the run's next start must
	// still find its volumes and the config's ecosystems.
	t.Run("before any start in this process", func(t *testing.T) {
		f := newFake()
		r := f.runner()

		require.NoError(t, r.Release(t.Context(), ref, false))

		c := testConfig()
		c.Ecosystems = []string{"node"}

		require.NoError(t, r.ensureUp(t.Context(), c))
		assert.Equal(t, []string{"down", "volumes", "up/agent"}, f.calls())

		agent := f.requests(sandbox.PhaseAgent)
		require.Len(t, agent, 1)
		assert.Equal(t, []string{"registry.example", "registry.npmjs.org"}, agent[0].AllowHosts)
	})

	t.Run("an error", func(t *testing.T) {
		f := newFake()
		f.downErr = errors.New("remove containers: daemon unreachable")

		require.ErrorIs(t, f.runner().Release(t.Context(), ref, false), f.downErr)
	})
}
