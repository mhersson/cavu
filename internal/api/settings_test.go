package api_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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

	assert.JSONEq(t, `{"settings": {}, "secrets": {"endpoint_credential": false, "forge_token": false}}`, settings(t),
		"a setting a person cannot set is not shown")

	status, body := f.call(t, "PATCH /api/settings", `{"settings": {"endpoint_mode": "subscription", "model_work": "sonnet",
		"concurrency": "4", "commit_name": {"b": 2, "a": [1, 2.50]}}, "secrets": {"forge_token": "`+secret+`"}}`)
	require.Equal(t, http.StatusOK, status, body)

	want := `{"settings": {"endpoint_mode": "subscription", "model_work": "sonnet", "concurrency": "4",
		"commit_name": {"b": 2, "a": [1, 2.50]}}, "secrets": {"endpoint_credential": false, "forge_token": true}}`
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
		"secrets": {"endpoint_credential": false, "forge_token": false}}`
	assert.JSONEq(t, want, body, "null and an empty value unset a key, and an empty secret removes it")
	assert.JSONEq(t, want, settings(t))

	_, ok, err = f.vault.Get(t.Context(), "forge_token")
	require.NoError(t, err)
	assert.False(t, ok)

	for _, b := range f.bodies {
		assert.NotContains(t, b, secret)
	}
}
