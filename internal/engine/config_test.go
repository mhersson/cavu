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
	task store.Task
	run  store.Run
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
				want.Repo.Branch, want.Repo.Access.Token = "cavu/fix-the-login-page", "ghp_forgetokenvalue"
				want.Repo.IgnoredChecks, want.Repo.ReviewBots = []string{"lint"}, []string{"copilot[bot]"}
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
				want.Agent.Review.Model, want.Agent.Review.Effort = "model-a", "high"
				want.Agent.Strong.Model, want.Agent.Strong.Effort = "model-a", "high"
				want.Agent.WebTools = true
				want.Identity.Name, want.Identity.Email = "CAVU Bot", "bot@example.com"
				want.Task.Title, want.Task.Body = "Fix the login page", "It fails on submit."

				assert.Equal(t, want, got)
			},
		},
		{
			name: "optional settings and secrets take their defaults",
			set:  map[string]string{"endpoint_mode": "subscription"},
			unset: []string{
				"endpoint_url", "endpoint_auth_header", "effort_work", "task_max_turns", "task_timeout_minutes",
				"verify_timeout_minutes", "forge_token", "endpoint_credential",
			},
			check: func(t *testing.T, got engine.RunConfig, _ configFixture) {
				assert.Empty(t, got.Endpoint.URL)
				assert.Empty(t, got.Endpoint.AuthHeader)
				assert.Empty(t, got.Agent.Effort)
				assert.Equal(t, 10*time.Minute, got.Verify.Timeout)
				assert.Equal(t, engine.ForgeAccess{}, got.Repo.Access, "the App's key is read only with an App id")
				assert.Empty(t, got.Endpoint.Credential)
			},
		},
		{name: "model_work is required", unset: []string{"model_work"}, errHas: "model_work"},
		{name: "endpoint_mode is required", unset: []string{"endpoint_mode"}, errHas: "endpoint_mode"},
		{name: "commit_name is required", unset: []string{"commit_name"}, errHas: "commit_name"},
		{name: "commit_email is required", unset: []string{"commit_email"}, errHas: "commit_email"},
		{name: "instance_id is required", unset: []string{"instance_id"}, errHas: "instance_id"},
		{name: "endpoint_url is required in key mode", unset: []string{"endpoint_url"}, errHas: "endpoint_url"},
		{name: "endpoint_auth_header is required in key mode", unset: []string{"endpoint_auth_header"}, errHas: "endpoint_auth_header"},
		{name: "an unknown endpoint mode", set: map[string]string{"endpoint_mode": "both"}, errHas: "endpoint_mode"},
		{name: "an unknown auth header", set: map[string]string{"endpoint_auth_header": "basic"}, errHas: "endpoint_auth_header"},
		{name: "turns that do not parse", set: map[string]string{"task_max_turns": "many"}, errHas: "task_max_turns"},
		{name: "zero turns", set: map[string]string{"task_max_turns": "0"}, errHas: "task_max_turns"},
		{name: "a negative task timeout", set: map[string]string{"task_timeout_minutes": "-5"}, errHas: "task_timeout_minutes"},
		{name: "a verify timeout that does not parse", set: map[string]string{"verify_timeout_minutes": "soon"}, errHas: "verify_timeout_minutes"},
		{name: "a verify timeout that is not a number", set: map[string]string{"verify_timeout_minutes": "NaN"}, errHas: "verify_timeout_minutes"},
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
			overrides: map[string]string{"model_work": "model-b", "effort_work": "low", "web_tools": "false", "verify_fix": "false", "review": "true"},
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

	runConfigCases(t, tests)
}

