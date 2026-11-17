package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mhersson/cavu/internal/engine"
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
	pollEvery = 5 * time.Millisecond

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
	name     string // the runtime detected
	builds   []sandbox.BuildOptions
	image    string              // the image whose labels were read
	labels   sandbox.ImageLabels // the image's, when found
	found    bool
	removals []removal
	runner   *fakeRunner
}

// removal is one RemoveOrphans call.
type removal struct {
	instanceID string
	keepRuns   []int64
}

func (f *fakeRuntime) RemoveOrphans(_ context.Context, instanceID string, keepRuns []int64) error {
	f.removals = append(f.removals, removal{instanceID, keepRuns})

	return nil
}

func (f *fakeRuntime) BuildImage(_ context.Context, o sandbox.BuildOptions, _ io.Writer) error {
	f.builds = append(f.builds, o)

	return nil
}

func (f *fakeRuntime) ImageInfo(_ context.Context, name string) (sandbox.ImageLabels, bool, error) {
	f.image = name

	return f.labels, f.found, nil
}

func (f *fakeRuntime) Runner() engine.Runner { return f.runner }

// fakeRunner answers a project check and a run's actions as a test sets them,
// records what it is asked, and runs no command.
type fakeRunner struct {
	checks     []engine.RunConfig
	check      engine.PrepareResult
	suggestion string
	checkErr   error

	actions    []engine.ActionKind // prepare, work, verify and export, in the order asked
	work       engine.WorkResult
	duringWork func() // runs inside Work, as a person might act while a session runs
	releases   []release
}

// release is one Release call, with its context's error at the call.
type release struct {
	ref           engine.RunRef
	removeVolumes bool
	ctxErr        error
}

func (r *fakeRunner) CheckEnvironment(_ context.Context, c engine.RunConfig) (engine.PrepareResult, string, error) {
	r.checks = append(r.checks, c)

	return r.check, r.suggestion, r.checkErr
}

func (r *fakeRunner) Prepare(context.Context, engine.RunConfig) (engine.PrepareResult, error) {
	r.actions = append(r.actions, engine.KindPrepare)

	return engine.PrepareResult{BaseCommit: "base123", Tools: []string{"go@1.27"}, Baseline: engine.ResultPassed}, nil
}

func (r *fakeRunner) Work(ctx context.Context, _ engine.RunConfig, _ engine.WorkRequest, progress func(engine.Progress)) (engine.WorkResult, error) {
	r.actions = append(r.actions, engine.KindWork)

	progress(engine.Progress{Type: engine.AgentText, Payload: engine.AgentTextPayload{Text: "Reading the login handler."}})

	if r.duringWork != nil {
		r.duringWork()
	}

	if err := ctx.Err(); err != nil {
		return engine.WorkResult{}, err
	}

	return r.work, nil
}

func (r *fakeRunner) Verify(context.Context, engine.RunConfig) (engine.VerifyResult, error) {
	r.actions = append(r.actions, engine.KindVerify)

	return engine.VerifyResult{Result: engine.ResultPassed, Command: "make test"}, nil
}

func (r *fakeRunner) Export(_ context.Context, _ engine.RunConfig, commit string) (engine.ExportResult, error) {
	r.actions = append(r.actions, engine.KindExport)

	return engine.ExportResult{
		HeadCommit: commit, Diff: []byte("diff --git a/login.go b/login.go\n"), Bundle: []byte("bundle"), FilesChanged: 1,
		DiffStat: []string{"login.go | 1 +", "1 file changed, 1 insertion(+)"},
	}, nil
}

func (r *fakeRunner) Release(ctx context.Context, ref engine.RunRef, removeVolumes bool) error {
	r.releases = append(r.releases, release{ref, removeVolumes, ctx.Err()})

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

	require.NoError(t, first.lock("verb"))

	err = second.lock("verb")
	require.Error(t, err)
	assert.Contains(t, err.Error(), lockFile)

	first.close()
	assert.FileExists(t, lockFile)
	require.NoError(t, second.lock("verb"), "closing the holder releases the lock")
}

func TestLockRole(t *testing.T) {
	dir := t.TempDir()

	// holder takes the lock as role, then a second app on dir is refused and reads the role.
	holder := func(role string) *app {
		first, err := openApp(t.Context(), dir)
		require.NoError(t, err)
		require.NoError(t, first.lock(role))

		second, err := openApp(t.Context(), dir)
		require.NoError(t, err)

		defer second.close()

		require.Error(t, second.lock("verb"))

		got, err := second.lockHolder()
		require.NoError(t, err)
		assert.Equal(t, role, got)

		return first
	}

	holder("serve").close()
	holder("verb").close()
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
