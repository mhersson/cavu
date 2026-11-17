package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mhersson/cavu/internal/doctor"
	"github.com/mhersson/cavu/internal/driver/claudecode"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeDoctor replaces doctor.Run for the test and returns what it was handed.
func fakeDoctor(t *testing.T, checks []doctor.Check) *doctor.Deps {
	t.Helper()

	var got doctor.Deps

	prev := runDoctor
	runDoctor = func(_ context.Context, d doctor.Deps) []doctor.Check {
		got = d

		return checks
	}

	t.Cleanup(func() { runDoctor = prev })

	return &got
}

func TestDoctorVerb(t *testing.T) {
	dir := t.TempDir()
	bundle := filepath.Join(t.TempDir(), "ca.pem")
	require.NoError(t, os.WriteFile(bundle, []byte("PEM"), 0o600))

	cavuIn(t, dir, "settings", "set", "ca_bundle_path", bundle)
	cavuIn(t, dir, "project", "add", "web", "--repo", repo, "--verify", "make test")
	cavuIn(t, dir, "project", "add", "api", "--repo", "https://example.com/api.git", "--base", "dev", "--verify", "make test")

	for name, value := range map[string]string{"endpoint_credential": "cred", "forge_token": "tok"} {
		code, _, stderr := withStdin(value+"\n", "secret", "set", name, "--data-dir", dir)
		require.Equal(t, 0, code, stderr)
	}

	cases := []struct {
		name   string
		checks []doctor.Check
		code   int
	}{
		{"a failure", []doctor.Check{
			{Name: "runtime", OK: true},
			{Name: "image", Detail: "missing", Fix: "rebuild it"},
			{Name: "claude_code", Skipped: true, Detail: "an earlier step failed"},
			{Name: "forge", OK: true, Warning: true, Detail: "slow"},
		}, 1},
		{"no failure", []doctor.Check{
			{Name: "runtime", OK: true},
			{Name: "claude_code", Skipped: true},
			{Name: "forge", OK: true, Warning: true},
		}, 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := fakeDoctor(t, tc.checks)

			code, stdout, stderr := runCavu("doctor", "--data-dir", dir)
			assert.Equal(t, tc.code, code, stderr)

			lines := strings.Split(strings.TrimSpace(stdout), "\n")
			require.GreaterOrEqual(t, len(lines), len(tc.checks))

			for _, c := range tc.checks {
				var mine []string

				for _, l := range lines {
					if strings.Contains(l, c.Name) {
						mine = append(mine, l)
					}
				}

				require.Len(t, mine, 1, c.Name)
				assert.Equal(t, c.Skipped, strings.Contains(mine[0], "skipped"), c.Name)
				assert.Equal(t, c.Warning, strings.Contains(mine[0], "warning"), c.Name)
			}

			assert.Equal(t, version, got.Version)
			assert.Equal(t, claudecode.Version, got.AgentVersion)
			assert.Equal(t, "cred", got.Credential)
			assert.Equal(t, "tok", got.ForgeToken)
			assert.Equal(t, []byte("PEM"), got.CABundle)
			assert.Equal(t, bundle, got.Settings["ca_bundle_path"])
			assert.NotNil(t, got.Driver)
			require.NotNil(t, got.Project)
			assert.Equal(t, "https://example.com/api.git", got.Project.RepoURL)
			assert.Equal(t, "dev", got.Project.BaseBranch)
		})
	}
}

func TestDoctorWithoutProject(t *testing.T) {
	got := fakeDoctor(t, []doctor.Check{{Name: "runtime", OK: true}})

	code, _, stderr := runCavu("doctor", "--data-dir", t.TempDir())
	assert.Equal(t, 0, code, stderr)
	assert.Nil(t, got.Project)
}

func TestDoctorIsRefusedByLock(t *testing.T) {
	dir := t.TempDir()
	got := fakeDoctor(t, nil)
	holdLock(t, dir, "verb")

	code, _, stderr := runCavu("doctor", "--data-dir", dir)
	assert.Equal(t, 1, code)
	assert.Contains(t, stderr, filepath.Join(dir, "lock"))
	assert.Empty(t, got.Version, "no check runs")
}

func TestDoctorRefusedUnderAServer(t *testing.T) {
	dir := t.TempDir()
	got := fakeDoctor(t, nil)
	holdLock(t, dir, "serve")

	code, _, stderr := runCavu("doctor", "--data-dir", dir)
	assert.Equal(t, 1, code)
	assert.NotEmpty(t, stderr)
	assert.Empty(t, got.Version, "no check runs")
}
