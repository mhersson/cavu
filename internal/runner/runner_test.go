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

	t.Run("after a restart", func(t *testing.T) {
		f := newFake()
		c := testConfig()
		c.Repo.BaseCommit = baseCommit
		r := f.runner()

		for range 2 {
			err := r.ensureUp(t.Context(), c)
			require.NoError(t, err)
		}

		assert.Equal(t, []string{"volumes", "up/agent", "read", "up/agent"}, f.calls())
		assert.Equal(t, []string{baseCommit}, f.reads, "detection reads the files at the base commit")

		agent := f.requests(sandbox.PhaseAgent)
		require.Len(t, agent, 2)
		assert.Empty(t, agent[0].AllowHosts, "nothing is reachable while the files are read")
		assert.Nil(t, agent[0].Model)
		assert.Equal(t, []string{"proxy.golang.org", "registry.example", "sum.golang.org"}, agent[1].AllowHosts)
		require.NotNil(t, agent[1].Model)
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

	t.Run("without a base commit", func(t *testing.T) {
		f := newFake()
		r := f.runner()

		err := r.ensureUp(t.Context(), testConfig())
		require.Error(t, err)

		c := testConfig()
		c.Repo.BaseCommit = baseCommit

		err = r.ensureUp(t.Context(), c)
		require.NoError(t, err)

		agent := f.requests(sandbox.PhaseAgent)
		require.Len(t, agent, 3, "the phase the read came up in does not count as up")
		assert.NotEmpty(t, agent[2].AllowHosts)
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

		c := testConfig()
		c.Repo.BaseCommit = baseCommit

		err = r.ensureUp(t.Context(), c)
		require.NoError(t, err)
		assert.Equal(t, append(fullOrder, "down", "up/agent"), f.calls(), "a release that keeps the volumes keeps what detection found")
		assert.Len(t, f.opened, 1)

		require.NoError(t, r.Release(t.Context(), ref, true))
		require.NoError(t, r.Release(t.Context(), ref, true))
		assert.Len(t, f.opened, 2, "a release that removes the volumes forgets the run")
	})

	t.Run("an error", func(t *testing.T) {
		f := newFake()
		f.downErr = errors.New("remove containers: daemon unreachable")

		require.ErrorIs(t, f.runner().Release(t.Context(), ref, false), f.downErr)
	})
}
