package runner

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/cavu/internal/driver"
	"github.com/mhersson/cavu/internal/engine"
	"github.com/mhersson/cavu/internal/env"
	"github.com/mhersson/cavu/internal/forge"
	"github.com/mhersson/cavu/internal/proc"
	"github.com/mhersson/cavu/internal/proxy"
	"github.com/mhersson/cavu/internal/sandbox"
	"github.com/mhersson/cavu/internal/sidecar"
	"github.com/mhersson/cavu/internal/testkit/forgefake"
	"github.com/mhersson/cavu/internal/verify"
)

// reshimName is how the fake names the step that makes the tools' shims.
var reshimName = stepName([]string{"sh", "-c", reshimScript})

// treeName is how the fake names the working tree check.
var treeName = stepName([]string{"sh", "-c", treeScript})

var treeCheck = "run/" + treeName

var fullOrder = []string{
	"destroy", "volumes", "clone", "up/setup", "read", "up/setup", "copy",
	"run/mise install", "run/make setup", "run/" + reshimName, "down", "up/agent", treeCheck, "run/make test", treeCheck,
}

// without is fullOrder without the given calls.
func without(calls ...string) []string {
	return slices.DeleteFunc(slices.Clone(fullOrder), func(s string) bool { return slices.Contains(calls, s) })
}

func TestPrepareOrder(t *testing.T) {
	f := newFake()
	c := testConfig()
	c.Repo.BaseCommit = baseCommit

	res, err := f.runner().Prepare(t.Context(), c)
	require.NoError(t, err)
	assert.Equal(t, fullOrder, f.calls())
	assert.Equal(t, []sidecar.CloneRequest{{
		URL: c.Repo.URL, Token: plantedForge, BaseBranch: "main", Commit: baseCommit, Branch: "cavu/fix-login",
		Name: "CAVU", Email: "cavu@example.com",
	}}, f.clones, "a redone run clones the recorded base, not the branch's tip")
	assert.Equal(t, engine.PrepareResult{
		BaseCommit: baseCommit, Tools: []string{"go@1.25.0"}, Ecosystems: []string{"go"}, Baseline: engine.ResultPassed,
	}, res)

	assert.Equal(t, env.MiseConfig([]env.Tool{{Name: "go", Version: "1.25.0"}}), f.copied[env.MiseConfigFile])
	assert.Equal(t, 20*time.Minute, f.run(t, "mise install").timeout)
	assert.Equal(t, 30*time.Minute, f.run(t, "make setup").timeout)
	assert.Equal(t, 2*time.Minute, f.run(t, "make test").timeout)
	assert.Equal(t, verify.Argv("make test"), f.run(t, "make test").cmd.Argv)
	assert.Equal(t, []string{baseCommit}, f.reads, "the files are read once, at the base commit")
}

// After the setup command, mise makes the shims for what it added to a tool,
// such as a console script pip installed, so every phase finds its commands.
func TestPrepareReshimsAfterTheSetupCommand(t *testing.T) {
	f := newFake()

	_, err := f.runner().Prepare(t.Context(), testConfig())
	require.NoError(t, err)

	run := f.run(t, reshimName)
	assert.Equal(t, sandbox.PhaseSetup, run.phase)
	assert.True(t, strings.HasPrefix(run.cmd.Argv[2], proc.SandboxOnly("")), "the script runs only in a sandbox")
}

func TestPrepareStartsFromEmptyVolumes(t *testing.T) {
	f := newFake()

	_, err := f.runner().Prepare(t.Context(), testConfig())
	require.NoError(t, err)

	calls := f.calls()
	assert.Equal(t, "destroy", calls[0], "the run's volumes and the task's home go first")
	assert.Equal(t, "volumes", calls[1])
	assert.Equal(t, []sandbox.Options{{
		Runtime: sandbox.Runtime{Bin: "docker", Kind: "docker"}, Image: "cavu-sandbox:test", InstanceID: "0123456789abcdef",
		RunID: 7, TaskID: 5, ProjectID: 3,
	}}, f.opened)
}

