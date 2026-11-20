package main

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mhersson/cavu/internal/driver/claudecode"
	"github.com/mhersson/cavu/internal/engine"
	"github.com/mhersson/cavu/internal/sandbox"
	"github.com/mhersson/cavu/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const repo = "https://example.com/web.git"

// cavuIn runs cavu on the data directory dir and wants exit 0.
func cavuIn(t *testing.T, dir string, args ...string) string {
	t.Helper()

	code, stdout, stderr := runCavu(append(args, "--data-dir", dir)...)
	require.Equal(t, 0, code, stderr)

	return stdout
}

// runData returns a data directory with the settings a run reads.
func runData(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()

	for _, kv := range [][2]string{
		{"endpoint_mode", "subscription"}, {"model_work", "model-a"}, {"commit_name", "CAVU"}, {"commit_email", "cavu@example.com"},
	} {
		cavuIn(t, dir, "settings", "set", kv[0], kv[1])
	}

	return dir
}

func storedProjects(t *testing.T, dir string) []store.Project {
	t.Helper()

	a, err := openApp(t.Context(), dir)
	require.NoError(t, err)

	defer a.close()

	ps, err := a.store.Projects(t.Context())
	require.NoError(t, err)

	return ps
}

func storedProject(t *testing.T, dir, name string) store.Project {
	t.Helper()

	for _, p := range storedProjects(t, dir) {
		if p.Name == name {
			return p
		}
	}

	require.Failf(t, "no project", "%s", name)

	return store.Project{}
}

// storeBaseline gives the project name a passed baseline.
func storeBaseline(t *testing.T, dir, name string) {
	t.Helper()

	a, err := openApp(t.Context(), dir)
	require.NoError(t, err)

	defer a.close()

	p, err := a.store.ProjectByName(t.Context(), name)
	require.NoError(t, err)

	p.Baseline = &store.Baseline{Commit: "base123", Passed: true}
	require.NoError(t, a.store.UpdateProject(t.Context(), p))
}

func TestProjectAddValidatesInput(t *testing.T) {
	tests := []struct {
		name  string
		args  []string // after project add
		names string   // the field or flag standard error names
	}{
		{"an upper-case name", []string{"Web", "--repo", repo}, "name"},
		{"no repository", []string{"web"}, "repo_url"},
		{"an http repository", []string{"web", "--repo", "http://example.com/web.git"}, "repo_url"},
		{"a base starting with -", []string{"web", "--repo", repo, "--base=-main"}, "base_branch"},
		{"verify with no-verify", []string{"web", "--repo", repo, "--verify", "make test", "--no-verify"}, "verify_waived"},
		{"a blank verify", []string{"web", "--repo", repo, "--verify", " "}, "verify_command"},
		{"an empty verify", []string{"web", "--repo", repo, "--verify", ""}, "--verify"},
		{"an empty setup", []string{"web", "--repo", repo, "--setup", ""}, "--setup"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()

			code, stdout, stderr := runCavu(append(append([]string{"project", "add"}, tc.args...), "--data-dir", dir)...)

			assert.Equal(t, 1, code)
			assert.Empty(t, stdout)
			assert.Contains(t, stderr, tc.names)
			assert.Empty(t, storedProjects(t, dir), "nothing is stored")
		})
	}

	t.Run("a duplicate name", func(t *testing.T) {
		dir := t.TempDir()
		cavuIn(t, dir, "project", "add", "web", "--repo", repo)

		code, _, stderr := runCavu("project", "add", "web", "--repo", "https://example.com/other.git", "--data-dir", dir)

		assert.Equal(t, 1, code)
		assert.Contains(t, stderr, strconv.Quote("web"))

		ps := storedProjects(t, dir)
		require.Len(t, ps, 1)
		assert.Equal(t, repo, ps[0].RepoURL)
	})
}

