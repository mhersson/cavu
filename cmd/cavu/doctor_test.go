package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mhersson/cavu/internal/doctor"
	"github.com/mhersson/cavu/internal/driver/claudecode"
	"github.com/mhersson/cavu/internal/engine"
	"github.com/mhersson/cavu/internal/forge"
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
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{}`)) }))
	t.Cleanup(srv.Close)
	require.NoError(t, os.WriteFile(bundle, bundleOf(srv), 0o600))

	srvRepo, err := forge.ParseRepo(srv.URL + "/o/n")
	require.NoError(t, err)

	cavuIn(t, dir, "settings", "set", "ca_bundle_path", bundle)
	cavuIn(t, dir, "project", "add", "web", "--repo", repo, "--verify", "make test")
	cavuIn(t, dir, "project", "add", "api", "--repo", "https://example.com/api.git", "--base", "dev", "--verify", "make test")

	for name, value := range map[string]string{"endpoint_credential": "cred", "forge_token": "tok", "forge_app_key": "app-key"} {
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

			for _, p := range got.Projects {
				assert.Equal(t, engine.ForgeAccess{Token: "tok"}, p.Access)
				require.NoError(t, p.AccessErr)
			}

			assert.Equal(t, bundleOf(srv), got.CABundle)
			assert.Equal(t, bundle, got.Settings["ca_bundle_path"])
			assert.NotNil(t, got.Driver)
			require.NotNil(t, got.Forge)
			require.NoError(t, got.Forge.Check(t.Context(), srvRepo, forge.Access{Token: "tok"}), "the forge client trusts the CA bundle")
			require.Len(t, got.Projects, 2, "every project, in the store's order")
			assert.Equal(t, "api", got.Projects[0].Name)
			assert.Equal(t, "https://example.com/api.git", got.Projects[0].RepoURL)
			assert.Equal(t, "dev", got.Projects[0].BaseBranch)
			assert.True(t, got.Projects[0].Publish, "publish is on by default")
			assert.Equal(t, "web", got.Projects[1].Name)
			assert.Equal(t, repo, got.Projects[1].RepoURL)
			assert.Equal(t, "main", got.Projects[1].BaseBranch)
			assert.NotEqual(t, got.Projects[0].ID, got.Projects[1].ID)
		})
	}

	cavuIn(t, dir, "project", "set", "api", "--switch", "publish=off")

	got := fakeDoctor(t, nil)
	code, _, stderr := runCavu("doctor", "--data-dir", dir)
	require.Equal(t, 0, code, stderr)
	require.Len(t, got.Projects, 2)
	assert.False(t, got.Projects[0].Publish)
	assert.True(t, got.Projects[1].Publish)
}

func TestDoctorWithoutProject(t *testing.T) {
	got := fakeDoctor(t, []doctor.Check{{Name: "runtime", OK: true}})

	code, _, stderr := runCavu("doctor", "--data-dir", t.TempDir())
	assert.Equal(t, 0, code, stderr)
	assert.Empty(t, got.Projects)
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
	assert.NotContains(t, stderr, filepath.Join(dir, "lock"), "the server is named, not the lock")
	assert.Empty(t, got.Version, "no check runs")
}

func TestDoctorDepsResolvesEachProjectsCredential(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()

	for name, value := range map[string]string{"forge_token": "default-tok", "endpoint_credential": "cred"} {
		code, _, stderr := withStdin(value+"\n", "secret", "set", name, "--data-dir", dir)
		require.Equal(t, 0, code, stderr)
	}

	code, _, stderr := withStdin("acme-tok\n", "credential", "set", "acme", "--data-dir", dir)
	require.Equal(t, 0, code, stderr)

	code, _, stderr = withStdin("app-key\n", "credential", "set", "work", "--app-id", "7", "--data-dir", dir)
	require.Equal(t, 0, code, stderr)

	cavuIn(t, dir, "project", "add", "plain", "--repo", repo, "--no-verify")
	cavuIn(t, dir, "project", "add", "named", "--repo", repo, "--no-verify", "--forge-credential", "acme")
	cavuIn(t, dir, "project", "add", "app", "--repo", repo, "--no-verify", "--forge-credential", "work")

	a, err := openApp(ctx, dir)
	require.NoError(t, err)

	defer a.close()

	byName := func(d doctor.Deps) map[string]doctor.Project {
		m := map[string]doctor.Project{}
		for _, p := range d.Projects {
			m[p.Name] = p
		}

		return m
	}

	d, err := a.doctorDeps(ctx)
	require.NoError(t, err)

	ps := byName(d)
	assert.Empty(t, ps["plain"].Credential)
	assert.Equal(t, engine.ForgeAccess{Token: "default-tok"}, ps["plain"].Access)
	assert.Equal(t, "acme", ps["named"].Credential)
	assert.Equal(t, engine.ForgeAccess{Token: "acme-tok"}, ps["named"].Access)
	assert.Equal(t, "work", ps["app"].Credential)
	assert.Equal(t, int64(7), ps["app"].Access.AppID)

	for _, p := range ps {
		require.NoError(t, p.AccessErr, p.Name)
	}

	require.NoError(t, a.vault.Delete(ctx, engine.CredentialSecretName("work", engine.CredentialApp)))

	d, err = a.doctorDeps(ctx)
	require.NoError(t, err, "a project's access failing is its line's to report")

	ps = byName(d)
	require.Error(t, ps["app"].AccessErr)
	require.NoError(t, ps["named"].AccessErr)
	require.NoError(t, ps["plain"].AccessErr)
}