func TestPrepareAllowlists(t *testing.T) {
	tests := []struct {
		mode  string
		agent []string
		model bool
	}{
		{mode: "key", agent: []string{"proxy.golang.org", "registry.example", "sum.golang.org"}, model: true},
		{mode: "subscription", agent: []string{"model.example", "proxy.golang.org", "registry.example", "sum.golang.org"}},
	}

	for _, tt := range tests {
		t.Run(tt.mode, func(t *testing.T) {
			f := newFake()
			c := testConfig()
			c.Endpoint.Mode = tt.mode

			_, err := f.runner().Prepare(t.Context(), c)
			require.NoError(t, err)

			setup := f.requests(sandbox.PhaseSetup)
			require.Len(t, setup, 2)
			assert.Empty(t, setup[0].AllowHosts, "nothing is reachable while the files are read")
			assert.Equal(t, sorted(append(env.SetupHosts([]string{"go"}), "registry.example")), setup[1].AllowHosts)

			for _, req := range setup {
				assert.Nil(t, req.Model, "the setup phase has no model route")
			}

			agent := f.requests(sandbox.PhaseAgent)
			require.Len(t, agent, 1)
			assert.Equal(t, tt.agent, agent[0].AllowHosts)

			if !tt.model {
				assert.Nil(t, agent[0].Model)

				return
			}

			require.NotNil(t, agent[0].Model)
			assert.Len(t, agent[0].Model.StandIn, 64)
			assert.Equal(t, proxy.ModelRoute{
				BaseURL: c.Endpoint.URL, Credential: plantedEndpoint, AuthHeader: "bearer", StandIn: agent[0].Model.StandIn,
			}, *agent[0].Model)
		})
	}
}

// Every phase reaches its egress through the bridge: every command but the
// read gets the proxy variables, and the agent phase gets the agent's
// environment.
func TestPrepareEnvironments(t *testing.T) {
	f := newFake()

	_, err := f.runner().Prepare(t.Context(), testConfig())
	require.NoError(t, err)

	proxyVars := []string{
		"HTTP_PROXY=http://127.0.0.1:3128", "HTTPS_PROXY=http://127.0.0.1:3128",
		"http_proxy=http://127.0.0.1:3128", "https_proxy=http://127.0.0.1:3128",
		"NO_PROXY=localhost,127.0.0.1,::1", "no_proxy=localhost,127.0.0.1,::1",
	}

	for _, name := range []string{"mise install", "make setup", reshimName} {
		cmd := f.run(t, name).cmd
		assert.Subset(t, cmd.Env, env.SetupEnv([]string{"go"}), name)
		assert.Subset(t, cmd.Env, proxyVars, name)
	}

	// The config of a first prepare has no base commit: the clone's is named.
	base := "CAVU_BASE_COMMIT=" + baseCommit
	assert.Contains(t, f.run(t, "make setup").cmd.Env, base)

	verifyEnv := f.run(t, "make test").cmd.Env
	assert.ElementsMatch(t, append(env.AgentEnv([]string{"go"}, "/home/agent"), append(proxyVars, base, "APP_ENV=test")...), verifyEnv)
	assert.Equal(t, "/workspace", f.run(t, "make test").cmd.Dir)
	assert.Equal(t, "/workspace", f.run(t, "make setup").cmd.Dir)
}

func TestSetupSecretsOnlyInSetupPhase(t *testing.T) {
	f := newFake()

	_, err := f.runner().Prepare(t.Context(), testConfig())
	require.NoError(t, err)

	setup := f.run(t, "make setup").cmd.Env
	assert.Contains(t, setup, "NPM_TOKEN="+plantedSetup)
	assert.Contains(t, setup, "APP_ENV=test")

	planted := []string{plantedSetup, plantedEndpoint, plantedForge, plantedAppKey}

	for _, name := range []string{"mise install", reshimName} {
		mise := fmt.Sprint(f.run(t, name).cmd)
		assert.NotContains(t, mise, "APP_ENV", name)

		for _, secret := range planted {
			assert.NotContains(t, mise, secret, name)
		}
	}

	for _, r := range f.runsIn(sandbox.PhaseAgent) {
		for _, secret := range planted {
			assert.NotContains(t, fmt.Sprint(r.cmd), secret, r.name)
		}
	}

	// The endpoint credential goes to the proxy's model route and nowhere else.
	for _, req := range f.requests(sandbox.PhaseAgent) {
		require.NotNil(t, req.Model)
		assert.Equal(t, plantedEndpoint, req.Model.Credential)

		model := *req.Model
		model.Credential = ""

		for _, secret := range planted {
			assert.NotContains(t, fmt.Sprint(req.AllowHosts, model), secret)
		}
	}
}

