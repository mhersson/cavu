package runner

import (
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/cavu/internal/driver"
	"github.com/mhersson/cavu/internal/engine"
	"github.com/mhersson/cavu/internal/env"
	"github.com/mhersson/cavu/internal/proc"
	"github.com/mhersson/cavu/internal/proxy"
	"github.com/mhersson/cavu/internal/sandbox"
	"github.com/mhersson/cavu/internal/sidecar"
)

func TestEnsureUpIsIdempotent(t *testing.T) {
	t.Run("after a prepare", func(t *testing.T) {
		f := newFake()
		r := f.runner()

		_, err := r.Prepare(t.Context(), testConfig())
		require.NoError(t, err)

		for range 2 {
			err := r.ensureUp(t.Context(), testConfig(), sandbox.PhaseAgent)
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
				require.NoError(t, r.ensureUp(t.Context(), c, sandbox.PhaseAgent))
			}

			assert.Equal(t, []string{"volumes", "up/agent"}, f.calls())
			assert.Empty(t, f.reads, "nothing is read from the workspace")

			agent := f.requests(sandbox.PhaseAgent)
			require.Len(t, agent, 1)
			assert.Equal(t, tt.hosts, agent[0].AllowHosts)
			require.NotNil(t, agent[0].Model)

			_, err := runVerify(t.Context(), r.state(c.RunID), c)
			require.NoError(t, err)
			assert.Equal(t, withProjectVars(agentEnv(tt.ecosystems, c.Repo.BaseCommit), c), f.run(t, "make test").cmd.Env, "the agent phase's environment follows them too")
		}
	})

	t.Run("after a start whose volumes failed", func(t *testing.T) {
		f := newFake()
		r := f.runner()
		c := testConfig()
		c.Ecosystems = []string{"node"}

		f.volErr = errors.New("sandbox: volume busy")
		require.ErrorIs(t, r.ensureUp(t.Context(), c, sandbox.PhaseAgent), f.volErr)

		f.volErr = nil

		require.NoError(t, r.ensureUp(t.Context(), c, sandbox.PhaseAgent))
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
			err := r.ensureUp(t.Context(), testConfig(), sandbox.PhaseAgent)
			require.NoError(t, err)
		}

		assert.Equal(t, append(fullOrder, "up/agent", goSeed), f.calls(), "the phase comes up again, once")

		second := f.requests(sandbox.PhaseAgent)[1].Model.StandIn
		assert.NotEqual(t, first, second, "each start of the agent phase has its own stand-in")
	})
}

func TestAgentPhaseCarriesTheBudget(t *testing.T) {
	for _, tt := range []struct {
		name string
		c    engine.RunConfig
		want *proxy.Budget
	}{
		{
			name: "capped", c: cappedConfig(1_800_000),
			want: &proxy.Budget{CapMicroUSD: 1_800_000, Prices: map[string]proxy.Price{"*": {Input: 3, Output: 15, CacheRead: 0.3, CacheWrite: 3.75}}},
		},
		{name: "no cap", c: workConfig()},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newFake()

			_, err := f.workRunner(&fakeAgent{results: []driver.Result{{Reason: driver.ReasonDone}}}).Work(t.Context(), tt.c, startWork, noProgress)
			require.NoError(t, err)

			agent := f.requests(sandbox.PhaseAgent)
			require.Len(t, agent, 1)
			assert.Equal(t, tt.want, agent[0].Budget)
		})
	}
}

