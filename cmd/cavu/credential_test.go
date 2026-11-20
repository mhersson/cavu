package main

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/cavu/internal/engine"
)

func runWithStdin(stdin string, args ...string) (code int, stdout, stderr string) {
	var out, errOut bytes.Buffer

	code = run(args, strings.NewReader(stdin), &out, &errOut)

	return code, out.String(), errOut.String()
}

func TestCredentialSetListRemove(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()

	code, stdout, stderr := runWithStdin("ghp_tokenvalue\n", "credential", "set", "acme", "--data-dir", dir)
	require.Equal(t, 0, code, stderr)
	assert.Empty(t, stdout)

	code, stdout, stderr = runWithStdin("-----BEGIN KEY-----\nappkeyvalue\n-----END KEY-----\n", "credential", "set", "work", "--app-id", "482913", "--data-dir", dir)
	require.Equal(t, 0, code, stderr)
	assert.Empty(t, stdout)

	list := cavuIn(t, dir, "credential", "list")
	t.Logf("cavu credential list:\n%s", list)

	acme, work := strings.Index(list, "acme"), strings.Index(list, "work")
	require.NotEqual(t, -1, acme)
	require.NotEqual(t, -1, work)
	assert.Less(t, acme, work)
	assert.Contains(t, list, engine.CredentialToken)
	assert.Contains(t, list, engine.CredentialApp)
	assert.Contains(t, list, "482913")
	assert.NotContains(t, list, "tokenvalue")
	assert.NotContains(t, list, "appkeyvalue")

	a, err := openApp(ctx, dir)
	require.NoError(t, err)

	defer a.close()

	got, ok, err := a.vault.Get(ctx, engine.CredentialSecretName("acme", engine.CredentialToken))
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, "ghp_tokenvalue", got)

	got, ok, err = a.vault.Get(ctx, engine.CredentialSecretName("work", engine.CredentialApp))
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Contains(t, got, "appkeyvalue")

	cavuIn(t, dir, "project", "add", "web", "--repo", "https://github.com/o/r.git", "--no-verify", "--forge-credential", "acme")

	code, _, _ = runCavu("credential", "remove", "acme", "--data-dir", dir)
	assert.NotEqual(t, 0, code, "a credential a project names stays")
	assert.Contains(t, cavuIn(t, dir, "credential", "list"), "acme")

	code, _, _ = runCavu("credential", "remove", "nobody", "--data-dir", dir)
	assert.NotEqual(t, 0, code)

	cavuIn(t, dir, "credential", "remove", "work")
	assert.NotContains(t, cavuIn(t, dir, "credential", "list"), "work")

	_, ok, err = a.vault.Get(ctx, engine.CredentialSecretName("work", engine.CredentialApp))
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestCredentialSetRefusesAnEmptyValueAndATerminal(t *testing.T) {
	dir := t.TempDir()

	devNull, err := os.Open(os.DevNull)
	require.NoError(t, err)

	defer devNull.Close()

	for _, stdin := range []io.Reader{strings.NewReader(""), strings.NewReader("\n"), devNull} {
		code := run([]string{"credential", "set", "acme", "--data-dir", dir}, stdin, io.Discard, io.Discard)
		assert.Equal(t, 1, code)
	}

	assert.NotContains(t, cavuIn(t, dir, "credential", "list"), "acme")
}

func TestCredentialAppIdZeroIsRefused(t *testing.T) {
	dir := t.TempDir()

	code, _, _ := runWithStdin("pem\n", "credential", "set", "work", "--app-id", "0", "--data-dir", dir)
	assert.Equal(t, 1, code)
	assert.NotContains(t, cavuIn(t, dir, "credential", "list"), "work")
}

func TestCredentialUsageErrors(t *testing.T) {
	dir := t.TempDir()

	for _, args := range [][]string{
		{"credential", "set"},
		{"credential", "set", "a", "b"},
		{"credential", "remove"},
		{"credential", "remove", "a", "b"},
		{"credential", "list", "a"},
		{"credential", "frobnicate"},
		{"credential"},
	} {
		code, _, _ := runWithStdin("value\n", append(args, "--data-dir", dir)...)
		assert.Equal(t, 2, code, args)
	}
}
