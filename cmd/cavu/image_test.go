package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDefaultBinary(t *testing.T) {
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)

	cellar := filepath.Join(tmp, "Cellar", "cavu", "1")
	require.NoError(t, os.MkdirAll(filepath.Join(cellar, "bin"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(cellar, "libexec"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(tmp, "bin"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(cellar, "bin", "cavu"), []byte("x"), 0o755))

	link := filepath.Join(tmp, "bin", "cavu")
	require.NoError(t, os.Symlink(filepath.Join(cellar, "bin", "cavu"), link))

	self := func() (string, error) { return link, nil }
	linuxBuild := filepath.Join(cellar, "libexec", "cavu-linux")

	// No libexec file yet.
	_, err = defaultBinary("darwin", "", self)
	require.Error(t, err)
	assert.Contains(t, err.Error(), linuxBuild)
	assert.Contains(t, err.Error(), "--cavu-binary")

	require.NoError(t, os.WriteFile(linuxBuild, []byte("x"), 0o755))

	got, err := defaultBinary("darwin", "", self)
	require.NoError(t, err)
	assert.Equal(t, linuxBuild, got)

	got, err = defaultBinary("darwin", "/x/cavu", self)
	require.NoError(t, err)
	assert.Equal(t, "/x/cavu", got)

	got, err = defaultBinary("linux", "", self)
	require.NoError(t, err)
	assert.Equal(t, link, got)

	got, err = defaultBinary("linux", "/x/cavu", self)
	require.NoError(t, err)
	assert.Equal(t, "/x/cavu", got)

	for _, goos := range []string{"linux", "darwin"} {
		_, err = defaultBinary(goos, "", func() (string, error) { return "", errors.New("no") })
		require.Error(t, err)
	}
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
	srv := httptest.NewTLSServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)

	require.NoError(t, os.WriteFile(setting, bundleOf(srv), 0o600))

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
	assert.Equal(t, bundleOf(srv), fake.builds[1].CABundle)
	assert.Equal(t, "podman", fake.name)

	set("ca_bundle_path", filepath.Join(dir, "missing.pem"))
	assert.Equal(t, 1, build())
	assert.Len(t, fake.builds, 2, "an unreadable bundle builds nothing")
}