func TestProjectAddStoresTheProject(t *testing.T) {
	dir := t.TempDir()

	cavuIn(t, dir, "project", "add", "web", "--repo", repo, "--setup", "make deps", "--verify", "make test")
	cavuIn(t, dir, "project", "add", "docs", "--repo", repo, "--base", "trunk", "--no-verify")
	cavuIn(t, dir, "project", "add", "bare", "--repo", repo)

	web := storedProject(t, dir, "web")
	assert.Equal(t, repo, web.RepoURL)
	assert.Equal(t, "main", web.BaseBranch, "the default base")
	assert.Equal(t, "make deps", web.SetupCommand)
	assert.Equal(t, "make test", web.VerifyCommand)
	assert.False(t, web.VerifyWaived)
	assert.Nil(t, web.Baseline)

	docs := storedProject(t, dir, "docs")
	assert.Equal(t, "trunk", docs.BaseBranch)
	assert.True(t, docs.VerifyWaived)
	assert.Empty(t, docs.VerifyCommand)

	bare := storedProject(t, dir, "bare")
	assert.Empty(t, bare.VerifyCommand)
	assert.False(t, bare.VerifyWaived)
}

func TestProjectSetValidatesInput(t *testing.T) {
	tests := []struct {
		name  string
		args  []string // after project set web
		names string   // the field or flag standard error names
	}{
		{"a base starting with -", []string{"--base=-x"}, "base_branch"},
		{"a blank setup", []string{"--setup", " "}, "setup_command"},
		{"an empty verify", []string{"--verify", ""}, "--verify"},
		{"verify with no-verify", []string{"--verify", "make test", "--no-verify"}, "verify_waived"},
		{"an environment name that starts with a digit", []string{"--env", "1CI=1"}, "env"},
		{"an environment value without a name", []string{"--env", "CI"}, "--env"},
		{"a secret variable with a dash", []string{"--secret", "NPM-TOKEN"}, "secret_vars"},
		{"a tool without a version", []string{"--tool", "go="}, "tool_overrides"},
		{"a tool without =", []string{"--tool", "go"}, "--tool"},
		{"an unknown switch", []string{"--switch", "fast=on"}, "switches"},
		{"a switch neither on nor off", []string{"--switch", "web_tools=yes"}, "--switch"},
		{"a cap a project may not hold", []string{"--cap", "concurrency=2"}, "caps"},
		{"a cap without =", []string{"--cap", "task_max_turns"}, "--cap"},
	}

	dir := t.TempDir()
	cavuIn(t, dir, "project", "add", "web", "--repo", repo, "--verify", "make test")

	before := storedProject(t, dir, "web")

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			code, stdout, stderr := runCavu(append(append([]string{"project", "set", "web"}, tc.args...), "--data-dir", dir)...)

			assert.Equal(t, 1, code)
			assert.Empty(t, stdout)
			assert.Contains(t, stderr, tc.names)
			assert.Equal(t, before, storedProject(t, dir, "web"), "nothing is changed")
		})
	}

	code, _, stderr := runCavu("project", "set", "api", "--setup", "make deps", "--data-dir", dir)
	assert.Equal(t, 1, code)
	assert.Contains(t, stderr, strconv.Quote("api"))
}

func TestProjectSetClearsBaseline(t *testing.T) {
	dir := t.TempDir()
	cavuIn(t, dir, "project", "add", "web", "--repo", repo, "--verify", "make test")

	storeBaseline(t, dir, "web")
	cavuIn(t, dir, "project", "set", "web", "--egress", "proxy.golang.org")
	assert.NotNil(t, storedProject(t, dir, "web").Baseline, "an egress edit keeps it")

	cavuIn(t, dir, "project", "set", "web", "--switch", "web_tools=on")
	assert.NotNil(t, storedProject(t, dir, "web").Baseline, "a switch edit keeps it")

	cavuIn(t, dir, "project", "set", "web", "--setup", "make deps")

	p := storedProject(t, dir, "web")
	assert.Nil(t, p.Baseline, "a setup edit clears it")
	assert.Equal(t, "make deps", p.SetupCommand)
	assert.Equal(t, []string{"proxy.golang.org"}, p.EgressHosts)
}

