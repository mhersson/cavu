//go:build integration

package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/cavu/internal/sandbox"
	"github.com/mhersson/cavu/internal/testkit/gitrepo"
	"github.com/mhersson/cavu/internal/testkit/mockapi"
	"github.com/mhersson/cavu/internal/testkit/sandboxtest"
)

// verify passes on the fixture repository and on every change that keeps its first line.
const verify = "grep -qx 'first line' notes.txt"

// change and done are one session's model calls: a commit that adds a line to
// notes.txt, then the structured result of finished work.
var (
	change = mockapi.Step{Tool: "Bash", Input: map[string]any{
		"command": "echo CAVU-E2E-LINE >> notes.txt && git commit -qam 'docs(notes): add a line'",
	}}
	done = mockapi.Step{Tool: mockapi.Structured, Input: map[string]any{
		"status": "done", "title": "Add a line", "summary": "Added a line to notes.txt.",
	}}
)

// image is the sandbox image the first child builds; bin is the runtime that
// built it, set once the build succeeded.
var image struct {
	once sync.Once
	res  result
	bin  string
}

func TestMain(m *testing.M) {
	code := m.Run()

	if image.bin != "" {
		name := sandbox.ImageName(sandboxtest.CavuVersion)
		if out, err := exec.CommandContext(context.Background(), image.bin, "image", "rm", "--no-prune", name).CombinedOutput(); err != nil { //nolint:gosec // our own image, by name
			fmt.Fprintf(os.Stderr, "remove image %s: %v: %s\n", name, err, out)
		}
	}

	sandboxtest.Cleanup()
	os.Exit(code)
}

// instance is a child cavu's data directory, its instance id, the endpoint
// credential it holds and the fixture repository it is set up for.
type instance struct {
	t    *testing.T
	dir  string
	id   string
	key  string
	repo string
}

// result is what one child printed and its exit code.
type result struct {
	stdout, stderr string
	code           int
}

func (r result) String() string { return r.stdout + r.stderr }

// cavu runs the child on the instance's data directory with stdin as its input.
func (in *instance) cavu(stdin string, args ...string) result {
	in.t.Helper()

	var stdout, stderr bytes.Buffer

	cmd := exec.CommandContext(in.t.Context(), sandboxtest.CavuBinary(in.t), args...)
	cmd.Env = append(os.Environ(), "CAVU_DATA_DIR="+in.dir)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = strings.NewReader(stdin), &stdout, &stderr

	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		require.ErrorAs(in.t, err, &exit)
	}

	return result{stdout: stdout.String(), stderr: stderr.String(), code: cmd.ProcessState.ExitCode()}
}

// ok runs the child, requires exit code 0 and returns its standard output.
func (in *instance) ok(args ...string) string {
	in.t.Helper()

	r := in.cavu("", args...)
	require.Zero(in.t, r.code, r)

	return r.stdout
}

// setup serves the fixture repository and sets a child up in key mode against
// endpoint, trusting the test CA. The first child builds the image.
func setup(t *testing.T, endpoint string) *instance {
	t.Helper()

	gitrepo.Isolate(t)

	rt := sandboxtest.Runtime(t)
	bare := strings.TrimPrefix(gitrepo.Bare(t, gitrepo.New(t, map[string]string{"notes.txt": "first line\n"})), "file://")
	port := sandboxtest.StartTLSServer(t, sandboxtest.HostAddr(t, rt), gitrepo.Handler(t, bare))

	in := &instance{
		t: t, dir: t.TempDir(), key: "e2e-key-" + rand.Text(),
		repo: "https://host.docker.internal:" + port + "/" + filepath.Base(bare),
	}

	caPEM, _, _ := sandboxtest.CA(t)
	bundle := filepath.Join(t.TempDir(), "ca.pem")
	require.NoError(t, os.WriteFile(bundle, caPEM, 0o600))

	for _, kv := range [][2]string{
		{"runtime", rt.Kind},
		{"ca_bundle_path", bundle},
		{"endpoint_mode", "key"},
		{"endpoint_url", endpoint},
		{"endpoint_auth_header", "bearer"},
		{"model_work", "claude-haiku-5-5"},
		{"commit_name", "CAVU E2E"},
		{"commit_email", "e2e@example.com"},
	} {
		in.ok("settings", "set", kv[0], kv[1])
	}

	secret := in.cavu(in.key, "secret", "set", "endpoint_credential")
	require.Zero(t, secret.code, secret)

	ids := values(in.ok("settings", "show"), "instance_id=")
	require.Len(t, ids, 1)
	in.id = ids[0]
	sandboxtest.RemoveInstanceAfter(t, rt, in.id)

	image.once.Do(func() {
		if image.res = in.cavu("", "image", "build"); image.res.code == 0 {
			image.bin = rt.Bin
		}
	})
	require.Zero(t, image.res.code, image.res)

	return in
}

// pipeline adds a project on the fixture repository, checks it, and adds and
// runs a task on it. It returns the task's id and the run's result.
func (in *instance) pipeline(project string) (string, result) {
	in.t.Helper()

	in.ok("project", "add", project, "--repo", in.repo, "--verify", verify)
	in.ok("project", "check", project)
	id := strings.TrimSpace(in.ok("task", "add", project, "--title", "Add a line"))

	return id, in.cavu("", "task", "run", id)
}

// values returns what follows prefix on every line of out that starts with it.
func values(out, prefix string) []string {
	var vs []string

	for line := range strings.Lines(out) {
		if v, ok := strings.CutPrefix(line, prefix); ok {
			vs = append(vs, strings.TrimSpace(v))
		}
	}

	return vs
}

func TestPipelineEndToEnd(t *testing.T) {
	mock := mockapi.Start(t, []mockapi.Step{change, done})
	in := setup(t, mock.HostURL())

	id, run := in.pipeline("notes")
	require.Zero(t, run.code, run)

	show := in.ok("task", "show", id)
	assert.Equal(t, []string{"in_review"}, values(show, "status:"))
	assert.Contains(t, strings.Split(in.ok("task", "diff", id), "\n"), "+CAVU-E2E-LINE")

	var sources []string
	for _, u := range values(show, "usage:") {
		sources = append(sources, strings.Fields(u)[0])
	}

	assert.Contains(t, sources, "proxy")

	reqs := mock.Requests()
	require.NotEmpty(t, reqs)

	for _, r := range reqs {
		assert.Equal(t, []string{"Bearer " + in.key}, r.Header.Values("Authorization"), r.Path)
		assert.Empty(t, r.Header.Values("X-Api-Key"), r.Path)
	}
}

func TestReplayAgainstMock(t *testing.T) {
	// The task's session, then the replay's.
	mock := mockapi.Start(t, []mockapi.Step{change, done, change, done})
	in := setup(t, mock.HostURL())

	id, run := in.pipeline("notes")
	require.Zero(t, run.code, run)

	in.ok("task", "mark", id)

	r := in.cavu("", "replay", "--name", "e2e", "--no-verify-fix")
	require.Zero(t, r.code, r)

	lines := strings.Split(strings.TrimSpace(r.stdout), "\n")
	header := slices.IndexFunc(lines, func(l string) bool { return strings.HasPrefix(l, "TASK ") })
	require.GreaterOrEqual(t, header, 0, r)

	rows := lines[header+1:]
	require.Len(t, rows, 1, r)
	assert.Equal(t, id, strings.Fields(rows[0])[0])

	// The table's columns are aligned, and a cell after VERIFY may be empty.
	assert.Equal(t, "passed", strings.Fields(rows[0][strings.Index(lines[header], "VERIFY"):])[0])
}
