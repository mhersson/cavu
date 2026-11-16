package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/mhersson/cavu/internal/sandbox"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMain points the default data directory at one of its own, so a test
// that forgets --data-dir never reaches the real one, and refuses the runtime
// to a test that forgets the fake.
func TestMain(m *testing.M) {
	detectRuntime = func(context.Context, string) (containerRuntime, error) {
		return nil, errors.New("a unit test drives no container runtime")
	}

	dir, err := os.MkdirTemp("", "cavu-cmd-test-")
	if err == nil {
		err = os.Setenv("XDG_DATA_HOME", dir)
	}

	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	os.Unsetenv("CAVU_DATA_DIR")

	code := m.Run()

	_ = os.RemoveAll(dir)

	os.Exit(code)
}

// fakeRuntime records what the verbs ask of the container runtime.
type fakeRuntime struct {
	name   string // the runtime detected
	builds []sandbox.BuildOptions
}

func (f *fakeRuntime) BuildImage(_ context.Context, o sandbox.BuildOptions, _ io.Writer) error {
	f.builds = append(f.builds, o)

	return nil
}

func useFakeRuntime(t *testing.T, fake *fakeRuntime) {
	t.Helper()

	prev := detectRuntime
	detectRuntime = func(_ context.Context, name string) (containerRuntime, error) {
		fake.name = name

		return fake, nil
	}

	t.Cleanup(func() { detectRuntime = prev })
}

// storedSettings reads the settings table of the data directory dir.
func storedSettings(t *testing.T, dir string) map[string]string {
	t.Helper()

	a, err := openApp(t.Context(), dir)
	require.NoError(t, err)

	defer a.close()

	all, err := a.store.Settings(t.Context())
	require.NoError(t, err)

	return all
}

func TestOpenAppCreatesTheDataDirectoryOnFirstUse(t *testing.T) {
	ctx := t.Context()
	dir := filepath.Join(t.TempDir(), "data")

	a, err := openApp(ctx, dir)
	require.NoError(t, err)

	id, ok, err := a.store.Setting(ctx, "instance_id")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Regexp(t, `^[0-9a-f]{16}$`, id)
	require.NoError(t, a.vault.Set(ctx, "forge_token", "sealed-on-first-open"))
	a.close()

	for path, mode := range map[string]os.FileMode{dir: 0o700, filepath.Join(dir, "master.key"): 0o600} {
		info, err := os.Stat(path)
		require.NoError(t, err)
		assert.Equal(t, mode, info.Mode().Perm(), path)
	}

	a, err = openApp(ctx, dir)
	require.NoError(t, err)

	defer a.close()

	again, _, err := a.store.Setting(ctx, "instance_id")
	require.NoError(t, err)
	assert.Equal(t, id, again)

	got, _, err := a.vault.Get(ctx, "forge_token")
	require.NoError(t, err)
	assert.Equal(t, "sealed-on-first-open", got, "the master key is kept")
}

func TestLockRefusesASecondHolder(t *testing.T) {
	dir := t.TempDir()
	lockFile := filepath.Join(dir, "lock")

	first, err := openApp(t.Context(), dir)
	require.NoError(t, err)

	second, err := openApp(t.Context(), dir)
	require.NoError(t, err)

	defer second.close()

	require.NoError(t, first.lock())

	err = second.lock()
	require.Error(t, err)
	assert.Contains(t, err.Error(), lockFile)

	first.close()
	assert.FileExists(t, lockFile)
	require.NoError(t, second.lock(), "closing the holder releases the lock")
}

func TestDataDirResolution(t *testing.T) {
	t.Setenv("HOME", "/home/person")
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("CAVU_DATA_DIR", "")

	steps := []struct {
		env, value, flag, want string
	}{
		{want: "/home/person/.local/share/cavu"},
		{env: "XDG_DATA_HOME", value: "/xdg", want: "/xdg/cavu"},
		{env: "CAVU_DATA_DIR", value: "/env/cavu", want: "/env/cavu"},
		{flag: "/flag/cavu", want: "/flag/cavu"},
	}

	for _, s := range steps {
		if s.env != "" {
			t.Setenv(s.env, s.value)
		}

		got, err := dataDir(s.flag)
		require.NoError(t, err)
		assert.Equal(t, s.want, got)
	}
}