// One sidecar counts a run's spend while the budget it was given is the
// action's and its proxy has not refused; otherwise a fresh one starts with
// a fresh count, and the phase's container stays.
func TestSidecarFollowsTheBudget(t *testing.T) {
	// upAll brings up each phase in turn with c and returns every request the sandbox was given.
	upAll := func(t *testing.T, f *fakeSandbox, r *Runner, c engine.RunConfig, phases ...sandbox.Phase) []sidecar.ProxyRequest {
		t.Helper()

		for _, p := range phases {
			require.NoError(t, r.ensureUp(t.Context(), c, p))
		}

		var reqs []sidecar.ProxyRequest
		for _, u := range f.ups {
			reqs = append(reqs, u.req)
		}

		return reqs
	}

	agent, readOnly := sandbox.PhaseAgent, sandbox.PhaseReadOnly

	t.Run("an unchanged budget keeps the request across the phases", func(t *testing.T) {
		f := newFake()

		reqs := upAll(t, f, f.runner(), cappedConfig(5_000_000), agent, agent, readOnly, agent)
		assert.Equal(t, []string{"volumes", "up/agent", goSeed, "up/readonly", goSeed, "up/agent", goSeed}, f.calls())
		require.Len(t, reqs, 3)
		assert.Equal(t, reqs[0], reqs[1])
		assert.Equal(t, reqs[0], reqs[2])
	})

	t.Run("a changed budget gives a new request and keeps the phase", func(t *testing.T) {
		f := newFake()
		r := f.runner()

		upAll(t, f, r, cappedConfig(5_000_000), agent)

		reqs := upAll(t, f, r, cappedConfig(4_000_000), agent)
		assert.Equal(t, []string{"volumes", "up/agent", goSeed, "up/agent", goSeed}, f.calls())
		require.Len(t, reqs, 2)
		assert.NotEqual(t, reqs[0].Model.StandIn, reqs[1].Model.StandIn, "a fresh stand-in starts a fresh sidecar")
		assert.Equal(t, int64(4_000_000), reqs[1].Budget.CapMicroUSD)
		assert.True(t, r.state(workConfig().RunID).kept, "the agent's container stays")
	})

	t.Run("a budget line while no session watches", func(t *testing.T) {
		f := newFake()
		r := f.runner()
		c := cappedConfig(5_000_000)

		upAll(t, f, r, c, agent)
		f.send(sidecar.Line{Type: "budget"})

		reqs := upAll(t, f, r, c, agent, agent)
		require.Len(t, reqs, 2, "the next action's sidecar is fresh, and the one after keeps it")
		assert.NotEqual(t, reqs[0].Model.StandIn, reqs[1].Model.StandIn)
		assert.Equal(t, reqs[0].Budget, reqs[1].Budget)
		assert.True(t, r.state(c.RunID).kept)
	})

	t.Run("no cap keeps one request across the phases", func(t *testing.T) {
		f := newFake()

		reqs := upAll(t, f, f.runner(), workConfig(), agent, readOnly, agent, readOnly)
		require.Len(t, reqs, 4)

		for _, req := range reqs {
			assert.Equal(t, reqs[0], req)
			assert.Nil(t, req.Budget)
		}
	})
}

func TestRemoveProject(t *testing.T) {
	ref := engine.RunRef{InstanceID: "0123456789abcdef", Image: "cavu-sandbox:test", ProjectID: 3}

	f := newFake()

	require.NoError(t, f.runner().RemoveProject(t.Context(), ref))
	assert.Equal(t, []string{"remove-project-volumes"}, f.calls(), "no volume is ensured first")
	assert.Equal(t, []sandbox.Options{{
		Runtime: sandbox.Runtime{Bin: "docker", Kind: "docker"}, Image: ref.Image, InstanceID: ref.InstanceID, ProjectID: 3,
	}}, f.opened)

	boom := errors.New("volume in use")
	f.rmErr = boom
	require.ErrorIs(t, f.runner().RemoveProject(t.Context(), ref), boom)
}

func TestRelease(t *testing.T) {
	ref := engine.RunRef{InstanceID: "0123456789abcdef", Image: "cavu-sandbox:test", TaskID: 5, RunID: 7, ProjectID: 3}

	t.Run("keeping the volumes", func(t *testing.T) {
		f := newFake()

		require.NoError(t, f.runner().Release(t.Context(), ref, engine.KeepVolumes))
		assert.Equal(t, []string{"down"}, f.calls())
		assert.Equal(t, []sandbox.Options{{
			Runtime: sandbox.Runtime{Bin: "docker", Kind: "docker"}, Image: ref.Image, InstanceID: ref.InstanceID,
			RunID: 7, TaskID: 5, ProjectID: 3,
		}}, f.opened)
	})

	t.Run("removing the run's volumes", func(t *testing.T) {
		f := newFake()
		r := f.runner()

		_, err := r.Prepare(t.Context(), testConfig())
		require.NoError(t, err)
		require.NoError(t, r.Release(t.Context(), ref, engine.RemoveRun))
		assert.Equal(t, append(fullOrder, "destroy/keep-home"), f.calls(), "the task's home stays")
		assert.Nil(t, r.state(ref.RunID), "the run is forgotten")
	})

	t.Run("removing the home too", func(t *testing.T) {
		f := newFake()

		require.NoError(t, f.runner().Release(t.Context(), ref, engine.RemoveAll))
		assert.Equal(t, []string{"destroy"}, f.calls(), "the task's home goes too")
	})

	t.Run("a prepared run", func(t *testing.T) {
		f := newFake()
		r := f.runner()

		_, err := r.Prepare(t.Context(), testConfig())
		require.NoError(t, err)
		require.NoError(t, r.Release(t.Context(), ref, engine.KeepVolumes))

		require.NoError(t, r.ensureUp(t.Context(), testConfig(), sandbox.PhaseAgent))
		assert.Equal(t, append(fullOrder, "down", "up/agent", goSeed), f.calls())
		assert.Len(t, f.opened, 1)
		assert.Equal(t, f.requests(sandbox.PhaseAgent)[0].AllowHosts, f.requests(sandbox.PhaseAgent)[1].AllowHosts,
			"a release that keeps the volumes keeps what detection found")

		require.NoError(t, r.Release(t.Context(), ref, engine.RemoveAll))
		require.NoError(t, r.Release(t.Context(), ref, engine.RemoveAll))
		assert.Len(t, f.opened, 2, "a release that removes the volumes forgets the run")
	})

	// A restart between a session's end and its keep-run stop makes the stop's
	// release the process's first call on the run; the run's next start must
	// still find its volumes and the config's ecosystems.
	t.Run("before any start in this process", func(t *testing.T) {
		f := newFake()
		r := f.runner()

		require.NoError(t, r.Release(t.Context(), ref, engine.KeepVolumes))

		c := testConfig()
		c.Ecosystems = []string{"node"}

		require.NoError(t, r.ensureUp(t.Context(), c, sandbox.PhaseAgent))
		assert.Equal(t, []string{"down", "volumes", "up/agent"}, f.calls())

		agent := f.requests(sandbox.PhaseAgent)
		require.Len(t, agent, 1)
		assert.Equal(t, []string{"registry.example", "registry.npmjs.org"}, agent[0].AllowHosts)
	})

	t.Run("an error", func(t *testing.T) {
		f := newFake()
		f.downErr = errors.New("remove containers: daemon unreachable")

		require.ErrorIs(t, f.runner().Release(t.Context(), ref, engine.KeepVolumes), f.downErr)
	})
}

