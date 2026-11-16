package main

import (
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

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
		names string   // what standard error names
	}{
		{"an upper-case name", []string{"Web", "--repo", repo}, strconv.Quote("Web")},
		{"an empty name", []string{"", "--repo", repo}, strconv.Quote("")},
		{"a name over 40 characters", []string{strings.Repeat("a", 41), "--repo", repo}, strconv.Quote(strings.Repeat("a", 41))},
		{"a name with a slash", []string{"web/app", "--repo", repo}, strconv.Quote("web/app")},
		{"no repository", []string{"web"}, "--repo"},
		{"an ssh repository", []string{"web", "--repo", "git@example.com:web.git"}, "--repo"},
		{"an http repository", []string{"web", "--repo", "http://example.com/web.git"}, "--repo"},
		{"a repository with whitespace", []string{"web", "--repo", "https://example.com/my web.git"}, "--repo"},
		{"a repository with a control character", []string{"web", "--repo", repo + "\x1b"}, "--repo"},
		{"a base starting with -", []string{"web", "--repo", repo, "--base=-main"}, "--base"},
		{"a base with whitespace", []string{"web", "--repo", repo, "--base", "my branch"}, "--base"},
		{"a base with a control character", []string{"web", "--repo", repo, "--base", "main\n"}, "--base"},
		{"an empty base", []string{"web", "--repo", repo, "--base", ""}, "--base"},
		{"verify with no-verify", []string{"web", "--repo", repo, "--verify", "make test", "--no-verify"}, "--no-verify"},
		{"a blank verify", []string{"web", "--repo", repo, "--verify", " "}, "--verify"},
		{"a blank setup", []string{"web", "--repo", repo, "--setup", ""}, "--setup"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "data")

			code, stdout, stderr := runCavu(append(append([]string{"project", "add"}, tc.args...), "--data-dir", dir)...)

			assert.Equal(t, 1, code)
			assert.Empty(t, stdout)
			assert.Contains(t, stderr, tc.names)
			assert.NoDirExists(t, dir, "nothing is created")
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
		names string
	}{
		{"a base starting with -", []string{"--base=-x"}, "--base"},
		{"a blank setup", []string{"--setup", " "}, "--setup"},
		{"a blank verify", []string{"--verify", ""}, "--verify"},
		{"verify with no-verify", []string{"--verify", "make test", "--no-verify"}, "--no-verify"},
		{"an environment name that starts with a digit", []string{"--env", "1CI=1"}, "--env"},
		{"an environment value without a name", []string{"--env", "CI"}, "--env"},
		{"a secret variable with a dash", []string{"--secret", "NPM-TOKEN"}, "--secret"},
		{"a tool without a version", []string{"--tool", "go="}, "--tool"},
		{"a version without a tool", []string{"--tool", "=1.27"}, "--tool"},
		{"an unknown switch", []string{"--switch", "fast=on"}, "--switch"},
		{"a switch neither on nor off", []string{"--switch", "web_tools=yes"}, "--switch"},
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

	fake := &fakeRuntime{labels: sandbox.ImageLabels{CavuVersion: version}, found: true, runner: &fakeRunner{}}
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
		fake.runner.checkErr = errors.New("runner: the verify command is blank and the project does not waive it")
		fake.runner.suggestion = "go test ./..."

		code, stdout, stderr := runCavu("project", "check", "web", "--data-dir", dir)

		assert.Equal(t, 1, code)
		assert.Contains(t, stderr, fake.runner.checkErr.Error())
		assert.Contains(t, stdout, "cavu project set web --verify 'go test ./...'")
		assert.Nil(t, storedProject(t, dir, "web").Baseline)
	})

	t.Run("no verify command and nothing to suggest", func(t *testing.T) {
		dir, fake := checkSetup(t)
		fake.runner.checkErr = errors.New("runner: the verify command is blank and the project does not waive it")

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
		{"an image with another CA bundle", func(_ *testing.T, _ string, fake *fakeRuntime) { fake.labels.CABundle = "sum-of-a-bundle" }, "cavu image build"},
		{"the lock held", func(t *testing.T, dir string, _ *fakeRuntime) {
			a, err := openApp(t.Context(), dir)
			require.NoError(t, err)
			require.NoError(t, a.lock())
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

func TestProjectUsageErrors(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")

	for _, args := range [][]string{
		{"project"},
		{"project", "remove", "web"},
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