func TestBuildRunConfigRoles(t *testing.T) {
	// roles is the model and effort of the review role, then of the strong role.
	roles := func(c engine.RunConfig) [4]string {
		return [4]string{c.Agent.Review.Model, c.Agent.Review.Effort, c.Agent.Strong.Model, c.Agent.Strong.Effort}
	}

	runConfigCases(t, []configCase{
		{
			name: "unset review and strong settings take the work values",
			check: func(t *testing.T, got engine.RunConfig, _ configFixture) {
				assert.Equal(t, [4]string{"model-a", "high", "model-a", "high"}, roles(got))
			},
		},
		{
			name: "set review and strong settings are used",
			set:  map[string]string{"model_review": "model-r", "effort_review": "low", "model_strong": "model-s", "effort_strong": "max"},
			check: func(t *testing.T, got engine.RunConfig, _ configFixture) {
				assert.Equal(t, [4]string{"model-r", "low", "model-s", "max"}, roles(got))
				assert.Equal(t, "model-a", got.Agent.Model)
				assert.Equal(t, "high", got.Agent.Effort)
			},
		},
		{
			name: "each unset value falls back on its own",
			set:  map[string]string{"model_strong": "model-s", "effort_review": "low"},
			check: func(t *testing.T, got engine.RunConfig, _ configFixture) {
				assert.Equal(t, [4]string{"model-a", "low", "model-s", "high"}, roles(got))
			},
		},
		{
			name:      "a replay's work model is what unset roles fall back on",
			overrides: map[string]string{"model_work": "model-b", "effort_work": "low"},
			check: func(t *testing.T, got engine.RunConfig, _ configFixture) {
				assert.Equal(t, [4]string{"model-b", "low", "model-b", "low"}, roles(got))
			},
		},
	})
}

func TestRunConfigCarriesTheAccess(t *testing.T) {
	runConfigCases(t, []configCase{
		{
			name: "a token alone",
			check: func(t *testing.T, got engine.RunConfig, _ configFixture) {
				assert.Equal(t, engine.ForgeAccess{Token: "ghp_forgetokenvalue"}, got.Repo.Access) //nolint:gosec // a dummy
			},
		},
		{
			name:  "an App id with its key",
			set:   map[string]string{"forge_app_id": "123456"},
			unset: []string{"forge_token"},
			check: func(t *testing.T, got engine.RunConfig, _ configFixture) {
				assert.Equal(t, engine.ForgeAccess{AppID: 123456, AppKey: "app-key-value"}, got.Repo.Access)
			},
		},
		{name: "an App id without its key", set: map[string]string{"forge_app_id": "123456"}, unset: []string{"forge_app_key"}, errHas: "forge_app_key"},
		{name: "an App id that does not parse", set: map[string]string{"forge_app_id": "abc"}, errHas: "forge_app_id"},
	})
}

func TestRunUsesTheProjectsCredential(t *testing.T) {
	setup := func(t *testing.T, tt configCase, creds map[string]*engine.NewCredential, name string) (engine.RunConfig, error) {
		t.Helper()

		loop, fx := configFixtureFor(t, tt)
		require.NoError(t, engine.ChangeCredentials(t.Context(), loop.Store, loop.Vault, creds))

		if name != "" {
			p, err := loop.Store.Project(t.Context(), fx.task.ProjectID)
			require.NoError(t, err)

			p.ForgeCredential = name
			require.NoError(t, engine.EditProject(t.Context(), loop.Store, p))
		}

		return loop.BuildRunConfig(t.Context(), fx.task, fx.run, nil)
	}

	creds := map[string]*engine.NewCredential{"bot": {Token: "ghp_bot"}, "team": {AppID: 42, AppKey: "pem-team"}}

	t.Run("a token credential", func(t *testing.T) {
		got, err := setup(t, configCase{set: map[string]string{"forge_app_id": "123456"}}, creds, "bot")
		require.NoError(t, err)
		assert.Equal(t, engine.ForgeAccess{Token: "ghp_bot"}, got.Repo.Access, "no App id, no default token")
	})

	t.Run("an App credential", func(t *testing.T) {
		got, err := setup(t, configCase{}, creds, "team")
		require.NoError(t, err)
		assert.Equal(t, engine.ForgeAccess{AppID: 42, AppKey: "pem-team"}, got.Repo.Access)
	})

	t.Run("none names today's", func(t *testing.T) {
		got, err := setup(t, configCase{}, creds, "")
		require.NoError(t, err)
		assert.Equal(t, engine.ForgeAccess{Token: "ghp_forgetokenvalue"}, got.Repo.Access) //nolint:gosec // a dummy

		got, err = setup(t, configCase{set: map[string]string{"forge_app_id": "123456"}, unset: []string{"forge_token"}}, creds, "")
		require.NoError(t, err)
		assert.Equal(t, engine.ForgeAccess{AppID: 123456, AppKey: "app-key-value"}, got.Repo.Access)
	})

	t.Run("a missing secret fails and does not fall back", func(t *testing.T) {
		loop, fx := configFixtureFor(t, configCase{})
		require.NoError(t, engine.ChangeCredentials(t.Context(), loop.Store, loop.Vault, creds))

		p, err := loop.Store.Project(t.Context(), fx.task.ProjectID)
		require.NoError(t, err)

		p.ForgeCredential = "bot"
		require.NoError(t, engine.EditProject(t.Context(), loop.Store, p))
		require.NoError(t, loop.Vault.Delete(t.Context(), engine.CredentialSecretName("bot", engine.CredentialToken)))

		_, err = loop.BuildRunConfig(t.Context(), fx.task, fx.run, nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), engine.CredentialSecretName("bot", engine.CredentialToken))
	})
}