func TestPrepareReusesBaseline(t *testing.T) {
	f := newFake()
	c := testConfig()
	c.Verify.BaselinePassed, c.Verify.BaselineCommit = true, baseCommit

	res, err := f.runner().Prepare(t.Context(), c)
	require.NoError(t, err)
	assert.Equal(t, "reused", res.Baseline)
	assert.Equal(t, fullOrder[:len(fullOrder)-2], f.calls(), "the agent phase comes up and the verify does not run")

	t.Run("not on another commit", func(t *testing.T) {
		f := newFake()
		c.Verify.BaselineCommit = "fedcba9876543210fedcba9876543210fedcba98"

		res, err := f.runner().Prepare(t.Context(), c)
		require.NoError(t, err)
		assert.Equal(t, engine.ResultPassed, res.Baseline)
		assert.Equal(t, fullOrder, f.calls())
	})

	t.Run("not after a failed one", func(t *testing.T) {
		f := newFake()
		c.Verify.BaselinePassed, c.Verify.BaselineCommit = false, baseCommit

		res, err := f.runner().Prepare(t.Context(), c)
		require.NoError(t, err)
		assert.Equal(t, engine.ResultPassed, res.Baseline)
		assert.Equal(t, fullOrder, f.calls())
	})
}

func TestPrepareWaived(t *testing.T) {
	f := newFake()
	c := testConfig()
	c.Verify.Waived, c.Verify.Command = true, ""

	res, err := f.runner().Prepare(t.Context(), c)
	require.NoError(t, err)
	assert.Equal(t, engine.ResultWaived, res.Baseline)
	assert.Empty(t, res.FailedStep)
	assert.Equal(t, fullOrder[:len(fullOrder)-2], f.calls())
}

func TestPrepareSkipsWhatIsNotThere(t *testing.T) {
	for _, command := range []string{"", " \t"} {
		t.Run(fmt.Sprintf("setup command %q", command), func(t *testing.T) {
			f := newFake()
			c := testConfig()
			c.Setup.Command = command

			_, err := f.runner().Prepare(t.Context(), c)
			require.NoError(t, err)
			// Only the setup step's check goes: the baseline's stays.
			want := without("run/make setup", "run/"+reshimName)
			i := slices.Index(want, treeCheck)

			assert.Equal(t, slices.Delete(want, i, i+1), f.calls(), "nothing was added to a tool")
		})
	}

	t.Run("no tools", func(t *testing.T) {
		f := newFake()
		f.files = map[string]string{"Makefile": "test:\n\ttrue\n"}

		res, err := f.runner().Prepare(t.Context(), testConfig())
		require.NoError(t, err)
		assert.Empty(t, res.Tools)
		assert.Equal(t, without("run/mise install", "run/"+reshimName), f.calls(), "no tool has shims")
		assert.Equal(t, env.MiseConfig(nil), f.copied[env.MiseConfigFile], "a stale configuration is replaced")
	})
}

func TestPrepareReportsIgnoredTools(t *testing.T) {
	f := newFake()
	f.files[".tool-versions"] = "ubi:owner/tool 1.0\n"

	res, err := f.runner().Prepare(t.Context(), testConfig())
	require.NoError(t, err)
	assert.Equal(t, []string{".tool-versions: ubi:owner/tool 1.0"}, res.IgnoredTools)
	assert.Equal(t, []string{"go@1.25.0"}, res.Tools)
}

func TestPrepareTreeCheckIgnoresRuntimeWarning(t *testing.T) {
	f := newFake()
	f.results[treeName] = sandbox.Result{Output: "WARN cgroupv2 manager is set to systemd\n"}

	res, err := f.runner().Prepare(t.Context(), testConfig())
	require.NoError(t, err)
	assert.Empty(t, res.FailedStep)
	assert.Equal(t, engine.ResultPassed, res.Baseline)
}

func TestPrepareBaselineLeavesTreeChanged(t *testing.T) {
	f := newFake()
	f.results[treeName] = sandbox.Result{ExitCode: 1, Output: "?? __pycache__/\n"}
	c := testConfig()
	c.Setup.Command = ""

	res, err := f.runner().Prepare(t.Context(), c)
	require.NoError(t, err)
	assert.Equal(t, "baseline", res.FailedStep)
	assert.Equal(t, engine.ResultFailed, res.Baseline)
}

