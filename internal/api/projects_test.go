package api_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/mhersson/cavu/internal/engine"
	"github.com/mhersson/cavu/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const web = `{"name": "web", "repo_url": "https://example.com/web.git", "verify_command": "make test"}`

func TestProjectsRoundTrip(t *testing.T) {
	f := newServer(t)

	status, created := f.call(t, "POST /api/projects", web)
	require.Equal(t, http.StatusCreated, status, created)

	switches, err := json.Marshal(engine.ProjectSwitches(store.Project{}))
	require.NoError(t, err)
	assert.JSONEq(t, fmt.Sprintf(`{"id": 1, "name": "web", "repo_url": "https://example.com/web.git", "base_branch": "main",
		"setup_command": "", "verify_command": "make test", "verify_waived": false,
		"tool_overrides": {}, "egress_hosts": [], "env": {}, "secret_vars": [], "switches": %s,
		"runnable": false, "baseline": null,
		"check": {"requested_at": null, "tools": [], "ignored_tools": [], "failed_step": "", "verify_suggestion": "",
			"fault": "", "commit": "", "output": ""}}`, switches), created)

	status, body := f.call(t, "GET /api/projects", "")
	require.Equal(t, http.StatusOK, status, body)
	assert.JSONEq(t, `{"projects": [`+created+`]}`, body)

	status, body = f.call(t, "GET /api/projects/1", "")
	require.Equal(t, http.StatusOK, status, body)
	assert.JSONEq(t, created, body)

	p, err := f.store.Project(t.Context(), 1)
	require.NoError(t, err)

	p.Baseline = &store.Baseline{Commit: "abc123", Passed: true, At: f.clock.Now()}
	require.NoError(t, f.store.UpdateProject(t.Context(), p))

	_, body = f.call(t, "GET /api/projects/1", "")
	before := object(t, body)
	assert.Equal(t, map[string]any{"commit": "abc123", "passed": true, "at": "2030-01-02T03:04:05Z", "output": ""}, before["baseline"])
	assert.Equal(t, true, before["runnable"])

	t.Run("a PATCH of one field leaves the others", func(t *testing.T) {
		status, body := f.call(t, "PATCH /api/projects/1", `{"egress_hosts": ["registry.npmjs.org"]}`)
		require.Equal(t, http.StatusOK, status, body)

		before["egress_hosts"] = []any{"registry.npmjs.org"}
		assert.Equal(t, before, object(t, body))
	})

	t.Run("switches merge per key", func(t *testing.T) {
		for _, patch := range []string{`{"switches": {"web_tools": true}}`, `{"switches": {"verify_fix": false}}`} {
			status, body := f.call(t, "PATCH /api/projects/1", patch)
			require.Equal(t, http.StatusOK, status, body)
		}

		p, err := f.store.Project(t.Context(), 1)
		require.NoError(t, err)
		assert.Equal(t, map[string]bool{"web_tools": true, "verify_fix": false}, p.Switches)
	})

	t.Run("a PATCH of the verify command clears the baseline", func(t *testing.T) {
		status, body := f.call(t, "PATCH /api/projects/1", `{"verify_command": "make check"}`)
		require.Equal(t, http.StatusOK, status, body)

		got := object(t, body)
		assert.Equal(t, "make check", got["verify_command"])
		assert.Nil(t, got["baseline"])
		assert.Equal(t, false, got["runnable"])
	})

	t.Run("refused", func(t *testing.T) {
		stored, err := f.store.Project(t.Context(), 1)
		require.NoError(t, err)

		for _, tc := range []struct {
			name, route, body string
			want              int
			field             string // the field the error names
		}{
			{"a duplicate name", "POST /api/projects", web, http.StatusConflict, ""},
			{"a repository that is not https", "POST /api/projects", `{"name": "api", "repo_url": "http://example.com/api.git"}`, http.StatusBadRequest, "repo_url"},
			{"a repository that is not https, edited", "PATCH /api/projects/1", `{"repo_url": "http://example.com/web.git"}`, http.StatusBadRequest, "repo_url"},
			{"a rename", "PATCH /api/projects/1", `{"name": "site"}`, http.StatusBadRequest, ""},
			{"an unknown id", "GET /api/projects/99", "", http.StatusNotFound, ""},
			{"an unknown id, edited", "PATCH /api/projects/99", `{}`, http.StatusNotFound, ""},
			{"an unknown id, checked", "POST /api/projects/99/check", "", http.StatusNotFound, ""},
			{"an id that is not a number", "GET /api/projects/web", "", http.StatusNotFound, ""},
		} {
			t.Run(tc.name, func(t *testing.T) {
				status, body := f.call(t, tc.route, tc.body)
				require.Equal(t, tc.want, status, body)
				assertError(t, body)

				if tc.field != "" {
					assert.Contains(t, object(t, body)["error"], tc.field)
				}

				ps, err := f.store.Projects(t.Context())
				require.NoError(t, err)
				assert.Equal(t, []store.Project{stored}, ps, "a refused request writes nothing")
			})
		}
	})
}

