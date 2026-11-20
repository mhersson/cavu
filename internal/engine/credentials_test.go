package engine_test

import (
	"maps"
	"testing"

	"github.com/mhersson/cavu/internal/engine"
	"github.com/mhersson/cavu/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func (h *harness) secretValue(name string) (string, bool) {
	h.t.Helper()

	v, ok, err := h.vault.Get(h.t.Context(), name)
	require.NoError(h.t, err)

	return v, ok
}

func TestChangeCredentials(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()

	require.NoError(t, engine.ChangeCredentials(ctx, h.store, h.vault, map[string]*engine.NewCredential{
		"bot":  {Token: "ghp_bot"},
		"team": {AppID: 42, AppKey: "pem-team"},
	}))

	got, err := h.store.Credentials(ctx)
	require.NoError(t, err)
	assert.Equal(t, []store.Credential{
		{Name: "bot", Kind: engine.CredentialToken},
		{Name: "team", Kind: engine.CredentialApp, AppID: 42},
	}, got)

	v, ok := h.secretValue(engine.CredentialSecretName("bot", engine.CredentialToken))
	assert.True(t, ok)
	assert.Equal(t, "ghp_bot", v)

	v, ok = h.secretValue(engine.CredentialSecretName("team", engine.CredentialApp))
	assert.True(t, ok)
	assert.Equal(t, "pem-team", v)

	t.Run("a replace that changes the kind leaves no old secret", func(t *testing.T) {
		require.NoError(t, engine.ChangeCredentials(ctx, h.store, h.vault, map[string]*engine.NewCredential{"bot": {AppID: 7, AppKey: "pem-bot"}}))

		_, ok := h.secretValue(engine.CredentialSecretName("bot", engine.CredentialToken))
		assert.False(t, ok)

		v, ok := h.secretValue(engine.CredentialSecretName("bot", engine.CredentialApp))
		assert.True(t, ok)
		assert.Equal(t, "pem-bot", v)

		got, err := h.store.Credentials(ctx)
		require.NoError(t, err)
		assert.Equal(t, store.Credential{Name: "bot", Kind: engine.CredentialApp, AppID: 7}, got[0])
	})

	t.Run("a removal removes the row and its secret", func(t *testing.T) {
		require.NoError(t, engine.ChangeCredentials(ctx, h.store, h.vault, map[string]*engine.NewCredential{"team": nil}))

		got, err := h.store.Credentials(ctx)
		require.NoError(t, err)
		require.Len(t, got, 1)
		assert.Equal(t, "bot", got[0].Name)

		_, ok := h.secretValue(engine.CredentialSecretName("team", engine.CredentialApp))
		assert.False(t, ok)
	})
}

func TestChangeCredentialsRefusesAndWritesNothing(t *testing.T) {
	tests := []struct {
		name   string
		change map[string]*engine.NewCredential
	}{
		{"a bad name", map[string]*engine.NewCredential{"Bad Name": {Token: "t"}}},
		{"an empty name", map[string]*engine.NewCredential{"": {Token: "t"}}},
		{"nothing", map[string]*engine.NewCredential{"x": {}}},
		{"a token with an App id", map[string]*engine.NewCredential{"x": {Token: "t", AppID: 1}}},
		{"a token with a key", map[string]*engine.NewCredential{"x": {Token: "t", AppKey: "k"}}},
		{"an App id without a key", map[string]*engine.NewCredential{"x": {AppID: 1}}},
		{"a key without an App id", map[string]*engine.NewCredential{"x": {AppKey: "k"}}},
		{"a negative App id", map[string]*engine.NewCredential{"x": {AppID: -1, AppKey: "k"}}},
		{"a removal of an unknown name", map[string]*engine.NewCredential{"nobody": nil}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			ctx := t.Context()

			change := map[string]*engine.NewCredential{"valid": {Token: "ghp_valid"}}
			maps.Copy(change, tt.change)

			err := engine.ChangeCredentials(ctx, h.store, h.vault, change)
			require.ErrorIs(t, err, engine.ErrRefused)

			got, err := h.store.Credentials(ctx)
			require.NoError(t, err)
			assert.Empty(t, got)
			assert.Empty(t, h.secretNames())
		})
	}
}

func TestRemoveCredentialRefusedWhileNamed(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	secret := engine.CredentialSecretName("bot", engine.CredentialToken)

	require.NoError(t, engine.ChangeCredentials(ctx, h.store, h.vault, map[string]*engine.NewCredential{"bot": {Token: "ghp_bot"}}))

	p := h.project()
	p.ForgeCredential = "bot"
	require.NoError(t, engine.EditProject(ctx, h.store, p))

	err := engine.ChangeCredentials(ctx, h.store, h.vault, map[string]*engine.NewCredential{"bot": nil})
	require.ErrorIs(t, err, engine.ErrRefused)
	assert.Contains(t, err.Error(), p.Name)

	got, err := h.store.Credentials(ctx)
	require.NoError(t, err)
	assert.Len(t, got, 1)

	_, ok := h.secretValue(secret)
	assert.True(t, ok)

	require.NoError(t, h.store.RemoveProject(ctx, p.ID, now, engine.ProjectSecretName(p.Name, "")))
	require.NoError(t, engine.ChangeCredentials(ctx, h.store, h.vault, map[string]*engine.NewCredential{"bot": nil}))

	got, err = h.store.Credentials(ctx)
	require.NoError(t, err)
	assert.Empty(t, got)

	_, ok = h.secretValue(secret)
	assert.False(t, ok)
}