func TestPrepareStepFailures(t *testing.T) {
	failed := sandbox.Result{ExitCode: 1, Output: "step output\n"}
	timedOut := sandbox.Result{ExitCode: 137, Output: "step output\n", TimedOut: true}

	tests := []struct {
		name   string
		script func(f *fakeSandbox)
		step   string
		last   string // the last call
	}{
		{name: "clone error", script: func(f *fakeSandbox) { f.cloneErr = errors.New("sandbox: clone: repository not found") }, step: "clone", last: "clone"},
		{name: "a version file that does not parse", script: func(f *fakeSandbox) { f.files["go.mod"] = "go not-a-version\n" }, step: "toolchain", last: "read"},
		{name: "mise install exit 1", script: func(f *fakeSandbox) { f.results["mise install"] = failed }, step: "toolchain", last: "run/mise install"},
		{name: "mise install timeout", script: func(f *fakeSandbox) { f.results["mise install"] = timedOut }, step: "toolchain", last: "run/mise install"},
		{name: "setup exit 1", script: func(f *fakeSandbox) { f.results["make setup"] = failed }, step: "setup", last: "run/make setup"},
		{name: "setup timeout", script: func(f *fakeSandbox) { f.results["make setup"] = timedOut }, step: "setup", last: "run/make setup"},
		{name: "reshim exit 1", script: func(f *fakeSandbox) { f.results[reshimName] = failed }, step: "setup", last: "run/" + reshimName},
		{name: "setup leaves the tree changed", script: func(f *fakeSandbox) {
			f.results[treeName] = sandbox.Result{ExitCode: 1, Output: " M package-lock.json\n"}
		}, step: "setup", last: treeCheck},
		{name: "baseline exit 1", script: func(f *fakeSandbox) { f.results["make test"] = failed }, step: "baseline", last: "run/make test"},
		{name: "baseline timeout", script: func(f *fakeSandbox) { f.results["make test"] = timedOut }, step: "baseline", last: "run/make test"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFake()
			tt.script(f)

			res, err := f.runner().Prepare(t.Context(), testConfig())
			require.NoError(t, err)
			assert.Equal(t, tt.step, res.FailedStep)
			assert.NotEmpty(t, res.Output)

			calls := f.calls()
			assert.Equal(t, tt.last, calls[len(calls)-1], "no later step runs")

			if tt.step == "baseline" {
				assert.Equal(t, engine.ResultFailed, res.Baseline)
			}
		})
	}

	t.Run("the output is the step's", func(t *testing.T) {
		f := newFake()
		f.results["make setup"] = failed

		res, err := f.runner().Prepare(t.Context(), testConfig())
		require.NoError(t, err)
		assert.Equal(t, failed.Output, res.Output)
	})

	t.Run("a clone error is its text", func(t *testing.T) {
		f := newFake()
		f.cloneErr = errors.New("sandbox: clone: repository not found")

		res, err := f.runner().Prepare(t.Context(), testConfig())
		require.NoError(t, err)
		assert.Equal(t, f.cloneErr.Error(), res.Output)
	})
}