func TestProjectSetReplacesAndClearsLists(t *testing.T) {
	dir := t.TempDir()
	cavuIn(t, dir, "project", "add", "web", "--repo", repo, "--verify", "make test")

	cavuIn(t, dir, "project", "set", "web",
		"--egress", "a.example.com", "--egress", "b.example.com", "--env", "CI=1", "--env", "GOFLAGS=-mod=mod",
		"--secret", "NPM_TOKEN", "--tool", "go=1.27", "--base", "develop")

	p := storedProject(t, dir, "web")
	assert.Equal(t, []string{"a.example.com", "b.example.com"}, p.EgressHosts)
	assert.Equal(t, map[string]string{"CI": "1", "GOFLAGS": "-mod=mod"}, p.Env)
	assert.Equal(t, []string{"NPM_TOKEN"}, p.SecretVars)
	assert.Equal(t, map[string]string{"go": "1.27"}, p.ToolOverrides)
	assert.Equal(t, "develop", p.BaseBranch)

	cavuIn(t, dir, "project", "set", "web", "--egress", "c.example.com")
	assert.Equal(t, []string{"c.example.com"}, storedProject(t, dir, "web").EgressHosts, "a list flag replaces the field")

	cavuIn(t, dir, "project", "set", "web", "--egress", "", "--env", "")

	p = storedProject(t, dir, "web")
	assert.Empty(t, p.EgressHosts)
	assert.Empty(t, p.Env)
	assert.Equal(t, []string{"NPM_TOKEN"}, p.SecretVars, "a field whose flag is not given stays")
	assert.Equal(t, map[string]string{"go": "1.27"}, p.ToolOverrides)

	cavuIn(t, dir, "project", "set", "web", "--secret", "", "--tool", "")

	p = storedProject(t, dir, "web")
	assert.Empty(t, p.SecretVars)
	assert.Empty(t, p.ToolOverrides)
}

func TestProjectSetVerifyAndSwitches(t *testing.T) {
	dir := t.TempDir()
	cavuIn(t, dir, "project", "add", "web", "--repo", repo, "--verify", "make test")

	cavuIn(t, dir, "project", "set", "web", "--no-verify")

	p := storedProject(t, dir, "web")
	assert.True(t, p.VerifyWaived)
	assert.Empty(t, p.VerifyCommand)

	cavuIn(t, dir, "project", "set", "web", "--verify", "make check")

	p = storedProject(t, dir, "web")
	assert.False(t, p.VerifyWaived)
	assert.Equal(t, "make check", p.VerifyCommand)

	cavuIn(t, dir, "project", "set", "web", "--switch", "web_tools=on", "--switch", "continuations=off")
	cavuIn(t, dir, "project", "set", "web", "--switch", "verify_fix=off")

	assert.Equal(t, map[string]bool{"web_tools": true, "continuations": false, "verify_fix": false},
		storedProject(t, dir, "web").Switches)
}

func TestProjectCaps(t *testing.T) {
	dir := t.TempDir()
	cavuIn(t, dir, "project", "add", "web", "--repo", repo, "--verify", "make test")
	assert.Equal(t, []string{"none"}, labelled(cavuIn(t, dir, "project", "show", "web"), "caps:"))

	cavuIn(t, dir, "project", "set", "web", "--cap", "task_max_turns=50", "--cap", "task_timeout_minutes=60")
	assert.Equal(t, map[string]string{"task_max_turns": "50", "task_timeout_minutes": "60"}, storedProject(t, dir, "web").Caps)
	assert.Equal(t, []string{"task_max_turns=50, task_timeout_minutes=60"}, labelled(cavuIn(t, dir, "project", "show", "web"), "caps:"))

	cavuIn(t, dir, "project", "set", "web", "--cap", "task_max_turns=")
	assert.Equal(t, map[string]string{"task_timeout_minutes": "60"}, storedProject(t, dir, "web").Caps, "an empty value removes the cap")
	assert.Equal(t, []string{"task_timeout_minutes=60"}, labelled(cavuIn(t, dir, "project", "show", "web"), "caps:"))
}

