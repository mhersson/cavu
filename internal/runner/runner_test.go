package runner

import (
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/cavu/internal/engine"
	"github.com/mhersson/cavu/internal/env"
	"github.com/mhersson/cavu/internal/proc"
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

// linkStep is the command a rust run's link step runs.
func linkStep() proc.Command {
	links := env.AgentLinks([]string{"rust"}, home)

	argv := []string{"sh", "-c", linkScript, "sh"}
	for _, p := range slices.Sorted(maps.Keys(links)) {
		argv = append(argv, p, links[p])
	}

	return proc.Command{Argv: argv}
}

// Every start of the agent phase makes the home's links before any command
// runs there, and a start whose links fail does not count as up.
func TestAgentPhaseLinksTheHome(t *testing.T) {
	link := stepName(linkStep().Argv)
	seed := seedStep(t)

	t.Run("in a prepare", func(t *testing.T) {
		f := newFake()
		f.files = map[string]string{"Cargo.toml": "[package]\nname = \"web\"\n"}

		_, err := f.runner().Prepare(t.Context(), testConfig())
		require.NoError(t, err)

		var names []string
		for _, r := range f.runsIn(sandbox.PhaseAgent) {
			names = append(names, r.name)
		}

		assert.Equal(t, []string{link, seed, "make test"}, names)

		argv := f.run(t, link).cmd.Argv
		assert.Equal(t, linkStep().Argv, argv, "each link goes as its path and its target")
		assert.Equal(t, []string{home + "/.cache/cargo/bin", cache + "/cargo/bin"}, argv[4:])
		assert.True(t, strings.HasPrefix(argv[2], proc.SandboxOnly("")), "the script runs only in a sandbox")
	})

	t.Run("after a restart", func(t *testing.T) {
		f := newFake()
		r := f.runner()
		c := testConfig()
		c.Ecosystems = []string{"rust"}

		f.results[link] = sandbox.Result{ExitCode: 1, Output: "ln: Permission denied\n"}

		require.Error(t, r.ensureUp(t.Context(), c))
		assert.False(t, r.state(c.RunID).agentUp())

		delete(f.results, link)

		for range 2 {
			require.NoError(t, r.ensureUp(t.Context(), c))
		}

		assert.Equal(t, []string{"volumes", "up/agent", "run/" + link, "up/agent", "run/" + link, "run/" + seed}, f.calls())
	})

	t.Run("without rust", func(t *testing.T) {
		f := newFake()
		r := f.runner()
		c := testConfig()
		c.Ecosystems = []string{"go", "node", "python"}

		require.NoError(t, r.ensureUp(t.Context(), c))
		assert.Equal(t, []string{"volumes", "up/agent"}, f.calls())
	})
}

// seedStep is the name the fake records for a rust run's seed step.
func seedStep(t *testing.T) string {
	t.Helper()

	cmd, err := seedCommand(env.AgentSeeds([]string{"rust"}, home))
	require.NoError(t, err)

	return stepName(cmd.Argv)
}

// Every start of the agent phase fills the home's Cargo directories from the
// cache after its links and before any other command, and a start whose
// copy fails does not count as up.
func TestAgentPhaseSeedsTheHome(t *testing.T) {
	seeds := env.AgentSeeds([]string{"rust"}, home)
	require.NotEmpty(t, seeds)

	seed := seedStep(t)
	link := stepName(linkStep().Argv)

	t.Run("in a prepare", func(t *testing.T) {
		f := newFake()
		f.files = map[string]string{"Cargo.toml": "[package]\nname = \"web\"\n"}

		_, err := f.runner().Prepare(t.Context(), testConfig())
		require.NoError(t, err)

		argv := f.run(t, seed).cmd.Argv

		want := []string{"sh", "-c", seedScript, "sh"}
		for _, dir := range slices.Sorted(maps.Keys(seeds)) {
			want = append(want, dir, seeds[dir])
		}

		assert.Equal(t, want, argv, "each directory goes with its source")
		assert.True(t, strings.HasPrefix(argv[2], proc.SandboxOnly("")), "the script runs only in a sandbox")
	})

	t.Run("after a restart", func(t *testing.T) {
		f := newFake()
		r := f.runner()
		c := testConfig()
		c.Ecosystems = []string{"rust"}

		f.results[seed] = sandbox.Result{ExitCode: 2, Output: "cp: No space left on device\n"}

		require.Error(t, r.ensureUp(t.Context(), c))
		assert.False(t, r.state(c.RunID).agentUp())

		delete(f.results, seed)

		for range 2 {
			require.NoError(t, r.ensureUp(t.Context(), c))
		}

		assert.Equal(t, []string{
			"volumes", "up/agent", "run/" + link, "run/" + seed, "up/agent", "run/" + link, "run/" + seed,
		}, f.calls(), "a start whose copy failed comes up again, and one that is up is not filled again")
	})

	t.Run("without rust", func(t *testing.T) {
		f := newFake()
		r := f.runner()
		c := testConfig()
		c.Ecosystems = []string{"go", "node", "python"}

		require.NoError(t, r.ensureUp(t.Context(), c))
		assert.Empty(t, f.runsIn(sandbox.PhaseAgent))
	})

	t.Run("a failed step", func(t *testing.T) {
		for _, tt := range []struct {
			name string
			res  sandbox.Result
			want error
		}{
			{name: "outside a sandbox", res: sandbox.Result{ExitCode: proc.NotInSandbox}, want: errNotInSandbox},
			{name: "a directory elsewhere", res: sandbox.Result{ExitCode: elsewhere}, want: errElsewhere},
			{name: "another failure", res: sandbox.Result{ExitCode: 2, Output: "cp: No space left on device\n"}},
			{name: "a timeout", res: sandbox.Result{ExitCode: elsewhere, TimedOut: true}},
		} {
			t.Run(tt.name, func(t *testing.T) {
				f := newFake()
				r := f.runner()
				c := testConfig()
				c.Ecosystems = []string{"rust"}

				f.results[seed] = tt.res

				err := r.ensureUp(t.Context(), c)
				require.Error(t, err)
				assert.False(t, r.state(c.RunID).agentUp())

				for _, sentinel := range []error{errNotInSandbox, errElsewhere} {
					if sentinel == tt.want {
						require.ErrorIs(t, err, sentinel)
					} else {
						require.NotErrorIs(t, err, sentinel)
					}
				}
			})
		}
	})
}

// A directory outside the home or a source outside the cache stops the step
// before a command is composed, and nothing runs.
func TestSeedHomeRefusesPathsOutOfBounds(t *testing.T) {
	const (
		dir = home + "/.cache/cargo/registry/index"
		src = cache + "/cargo/registry/index"
	)

	for _, tt := range []struct {
		name     string
		dir, src string
	}{
		{name: "relative directory", dir: ".cache/cargo/registry/index", src: src},
		{name: "empty directory", dir: "", src: src},
		{name: "directory climbing out of the home", dir: home + "/../../etc/cargo", src: src},
		{name: "directory with a trailing slash", dir: dir + "/", src: src},
		{name: "the home itself", dir: home, src: src},
		{name: "directory outside the home", dir: "/etc/cargo", src: src},
		{name: "directory in a sibling of the home", dir: home + "2/index", src: src},
		{name: "relative source", dir: dir, src: "cache/cargo/registry/index"},
		{name: "source climbing out of the cache", dir: dir, src: cache + "/../etc"},
		{name: "source outside the cache", dir: dir, src: "/etc"},
		{name: "source in a sibling of the cache", dir: dir, src: cache + "2/index"},
		{name: "the cache itself", dir: dir, src: cache},
	} {
		t.Run(tt.name, func(t *testing.T) {
			seeds := map[string]string{tt.dir: tt.src}

			cmd, err := seedCommand(seeds)
			require.ErrorIs(t, err, errOutOfBounds)
			assert.Empty(t, cmd.Argv, "nothing is composed")

			f := newFake()
			err = seedHome(t.Context(), fakeContainer{f: f, phase: sandbox.PhaseAgent}, seeds)
			require.ErrorIs(t, err, errOutOfBounds)
			assert.Empty(t, f.calls(), "nothing runs")

			seeds[home+"/.cache/other/index"] = cache + "/other/index"

			_, err = seedCommand(seeds)
			require.ErrorIs(t, err, errOutOfBounds, "a valid directory does not let an invalid one through")
		})
	}
}