// A fault of the sandbox, a command that cannot start or a blank verify
// command is an error, never a failed step.
func TestPrepareFaults(t *testing.T) {
	tests := []struct {
		name   string
		script func(f *fakeSandbox, c *engine.RunConfig)
		is     error
	}{
		{name: "a clone killed for memory", script: func(f *fakeSandbox, _ *engine.RunConfig) {
			f.cloneErr = fmt.Errorf("%w: the clone container hit its 2048 MB limit", sandbox.ErrOutOfMemory)
		}, is: sandbox.ErrOutOfMemory},
		{name: "a clone container not isolated", script: func(f *fakeSandbox, _ *engine.RunConfig) {
			f.cloneErr = fmt.Errorf("sandbox: clone: %w", sandbox.ErrNotIsolated)
		}, is: sandbox.ErrNotIsolated},
		{name: "a clone sidecar down", script: func(f *fakeSandbox, _ *engine.RunConfig) {
			f.cloneErr = fmt.Errorf("sandbox: clone: %w", sandbox.ErrSidecarDown)
		}, is: sandbox.ErrSidecarDown},
		{name: "an up that fails", script: func(f *fakeSandbox, _ *engine.RunConfig) { f.upErr = sandbox.ErrNotIsolated }, is: sandbox.ErrNotIsolated},
		{name: "a read that fails", script: func(f *fakeSandbox, _ *engine.RunConfig) { f.readCode = 128 }},
		{name: "mise install that cannot start", script: func(f *fakeSandbox, _ *engine.RunConfig) {
			f.runErrs["mise install"] = errors.New("exec in cavu-setup: no such container")
		}},
		{name: "a setup command that cannot start", script: func(f *fakeSandbox, _ *engine.RunConfig) {
			f.runErrs["make setup"] = errors.New("environment entry at index 3: invalid variable name")
		}},
		{name: "a blank verify command", script: func(_ *fakeSandbox, c *engine.RunConfig) { c.Verify.Command = "  " }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFake()
			c := testConfig()
			tt.script(f, &c)

			res, err := f.runner().Prepare(t.Context(), c)
			require.Error(t, err)
			assert.Empty(t, res.FailedStep)

			if tt.is != nil {
				require.ErrorIs(t, err, tt.is)
			}
		})
	}

	t.Run("a context that ends during the clone", func(t *testing.T) {
		f := newFake()
		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		res, err := f.runner().Prepare(ctx, testConfig())
		require.ErrorIs(t, err, context.Canceled)
		assert.Empty(t, res.FailedStep)
	})

	t.Run("a sidecar that ends during a command", func(t *testing.T) {
		f := newFake()

		const down = "sandbox: the sidecar does not answer: it exited: killed"

		f.onRun = func(name string) {
			if name == "make setup" {
				f.fail(down)
			}
		}

		res, err := f.runner().Prepare(t.Context(), testConfig())
		require.Error(t, err)
		require.NotErrorIs(t, err, context.Canceled, "the sidecar's fault replaces the cancelled command's error")
		assert.Empty(t, res.FailedStep)
		assert.Equal(t, "run/make setup", f.calls()[len(f.calls())-1], "the action in flight ends")
	})
}

func TestPrepareOutputIsRedacted(t *testing.T) {
	leak := "token " + plantedSetup + " key " + plantedEndpoint + " forge " + plantedForge + " app " + plantedAppKey

	t.Run("a failed step", func(t *testing.T) {
		f := newFake()
		f.results["make setup"] = sandbox.Result{ExitCode: 1, Output: leak}

		res, err := f.runner().Prepare(t.Context(), testConfig())
		require.NoError(t, err)
		assert.Equal(t, "setup", res.FailedStep)
		assertMasked(t, res.Output)
		assert.Contains(t, res.Output, "[redacted]")
	})

	t.Run("a failed clone", func(t *testing.T) {
		f := newFake()
		f.cloneErr = errors.New(leak)

		res, err := f.runner().Prepare(t.Context(), testConfig())
		require.NoError(t, err)
		assertMasked(t, res.Output)
	})

	t.Run("an error", func(t *testing.T) {
		f := newFake()
		f.runErrs["make setup"] = errors.New(leak)

		_, err := f.runner().Prepare(t.Context(), testConfig())
		require.Error(t, err)
		assertMasked(t, err.Error())
	})

	t.Run("a long output is cut after masking", func(t *testing.T) {
		f := newFake()
		f.results["make test"] = sandbox.Result{ExitCode: 1, Output: strings.Repeat(leak+"\n", 4000)}

		res, err := f.runner().Prepare(t.Context(), testConfig())
		require.NoError(t, err)
		assertMasked(t, res.Output)
		assert.LessOrEqual(t, len(res.Output), 64<<10)
	})
}

func assertMasked(t *testing.T, s string) {
	t.Helper()

	for _, secret := range []string{plantedSetup, plantedEndpoint, plantedForge, plantedAppKey} {
		assert.NotContains(t, s, secret)
	}
}

// appRunner returns a runner whose every sandbox is f and whose forge client
// reaches a fake GitHub, on the real clock since the fake's tokens expire by
// it, with the fake and a repository's address on it.
func (f *fakeSandbox) appRunner(t *testing.T) (*Runner, *forgefake.Fake, string) {
	t.Helper()

	fake := forgefake.New(t, t.TempDir())
	srv := httptest.NewTLSServer(fake)
	t.Cleanup(srv.Close)

	client, err := forge.New(forge.Options{Transport: srv.Client().Transport, Version: "test"})
	require.NoError(t, err)

	r := f.runner()
	r.forge = client

	return r, fake, srv.URL + "/o/n"
}

