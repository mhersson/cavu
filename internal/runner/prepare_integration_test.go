//go:build integration

package runner

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/cavu/internal/driver/claudecode"
	"github.com/mhersson/cavu/internal/engine"
	"github.com/mhersson/cavu/internal/sandbox"
	"github.com/mhersson/cavu/internal/testkit/gitrepo"
	"github.com/mhersson/cavu/internal/testkit/sandboxtest"
)

// helloFiles is a Go module with one package and its test.
var helloFiles = map[string]string{
	"go.mod":   "module example.com/hello\n\ngo " + goVersion + "\n",
	"hello.go": "package hello\n\nfunc Hello() string { return \"hello\" }\n",
	"hello_test.go": "package hello\n\nimport \"testing\"\n\n" +
		"func TestHello(t *testing.T) {\n\tif Hello() != \"hello\" {\n\t\tt.Fatal(Hello())\n\t}\n}\n",
}

func TestPrepareGoRepository(t *testing.T) {
	if os.Getenv("CAVU_TEST_OFFLINE") == "1" {
		t.Skip("CAVU_TEST_OFFLINE=1: this test downloads a Go toolchain through the allowlist")
	}

	r, c, head := fixtureRun(t, helloFiles)
	c.Setup.Command = "go mod download"
	c.Verify.Command, c.Verify.Timeout = "go test ./...", 5*time.Minute

	started := time.Now()
	res, err := r.Prepare(t.Context(), c)
	cold := time.Since(started)

	require.NoError(t, err)
	require.Empty(t, res.FailedStep, res.Output)
	assert.Equal(t, head, res.BaseCommit)
	assert.Equal(t, []string{"go@" + goVersion}, res.Tools)
	assert.Equal(t, []string{"go"}, res.Ecosystems)
	assert.Equal(t, engine.ResultPassed, res.Baseline)

	st := r.state(c.RunID)
	require.NotNil(t, st.agent)

	out := runIn(t, st, "go", "version")
	assert.Equal(t, 0, out.ExitCode, out.Output)
	assert.Contains(t, out.Output, "go"+goVersion+" ")

	out = runIn(t, st, "touch", "/cache/x")
	assert.NotEqual(t, 0, out.ExitCode, "the agent phase writes the project cache")

	c.Verify.BaselinePassed, c.Verify.BaselineCommit = true, head

	started = time.Now()
	res, err = r.Prepare(t.Context(), c)
	warm := time.Since(started)

	require.NoError(t, err)
	require.Empty(t, res.FailedStep, res.Output)
	assert.Equal(t, "reused", res.Baseline)

	// A process that restarted brings the agent phase up from the config's
	// ecosystems and verifies with the toolchain the prepare installed.
	restarted := New(r.rt, claudecode.New(), r.forge)
	restarted.labels = sandboxtest.Labels()
	c.Ecosystems = res.Ecosystems

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()

		assert.NoError(t, restarted.Release(ctx, refOf(c), engine.RemoveAll))
	})

	require.NoError(t, restarted.ensureUp(t.Context(), c, sandbox.PhaseAgent))

	vr, err := runVerify(t.Context(), restarted.state(c.RunID), c)
	require.NoError(t, err)
	assert.Equal(t, engine.ResultPassed, vr.Result, vr.Output)

	t.Logf("%s: cold prepare %s, warm prepare %s", r.rt.Kind, cold.Round(time.Millisecond), warm.Round(time.Millisecond))
}

