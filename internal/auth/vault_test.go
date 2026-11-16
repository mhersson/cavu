package auth_test

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"

	"github.com/mhersson/cavu/internal/auth"
	"github.com/mhersson/cavu/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func openStore(t *testing.T) *store.Store {
	t.Helper()

	s, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "cavu.db"))
	require.NoError(t, err)

	t.Cleanup(func() { _ = s.Close() })

	return s
}

func newVault(t *testing.T, s *store.Store, master []byte) *auth.Vault {
	t.Helper()

	v, err := auth.NewVault(s, master)
	require.NoError(t, err)

	return v
}

func TestVaultRoundTrip(t *testing.T) {
	ctx := context.Background()
	v := newVault(t, openStore(t), testKey(t))

	got, ok, err := v.Get(ctx, "forge_token")
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Empty(t, got)

	require.NoError(t, v.Set(ctx, "forge_token", "ghp_firstvalue"))
	require.NoError(t, v.Set(ctx, "endpoint_credential", "sk-ant-secretvalue"))
	require.NoError(t, v.Set(ctx, "forge_token", "ghp_replaced"))

	got, ok, err = v.Get(ctx, "forge_token")
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, "ghp_replaced", got)

	values, err := v.Values(ctx)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"ghp_replaced", "sk-ant-secretvalue"}, values)
}

func TestVaultStoresCiphertext(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	v := newVault(t, s, testKey(t))

	const plain = "ghp_plaintextmustnotappear"

	require.NoError(t, v.Set(ctx, "forge_token", plain))

	sealed, ok, err := s.Secret(ctx, "forge_token")
	require.NoError(t, err)
	require.True(t, ok)
	assert.False(t, bytes.Contains(sealed, []byte(plain)))
}

func TestVaultWrongKey(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)

	require.NoError(t, newVault(t, s, testKey(t)).Set(ctx, "forge_token", "ghp_value12345"))

	other := newVault(t, s, testKey(t))

	got, _, err := other.Get(ctx, "forge_token")
	require.ErrorIs(t, err, auth.ErrDecrypt)
	assert.Empty(t, got)

	values, err := other.Values(ctx)
	require.ErrorIs(t, err, auth.ErrDecrypt)
	assert.Nil(t, values)
}