// appConfig is testConfig for a GitHub App's repository at url.
func appConfig(url string) engine.RunConfig {
	c := testConfig()
	c.Repo.URL = url
	c.Repo.Access = engine.ForgeAccess{AppID: 42, AppKey: forgefake.AppKey()}

	return c
}

func TestCloneUsesAnAppToken(t *testing.T) {
	f := newFake()
	r, fake, url := f.appRunner(t)
	c := appConfig(url)

	_, err := r.Prepare(t.Context(), c)
	require.NoError(t, err)

	minted := fake.Tokens()
	require.Len(t, minted, 1)
	require.Len(t, f.clones, 1)
	assert.Equal(t, minted[0].Value, f.clones[0].Token)

	// What every container got: the clone's request, the sidecars' proxy
	// requests, the commands and the files copied in.
	var got strings.Builder

	fmt.Fprintf(&got, "%+v\n", f.clones)

	for _, u := range f.ups {
		fmt.Fprintf(&got, "%+v %+v %+v\n", u.req.AllowHosts, u.req.Model, u.req.Budget)
	}

	for _, run := range f.runs {
		fmt.Fprintf(&got, "%+v %s %s\n", run.cmd, run.cmd.Env, run.cmd.Files)
	}

	for p, data := range f.copied {
		fmt.Fprintf(&got, "%s %s\n", p, data)
	}

	assert.NotContains(t, got.String(), c.Repo.Access.AppKey, "the App's key reaches no container")
	assert.Equal(t, 1, strings.Count(got.String(), minted[0].Value), "the token reaches the clone alone")
}

func TestRunnerMasksAMintedToken(t *testing.T) {
	f := newFake()
	f.onClone = func(r sidecar.CloneRequest) error {
		return fmt.Errorf("fatal: could not read from https://x-access-token:%s@forge.example/o/n", r.Token)
	}
	r, _, url := f.appRunner(t)

	res, err := r.Prepare(t.Context(), appConfig(url))
	require.NoError(t, err)
	require.Len(t, f.clones, 1)
	assert.Equal(t, stepClone, res.FailedStep)
	assert.NotContains(t, res.Output, f.clones[0].Token)
	assert.Contains(t, res.Output, "[redacted]")
}

// A personal access token reaches the clone as it is, on a repository address
// no forge client takes, and the forge is never asked: its client is nil.
func TestCloneUsesAPersonalTokenAsItIs(t *testing.T) {
	for _, url := range []string{"https://gitlab.example/group/subgroup/web.git", "https://git.example/web.git"} {
		t.Run(url, func(t *testing.T) {
			f := newFake()
			c := testConfig()
			c.Repo.URL = url

			res, err := f.runner().Prepare(t.Context(), c)
			require.NoError(t, err)
			assert.Empty(t, res.FailedStep)
			require.Len(t, f.clones, 1)
			assert.Equal(t, plantedForge, f.clones[0].Token)
		})
	}
}

// An App's token the forge cannot give fails the clone step with the forge's
// error, as a clone refused for its token does; a context that ended is an error.
func TestCloneWithoutAnAppToken(t *testing.T) {
	t.Run("an address that names no repository on a forge", func(t *testing.T) {
		f := newFake()

		res, err := f.runner().Prepare(t.Context(), appConfig("https://gitlab.example/group/subgroup/web.git"))
		require.NoError(t, err)
		assert.Equal(t, stepClone, res.FailedStep)
		assert.NotEmpty(t, res.Output)
		assert.Empty(t, f.clones)
	})

	t.Run("a mint the forge refuses", func(t *testing.T) {
		f := newFake()
		r, fake, url := f.appRunner(t)
		fake.Fail(1)

		res, err := r.Prepare(t.Context(), appConfig(url))
		require.NoError(t, err)
		assert.Equal(t, stepClone, res.FailedStep)
		assert.NotEmpty(t, res.Output)
		assert.Empty(t, f.clones)
	})

	t.Run("a context that ended", func(t *testing.T) {
		f := newFake()
		r, _, url := f.appRunner(t)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		res, err := r.Prepare(ctx, appConfig(url))
		require.ErrorIs(t, err, context.Canceled)
		assert.Empty(t, res.FailedStep)
	})
}