func runConfigCases(t *testing.T, tests []configCase) {
	t.Helper()

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

func TestReadSettingsNamesEveryMissingKey(t *testing.T) {
	_, err := engine.ReadSettings(map[string]string{"endpoint_mode": "key", "task_max_turns": "many"})
	require.Error(t, err)

	for _, key := range []string{"instance_id", "model_work", "commit_name", "commit_email", "endpoint_url", "endpoint_auth_header"} {
		assert.Contains(t, err.Error(), key)
	}

	assert.NotContains(t, err.Error(), "task_max_turns", "a key not set comes before a value that does not parse")
}

func TestCheckSetting(t *testing.T) {
	tests := []struct {
		key, value string
		ok         bool
	}{
		{"endpoint_mode", "subscription", true},
		{"endpoint_mode", "key", true},
		{"endpoint_mode", "both", false},
		{"endpoint_auth_header", "x-api-key", true},
		{"endpoint_auth_header", "basic", false},
		{"task_max_turns", "50", true},
		{"task_max_turns", "0", false},
		{"task_timeout_minutes", "soon", false},
		{"work_max_turns", "50", false},
		{"verify_timeout_minutes", "2.5", true},
		{"verify_timeout_minutes", "soon", false},
		{"endpoint_mode", "", true},
		{"model_work", "any-model", true},
		{"model_review", "any-model", true},
		{"effort_review", "low", true},
		{"model_strong", "any-model", true},
		{"effort_strong", "", true},
		{"runtime", "podman", true},
		{"runtime", "docker", true},
		{"runtime", "", true},
		{"runtime", "dokcer", false},
		{"ca_bundle_path", "/etc/ca.pem", true},
		{"commit_email", "bot@example.com", true},
		{"instance_id", "0123456789abcdef", false},
		{"max_turns", "5", false},
		{"task_budget_usd", "5", true},
		{"task_budget_usd", "2.50", true},
		{"task_budget_usd", "0", true},
		{"task_budget_usd", "", true},
		{"task_budget_usd", "-1", false},
		{"task_budget_usd", "five", false},
		{"daily_budget_usd", "20", true},
		{"daily_budget_usd", "-0.01", false},
		{"model_prices", `{"*": {"input": 3, "output": 15, "cache_read": 0.3, "cache_write": 3.75}}`, true},
		{"model_prices", "", true},
		{"model_prices", "[]", false},
		{"model_prices", `{"*": {"input": 3, "output": 15}}`, false},
	}

	for _, tt := range tests {
		t.Run(tt.key+"="+tt.value, func(t *testing.T) {
			err := engine.CheckSetting(tt.key, tt.value)
			if tt.ok {
				require.NoError(t, err)

				return
			}

			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.key)
		})
	}
}

func TestCheckSettingForge(t *testing.T) {
	tests := []struct {
		key, value string
		ok         bool
	}{
		{"forge_app_id", "123456", true},
		{"forge_app_id", "", true},
		{"forge_app_id", "abc", false},
		{"forge_app_id", "0", false},
		{"ci_timeout_minutes", "90", true},
		{"ci_timeout_minutes", "0", true},
		{"ci_timeout_minutes", "", true},
		{"ci_timeout_minutes", "-1", false},
		{"ci_timeout_minutes", "1.5", false},
	}

	for _, tt := range tests {
		t.Run(tt.key+"="+tt.value, func(t *testing.T) {
			err := engine.CheckSetting(tt.key, tt.value)
			if tt.ok {
				require.NoError(t, err)

				return
			}

			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.key)
		})
	}

	for value, want := range map[string]time.Duration{"": 120 * time.Minute, "0": 0, "90": 90 * time.Minute} {
		got, err := engine.CITimeout(map[string]string{"ci_timeout_minutes": value})
		require.NoError(t, err)
		assert.Equal(t, want, got, value)
	}
}

