package auth_test

import (
	"testing"

	"github.com/mhersson/cavu/internal/auth"
	"github.com/stretchr/testify/require"
)

func TestNewToken(t *testing.T) {
	t.Parallel()

	raw, hash, err := auth.NewToken()
	require.NoError(t, err)
	require.Len(t, raw, 43)
	require.Len(t, hash, 64)
	require.Equal(t, auth.HashToken(raw), hash)

	raw2, hash2, err := auth.NewToken()
	require.NoError(t, err)
	require.NotEqual(t, raw, raw2)
	require.NotEqual(t, hash, hash2)
}

func TestHashToken(t *testing.T) {
	t.Parallel()

	require.Equal(t, "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad", auth.HashToken("abc"))
}