func TestCheckEnvironmentNeverReusesBaseline(t *testing.T) {
	f := newFake()
	c := testConfig()
	c.Verify.BaselinePassed, c.Verify.BaselineCommit = true, baseCommit

	res, suggestion, err := f.runner().CheckEnvironment(t.Context(), c)
	require.NoError(t, err)
	assert.Equal(t, engine.ResultPassed, res.Baseline)
	assert.Equal(t, "make test", suggestion)

	calls := f.calls()
	assert.Equal(t, "destroy", calls[0], "a check that died must not hand its home to the next")
	assert.Contains(t, calls, "run/make test")
	assert.Equal(t, "destroy", calls[len(calls)-1], "the check removes its volumes")

	for _, o := range f.opened {
		assert.Zero(t, o.RunID)
		assert.Zero(t, o.TaskID)
		assert.Equal(t, c.ProjectID, o.ProjectID)
	}

	t.Run("released after a fault", func(t *testing.T) {
		f := newFake()
		f.upErr = sandbox.ErrNotIsolated

		_, _, err := f.runner().CheckEnvironment(t.Context(), testConfig())
		require.ErrorIs(t, err, sandbox.ErrNotIsolated)
		assert.Equal(t, "destroy", f.calls()[len(f.calls())-1])
	})

	t.Run("released after a failed step", func(t *testing.T) {
		f := newFake()
		f.results["make setup"] = sandbox.Result{ExitCode: 2, Output: "no rule"}

		res, suggestion, err := f.runner().CheckEnvironment(t.Context(), testConfig())
		require.NoError(t, err)
		assert.Equal(t, "setup", res.FailedStep)
		assert.Equal(t, "make test", suggestion, "the files were read before the failure")
		assert.Equal(t, "destroy", f.calls()[len(f.calls())-1])
	})
}

func TestVerifyResultsMatchEngine(t *testing.T) {
	assert.Equal(t, engine.ResultPassed, verify.Passed)
	assert.Equal(t, engine.ResultFailed, verify.Failed)
	assert.Equal(t, engine.ResultTimeout, verify.Timeout)
}

func sorted(s []string) []string {
	s = slices.Clone(s)
	slices.Sort(s)

	return slices.Compact(s)
}

func TestDeniedHostsAreNamedInAFailedStep(t *testing.T) {
	deny := func(f *fakeSandbox, command string) {
		f.onRun = func(name string) {
			if name == command {
				f.send(sidecar.Line{Type: "denied", Host: "registry.example.test"})
				f.send(sidecar.Line{Type: "denied", Host: "registry.example.test"})
				f.send(sidecar.Line{Type: "denied", Host: "cdn.example.test"})
			}
		}
	}

	t.Run("a failed setup step", func(t *testing.T) {
		f := newFake()
		f.results["make setup"] = sandbox.Result{ExitCode: 1, Output: "step output\n"}
		deny(f, "make setup")

		res, err := f.runner().Prepare(t.Context(), testConfig())
		require.NoError(t, err)
		assert.Equal(t, "setup", res.FailedStep)
		assert.Equal(t, 1, strings.Count(res.Output, "registry.example.test"))
		assert.Contains(t, res.Output, "cdn.example.test")
	})

	t.Run("a passing step adds nothing", func(t *testing.T) {
		f := newFake()
		deny(f, "make setup")

		res, err := f.runner().Prepare(t.Context(), testConfig())
		require.NoError(t, err)
		assert.NotContains(t, res.Output, "example.test")
	})

	t.Run("a host denied before the step is not named", func(t *testing.T) {
		f := newFake()
		f.results["make setup"] = sandbox.Result{ExitCode: 1, Output: "step output\n"}
		f.onRun = func(name string) {
			if name == "mise install" {
				f.send(sidecar.Line{Type: "denied", Host: "earlier.example.test"})
			}
		}

		res, err := f.runner().Prepare(t.Context(), testConfig())
		require.NoError(t, err)
		assert.NotContains(t, res.Output, "earlier.example.test")
	})
}

func TestCheckEnvironmentWithoutAVerifyCommandReportsTheWorkspace(t *testing.T) {
	f := newFake()
	c := testConfig()
	c.Verify.Command = " "

	res, suggestion, err := f.runner().CheckEnvironment(t.Context(), c)
	require.NoError(t, err)
	assert.Equal(t, baseCommit, res.BaseCommit)
	assert.Empty(t, res.Baseline)
	assert.Equal(t, "make test", suggestion)

	_, err = f.runner().Prepare(t.Context(), c)
	require.Error(t, err, "a task cannot start without a verify command")
}

