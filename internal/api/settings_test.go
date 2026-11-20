package api_test

import (
	"crypto/ecdh"
	"encoding/base64"
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
		"forge_credentials": [], "push_key": "`+f.pushKey+`",
		"defaults": `+settingDefaults+`, "switches": `+settingSwitches+`}`, settings(t),
		"a setting a person cannot set is not shown")

	status, body := f.call(t, "PATCH /api/settings", `{"settings": {"endpoint_mode": "subscription", "model_work": "sonnet",
		"concurrency": "4", "commit_name": {"b": 2, "a": [1, 2.50]}}, "secrets": {"forge_token": "`+secret+`"}}`)
	require.Equal(t, http.StatusOK, status, body)

	want := `{"settings": {"endpoint_mode": "subscription", "model_work": "sonnet", "concurrency": "4",
		"commit_name": {"b": 2, "a": [1, 2.50]}}, "secrets": {"endpoint_credential": false, "forge_token": true, "forge_app_key": false},
		"forge_credentials": [], "push_key": "` + f.pushKey + `",
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
		{"a credential beside a bad value", `{"settings": {"concurrency": "zero"}, "forge_credentials": {"x": {"token": "t"}}}`},
		{"a credential beside a bad secret name", `{"secrets": {"nope": "v"}, "forge_credentials": {"x": {"token": "t"}}}`},
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
		"forge_credentials": [], "push_key": "` + f.pushKey + `",
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

func TestSettingsCarryThePushKey(t *testing.T) {
	f := newServer(t)

	for route, body := range map[string]string{"GET /api/settings": "", "PATCH /api/settings": "{}"} {
		status, got := f.call(t, route, body)
		require.Equal(t, http.StatusOK, status, got)

		key, ok := object(t, got)["push_key"].(string)
		require.True(t, ok, got)
		assert.Equal(t, f.pushKey, key, route)

		point, err := base64.RawURLEncoding.DecodeString(key)
		require.NoError(t, err, route)

		_, err = ecdh.P256().NewPublicKey(point)
		require.NoError(t, err, "%s: an uncompressed P-256 point, as applicationServerKey takes", route)
	}
}

func TestDefaultsCoverEverySettableKey(t *testing.T) {
	f := newServer(t)

	status, body := f.call(t, "GET /api/settings", "")
	require.Equal(t, http.StatusOK, status, body)

	defaults, ok := object(t, body)["defaults"].(map[string]any)
	require.True(t, ok, body)
	assert.ElementsMatch(t, engine.SettableKeys(), slices.Collect(maps.Keys(defaults)))
}

func TestCredentialsOverTheAPI(t *testing.T) {
	const (
		secret = "sealed-pat-value-1"
		appKey = "sealed-app-key-material"
	)

	f := newServer(t)

	credentials := func(t *testing.T) any {
		t.Helper()

		status, body := f.call(t, "GET /api/settings", "")
		require.Equal(t, http.StatusOK, status, body)

		return object(t, body)["forge_credentials"]
	}

	assert.Equal(t, []any{}, credentials(t), "no credential is [] and never null")

	status, body := f.call(t, "PATCH /api/settings", `{"forge_credentials": {"acme-org": {"token": "`+secret+`"},
		"work-app": {"app_id": 482913, "app_key": "`+appKey+`"}}}`)
	require.Equal(t, http.StatusOK, status, body)
	assert.Equal(t, []any{
		map[string]any{"name": "acme-org", "kind": "token"},
		map[string]any{"name": "work-app", "kind": "app", "app_id": float64(482913)},
	}, object(t, body)["forge_credentials"])
	assert.Equal(t, object(t, body)["forge_credentials"], credentials(t))
	assert.NotContains(t, body, secret)
	assert.NotContains(t, body, appKey)

	status, body = f.call(t, "PATCH /api/settings", `{"forge_credentials": {"acme-org": {"app_id": 7, "app_key": "`+appKey+`"}}}`)
	require.Equal(t, http.StatusOK, status, body)
	assert.Equal(t, []any{
		map[string]any{"name": "acme-org", "kind": "app", "app_id": float64(7)},
		map[string]any{"name": "work-app", "kind": "app", "app_id": float64(482913)},
	}, credentials(t), "a replace is whole and its kind follows its fields")

	status, body = f.call(t, "PATCH /api/settings", `{"forge_credentials": {"work-app": null}}`)
	require.Equal(t, http.StatusOK, status, body)
	assert.Equal(t, []any{map[string]any{"name": "acme-org", "kind": "app", "app_id": float64(7)}}, credentials(t))

	status, body = f.call(t, "POST /api/projects", `{"name": "web", "repo_url": "https://example.com/web.git", "forge_credential": "acme-org"}`)
	require.Equal(t, http.StatusCreated, status, body)

	status, body = f.call(t, "PATCH /api/settings", `{"forge_credentials": {"acme-org": null}}`)
	require.Equal(t, http.StatusBadRequest, status, body)
	assertError(t, body)
	assert.Contains(t, body, "web", "the line names the project")
	assert.Len(t, credentials(t), 1, "the credential stays")

	for _, b := range f.bodies {
		assert.NotContains(t, b, secret)
		assert.NotContains(t, b, appKey)
	}
}

func TestSettingsPatchRefusedWritesNothing(t *testing.T) {
	f := newServer(t)

	status, body := f.call(t, "PATCH /api/settings", `{"forge_credentials": {"keep": {"token": "t"}}}`)
	require.Equal(t, http.StatusOK, status, body)

	for _, tc := range []struct{ name, bad string }{
		{"a bad name", `"Bad_Name": {"token": "t"}`},
		{"an App id without a key", `"half": {"app_id": 5}`},
		{"a removal of an unknown name", `"ghost": null`},
		{"a token and an App key together", `"both": {"token": "t", "app_id": 5, "app_key": "k"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, body := f.call(t, "PATCH /api/settings", `{"settings": {"model_work": "opus"}, "secrets": {"forge_token": "s"},
				"forge_credentials": {"fresh": {"token": "t"}, `+tc.bad+`}}`)
			require.Equal(t, http.StatusBadRequest, status, body)
			assertError(t, body)

			settings, err := f.store.Settings(t.Context())
			require.NoError(t, err)
			assert.NotContains(t, settings, "model_work")

			_, ok, err := f.vault.Get(t.Context(), "forge_token")
			require.NoError(t, err)
			assert.False(t, ok)

			cs, err := f.store.Credentials(t.Context())
			require.NoError(t, err)
			require.Len(t, cs, 1)
			assert.Equal(t, "keep", cs[0].Name)
		})
	}

	for _, bad := range []string{`{"forge_credentials": {"x": {"kind": "token"}}}`, `{"forge_credentials": {"x": {"app_id": "5", "app_key": "k"}}}`} {
		status, body := f.call(t, "PATCH /api/settings", bad)
		assert.Equal(t, http.StatusBadRequest, status, body)
	}
}
