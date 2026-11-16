package store

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSecretsStoreBytesVerbatim(t *testing.T) {
	t.Parallel()

	s := openTest(t)
	ctx := context.Background()

	_, ok, err := s.Secret(ctx, "forge_token")
	require.NoError(t, err)
	require.False(t, ok)

	sealed := []byte{0x00, 0xff, 0x10, '\n', 0x00, 0x80}
	require.NoError(t, s.PutSecret(ctx, "forge_token", sealed))
	require.NoError(t, s.PutSecret(ctx, "endpoint_credential", []byte("x")))

	got, ok, err := s.Secret(ctx, "forge_token")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, sealed, got)

	require.NoError(t, s.PutSecret(ctx, "forge_token", []byte("new")))

	got, _, err = s.Secret(ctx, "forge_token")
	require.NoError(t, err)
	require.Equal(t, []byte("new"), got)

	names, err := s.SecretNames(ctx)
	require.NoError(t, err)
	require.Equal(t, []string{"endpoint_credential", "forge_token"}, names)
}
