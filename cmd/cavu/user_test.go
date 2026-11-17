package main

import (
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/mhersson/cavu/internal/auth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUserAndTokenVerbs(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()

	var stdout strings.Builder

	code := run([]string{"user", "set", "ada", "--data-dir", dir}, strings.NewReader("a long enough password\n"), &stdout, io.Discard)
	require.Equal(t, 0, code)
	assert.Empty(t, stdout.String())

	devNull, err := os.Open(os.DevNull)
	require.NoError(t, err)

	defer devNull.Close()

	code = run([]string{"user", "set", "ada", "--data-dir", dir}, devNull, io.Discard, io.Discard)
	assert.Equal(t, 1, code, "a terminal is refused")

	code = run([]string{"user", "set", "ada", "--data-dir", dir}, strings.NewReader("short\n"), io.Discard, io.Discard)
	assert.Equal(t, 1, code, "a short password is refused")

	code, _, _ = runCavu("user", "set", "ada", "a-password-as-argument", "--data-dir", dir)
	assert.Equal(t, 2, code, "a password as an argument is a usage error")

	a, err := openApp(ctx, dir)
	require.NoError(t, err)

	svc := auth.NewService(a.store, time.Now)
	_, _, err = svc.Login(ctx, "ada", "a long enough password", "local")
	require.NoError(t, err)
	a.close()

	code, out, _ := runCavu("token", "add", "ci", "--data-dir", dir)
	require.Equal(t, 0, code)

	token := strings.TrimSuffix(out, "\n")
	require.NotEmpty(t, token)
	assert.NotContains(t, token, "\n", "one line, the token alone")

	code, _, _ = runCavu("token", "add", "ci", "--data-dir", dir)
	assert.Equal(t, 1, code, "a second token of the name fails")

	a, err = openApp(ctx, dir)
	require.NoError(t, err)

	ok, err := auth.NewService(a.store, time.Now).Bearer(ctx, token)
	require.NoError(t, err)
	assert.True(t, ok)
	a.close()

	code, out, _ = runCavu("token", "list", "--data-dir", dir)
	require.Equal(t, 0, code)
	assert.Contains(t, out, "ci")
	assert.NotContains(t, out, token)

	code, _, _ = runCavu("token", "remove", "nope", "--data-dir", dir)
	assert.Equal(t, 1, code)

	code, _, _ = runCavu("token", "remove", "ci", "--data-dir", dir)
	require.Equal(t, 0, code)

	a, err = openApp(ctx, dir)
	require.NoError(t, err)

	defer a.close()

	ok, err = auth.NewService(a.store, time.Now).Bearer(ctx, token)
	require.NoError(t, err)
	assert.False(t, ok)

	tokens, err := a.store.APITokens(ctx)
	require.NoError(t, err)
	assert.Empty(t, tokens)
}
