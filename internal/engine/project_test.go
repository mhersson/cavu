package engine_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/mhersson/cavu/internal/engine"
	"github.com/mhersson/cavu/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// withBaseline stores a passed baseline on the harness's project and returns the project.
func (h *harness) withBaseline() store.Project {
	h.t.Helper()

	p, err := h.store.Project(h.t.Context(), h.task.ProjectID)
	require.NoError(h.t, err)

	p.Baseline = &store.Baseline{Commit: "old999", Passed: true, At: now, Output: "ok"}
	require.NoError(h.t, h.store.UpdateProject(h.t.Context(), p))

	return p
}

func (h *harness) project() store.Project {
	h.t.Helper()

	p, err := h.store.Project(h.t.Context(), h.task.ProjectID)
	require.NoError(h.t, err)

	return p
}

func TestEditProjectClearsBaseline(t *testing.T) {
	tests := []struct {
		name  string
		edit  func(p *store.Project)
		keeps bool
	}{
		{"repository", func(p *store.Project) { p.RepoURL = "https://example.com/other.git" }, false},
		{"base branch", func(p *store.Project) { p.BaseBranch = "develop" }, false},
		{"setup command", func(p *store.Project) { p.SetupCommand = "make deps" }, false},
		{"verify command", func(p *store.Project) { p.VerifyCommand = "make check" }, false},
		{"waiver", func(p *store.Project) { p.VerifyCommand, p.VerifyWaived = "", true }, false},
		{"tool overrides", func(p *store.Project) { p.ToolOverrides = map[string]string{"go": "1.27"} }, false},
		{"environment", func(p *store.Project) { p.Env = map[string]string{"CI": "1"} }, false},
		{"secret variables", func(p *store.Project) { p.SecretVars = []string{"NPM_TOKEN"} }, false},
		{"egress hosts alone", func(p *store.Project) { p.EgressHosts = []string{"proxy.golang.org"} }, true},
		{"switches alone", func(p *store.Project) { p.Switches = map[string]bool{"web_tools": true} }, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			stored := h.withBaseline().Baseline

			edit := h.project()
			edit.Baseline = nil // an edit keeps or drops the stored baseline, whatever it carries
			tc.edit(&edit)

			require.NoError(t, engine.EditProject(t.Context(), h.store, edit))

			got := h.project()
			if tc.keeps {
				require.NotNil(t, got.Baseline)
				assert.Equal(t, stored.Commit, got.Baseline.Commit)
				assert.True(t, got.Baseline.Passed)
			} else {
				assert.Nil(t, got.Baseline)
			}

			got.Baseline = nil
			assert.Equal(t, edit, got, "the edit is written")
		})
	}
}

func TestEditProjectOfAMissingProject(t *testing.T) {
	h := newHarness(t)

	gone := h.project()
	gone.ID, gone.Name = 999, "gone"

	assert.ErrorIs(t, engine.EditProject(t.Context(), h.store, gone), store.ErrNotFound)
}

// validProject is a project that holds only what a project may hold.
func validProject() store.Project {
	return store.Project{
		Name: "api", RepoURL: "https://example.com/api.git", BaseBranch: "main", SetupCommand: "make deps", VerifyCommand: "make test",
		Env: map[string]string{"CI": "1", "_X2": "y"}, SecretVars: []string{"NPM_TOKEN"}, ToolOverrides: map[string]string{"go": "1.27"},
		Switches: map[string]bool{"continuations": false, "verify_fix": true, "web_tools": true}, EgressHosts: []string{"proxy.golang.org"},
	}
}

func TestProjectRules(t *testing.T) {
	tests := []struct {
		name  string
		edit  func(p *store.Project)
		field string // what the error names
	}{
		{"an upper-case name", func(p *store.Project) { p.Name = "Api" }, "name"},
		{"an empty name", func(p *store.Project) { p.Name = "" }, "name"},
		{"a name over 40 characters", func(p *store.Project) { p.Name = strings.Repeat("a", 41) }, "name"},
		{"a name with a slash", func(p *store.Project) { p.Name = "api/v2" }, "name"},
		{"no repository", func(p *store.Project) { p.RepoURL = "" }, "repo_url"},
		{"an ssh repository", func(p *store.Project) { p.RepoURL = "git@example.com:api.git" }, "repo_url"},
		{"an http repository", func(p *store.Project) { p.RepoURL = "http://example.com/api.git" }, "repo_url"},
		{"a repository with whitespace", func(p *store.Project) { p.RepoURL = "https://example.com/my api.git" }, "repo_url"},
		{"a repository with a control character", func(p *store.Project) { p.RepoURL = "https://example.com/api.git\x1b" }, "repo_url"},
		{"an empty base", func(p *store.Project) { p.BaseBranch = "" }, "base_branch"},
		{"a base starting with -", func(p *store.Project) { p.BaseBranch = "-main" }, "base_branch"},
		{"a base with whitespace", func(p *store.Project) { p.BaseBranch = "my branch" }, "base_branch"},
		{"a base with a control character", func(p *store.Project) { p.BaseBranch = "main\n" }, "base_branch"},
		{"a blank setup", func(p *store.Project) { p.SetupCommand = " " }, "setup_command"},
		{"a blank verify", func(p *store.Project) { p.VerifyCommand = "\t" }, "verify_command"},
		{"verify with its waiver", func(p *store.Project) { p.VerifyWaived = true }, "verify_waived"},
		{"an environment name starting with a digit", func(p *store.Project) { p.Env = map[string]string{"1CI": "1"} }, "env"},
		{"an environment name with a dash", func(p *store.Project) { p.Env = map[string]string{"GO-FLAGS": "x"} }, "env"},
		{"a secret variable with a dash", func(p *store.Project) { p.SecretVars = []string{"NPM-TOKEN"} }, "secret_vars"},
		{"a tool without a version", func(p *store.Project) { p.ToolOverrides = map[string]string{"go": ""} }, "tool_overrides"},
		{"a version without a tool", func(p *store.Project) { p.ToolOverrides = map[string]string{"": "1.27"} }, "tool_overrides"},
		{"an unknown switch", func(p *store.Project) { p.Switches = map[string]bool{"fast": true} }, "switches"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)

			add := validProject()
			tc.edit(&add)

			_, err := engine.AddProject(t.Context(), h.store, add)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.field)

			ps, err := h.store.Projects(t.Context())
			require.NoError(t, err)
			assert.Len(t, ps, 1, "nothing is added")

			h.withBaseline()

			before := h.project()
			edit := before
			tc.edit(&edit)

			err = engine.EditProject(t.Context(), h.store, edit)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.field)
			assert.Equal(t, before, h.project(), "nothing is changed")
		})
	}
}