func TestProjectList(t *testing.T) {
	dir := t.TempDir()

	cavuIn(t, dir, "project", "add", "web", "--repo", repo, "--verify", "pytest")
	cavuIn(t, dir, "project", "add", "docs", "--repo", repo, "--base", "trunk", "--no-verify")
	cavuIn(t, dir, "project", "add", "bare", "--repo", repo)
	storeBaseline(t, dir, "web")

	lines := strings.Split(strings.TrimSpace(cavuIn(t, dir, "project", "list")), "\n")
	require.Len(t, lines, 4, "a header and one line per project")

	assert.Equal(t, []string{"bare", repo, "main", "none", "none"}, strings.Fields(lines[1]))
	assert.Equal(t, []string{"docs", repo, "trunk", "waived", "none"}, strings.Fields(lines[2]))
	assert.Equal(t, []string{"web", repo, "main", "pytest", "passed"}, strings.Fields(lines[3]))
}

// checkSetup is a data directory with the project web, and a fake runtime
// whose image is this binary's.
func checkSetup(t *testing.T, addArgs ...string) (string, *fakeRuntime) {
	t.Helper()

	dir := runData(t)
	cavuIn(t, dir, append([]string{"project", "add", "web", "--repo", repo}, addArgs...)...)

	fake := &fakeRuntime{labels: sandbox.ImageLabels{CavuVersion: version, AgentVersion: claudecode.Version}, found: true, runner: &fakeRunner{}}
	useFakeRuntime(t, fake)

	return dir, fake
}

func TestProjectCheck(t *testing.T) {
	t.Run("a passed baseline is stored", func(t *testing.T) {
		dir, fake := checkSetup(t, "--verify", "make test")
		fake.runner.check = engine.PrepareResult{
			BaseCommit: "base123", Tools: []string{"go@1.27"}, IgnoredTools: []string{"python@2.7"}, Baseline: engine.ResultPassed,
		}
		fake.runner.suggestion = "go test ./..."

		code, stdout, stderr := runCavu("project", "check", "web", "--data-dir", dir)

		require.Equal(t, 0, code, stderr)
		assert.Equal(t, sandbox.ImageName(version), fake.image)
		require.Len(t, fake.runner.checks, 1)
		assert.Equal(t, storedProject(t, dir, "web").ID, fake.runner.checks[0].ProjectID)
		assert.Equal(t, sandbox.ImageName(version), fake.runner.checks[0].Image)

		for _, v := range []string{"base123", "go@1.27", "python@2.7", engine.ResultPassed} {
			assert.Contains(t, stdout, v)
		}

		assert.NotContains(t, stdout, "cavu project set", "a project with a verify command gets no suggestion")

		b := storedProject(t, dir, "web").Baseline
		require.NotNil(t, b)
		assert.True(t, b.Passed)
		assert.Equal(t, "base123", b.Commit)
	})

	t.Run("a failed step is stored as not passed", func(t *testing.T) {
		dir, fake := checkSetup(t, "--verify", "make test")
		fake.runner.check = engine.PrepareResult{BaseCommit: "base123", FailedStep: "setup", Output: "no rule to make target"}

		code, stdout, _ := runCavu("project", "check", "web", "--data-dir", dir)

		assert.Equal(t, 1, code)
		assert.Contains(t, stdout, "setup")
		assert.Contains(t, stdout, "no rule to make target")

		b := storedProject(t, dir, "web").Baseline
		require.NotNil(t, b)
		assert.False(t, b.Passed)
	})

	t.Run("a waived verify passes", func(t *testing.T) {
		dir, fake := checkSetup(t, "--no-verify")
		fake.runner.check = engine.PrepareResult{BaseCommit: "base123", Baseline: engine.ResultWaived}

		code, stdout, stderr := runCavu("project", "check", "web", "--data-dir", dir)

		require.Equal(t, 0, code, stderr)
		assert.Contains(t, stdout, engine.ResultWaived)
		assert.NotContains(t, stdout, "cavu project set")
	})

	t.Run("no verify command prints the suggestion", func(t *testing.T) {
		dir, fake := checkSetup(t)
		fake.runner.check = engine.PrepareResult{BaseCommit: "base123"}
		fake.runner.suggestion = "go test ./..."

		code, stdout, stderr := runCavu("project", "check", "web", "--data-dir", dir)

		assert.Equal(t, 1, code)
		assert.Contains(t, stdout, "base123")
		assert.Empty(t, stderr)
		assert.Contains(t, stdout, "cavu project set web --verify 'go test ./...'")
		assert.Nil(t, storedProject(t, dir, "web").Baseline)
	})

	t.Run("no verify command and nothing to suggest", func(t *testing.T) {
		dir, fake := checkSetup(t)
		fake.runner.check = engine.PrepareResult{BaseCommit: "base123"}

		code, stdout, _ := runCavu("project", "check", "web", "--data-dir", dir)

		assert.Equal(t, 1, code)
		assert.Contains(t, stdout, "cavu project set web --no-verify")
	})
}

