package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSecretSetReadsStdin(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	args := []string{"secret", "set", "forge_token", "--data-dir", dir}

	var stdout strings.Builder

	code := run(args, strings.NewReader("forge-token-value\n"), &stdout, io.Discard)

	require.Equal(t, 0, code)
	assert.Empty(t, stdout.String())

	a, err := openApp(ctx, dir)
	require.NoError(t, err)

	defer a.close()

	raw, ok, err := a.store.Secret(ctx, "forge_token")
	require.NoError(t, err)
	require.True(t, ok)
	assert.NotContains(t, string(raw), "forge-token-value")

	got, _, err := a.vault.Get(ctx, "forge_token")
	require.NoError(t, err)
	assert.Equal(t, "forge-token-value", got)

	code, _, _ = runCavu("secret", "set", "forge_token", "another-value", "--data-dir", dir)
	assert.Equal(t, 2, code, "a value as an argument is a usage error")

	got, _, err = a.vault.Get(ctx, "forge_token")
	require.NoError(t, err)
	assert.Equal(t, "forge-token-value", got)
}

func TestSecretSetRefusesAnEmptyValueAndATerminal(t *testing.T) {
	dir := t.TempDir()

	devNull, err := os.Open(os.DevNull)
	require.NoError(t, err)

	defer devNull.Close()

	for _, stdin := range []io.Reader{strings.NewReader(""), strings.NewReader("\n"), devNull} {
		code := run([]string{"secret", "set", "forge_token", "--data-dir", dir}, stdin, io.Discard, io.Discard)
		assert.Equal(t, 1, code)
	}

	a, err := openApp(t.Context(), dir)
	require.NoError(t, err)

	defer a.close()

	names, err := a.store.SecretNames(t.Context())
	require.NoError(t, err)
	assert.Empty(t, names)
}

func TestIsTerminal(t *testing.T) {
	// /dev/null is a character device, as a terminal is.
	devNull, err := os.Open(os.DevNull)
	require.NoError(t, err)

	defer devNull.Close()

	r, w, err := os.Pipe()
	require.NoError(t, err)

	defer r.Close()
	defer w.Close()

	file, err := os.Create(filepath.Join(t.TempDir(), "value"))
	require.NoError(t, err)

	defer file.Close()

	assert.True(t, isTerminal(devNull))
	assert.False(t, isTerminal(r), "a pipe")
	assert.False(t, isTerminal(file), "a redirected file")
	assert.False(t, isTerminal(strings.NewReader("value")))
}
