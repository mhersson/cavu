package engine_test

import (
	"bytes"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/mhersson/cavu/internal/auth"
	"github.com/mhersson/cavu/internal/engine"
	"github.com/mhersson/cavu/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type configFixture struct {
	task    store.Task
	run     store.Run
	dataDir string
	caPath  string
}

type configCase struct {
	name      string
	set       map[string]string // settings over the base
	unset     []string          // settings and secrets left out
	overrides map[string]string
	errHas    string
	check     func(t *testing.T, got engine.RunConfig, fx configFixture)
}

func TestBuildRunConfig(t *testing.T) {
	tests := []configCase{
		{
			name: "settings, project and secrets map to the fields",
			check: func(t *testing.T, got engine.RunConfig, fx configFixture) {
				var want engine.RunConfig

				want.TaskID, want.RunID, want.ProjectID = fx.task.ID, fx.run.ID, fx.task.ProjectID
				want.InstanceID, want.Image = "inst-1", "cavu-sandbox:test"
				want.Repo.URL, want.Repo.BaseBranch, want.Repo.BaseCommit = "https://example.com/web.git", "main", "abc123"
				want.Repo.Branch, want.Repo.Token = "cavu/fix-the-login-page", "ghp_forgetokenvalue"
				want.Setup.Command = "make setup"
				want.Setup.Env = map[string]string{"CI": "1"}
				want.Setup.Secrets = map[string]string{"NPM_AUTH": "npm-auth-value"}
				want.Setup.ToolOverrides = map[string]string{"go": "1.27"}
				want.Setup.EgressHosts = []string{"proxy.golang.org"}
				want.Verify.Command, want.Verify.Timeout = "make test", 150*time.Second
				want.Verify.BaselineCommit, want.Verify.BaselinePassed = "base999", true
				want.Endpoint.Mode, want.Endpoint.URL = "key", "https://llm.example.com"
				want.Endpoint.AuthHeader, want.Endpoint.Credential = "x-api-key", "sk-credential-value"
				want.Agent.Model, want.Agent.Effort = "model-a", "high"
				want.Agent.MaxTurns, want.Agent.Timeout, want.Agent.WebTools = 50, 30*time.Minute, true
				want.Identity.Name, want.Identity.Email = "CAVU Bot", "bot@example.com"
				want.CABundle = []byte("-----BEGIN CERTIFICATE-----\n")
				want.Task.Title, want.Task.Body = "Fix the login page", "It fails on submit."
				want.ArtifactDir = filepath.Join(fx.dataDir, "artifacts",
					strconv.FormatInt(fx.task.ID, 10), strconv.FormatInt(fx.run.ID, 10))

				assert.Equal(t, want, got)
			},
		},
		{
			name: "optional settings and secrets take their defaults",
			unset: []string{
				"endpoint_url", "endpoint_auth_header", "effort_work", "work_max_turns", "work_timeout_minutes",
				"verify_timeout_minutes", "ca_bundle_path", "forge_token", "endpoint_credential",
			},
			check: func(t *testing.T, got engine.RunConfig, _ configFixture) {
				assert.Empty(t, got.Endpoint.URL)
				assert.Empty(t, got.Endpoint.AuthHeader)
				assert.Empty(t, got.Agent.Effort)
				assert.Equal(t, 200, got.Agent.MaxTurns)
				assert.Equal(t, 60*time.Minute, got.Agent.Timeout)
				assert.Equal(t, 10*time.Minute, got.Verify.Timeout)
				assert.Nil(t, got.CABundle)
				assert.Empty(t, got.Repo.Token)
				assert.Empty(t, got.Endpoint.Credential)
			},
		},
		{name: "model_work is required", unset: []string{"model_work"}, errHas: "model_work"},
		{name: "endpoint_mode is required", unset: []string{"endpoint_mode"}, errHas: "endpoint_mode"},
		{name: "commit_name is required", unset: []string{"commit_name"}, errHas: "commit_name"},
		{name: "commit_email is required", unset: []string{"commit_email"}, errHas: "commit_email"},
		{name: "instance_id is required", unset: []string{"instance_id"}, errHas: "instance_id"},
		{name: "an unknown endpoint mode", set: map[string]string{"endpoint_mode": "both"}, errHas: "endpoint_mode"},
		{name: "an unknown auth header", set: map[string]string{"endpoint_auth_header": "basic"}, errHas: "endpoint_auth_header"},
		{name: "turns that do not parse", set: map[string]string{"work_max_turns": "many"}, errHas: "work_max_turns"},
		{name: "zero turns", set: map[string]string{"work_max_turns": "0"}, errHas: "work_max_turns"},
		{name: "a negative work timeout", set: map[string]string{"work_timeout_minutes": "-5"}, errHas: "work_timeout_minutes"},
		{name: "a verify timeout that does not parse", set: map[string]string{"verify_timeout_minutes": "soon"}, errHas: "verify_timeout_minutes"},
		{name: "a verify timeout that is not a number", set: map[string]string{"verify_timeout_minutes": "NaN"}, errHas: "verify_timeout_minutes"},
		{name: "a CA bundle that cannot be read", set: map[string]string{"ca_bundle_path": "/nonexistent/ca.pem"}, errHas: "ca_bundle_path"},
		{name: "a project secret with no stored value", unset: []string{"project.web.NPM_AUTH"}, errHas: "project.web.NPM_AUTH"},
		{
			name: "a long verify timeout is clamped to 120 minutes",
			set:  map[string]string{"verify_timeout_minutes": "500"},
			check: func(t *testing.T, got engine.RunConfig, _ configFixture) {
				assert.Equal(t, 120*time.Minute, got.Verify.Timeout)
			},
		},
		{
			name: "a short verify timeout is clamped to half a minute",
			set:  map[string]string{"verify_timeout_minutes": "0.1"},
			check: func(t *testing.T, got engine.RunConfig, _ configFixture) {
				assert.Equal(t, 30*time.Second, got.Verify.Timeout)
			},
		},
		{
			name:      "overrides replace the model, the effort and the switches",
			overrides: map[string]string{"model_work": "model-b", "effort_work": "low", "web_tools": "false", "verify_fix": "false"},
			check: func(t *testing.T, got engine.RunConfig, _ configFixture) {
				assert.Equal(t, "model-b", got.Agent.Model)
				assert.Equal(t, "low", got.Agent.Effort)
				assert.False(t, got.Agent.WebTools)
			},
		},
		{
			name:      "an override supplies a model the settings lack",
			unset:     []string{"model_work"},
			overrides: map[string]string{"model_work": "model-b"},
			check: func(t *testing.T, got engine.RunConfig, _ configFixture) {
				assert.Equal(t, "model-b", got.Agent.Model)
			},
		},
		{name: "an unknown override", overrides: map[string]string{"max_turns": "5"}, errHas: "max_turns"},
		{name: "a switch override that does not parse", overrides: map[string]string{"web_tools": "maybe"}, errHas: "web_tools"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			loop, fx := configFixtureFor(t, tt)

			got, err := loop.BuildRunConfig(t.Context(), fx.task, fx.run, tt.overrides)
			if tt.errHas != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.errHas)

				return
			}

			require.NoError(t, err)
			tt.check(t, got, fx)
		})
	}
}

