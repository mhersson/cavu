package runner

import (
	"errors"
	"maps"
	"path"
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

// Every start of the agent phase makes the home's links before any command
// runs there, and a start whose links fail does not count as up.
func TestAgentPhaseLinksTheHome(t *testing.T) {
	links := env.AgentLinks([]string{"rust"}, home)
	require.NotEmpty(t, links)

	cmd, err := linkCommand(links)
	require.NoError(t, err)

	link := stepName(cmd.Argv)

	t.Run("in a prepare", func(t *testing.T) {
		f := newFake()
		f.files = map[string]string{"Cargo.toml": "[package]\nname = \"web\"\n"}

		_, err := f.runner().Prepare(t.Context(), testConfig())
		require.NoError(t, err)

		var names []string
		for _, r := range f.runsIn(sandbox.PhaseAgent) {
			names = append(names, r.name)
		}

		assert.Equal(t, []string{link, "make test"}, names)

		argv := f.run(t, link).cmd.Argv

		want := []string{"sh", "-c", linkScript, "sh"}
		for _, p := range slices.Sorted(maps.Keys(links)) {
			want = append(want, path.Dir(p), path.Base(p), links[p])
		}

		assert.Equal(t, want, argv, "each link goes as its parent, its name and its target")
		assert.True(t, strings.HasPrefix(argv[2], proc.SandboxOnly("")), "the script runs only in a sandbox")
	})

	t.Run("after a restart", func(t *testing.T) {
		f := newFake()
		r := f.runner()
		c := testConfig()
		c.Ecosystems = []string{"rust"}

		f.results[link] = sandbox.Result{ExitCode: 2, Output: "ln: Permission denied\n"}

		require.Error(t, r.ensureUp(t.Context(), c))

		delete(f.results, link)

		for range 2 {
			require.NoError(t, r.ensureUp(t.Context(), c))
		}

		assert.Equal(t, []string{"volumes", "up/agent", "run/" + link, "up/agent", "run/" + link}, f.calls())
	})

	t.Run("without rust", func(t *testing.T) {
		f := newFake()
		r := f.runner()
		c := testConfig()
		c.Ecosystems = []string{"go", "node", "python"}

		require.NoError(t, r.ensureUp(t.Context(), c))
		assert.Equal(t, []string{"volumes", "up/agent"}, f.calls())
	})

	// A missing marker and a parent that resolves elsewhere each have their
	// own error; any other failure has neither.
	t.Run("a failed step", func(t *testing.T) {
		for _, tt := range []struct {
			name string
			res  sandbox.Result
			want error
		}{
			{name: "outside a sandbox", res: sandbox.Result{ExitCode: proc.NotInSandbox}, want: errLinkRefused},
			{name: "a parent elsewhere", res: sandbox.Result{ExitCode: parentElsewhere}, want: errLinkParent},
			{name: "another failure", res: sandbox.Result{ExitCode: 2, Output: "ln: Permission denied\n"}},
			{name: "a timeout", res: sandbox.Result{ExitCode: parentElsewhere, TimedOut: true}},
		} {
			t.Run(tt.name, func(t *testing.T) {
				f := newFake()
				r := f.runner()
				c := testConfig()
				c.Ecosystems = []string{"rust"}

				f.results[link] = tt.res

				err := r.ensureUp(t.Context(), c)
				require.Error(t, err)
				assert.False(t, r.state(c.RunID).agentUp())

				for _, sentinel := range []error{errLinkRefused, errLinkParent} {
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

// A link outside the home or a target outside the cache stops the step before
// a command is composed, and nothing runs.
func TestLinkHomeRefusesPathsOutOfBounds(t *testing.T) {
	const (
		link   = home + "/.cache/cargo/bin"
		target = cache + "/cargo/bin"
	)

	for _, tt := range []struct {
		name         string
		link, target string
	}{
		{name: "relative link", link: ".cache/cargo/bin", target: target},
		{name: "empty link", link: "", target: target},
		{name: "link climbing out of the home", link: home + "/../../etc/cargo", target: target},
		{name: "link climbing within the home", link: home + "/.cache/../.cache/cargo/bin", target: target},
		{name: "link with a trailing slash", link: link + "/", target: target},
		{name: "the home itself", link: home, target: target},
		{name: "the home with a trailing slash", link: home + "/", target: target},
		{name: "link outside the home", link: "/etc/cargo/bin", target: target},
		{name: "link in a sibling of the home", link: home + "2/bin", target: target},
		{name: "relative target", link: link, target: "cache/cargo/bin"},
		{name: "target climbing out of the cache", link: link, target: cache + "/../etc"},
		{name: "target outside the cache", link: link, target: "/etc/passwd"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			links := map[string]string{tt.link: tt.target}

			cmd, err := linkCommand(links)
			require.ErrorIs(t, err, errLinkPath)
			assert.Empty(t, cmd.Argv, "nothing is composed")

			f := newFake()
			err = linkHome(t.Context(), fakeContainer{f: f, phase: sandbox.PhaseAgent}, links)
			require.ErrorIs(t, err, errLinkPath)
			assert.Empty(t, f.calls(), "nothing runs")

			links[home+"/.cache/other/bin"] = cache + "/other/bin"

			_, err = linkCommand(links)
			require.ErrorIs(t, err, errLinkPath, "a valid link does not let an invalid one through")
		})
	}
}
