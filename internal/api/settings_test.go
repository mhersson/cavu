package api_test

import (
	"encoding/json"
	"maps"
	"net/http"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/cavu/internal/engine"
)

const settingSwitches = `{"continuations": true, "verify_fix": true, "web_tools": false, "readiness": true, "review": true, "publish": true}`

// The parts every answer of GET /api/settings carries beside what is set.
const settingDefaults = `{"runtime": "", "ca_bundle_path": "", "endpoint_mode": "", "endpoint_url": "", "endpoint_auth_header": "",
	"model_work": "", "effort_work": "", "model_review": "", "effort_review": "", "model_strong": "", "effort_strong": "",
	"task_timeout_minutes": "300", "task_max_turns": "1000", "task_budget_usd": "", "daily_budget_usd": "", "model_prices": "",
	"verify_timeout_minutes": "10", "commit_name": "", "commit_email": "", "concurrency": "3", "listen_address": "127.0.0.1:7117",
	"skills_path": "", "instructions_path": "", "forge_app_id": "", "ci_timeout_minutes": "120"}`

func TestSettingsRoundTrip(t *testing.T) {
	const secret = "sealed-forge-token-value"

	f := newServer(t)
	require.NoError(t, f.store.SetSetting(t.Context(), "instance_id", "abc123"))

	settings := func(t *testing.T) string {
		t.Helper()

		status, body := f.call(t, "GET /api/settings", "")
		require.Equal(t, http.StatusOK, status, body)

		return body
	}

	assert.JSONEq(t, `{"settings": {}, "secrets": {"endpoint_credential": false, "forge_token": false, "forge_app_key": false},
		"defaults": `+settingDefaults+`, "switches": `+settingSwitches+`}`, settings(t),
		"a setting a person cannot set is not shown")

	status, body := f.call(t, "PATCH /api/settings", `{"settings": {"endpoint_mode": "subscription", "model_work": "sonnet",
		"concurrency": "4", "commit_name": {"b": 2, "a": [1, 2.50]}}, "secrets": {"forge_token": "`+secret+`"}}`)
	require.Equal(t, http.StatusOK, status, body)

	want := `{"settings": {"endpoint_mode": "subscription", "model_work": "sonnet", "concurrency": "4",
		"commit_name": {"b": 2, "a": [1, 2.50]}}, "secrets": {"endpoint_credential": false, "forge_token": true, "forge_app_key": false},
		"defaults": ` + settingDefaults + `, "switches": ` + settingSwitches + `}`
	assert.JSONEq(t, want, body)
	assert.JSONEq(t, want, settings(t))

	stored, err := f.store.Settings(t.Context())
	require.NoError(t, err)
	assert.Equal(t, map[string]string{
		"instance_id": "abc123", "endpoint_mode": "subscription", "model_work": "sonnet", "concurrency": "4",
		"commit_name": `{"b":2,"a":[1,2.50]}`,
	}, stored, "an object is stored as its text")

	got, ok, err := f.vault.Get(t.Context(), "forge_token")
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, secret, got)

	sealed, _, err := f.store.Secret(t.Context(), "forge_token")
	require.NoError(t, err)
	assert.NotContains(t, string(sealed), secret)

	for _, tc := range []struct{ name, body string }{
		{"one bad value among good ones", `{"settings": {"model_work": "opus", "concurrency": "zero"}}`},
		{"a key a person cannot set", `{"settings": {"model_work": "opus", "instance_id": "other"}}`},
		{"an unknown key", `{"settings": {"model_work": "opus", "no_such_key": "x"}}`},
		{"a number", `{"settings": {"model_work": "opus", "concurrency": 4}}`},
		{"a bool", `{"settings": {"model_work": "opus", "commit_name": true}}`},
		{"an array", `{"settings": {"model_work": ["opus"]}}`},
		{"a secret that is not the instance's", `{"settings": {"model_work": "opus"}, "secrets": {"project.web.NPM_TOKEN": "x"}}`},
		{"a secret beside a bad value", `{"settings": {"concurrency": "zero"}, "secrets": {"endpoint_credential": "sk-x"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, body := f.call(t, "PATCH /api/settings", tc.body)
			require.Equal(t, http.StatusBadRequest, status, body)
			assertError(t, body)
			assert.JSONEq(t, want, settings(t), "a refused request writes nothing")

			// GET never shows instance_id, so the store is read too.
			after, err := f.store.Settings(t.Context())
			require.NoError(t, err)
			assert.Equal(t, stored, after, "a refused request writes no setting")

			names, err := f.store.SecretNames(t.Context())
			require.NoError(t, err)
			assert.Equal(t, []string{"forge_token"}, names, "a refused request seals nothing")
		})
	}

	status, body = f.call(t, "PATCH /api/settings", `{"settings": {"model_work": null, "concurrency": ""},
		"secrets": {"forge_token": ""}}`)
	require.Equal(t, http.StatusOK, status, body)

	want = `{"settings": {"endpoint_mode": "subscription", "commit_name": {"b": 2, "a": [1, 2.50]}},
		"secrets": {"endpoint_credential": false, "forge_token": false, "forge_app_key": false},
		"defaults": ` + settingDefaults + `, "switches": ` + settingSwitches + `}`
	assert.JSONEq(t, want, body, "null and an empty value unset a key, and an empty secret removes it")
	assert.JSONEq(t, want, settings(t))

	_, ok, err = f.vault.Get(t.Context(), "forge_token")
	require.NoError(t, err)
	assert.False(t, ok)

	for _, b := range f.bodies {
		assert.NotContains(t, b, secret)
	}
}

// The App's key is an instance secret: sealed when given, shown as set, and
// removed with "", and refused only by the rules every request meets.
func TestSettingsTakeTheAppKey(t *testing.T) {
	const key = "sealed-app-key-value"

	f := newServer(t)

	secrets := func(t *testing.T, body string) any {
		t.Helper()

		return object(t, body)["secrets"]
	}

	status, body := f.call(t, "PATCH /api/settings", `{"settings": {"forge_app_id": "0"}, "secrets": {"forge_app_key": "x"}}`)
	require.Equal(t, http.StatusBadRequest, status, body)

	names, err := f.store.SecretNames(t.Context())
	require.NoError(t, err)
	assert.Empty(t, names, "a refused request seals nothing")

	status, body = f.call(t, "PATCH /api/settings", `{"settings": {"forge_app_id": "123456"},
		"secrets": {"forge_app_key": "`+key+`"}}`)
	require.Equal(t, http.StatusOK, status, body)
	assert.Equal(t, map[string]any{"endpoint_credential": false, "forge_token": false, "forge_app_key": true}, secrets(t, body))
	assert.Equal(t, "123456", object(t, body)["settings"].(map[string]any)["forge_app_id"])

	got, ok, err := f.vault.Get(t.Context(), engine.SecretForgeAppKey)
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, key, got)

	status, body = f.call(t, "PATCH /api/settings", `{"secrets": {"forge_app_key": ""}}`)
	require.Equal(t, http.StatusOK, status, body)
	assert.Equal(t, map[string]any{"endpoint_credential": false, "forge_token": false, "forge_app_key": false}, secrets(t, body))

	for _, b := range f.bodies {
		assert.NotContains(t, b, key)
	}
}

func TestModelPricesRoundTrip(t *testing.T) {
	const prices = `{"claude-sonnet-4-5-20250929": {"input": 3, "output": 15, "cache_read": 0.3, "cache_write": 3.75},
		"*": {"input": 15, "output": 75, "cache_read": 1.5, "cache_write": 18.75}}`

	f := newServer(t)

	shown := func(t *testing.T, body string) string {
		t.Helper()

		settings, ok := object(t, body)["settings"].(map[string]any)
		require.True(t, ok, body)

		b, err := json.Marshal(settings["model_prices"])
		require.NoError(t, err)

		return string(b)
	}

	status, body := f.call(t, "PATCH /api/settings", `{"settings": {"model_prices": `+prices+`}}`)
	require.Equal(t, http.StatusOK, status, body)
	assert.JSONEq(t, prices, shown(t, body))

	status, body = f.call(t, "GET /api/settings", "")
	require.Equal(t, http.StatusOK, status, body)
	assert.JSONEq(t, prices, shown(t, body), "an object comes back as one")

	stored, err := f.store.Settings(t.Context())
	require.NoError(t, err)

	got, err := engine.Prices(stored)
	require.NoError(t, err)
	assert.Equal(t, map[string]engine.Price{
		"claude-sonnet-4-5-20250929": {Input: 3, Output: 15, CacheRead: 0.3, CacheWrite: 3.75},
		"*":                          {Input: 15, Output: 75, CacheRead: 1.5, CacheWrite: 18.75},
	}, got)

	status, body = f.call(t, "PATCH /api/settings", `{"settings": {"model_prices":
		{"*": {"input": -1, "output": 75, "cache_read": 1.5, "cache_write": 18.75}}}}`)
	require.Equal(t, http.StatusBadRequest, status, body)
	assertError(t, body)

	after, err := f.store.Settings(t.Context())
	require.NoError(t, err)
	assert.Equal(t, stored, after, "a refused price writes nothing")
}

func TestDefaultsCoverEverySettableKey(t *testing.T) {
	f := newServer(t)

	status, body := f.call(t, "GET /api/settings", "")
	require.Equal(t, http.StatusOK, status, body)

	defaults, ok := object(t, body)["defaults"].(map[string]any)
	require.True(t, ok, body)
	assert.ElementsMatch(t, engine.SettableKeys(), slices.Collect(maps.Keys(defaults)))
}