func configFixtureFor(t *testing.T, tt configCase) (*engine.Loop, configFixture) {
	t.Helper()

	ctx := t.Context()
	dir := t.TempDir()

	s, err := store.Open(ctx, filepath.Join(dir, "cavu.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	v, err := auth.NewVault(s, bytes.Repeat([]byte{9}, auth.MasterKeyLen))
	require.NoError(t, err)

	fx := configFixture{dataDir: filepath.Join(dir, "data"), caPath: filepath.Join(dir, "ca.pem")}
	require.NoError(t, os.WriteFile(fx.caPath, []byte("-----BEGIN CERTIFICATE-----\n"), 0o600))

	settings := map[string]string{
		"instance_id": "inst-1", "endpoint_mode": "key", "endpoint_url": "https://llm.example.com",
		"endpoint_auth_header": "x-api-key", "model_work": "model-a", "effort_work": "high",
		"work_timeout_minutes": "30", "work_max_turns": "50", "verify_timeout_minutes": "2.5",
		"ca_bundle_path": fx.caPath, "commit_name": "CAVU Bot", "commit_email": "bot@example.com",
	}
	sealed := [][2]string{
		{"endpoint_credential", "sk-credential-value"},
		{"forge_token", "ghp_forgetokenvalue"},
		{"project.web.NPM_AUTH", "npm-auth-value"},
	}

	maps.Copy(settings, tt.set)

	for _, k := range tt.unset {
		delete(settings, k)
	}

	for k, val := range settings {
		require.NoError(t, s.SetSetting(ctx, k, val))
	}

	for _, kv := range sealed {
		if !slices.Contains(tt.unset, kv[0]) {
			require.NoError(t, v.Set(ctx, kv[0], kv[1]))
		}
	}

	p, err := s.CreateProject(ctx, store.Project{
		Name: "web", RepoURL: "https://example.com/web.git", BaseBranch: "main",
		SetupCommand: "make setup", VerifyCommand: "make test",
		ToolOverrides: map[string]string{"go": "1.27"}, EgressHosts: []string{"proxy.golang.org"},
		Env: map[string]string{"CI": "1"}, SecretVars: []string{"NPM_AUTH"},
		Switches: map[string]bool{"web_tools": true},
		Baseline: &store.Baseline{Commit: "base999", Passed: true, At: now},
	})
	require.NoError(t, err)

	fx.task, err = s.CreateTask(ctx, store.Task{
		ProjectID: p.ID, Title: "Fix the login page", Body: "It fails on submit.", BaseCommit: "abc123",
	})
	require.NoError(t, err)

	fx.run, err = s.CreateRun(ctx, store.Run{TaskID: fx.task.ID, Reason: "initial"})
	require.NoError(t, err)

	return &engine.Loop{Store: s, Vault: v, Runner: &fakeRunner{}, Now: func() time.Time { return now }, DataDir: fx.dataDir, Image: "cavu-sandbox:test"}, fx
}
