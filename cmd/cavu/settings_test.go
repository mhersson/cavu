package main

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSettingsRejectUnknownKey(t *testing.T) {
	dir := t.TempDir()
	before := storedSettings(t, dir)

	for _, key := range []string{"max_turns", "instance_id"} {
		code, stdout, stderr := runCavu("settings", "set", key, "0123456789abcdef", "--data-dir", dir)

		assert.Equal(t, 1, code)
		assert.Empty(t, stdout)
		assert.Contains(t, stderr, key)
	}

	assert.Equal(t, before, storedSettings(t, dir))
}

func TestSettingsValidateEnums(t *testing.T) {
	dir := t.TempDir()

	code, _, _ := runCavu("settings", "set", "endpoint_mode", "both", "--data-dir", dir)
	assert.Equal(t, 1, code)
	assert.NotContains(t, storedSettings(t, dir), "endpoint_mode")

	for _, mode := range []string{"subscription", "key"} {
		code, _, _ = runCavu("settings", "set", "endpoint_mode", mode, "--data-dir", dir)
		require.Equal(t, 0, code)
		assert.Equal(t, mode, storedSettings(t, dir)["endpoint_mode"])
	}
}

func TestSettingsShowPrintsSortedPairs(t *testing.T) {
	dir := t.TempDir()

	for _, kv := range [][2]string{{"model_work", "model-a"}, {"commit_name", "CAVU Bot"}} {
		code, _, _ := runCavu("settings", "set", kv[0], kv[1], "--data-dir", dir)
		require.Equal(t, 0, code)
	}

	code, stdout, stderr := runCavu("settings", "show", "--data-dir", dir)

	require.Equal(t, 0, code)
	assert.Empty(t, stderr)

	id := storedSettings(t, dir)["instance_id"]
	assert.Equal(t, "commit_name=CAVU Bot\ninstance_id="+id+"\nmodel_work=model-a\n", stdout)
}

func TestSettingsUsageErrors(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")

	for _, args := range [][]string{
		{"settings"},
		{"settings", "list"},
		{"settings", "set", "model_work"},
		{"settings", "set", "model_work", "model-a", "extra"},
		{"settings", "show", "extra"},
		{"settings", "show", "--verbose"},
	} {
		code, stdout, _ := runCavu(append(args, "--data-dir", dir)...)

		assert.Equal(t, 2, code, args)
		assert.Empty(t, stdout, args)
	}

	assert.NoDirExists(t, dir)
}