func TestProjectSwitchesDefaults(t *testing.T) {
	assert.Equal(t, map[string]bool{
		"continuations": true, "verify_fix": true, "web_tools": false, "readiness": true, "review": true, "publish": true,
	}, engine.ProjectSwitches(store.Project{}))
}

func TestSettingDefaultsValues(t *testing.T) {
	defaults := engine.SettingDefaults()
	assert.ElementsMatch(t, engine.SettableKeys(), slices.Collect(maps.Keys(defaults)))

	want := map[string]string{
		"concurrency": "3", "ci_timeout_minutes": "120", "listen_address": "127.0.0.1:7117",
		"verify_timeout_minutes": "10", "task_max_turns": "1000", "task_timeout_minutes": "300",
	}
	for k, v := range defaults {
		assert.Equal(t, want[k], v, k)
	}
}

// A default served is what a run reads with the key unset.
func TestADefaultIsWhatARunReads(t *testing.T) {
	defaults := engine.SettingDefaults()

	concurrency, err := engine.Concurrency(map[string]string{})
	require.NoError(t, err)
	assert.Equal(t, strconv.Itoa(concurrency), defaults["concurrency"])

	cfg, err := engine.ReadSettings(map[string]string{
		"instance_id": "i", "endpoint_mode": "subscription", "model_work": "m", "commit_name": "n", "commit_email": "e",
	})
	require.NoError(t, err)
	assert.Equal(t, strconv.Itoa(int(cfg.Verify.Timeout/time.Minute)), defaults["verify_timeout_minutes"])

	h := newHarness(t)
	s, err := h.loop.Snapshot(t.Context(), h.task.ID)
	require.NoError(t, err)
	assert.Equal(t, strconv.Itoa(s.Limits.Turns), defaults["task_max_turns"])
	assert.Equal(t, strconv.Itoa(int(s.Limits.SessionTime/time.Minute)), defaults["task_timeout_minutes"])
}

func TestSettingsLimit(t *testing.T) {
	assert.LessOrEqual(t, len(engine.SettableKeys())+1, 30, "the settable keys and instance_id")
}

func TestServerSettings(t *testing.T) {
	tests := []struct {
		key, value string
		ok         bool
	}{
		{"concurrency", "1", true},
		{"concurrency", "12", true},
		{"concurrency", "", true},
		{"concurrency", "0", false},
		{"concurrency", "-1", false},
		{"concurrency", "x", false},
		{"listen_address", "127.0.0.1:7117", true},
		{"listen_address", "0.0.0.0:8080", true},
		{"listen_address", "[::1]:7117", true},
		{"listen_address", "localhost:7117", true},
		{"listen_address", "", true},
		{"listen_address", "127.0.0.1", false},
		{"listen_address", "localhost", false},
		{"listen_address", "127.0.0.1:", false},
		{"listen_address", ":7117", false},
	}

	for _, tt := range tests {
		t.Run(tt.key+"="+tt.value, func(t *testing.T) {
			err := engine.CheckSetting(tt.key, tt.value)
			if tt.ok {
				require.NoError(t, err)

				return
			}

			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.key)
		})
	}

	n, err := engine.Concurrency(map[string]string{})
	require.NoError(t, err)
	assert.Equal(t, 3, n)

	n, err = engine.Concurrency(map[string]string{"concurrency": "5"})
	require.NoError(t, err)
	assert.Equal(t, 5, n)

	addr, err := engine.ListenAddress(map[string]string{})
	require.NoError(t, err)
	assert.Equal(t, "127.0.0.1:7117", addr)

	addr, err = engine.ListenAddress(map[string]string{"listen_address": "0.0.0.0:8080"})
	require.NoError(t, err)
	assert.Equal(t, "0.0.0.0:8080", addr)
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

	var fx configFixture

	settings := map[string]string{
		"instance_id": "inst-1", "endpoint_mode": "key", "endpoint_url": "https://llm.example.com",
		"endpoint_auth_header": "x-api-key", "model_work": "model-a", "effort_work": "high",
		"task_timeout_minutes": "30", "task_max_turns": "50", "verify_timeout_minutes": "2.5",
		"commit_name": "CAVU Bot", "commit_email": "bot@example.com",
	}
	sealed := [][2]string{
		{"endpoint_credential", "sk-credential-value"},
		{"forge_token", "ghp_forgetokenvalue"},
		{"forge_app_key", "app-key-value"},
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
		Forge:    store.ForgePolicy{NoCI: true, IgnoredChecks: []string{"lint"}, ReviewBots: []string{"copilot[bot]"}},
	})
	require.NoError(t, err)

	fx.task, err = s.CreateTask(ctx, store.Task{
		ProjectID: p.ID, Title: "Fix the login page", Body: "It fails on submit.", BaseCommit: "abc123",
	})
	require.NoError(t, err)

	fx.run, err = s.CreateRun(ctx, store.Run{TaskID: fx.task.ID, Reason: "initial"})
	require.NoError(t, err)

	return &engine.Loop{Store: s, Vault: v, Runner: &fakeRunner{}, Now: func() time.Time { return now }, Image: "cavu-sandbox:test"}, fx
}