func TestProjectCheckRefusesBeforeTheRunner(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, dir string, fake *fakeRuntime)
		names string // what standard error names
	}{
		{"an image not built", func(_ *testing.T, _ string, fake *fakeRuntime) { fake.found = false }, "cavu image build"},
		{"an image of another version", func(_ *testing.T, _ string, fake *fakeRuntime) { fake.labels.CavuVersion = "0.0.1" }, "cavu image build"},
		{"an image of another Claude Code", func(_ *testing.T, _ string, fake *fakeRuntime) { fake.labels.AgentVersion = "0.0.1" }, "0.0.1"},
		{"an image with another CA bundle", func(_ *testing.T, _ string, fake *fakeRuntime) { fake.labels.CABundle = "sum-of-a-bundle" }, "cavu image build"},
		{"the lock held", func(t *testing.T, dir string, _ *fakeRuntime) {
			a, err := openApp(t.Context(), dir)
			require.NoError(t, err)
			require.NoError(t, a.lock("verb"))
			t.Cleanup(a.close)
		}, "lock"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir, fake := checkSetup(t, "--verify", "make test")
			tc.setup(t, dir, fake)

			code, stdout, stderr := runCavu("project", "check", "web", "--data-dir", dir)

			assert.Equal(t, 1, code)
			assert.Empty(t, stdout)
			assert.Contains(t, stderr, tc.names)
			assert.Empty(t, fake.runner.checks, "no container starts")
			assert.Nil(t, storedProject(t, dir, "web").Baseline)
		})
	}

	t.Run("an unknown project", func(t *testing.T) {
		dir, fake := checkSetup(t, "--verify", "make test")

		code, _, stderr := runCavu("project", "check", "api", "--data-dir", dir)

		assert.Equal(t, 1, code)
		assert.Contains(t, stderr, strconv.Quote("api"))
		assert.Empty(t, fake.runner.checks)
	})
}

// serveChecks plays the server that holds dir's lock: it runs each requested
// check on fake's runner, as the server's scheduler does, until the test ends.
func serveChecks(t *testing.T, dir string, fake *fakeRuntime) {
	t.Helper()

	server := holdLock(t, dir, "serve")
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)

	s := &engine.Scheduler{Loop: server.loop(fake), Tick: pollEvery, Checks: true, Only: func(store.Task) bool { return false }}

	go func() { done <- s.Run(ctx) }()

	t.Cleanup(func() {
		cancel()
		assert.NoError(t, <-done)
	})
}