func TestProjectSecretsAreWriteOnly(t *testing.T) {
	const secret = "npm_s3cr3t-value"

	f := newServer(t)

	vault := func(t *testing.T, name string) (string, bool) {
		t.Helper()

		v, ok, err := f.vault.Get(t.Context(), name)
		require.NoError(t, err)

		return v, ok
	}

	status, body := f.call(t, "POST /api/projects", `{"name": "web", "repo_url": "https://example.com/web.git",
		"secrets": {"NPM_TOKEN": "`+secret+`"}}`)
	require.Equal(t, http.StatusCreated, status, body)
	assert.Equal(t, []any{"NPM_TOKEN"}, object(t, body)["secret_vars"])

	got, ok := vault(t, "project.web.NPM_TOKEN")
	assert.True(t, ok)
	assert.Equal(t, secret, got)

	status, body = f.call(t, "PATCH /api/projects/1", `{"secrets": {"GH_TOKEN": "gh-`+secret+`"}}`)
	require.Equal(t, http.StatusOK, status, body)
	assert.Equal(t, []any{"NPM_TOKEN", "GH_TOKEN"}, object(t, body)["secret_vars"])

	got, ok = vault(t, "project.web.GH_TOKEN")
	assert.True(t, ok)
	assert.Equal(t, "gh-"+secret, got)

	for _, patch := range []string{
		`{"repo_url": "http://example.com/web.git", "secrets": {"OTHER": "o-` + secret + `"}}`,
		`{"secrets": {"1OTHER": "o-` + secret + `"}}`,
	} {
		status, body = f.call(t, "PATCH /api/projects/1", patch)
		require.Equal(t, http.StatusBadRequest, status, body)
	}

	names, err := f.store.SecretNames(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []string{"project.web.GH_TOKEN", "project.web.NPM_TOKEN"}, names, "a refused request seals nothing")

	p, err := f.store.Project(t.Context(), 1)
	require.NoError(t, err)

	p.Baseline = &store.Baseline{Commit: "abc123", Passed: true, At: f.clock.Now()}
	require.NoError(t, f.store.UpdateProject(t.Context(), p))

	status, body = f.call(t, "PATCH /api/projects/1", `{"secrets": {"NPM_TOKEN": "new-`+secret+`"}}`)
	require.Equal(t, http.StatusOK, status, body)

	rotated := object(t, body)
	assert.Equal(t, []any{"NPM_TOKEN", "GH_TOKEN"}, rotated["secret_vars"])
	assert.NotNil(t, rotated["baseline"], "a new value for a variable keeps the baseline")

	got, _ = vault(t, "project.web.NPM_TOKEN")
	assert.Equal(t, "new-"+secret, got)

	status, body = f.call(t, "PATCH /api/projects/1", `{"secrets": {"NPM_TOKEN": ""}}`)
	require.Equal(t, http.StatusOK, status, body)
	assert.Equal(t, []any{"GH_TOKEN"}, object(t, body)["secret_vars"])

	_, ok = vault(t, "project.web.NPM_TOKEN")
	assert.False(t, ok)

	f.call(t, "GET /api/projects/1", "")
	f.call(t, "GET /api/projects", "")

	for _, b := range f.bodies {
		assert.NotContains(t, b, secret)
	}
}

func TestCheckIsRequested(t *testing.T) {
	f := newServer(t)

	status, body := f.call(t, "POST /api/projects", web)
	require.Equal(t, http.StatusCreated, status, body)

	status, body = f.call(t, "POST /api/projects/1/check", "")
	require.Equal(t, http.StatusAccepted, status, body)

	check, ok := object(t, body)["check"].(map[string]any)
	require.True(t, ok, body)
	assert.Equal(t, "2030-01-02T03:04:05Z", check["requested_at"])
	assert.Equal(t, int32(1), f.nudges.Load())

	p, err := f.store.Project(t.Context(), 1)
	require.NoError(t, err)
	require.NotNil(t, p.CheckRequestedAt)
	assert.True(t, p.CheckRequestedAt.Equal(f.clock.Now()))
}
