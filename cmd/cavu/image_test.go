package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDefaultBinaryOnLinux(t *testing.T) {
	self := func() (string, error) { return "/run/cavu", nil }

	got, err := defaultBinary("linux", "", self)
	require.NoError(t, err)
	assert.Equal(t, "/run/cavu", got)

	got, err = defaultBinary("linux", "/x/cavu", self)
	require.NoError(t, err)
	assert.Equal(t, "/x/cavu", got)

	_, err = defaultBinary("darwin", "", self)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--cavu-binary")

	got, err = defaultBinary("darwin", "/x/cavu", self)
	require.NoError(t, err)
	assert.Equal(t, "/x/cavu", got)

	_, err = defaultBinary("linux", "", func() (string, error) { return "", errors.New("no") })
	require.Error(t, err)
}

func TestImageVerbRefusesOtherActions(t *testing.T) {
	code, _, stderr := runCavu("image", "frobnicate", "--data-dir", t.TempDir())

	assert.Equal(t, 2, code)
	assert.NotEmpty(t, stderr)
}

func TestImageBuildUsesCABundleSetting(t *testing.T) {
	fake := &fakeRuntime{}
	useFakeRuntime(t, fake)

	dir := t.TempDir()
	data := filepath.Join(dir, "data")
	setting := filepath.Join(dir, "setting.pem")

	require.NoError(t, os.WriteFile(setting, []byte("bundle of the setting"), 0o600))

	build := func() int {
		code, _, _ := runCavu("image", "build", "--cavu-binary", "/linux/cavu", "--data-dir", data)

		return code
	}
	set := func(key, value string) {
		code, _, _ := runCavu("settings", "set", key, value, "--data-dir", data)
		require.Equal(t, 0, code)
	}

	require.Equal(t, 0, build())
	require.Len(t, fake.builds, 1)
	assert.Empty(t, fake.builds[0].CABundle)
	assert.Empty(t, fake.name)
	assert.Equal(t, "/linux/cavu", fake.builds[0].CavuBinary)

	set("ca_bundle_path", setting)
	set("runtime", "podman")

	require.Equal(t, 0, build())
	require.Len(t, fake.builds, 2)
	assert.Equal(t, []byte("bundle of the setting"), fake.builds[1].CABundle)
	assert.Equal(t, "podman", fake.name)

	set("ca_bundle_path", filepath.Join(dir, "missing.pem"))
	assert.Equal(t, 1, build())
	assert.Len(t, fake.builds, 2, "an unreadable bundle builds nothing")
}