func TestAddProject(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()

	want := validProject()

	got, err := engine.AddProject(ctx, h.store, want)
	require.NoError(t, err)
	assert.NotZero(t, got.ID)

	stored, err := h.store.ProjectByName(ctx, "api")
	require.NoError(t, err)

	want.ID = got.ID
	assert.Equal(t, want, stored)

	_, err = engine.AddProject(ctx, h.store, want)
	require.ErrorIs(t, err, store.ErrConflict, "a duplicate name")

	for name, edit := range map[string]func(p *store.Project){
		"bare":   func(p *store.Project) { p.SetupCommand, p.VerifyCommand = "", "" },
		"waived": func(p *store.Project) { p.VerifyCommand, p.VerifyWaived = "", true },
		"empty": func(p *store.Project) {
			p.Env, p.SecretVars, p.ToolOverrides, p.Switches, p.EgressHosts = nil, nil, nil, nil, nil
		},
	} {
		p := validProject()
		p.Name = name
		edit(&p)

		_, err := engine.AddProject(ctx, h.store, p)
		assert.NoError(t, err, name)
	}
}

func TestCheckProjectStoresTheBaseline(t *testing.T) {
	tests := []struct {
		name   string
		res    engine.PrepareResult
		waive  bool
		passed bool
		none   bool // the check stores no baseline
	}{
		{name: "passed", res: engine.PrepareResult{BaseCommit: "base123", Tools: []string{"go@1.27"}, Baseline: engine.ResultPassed}, passed: true},
		{name: "a failed step", res: engine.PrepareResult{BaseCommit: "base123", FailedStep: "setup", Output: "no rule"}},
		{name: "a failed baseline", res: engine.PrepareResult{
			BaseCommit: "base123", Baseline: engine.ResultFailed, FailedStep: "baseline", Output: "FAIL",
		}},
		{name: "waived", res: engine.PrepareResult{BaseCommit: "base123", Baseline: engine.ResultWaived}, waive: true, none: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			p := h.withBaseline()

			if tc.waive {
				p.VerifyCommand, p.VerifyWaived = "", true
				require.NoError(t, h.store.UpdateProject(t.Context(), p))
			}

			h.runner.prepares = []step[engine.PrepareResult]{{res: tc.res}}
			h.runner.suggestion = "make test"

			res, suggestion, err := h.loop.CheckProject(t.Context(), p.ID)
			require.NoError(t, err)
			assert.Equal(t, tc.res, res)
			assert.Equal(t, "make test", suggestion)

			checks := h.runner.callsOf("check")
			require.Len(t, checks, 1)
			c := checks[0].cfg
			assert.Zero(t, c.RunID)
			assert.Zero(t, c.TaskID)
			assert.Equal(t, p.ID, c.ProjectID)
			assert.Equal(t, "cavu-sandbox:test", c.Image)
			assert.Equal(t, p.VerifyCommand, c.Verify.Command)
			assert.Equal(t, tc.waive, c.Verify.Waived)
			assert.Equal(t, "inst-1", c.InstanceID)

			got := h.project().Baseline
			if tc.none {
				assert.Nil(t, got, "a waived verify has no baseline, so nothing is stored as passed")

				return
			}

			require.NotNil(t, got)
			assert.Equal(t, tc.res.BaseCommit, got.Commit)
			assert.Equal(t, tc.passed, got.Passed)
			assert.Equal(t, tc.res.Output, got.Output)
			assert.True(t, got.At.Equal(now), "the time is the loop's clock")
		})
	}
}

func TestCheckProjectKeepsTheBaselineOnARunnerError(t *testing.T) {
	h := newHarness(t)
	p := h.withBaseline()
	fault := errors.New("runner: the verify command is blank and the project does not waive it")

	h.runner.prepares = []step[engine.PrepareResult]{{err: fault}}
	h.runner.suggestion = "go test ./..."

	_, suggestion, err := h.loop.CheckProject(t.Context(), p.ID)
	require.ErrorIs(t, err, fault)
	assert.Equal(t, "go test ./...", suggestion, "the suggestion comes back with the error")
	assert.Equal(t, p.Baseline.Commit, h.project().Baseline.Commit)
	assert.True(t, h.project().Baseline.Passed)
}

func TestCheckProjectRefusesAMissingSetting(t *testing.T) {
	h := newHarness(t)
	require.NoError(t, h.store.SetSetting(t.Context(), "model_work", ""))

	_, _, err := h.loop.CheckProject(t.Context(), h.task.ProjectID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "model_work")
	assert.Empty(t, h.runner.callsOf("check"))
}