func TestProjectCheckUnderAServer(t *testing.T) {
	cases := []struct {
		name  string
		args  []string // after project add web --repo URL
		check engine.PrepareResult
		code  int
		shows []string
	}{
		{"a passed baseline", []string{"--verify", "make test"}, engine.PrepareResult{
			BaseCommit: "base123", Tools: []string{"go@1.27"}, IgnoredTools: []string{"python@2.7"}, Baseline: engine.ResultPassed,
		}, 0, []string{"base123", "go@1.27", "python@2.7", engine.ResultPassed}},
		{"a failed step", []string{"--verify", "make test"}, engine.PrepareResult{
			BaseCommit: "base123", FailedStep: "setup", Output: "no rule to make target",
		}, 1, []string{"setup", "no rule to make target"}},
		{"a waived verify", []string{"--no-verify"}, engine.PrepareResult{
			BaseCommit: "base123", Baseline: engine.ResultWaived,
		}, 0, []string{"base123", engine.ResultWaived}},
		{"a waived verify's failed setup", []string{"--no-verify"}, engine.PrepareResult{
			BaseCommit: "base123", FailedStep: "setup", Output: "no rule to make target",
		}, 1, []string{"base123", "setup", "no rule to make target"}},
		{"no verify command", nil, engine.PrepareResult{BaseCommit: "base123"}, 1, []string{
			"base123", "cavu project set web --verify 'go test ./...'",
		}},
		{"no verify command and a failed setup", nil, engine.PrepareResult{
			BaseCommit: "base123", FailedStep: "setup", Output: "no rule to make target",
		}, 1, []string{"base123", "setup", "no rule to make target", "cavu project set web --verify 'go test ./...'"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir, fake := checkSetup(t, tc.args...)
			fake.runner.check, fake.runner.suggestion = tc.check, "go test ./..."
			serveChecks(t, dir, fake)
			refuseRuntime(t)

			code, stdout, stderr := runCavu("project", "check", "web", "--data-dir", dir)

			assert.Equal(t, tc.code, code, stderr)
			assert.Len(t, fake.runner.checks, 1, "the server ran the check")
			assert.Nil(t, storedProject(t, dir, "web").CheckRequestedAt)

			for _, v := range tc.shows {
				assert.Contains(t, stdout, v)
			}
		})
	}

	t.Run("a fault", func(t *testing.T) {
		dir, fake := checkSetup(t, "--verify", "make test")
		fake.runner.checkErr = errors.New("the container runtime is gone")
		serveChecks(t, dir, fake)
		refuseRuntime(t)

		code, _, stderr := runCavu("project", "check", "web", "--data-dir", dir)

		assert.Equal(t, 1, code)
		assert.Contains(t, stderr, fake.runner.checkErr.Error())
	})
}

func TestProjectCheckInterruptedUnderAServer(t *testing.T) {
	dir, fake := checkSetup(t, "--verify", "make test")
	holdLock(t, dir, "serve")
	refuseRuntime(t)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	var stderr syncBuffer

	done := make(chan error, 1)

	go func() { done <- runProject(ctx, []string{"check", "web", "--data-dir", dir}, nil, io.Discard, &stderr) }()

	require.Eventually(t, func() bool { return storedProject(t, dir, "web").CheckRequestedAt != nil }, within, time.Millisecond)
	cancel()

	assert.Equal(t, 130, exitCodeOf(t, done))
	assert.NotEmpty(t, stderr.String())
	assert.NotNil(t, storedProject(t, dir, "web").CheckRequestedAt, "the request stays for the server")
	assert.Empty(t, fake.runner.checks)
}

