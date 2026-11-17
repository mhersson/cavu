package auth_test

import (
	"strings"
	"testing"

	"github.com/mhersson/cavu/internal/auth"
	"github.com/stretchr/testify/require"
)

func TestPasswordRoundTrip(t *testing.T) {
	t.Parallel()

	hash, err := auth.HashPassword("correct horse battery staple")
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(hash, "$argon2id$v=19$m=65536,t=3,p=4$"))

	ok, err := auth.VerifyPassword("correct horse battery staple", hash)
	require.NoError(t, err)
	require.True(t, ok)

	ok, err = auth.VerifyPassword("wrong password", hash)
	require.NoError(t, err)
	require.False(t, ok)
}

func TestPasswordSaltsDiffer(t *testing.T) {
	t.Parallel()

	a, err := auth.HashPassword("same input")
	require.NoError(t, err)

	b, err := auth.HashPassword("same input")
	require.NoError(t, err)

	require.NotEqual(t, a, b)
}

func TestVerifyPasswordMalformed(t *testing.T) {
	t.Parallel()

	tests := []struct{ name, encoded string }{
		{"empty", ""},
		{"not argon2id", "$argon2i$v=19$m=65536,t=3,p=4$c2FsdA$aGFzaA"},
		{"wrong section count", "$argon2id$v=19$m=65536,t=3,p=4$c2FsdA"},
		{"bad params", "$argon2id$v=19$m=abc,t=3,p=4$c2FsdA$aGFzaA"},
		{"bad salt encoding", "$argon2id$v=19$m=65536,t=3,p=4$!!!$aGFzaA"},
		{"bad key encoding", "$argon2id$v=19$m=65536,t=3,p=4$c2FsdA$!!!"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ok, err := auth.VerifyPassword("anything", tt.encoded)
			require.False(t, ok)
			require.ErrorIs(t, err, auth.ErrInvalidHash)
		})
	}
}
