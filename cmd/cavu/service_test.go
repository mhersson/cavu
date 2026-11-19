package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/mhersson/cavu/internal/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeServices gives the test a home of its own and a service manager whose
// runner records each command and runs none.
func fakeServices(t *testing.T) (home string, calls *[][]string) {
	t.Helper()

	home = t.TempDir()
	t.Setenv("HOME", home)

	var ran [][]string

	prev := services
	services = service.Manager{OS: "linux", UID: 1000, Run: func(_ context.Context, argv ...string) (string, error) {
		ran = append(ran, argv)

		return "", nil
	}}

	t.Cleanup(func() { services = prev })

	return home, &ran
}

func TestServiceVerbInstallsAndUninstalls(t *testing.T) {
	home, calls := fakeServices(t)
	unit := filepath.Join(home, ".config", "systemd", "user", "cavu.service")

	exe, err := os.Executable()
	require.NoError(t, err)

	code, _, stderr := runCavu("service", "install", "--linger")
	require.Equal(t, 0, code, stderr)

	assert.Contains(t, unitText(t, home), "\nExecStart="+exe+" serve\n")
	assert.Contains(t, *calls, []string{"loginctl", "enable-linger"})

	code, _, stderr = runCavu("service", "uninstall")
	require.Equal(t, 0, code, stderr)
	assert.NoFileExists(t, unit)
	assert.Contains(t, *calls, []string{"systemctl", "--user", "disable", "--now", "cavu.service"})
}

func TestServiceVerbWritesTheAbsoluteDataDirectory(t *testing.T) {
	t.Run("a relative flag", func(t *testing.T) {
		home, _ := fakeServices(t)
		work := t.TempDir()
		t.Chdir(work)

		code, _, stderr := runCavu("service", "install", "--data-dir", "data")
		require.Equal(t, 0, code, stderr)
		assert.Contains(t, unitText(t, home), "\nEnvironment=CAVU_DATA_DIR="+filepath.Join(work, "data")+"\n")
	})

	t.Run("XDG_DATA_HOME", func(t *testing.T) {
		home, _ := fakeServices(t)
		xdg := t.TempDir()
		t.Setenv("CAVU_DATA_DIR", "")
		t.Setenv("XDG_DATA_HOME", xdg)

		code, _, stderr := runCavu("service", "install")
		require.Equal(t, 0, code, stderr)
		assert.Contains(t, unitText(t, home), "\nEnvironment=CAVU_DATA_DIR="+filepath.Join(xdg, "cavu")+"\n")
	})
}

func unitText(t *testing.T, home string) string {
	t.Helper()

	b, err := os.ReadFile(filepath.Join(home, ".config", "systemd", "user", "cavu.service"))
	require.NoError(t, err)

	return string(b)
}

func TestServiceVerbArguments(t *testing.T) {
	home, calls := fakeServices(t)

	for _, tc := range []struct {
		args []string
		code int
	}{
		{[]string{"service"}, 2},
		{[]string{"service", "start"}, 2},
		{[]string{"service", "-h"}, 0},
		{[]string{"service", "install", "-h"}, 0},
		{[]string{"service", "uninstall", "--data-dir", home}, 2},
		{[]string{"service", "uninstall", "now"}, 2},
	} {
		code, _, _ := runCavu(tc.args...)
		assert.Equal(t, tc.code, code, tc.args)
	}

	assert.Empty(t, *calls)

	entries, err := os.ReadDir(home)
	require.NoError(t, err)
	assert.Empty(t, entries)
}