func TestProjectUsageErrors(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")

	for _, args := range [][]string{
		{"project"},
		{"project", "remove"},
		{"project", "remove", "web", "extra"},
		{"project", "add"},
		{"project", "set"},
		{"project", "check"},
		{"project", "check", "web", "extra"},
		{"project", "list", "extra"},
		{"project", "set", "web", "--name", "api"},
	} {
		code, stdout, _ := runCavu(append(args, "--data-dir", dir)...)

		assert.Equal(t, 2, code, args)
		assert.Empty(t, stdout, args)
	}

	assert.NoDirExists(t, dir)
}

func TestProjectShowPrintsEveryField(t *testing.T) {
	dir := t.TempDir()
	cavuIn(t, dir, "project", "add", "web", "--repo", repo, "--no-verify")
	cavuIn(t, dir, "project", "set", "web", "--egress", "example.org", "--env", "A=b", "--secret", "TOKEN",
		"--tool", "go=1.22", "--switch", "web_tools=on")

	a, err := openApp(t.Context(), dir)
	require.NoError(t, err)

	p, err := a.store.ProjectByName(t.Context(), "web")
	require.NoError(t, err)

	p.CheckResult = store.CheckResult{Tools: []string{"node@22"}, VerifySuggestion: "npm test"}
	require.NoError(t, a.store.UpdateProject(t.Context(), p))
	a.close()

	out := cavuIn(t, dir, "project", "show", "web")

	for _, want := range []string{
		repo, "waived", "example.org", "A=b", "TOKEN", "go=1.22", "web_tools=on", "continuations=on", "readiness=on", "review=on", "publish=on",
		"node@22",
		"npm test",
	} {
		assert.Contains(t, out, want)
	}

	code, _, _ := runCavu("project", "show", "nope", "--data-dir", dir)
	assert.NotEqual(t, 0, code)
}

func TestProjectSetForgePolicy(t *testing.T) {
	dir := t.TempDir()
	cavuIn(t, dir, "project", "add", "web", "--repo", repo, "--verify", "make test")
	storeBaseline(t, dir, "web")

	shown := func(t *testing.T) [3][]string {
		t.Helper()

		out := cavuIn(t, dir, "project", "show", "web")

		return [3][]string{labelled(out, "no ci:"), labelled(out, "ignored:"), labelled(out, "bots:")}
	}

	assert.Equal(t, [3][]string{{"off"}, {"none"}, {"none"}}, shown(t))

	cavuIn(t, dir, "project", "set", "web", "--no-ci", "on", "--ignore-check", "lint", "--ignore-check", "docs / deploy",
		"--review-bot", "copilot[bot]")

	p := storedProject(t, dir, "web")
	assert.Equal(t, store.ForgePolicy{NoCI: true, IgnoredChecks: []string{"lint", "docs / deploy"}, ReviewBots: []string{"copilot[bot]"}}, p.Forge)
	assert.NotNil(t, p.Baseline, "the policy is no ground of the baseline")
	assert.Equal(t, [3][]string{{"on"}, {"lint, docs / deploy"}, {"copilot[bot]"}}, shown(t))

	cavuIn(t, dir, "project", "set", "web", "--ignore-check", "build")
	assert.Equal(t, store.ForgePolicy{NoCI: true, IgnoredChecks: []string{"build"}, ReviewBots: []string{"copilot[bot]"}},
		storedProject(t, dir, "web").Forge, "a list flag replaces its list, and a flag not given keeps its field")

	cavuIn(t, dir, "project", "set", "web", "--no-ci", "off", "--review-bot", "")

	p = storedProject(t, dir, "web")
	assert.False(t, p.Forge.NoCI)
	assert.Equal(t, []string{"build"}, p.Forge.IgnoredChecks)
	assert.Empty(t, p.Forge.ReviewBots, "one empty value empties the list")

	before := storedProject(t, dir, "web")

	for _, v := range []string{"yes", "true", ""} {
		code, _, stderr := runCavu("project", "set", "web", "--no-ci", v, "--data-dir", dir)
		assert.Equal(t, 1, code, v)
		assert.Contains(t, stderr, "--no-ci", v)
	}

	assert.Equal(t, before, storedProject(t, dir, "web"), "nothing is changed")
}