// homeArgv is the command a rust run's home step with script and pairs runs.
func homeArgv(script string, pairs map[string]string) []string {
	argv := []string{"sh", "-c", script, "sh"}
	for _, p := range slices.Sorted(maps.Keys(pairs)) {
		argv = append(argv, p, pairs[p])
	}

	return argv
}

// Every start of the agent phase makes the home's links before any command
// runs there, and a start whose links fail does not count as up.
func TestAgentPhaseLinksTheHome(t *testing.T) {
	link := stepName(homeArgv(linkScript, env.AgentLinks([]string{"rust"}, home)))
	seed := stepName(homeArgv(seedScript, env.AgentSeeds([]string{"rust"}, home)))

	t.Run("in a prepare", func(t *testing.T) {
		f := newFake()
		f.files = map[string]string{"Cargo.toml": "[package]\nname = \"web\"\n"}

		_, err := f.runner().Prepare(t.Context(), testConfig())
		require.NoError(t, err)

		var names []string
		for _, r := range f.runsIn(sandbox.PhaseAgent) {
			names = append(names, r.name)
		}

		assert.Equal(t, []string{link, seed, treeName, "make test", treeName}, names)

		argv := f.run(t, link).cmd.Argv
		assert.Equal(t, []string{"/home/agent/.cache/cargo/bin", "/cache/cargo/bin"}, argv[4:], "the link goes as its path and its target")
		assert.True(t, strings.HasPrefix(argv[2], proc.SandboxOnly("")), "the script runs only in a sandbox")
	})

	t.Run("after a restart", func(t *testing.T) {
		f := newFake()
		r := f.runner()
		c := testConfig()
		c.Ecosystems = []string{"rust"}

		f.results[link] = sandbox.Result{ExitCode: 1, Output: "ln: Permission denied\n"}

		require.Error(t, r.ensureUp(t.Context(), c, sandbox.PhaseAgent))
		assert.False(t, r.state(c.RunID).serves(nil))

		delete(f.results, link)

		for range 2 {
			require.NoError(t, r.ensureUp(t.Context(), c, sandbox.PhaseAgent))
		}

		assert.Equal(t, []string{"volumes", "up/agent", "run/" + link, "up/agent", "run/" + link, "run/" + seed}, f.calls())
	})

	t.Run("without rust", func(t *testing.T) {
		f := newFake()
		r := f.runner()
		c := testConfig()
		c.Ecosystems = []string{"go", "node", "python"}

		require.NoError(t, r.ensureUp(t.Context(), c, sandbox.PhaseAgent))
		assert.Equal(t, []string{"volumes", "up/agent", goSeed}, f.calls(), "no link is made")
	})
}

// Every start of the agent phase fills the home's Cargo directories from the
// cache with a script that runs only in a sandbox.
func TestAgentPhaseSeedsTheHome(t *testing.T) {
	seed := stepName(homeArgv(seedScript, env.AgentSeeds([]string{"rust"}, home)))

	f := newFake()
	f.files = map[string]string{"Cargo.toml": "[package]\nname = \"web\"\n"}

	_, err := f.runner().Prepare(t.Context(), testConfig())
	require.NoError(t, err)

	argv := f.run(t, seed).cmd.Argv
	assert.Equal(t, []string{
		"/home/agent/.cache/cargo/git/db", "/cache/cargo/git/db",
		"/home/agent/.cache/cargo/registry/cache", "/cache/cargo/registry/cache",
		"/home/agent/.cache/cargo/registry/index", "/cache/cargo/registry/index",
	}, argv[4:], "each directory goes with its source")
	assert.True(t, strings.HasPrefix(argv[2], proc.SandboxOnly("")), "the script runs only in a sandbox")
}