// writeFile writes content at p with mode, making its directories.
func writeFile(t *testing.T, p, content string, mode os.FileMode) {
	t.Helper()

	require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
	require.NoError(t, os.WriteFile(p, []byte(content), mode))
	require.NoError(t, os.Chmod(p, mode))
}

// sparseFile makes a file of size at p without writing its bytes.
func sparseFile(t *testing.T, p string, size int64) {
	t.Helper()

	require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))

	f, err := os.Create(p)
	require.NoError(t, err)
	require.NoError(t, f.Truncate(size))
	require.NoError(t, f.Close())
}

func TestCheckSettingInstancePaths(t *testing.T) {
	root := t.TempDir()
	dir, file, missing := filepath.Join(root, "skills"), filepath.Join(root, "AGENTS.md"), filepath.Join(root, "missing")
	writeFile(t, filepath.Join(dir, "one", "SKILL.md"), "one\n", 0o644)
	writeFile(t, file, "instructions\n", 0o644)
	require.NoError(t, os.Symlink(dir, filepath.Join(root, "linked-skills")))
	require.NoError(t, os.Symlink(file, filepath.Join(root, "linked.md")))

	for _, tt := range []struct {
		key, value string
		ok         bool
	}{
		{"skills_path", dir, true},
		{"skills_path", filepath.Join(root, "linked-skills"), true},
		{"skills_path", "", true},
		{"skills_path", file, false},
		{"skills_path", missing, false},
		{"instructions_path", file, true},
		{"instructions_path", filepath.Join(root, "linked.md"), true},
		{"instructions_path", "", true},
		{"instructions_path", dir, false},
		{"instructions_path", missing, false},
	} {
		t.Run(tt.key+"="+tt.value, func(t *testing.T) {
			err := engine.CheckSetting(tt.key, tt.value)
			if tt.ok {
				require.NoError(t, err)

				return
			}

			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.key)
		})
	}
}

// Each action reads the task's images, in the order added, as it builds its
// config, so a session gets one kept while the prepare ran, after its claim.
func TestRunConfigCarriesImages(t *testing.T) {
	h := newHarness(t)
	h.withReview()
	h.putImage(h.task.ID, "one.png")
	h.runner.onCall = func(c call) {
		if c.method == "prepare" {
			h.putImage(h.task.ID, "two.png")
		}
	}

	_, err := h.run(t.Context())
	require.NoError(t, err)

	want := []engine.TaskImage{
		{Name: "one.png", Type: "image/png", Data: []byte("one.png")}, {Name: "two.png", Type: "image/png", Data: []byte("two.png")},
	}

	for _, method := range []string{"work", "review"} {
		calls := h.runner.callsOf(method)
		require.NotEmpty(t, calls, method)

		for _, c := range calls {
			assert.Equal(t, want, c.cfg.Task.Images, method)
		}
	}
}