func TestProjectRemove(t *testing.T) {
	t.Run("a removal frees the name and looks up no image", func(t *testing.T) {
		dir, fake := checkSetup(t, "--verify", "make test")
		id := storedProject(t, dir, "web").ID

		code, _, stderr := runCavu("project", "remove", "web", "--data-dir", dir)

		require.Equal(t, 0, code, stderr)
		require.Len(t, fake.runner.removed, 1)
		assert.Equal(t, id, fake.runner.removed[0].ProjectID)
		assert.Empty(t, storedProjects(t, dir))
		assert.Empty(t, fake.image, "no image is looked up")

		cavuIn(t, dir, "project", "add", "web", "--repo", repo)

		code, _, stderr = runCavu("project", "remove", "web", "--data-dir", dir)
		require.Equal(t, 0, code, stderr)
		assert.Len(t, fake.runner.removed, 2)
	})

	t.Run("an unknown project", func(t *testing.T) {
		dir, fake := checkSetup(t)

		code, _, stderr := runCavu("project", "remove", "api", "--data-dir", dir)

		assert.Equal(t, 1, code)
		assert.Contains(t, stderr, strconv.Quote("api"))
		assert.Empty(t, fake.runner.removed)
	})

	t.Run("an open task is a refusal", func(t *testing.T) {
		dir, fake := checkSetup(t)
		cavuIn(t, dir, "task", "add", "web", "--title", "Add a footer")

		code, stdout, stderr := runCavu("project", "remove", "web", "--data-dir", dir)

		t.Logf("exit %d, stdout %q, stderr %q", code, stdout, stderr)
		assert.Equal(t, 1, code)
		assert.Empty(t, fake.runner.removed)
		assert.Len(t, storedProjects(t, dir), 1)
	})

	t.Run("volumes the runtime will not remove leave the project", func(t *testing.T) {
		dir, fake := checkSetup(t)
		fake.runner.removeErr = errors.New("volume is in use")

		code, stdout, stderr := runCavu("project", "remove", "web", "--data-dir", dir)

		t.Logf("exit %d, stdout %q, stderr %q", code, stdout, stderr)
		assert.Equal(t, 1, code)
		assert.Contains(t, stderr, "volume is in use")
		assert.Len(t, storedProjects(t, dir), 1)
	})
}

func TestProjectForgeCredential(t *testing.T) {
	dir := t.TempDir()

	code, _, stderr := withStdin("ghp_token\n", "credential", "set", "acme", "--data-dir", dir)
	require.Equal(t, 0, code, stderr)

	cavuIn(t, dir, "project", "add", "web", "--repo", "https://github.com/o/r.git", "--no-verify", "--forge-credential", "acme")
	require.Equal(t, "acme", storedProjects(t, dir)[0].ForgeCredential)
	assert.Contains(t, cavuIn(t, dir, "project", "show", "web"), "acme")

	code, _, _ = runCavu("project", "set", "web", "--forge-credential", "nobody", "--data-dir", dir)
	assert.NotEqual(t, 0, code)
	assert.Equal(t, "acme", storedProjects(t, dir)[0].ForgeCredential)

	cavuIn(t, dir, "project", "set", "web", "--forge-credential", "")
	assert.Empty(t, storedProjects(t, dir)[0].ForgeCredential)

	code, _, _ = runCavu("project", "add", "other", "--repo", "https://github.com/o/s.git", "--no-verify", "--forge-credential", "nobody", "--data-dir", dir)
	assert.NotEqual(t, 0, code)
	assert.Len(t, storedProjects(t, dir), 1)
}