func TestSetupsOfOneProjectTakeTurns(t *testing.T) {
	// block holds each setup command until the test releases it; inside counts
	// the setups between their install and their reshim.
	setup := func(f *fakeSandbox) (entered, release chan struct{}, maxInside func() int) {
		entered, release = make(chan struct{}, 2), make(chan struct{}, 2)

		var (
			mu      sync.Mutex
			now, hi int
		)

		f.onRun = func(name string) {
			mu.Lock()
			defer mu.Unlock()

			switch name {
			case "mise install":
				now++
				hi = max(hi, now)
			case reshimName:
				now--
			case "make setup":
				mu.Unlock()

				entered <- struct{}{}

				<-release

				mu.Lock()
			}
		}

		return entered, release, func() int {
			mu.Lock()
			defer mu.Unlock()

			return hi
		}
	}

	prepareIn := func(r *Runner, project, run int64) <-chan error {
		c := testConfig()
		c.ProjectID, c.RunID = project, run
		done := make(chan error, 1)

		go func() {
			_, err := r.Prepare(t.Context(), c)
			done <- err
		}()

		return done
	}

	t.Run("one project", func(t *testing.T) {
		f := newFake()
		read := make(chan struct{}, 1)
		f.onRead = func() { read <- struct{}{} }
		r := f.runner()

		held, err := r.takeTurn(t.Context(), 3)
		require.NoError(t, err)

		defer held()

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		c := testConfig()
		c.ProjectID = 3
		done := make(chan error, 1)

		go func() {
			_, err := r.Prepare(ctx, c)
			done <- err
		}()

		// Past detection the prepare reaches its setup, which only the turn holds back.
		<-read
		cancel()

		require.ErrorIs(t, <-done, context.Canceled)

		for _, run := range f.runsIn(sandbox.PhaseSetup) {
			assert.NotEqual(t, "mise install", run.name, "the install ran while another setup held the turn")
		}
	})

	t.Run("two projects", func(t *testing.T) {
		f := newFake()
		entered, release, maxInside := setup(f)
		r := f.runner()

		first, second := prepareIn(r, 3, 7), prepareIn(r, 4, 8)

		// Both sit inside their setup command at once.
		<-entered
		<-entered

		release <- struct{}{}

		release <- struct{}{}

		require.NoError(t, <-first)
		require.NoError(t, <-second)
		assert.Equal(t, 2, maxInside())
	})

	t.Run("a wait ends with its context", func(t *testing.T) {
		r := newFake().runner()

		held, err := r.takeTurn(t.Context(), 3)
		require.NoError(t, err)

		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		_, err = r.takeTurn(ctx, 3)
		require.ErrorIs(t, err, context.Canceled)

		held()

		release, err := r.takeTurn(t.Context(), 3)
		require.NoError(t, err)
		release()
	})
}

func TestPlainVariablesReachEveryPhase(t *testing.T) {
	f := newFake()
	c := workConfig()
	c.Setup.Env = map[string]string{"LINT_HOME": "/x"}
	c.Setup.Secrets = map[string]string{"TOKEN": "tok-secret"}
	f.results[freezeName(c, "Fix login")] = sandbox.Result{Output: headCommit + "\n"}
	d := &fakeAgent{results: []driver.Result{{Reason: driver.ReasonDone, Structured: structured(map[string]any{"status": "done"})}}}
	r := f.workRunner(d)

	_, err := r.Prepare(t.Context(), c)
	require.NoError(t, err)

	_, err = r.Work(t.Context(), c, startWork, noProgress)
	require.NoError(t, err)
	require.Len(t, d.specs, 1)

	setupEnv := f.run(t, "make setup").cmd.Env
	assert.Contains(t, setupEnv, "LINT_HOME=/x")
	assert.Contains(t, setupEnv, "TOKEN=tok-secret")

	for name, got := range map[string][]string{
		"verify":  f.run(t, "make test").cmd.Env,
		"session": d.specs[0].Env,
	} {
		assert.Contains(t, got, "LINT_HOME=/x", name)
		assert.NotContains(t, fmt.Sprint(got), "tok-secret", name)
	}

	freeze := fmt.Sprint(f.run(t, freezeName(c, "Fix login")).cmd.Env)
	assert.NotContains(t, freeze, "LINT_HOME")
	assert.NotContains(t, freeze, "tok-secret")
}