func TestBuildRunConfigInstanceFiles(t *testing.T) {
	build := func(t *testing.T, set map[string]string) (engine.RunConfig, error) {
		t.Helper()

		loop, fx := configFixtureFor(t, configCase{set: set})

		return loop.BuildRunConfig(t.Context(), fx.task, fx.run, nil)
	}

	t.Run("the instruction file and the skills, links followed, are in the config", func(t *testing.T) {
		root := t.TempDir()
		instructions, skills, shared := filepath.Join(root, "AGENTS.md"), filepath.Join(root, "skills"), filepath.Join(root, "shared")
		writeFile(t, instructions, "Work in small commits.\n", 0o644)
		writeFile(t, filepath.Join(skills, "one", "SKILL.md"), "one\n", 0o644)
		writeFile(t, filepath.Join(skills, "one", "scripts", "run.sh"), "echo run\n", 0o755)
		writeFile(t, filepath.Join(shared, "two", "SKILL.md"), "two\n", 0o600)
		writeFile(t, filepath.Join(shared, "note.md"), "note\n", 0o644)
		require.NoError(t, os.Symlink(filepath.Join(shared, "two"), filepath.Join(skills, "two")))
		require.NoError(t, os.Symlink(filepath.Join(shared, "note.md"), filepath.Join(skills, "one", "note.md")))

		got, err := build(t, map[string]string{"instructions_path": instructions, "skills_path": skills})
		require.NoError(t, err)
		assert.Equal(t, "Work in small commits.\n", got.Agent.Instance.Instructions)
		assert.Equal(t, map[string]engine.File{
			"one/SKILL.md":       {Data: []byte("one\n")},
			"one/scripts/run.sh": {Data: []byte("echo run\n"), Executable: true},
			"one/note.md":        {Data: []byte("note\n")},
			"two/SKILL.md":       {Data: []byte("two\n")},
		}, got.Agent.Instance.Skills)
	})

	t.Run("a .git directory is left out at any depth", func(t *testing.T) {
		skills := t.TempDir()
		writeFile(t, filepath.Join(skills, "one", "SKILL.md"), "one\n", 0o644)
		writeFile(t, filepath.Join(skills, "one", ".git", "HEAD"), "ref: refs/heads/main\n", 0o644)
		sparseFile(t, filepath.Join(skills, ".git", "objects", "pack", "pack.pack"), 17<<20)

		got, err := build(t, map[string]string{"skills_path": skills})
		require.NoError(t, err)
		assert.Equal(t, map[string]engine.File{"one/SKILL.md": {Data: []byte("one\n")}}, got.Agent.Instance.Skills)
	})

	t.Run("unset settings give none", func(t *testing.T) {
		got, err := build(t, nil)
		require.NoError(t, err)
		assert.Zero(t, got.Agent.Instance)
	})

	for _, tc := range []struct {
		name, key string
		path      func(t *testing.T) string
	}{
		{"a skills directory that does not exist", "skills_path", func(t *testing.T) string { return filepath.Join(t.TempDir(), "missing") }},
		{"an instruction file that does not exist", "instructions_path", func(t *testing.T) string { return filepath.Join(t.TempDir(), "missing") }},
		{"a skills directory over the bound", "skills_path", func(t *testing.T) string {
			dir := t.TempDir()
			sparseFile(t, filepath.Join(dir, "one", "data.bin"), 10<<20)
			sparseFile(t, filepath.Join(dir, "two", "data.bin"), 7<<20)

			return dir
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := build(t, map[string]string{tc.key: tc.path(t)})
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.key)
		})
	}

	t.Run("a broken path stops the task with the setting named", func(t *testing.T) {
		h := newHarness(t)
		require.NoError(t, h.store.SetSetting(t.Context(), "skills_path", filepath.Join(t.TempDir(), "missing")))

		status, err := h.run(t.Context())
		require.NoError(t, err)
		assert.Equal(t, engine.StatusBlocked, status)
		assert.Empty(t, h.runner.trace())

		stop := h.lastStop()
		assert.Equal(t, engine.StopSandbox, stop.Reason)
		assert.Equal(t, engine.ClearRetry, stop.Action)
		assert.Contains(t, stop.Explanation, "skills_path")
	})

	t.Run("an environment check reads neither", func(t *testing.T) {
		h := newHarness(t)
		missing := filepath.Join(t.TempDir(), "missing")
		require.NoError(t, h.store.SetSetting(t.Context(), "skills_path", missing))
		require.NoError(t, h.store.SetSetting(t.Context(), "instructions_path", missing))

		_, _, err := h.loop.CheckProject(t.Context(), h.task.ProjectID)
		require.NoError(t, err)

		checks := h.runner.callsOf("check")
		require.Len(t, checks, 1)
		assert.Zero(t, checks[0].cfg.Agent.Instance)
	})
}