// A run at a start commit an earlier task pushed is set up at the base
// commit: its tools come from the base's files and its setup runs on them.
// Only then is its workspace moved to the start commit, and no baseline runs.
func TestPrepareAtAStartCommit(t *testing.T) {
	const saw = "setup-saw.txt" // the setup's output, which the base ignores

	var start string

	r, c, base := fixtureRunOn(t, map[string]string{"notes.txt": "first line\n", ".gitignore": saw + "\n"}, func(bare string) (http.Handler, string) {
		// The earlier task's commit adds a Go module, which detection at it would install a toolchain for.
		dir := t.TempDir()
		gitrepo.Git(t, dir, "clone", "-q", bare, "work")
		work := filepath.Join(dir, "work")
		gitrepo.Git(t, work, "checkout", "-q", "-b", "cavu/earlier")
		start = gitrepo.Commit(t, work, map[string]string{"go.mod": "module example.com/hello\n\ngo " + goVersion + "\n"}, "feat(hello): add a module")
		gitrepo.Git(t, work, "push", "-q", "origin", "cavu/earlier")

		return http.StripPrefix("/cavu", gitrepo.Handler(t, bare)), "/cavu/" + filepath.Base(bare)
	})
	c.Repo.Start = start
	c.Setup.Command = "if [ -e go.mod ]; then echo CAVU-SETUP-SAW-START; else echo CAVU-SETUP-SAW-BASE; fi > " + saw
	c.Verify.Command, c.Verify.Timeout = "exit 1", time.Minute // a baseline that ran fails the prepare

	res, err := r.Prepare(t.Context(), c)
	require.NoError(t, err)
	require.Empty(t, res.FailedStep, res.Output)
	assert.Equal(t, base, res.BaseCommit)
	assert.Empty(t, res.Tools, "the tools were detected at the start commit")
	assert.Empty(t, res.Ecosystems)
	assert.Equal(t, baselineSkipped, res.Baseline)

	assert.Equal(t, "CAVU-SETUP-SAW-BASE", strings.TrimSpace(runIn(t, r.state(c.RunID), "cat", saw).Output), "the setup ran on the start commit's files")
	assert.Equal(t, start, git(t, r, c, "rev-parse", "HEAD"))
	assert.Equal(t, "refs/heads/"+c.Repo.Branch, git(t, r, c, "symbolic-ref", "HEAD"))
	assert.Empty(t, git(t, r, c, "status", "--porcelain"), "the working tree is not the start commit's")
}

// A passed check of a Go project leaves its build cache in the project's
// cache, a second check replaces that copy, and a task's home starts from it
// while the project's cache stays read-only to the task.
func TestCheckSeedsATaskWithItsGoBuildCache(t *testing.T) {
	if os.Getenv("CAVU_TEST_OFFLINE") == "1" {
		t.Skip("CAVU_TEST_OFFLINE=1: this test downloads a Go toolchain through the allowlist")
	}

	logged := captureLog(t)

	r, c, head := fixtureRun(t, helloFiles)
	c.Setup.Command = "go mod download"
	c.Verify.Command, c.Verify.Timeout = "go test ./...", 5*time.Minute

	started := time.Now()
	res, _, err := r.CheckEnvironment(t.Context(), c)
	firstCheck := time.Since(started)

	require.NoError(t, err)
	require.Equal(t, engine.ResultPassed, res.Baseline, res.Output)

	started = time.Now()
	res, _, err = r.CheckEnvironment(t.Context(), c)
	secondCheck := time.Since(started)

	require.NoError(t, err)
	require.Equal(t, engine.ResultPassed, res.Baseline, res.Output)
	assert.Empty(t, logRecords(t, logged), "a fill failed")

	// A reused baseline leaves the task's home as its seed made it: nothing has built there yet.
	c.Verify.BaselinePassed, c.Verify.BaselineCommit = true, head

	res, err = r.Prepare(t.Context(), c)
	require.NoError(t, err)
	require.Empty(t, res.FailedStep, res.Output)

	st := r.state(c.RunID)
	require.NotNil(t, st.agent)

	out := runIn(t, st, "sh", "-c", `cd /cache/go && [ -d check-build ] && [ ! -e check-build.new ] && [ ! -e check-build.old ]`)
	assert.Equal(t, 0, out.ExitCode, "the cache holds the copy and nothing a fill left: %s", out.Output)

	out = runIn(t, st, "sh", "-c", `cd /cache/go/check-build && n=0 && for f in $(find . -type f); do [ -f "$GOCACHE/$f" ] || exit 1; n=$((n+1)); done; [ "$n" -gt 0 ]`)
	assert.Equal(t, 0, out.ExitCode, "the home's build cache lacks a file of the copy: %s", out.Output)

	out = runIn(t, st, "touch", "/cache/go/check-build/x")
	assert.NotEqual(t, 0, out.ExitCode, "the agent phase writes the project cache")

	vr, err := runVerify(t.Context(), st, c)
	require.NoError(t, err)
	assert.Equal(t, engine.ResultPassed, vr.Result, vr.Output)

	t.Logf("%s: first check %s, second check %s, the task's first verify %s", r.rt.Kind,
		firstCheck.Round(time.Millisecond), secondCheck.Round(time.Millisecond), vr.Duration.Round(time.Millisecond))
}
