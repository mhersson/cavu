//go:build integration

package e2e

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/cavu/internal/sandbox"
	"github.com/mhersson/cavu/internal/testkit/gitrepo"
	"github.com/mhersson/cavu/internal/testkit/mockapi"
	"github.com/mhersson/cavu/internal/testkit/sandboxtest"
)

const (
	// verify passes on the fixture repository and on every change that keeps its first line.
	verify = "grep -qx 'first line' notes.txt"
	// marker is the line the change step adds; title is every task's.
	marker = "CAVU-E2E-LINE"
	title  = "Add a line"
	// endpointKey and forgeToken are the credentials every child holds.
	endpointKey = "sk-test-REALKEY-0123456789"
	forgeToken  = "ghp_TESTFORGETOKEN0123456789" //nolint:gosec // a test value
)

// change and done are one session's model calls: a commit that adds a line to
// notes.txt, then the structured result of finished work.
var (
	change = mockapi.Step{Tool: "Bash", Input: map[string]any{
		"command": "echo " + marker + " >> notes.txt && git commit -qam 'docs(notes): add a line'",
	}}
	done = mockapi.Step{Tool: mockapi.Structured, Input: map[string]any{
		"status": "done", "title": title, "summary": "Added a line to notes.txt.",
	}}
)

// standInForm is the form of the stand-in token: 32 random bytes in hex.
var standInForm = regexp.MustCompile(`^[0-9a-f]{64}$`)

// image is the sandbox image the first child builds; bin is the runtime that
// built it, set once the build succeeded.
var image struct {
	once sync.Once
	res  result
	bin  string
}

// host is the child built for a host that is not Linux.
var host struct {
	once sync.Once
	bin  string
	err  error
}

// child is the cavu the tests run on this host: on Linux the CavuBinary that
// also goes into the image, elsewhere the same tree with the same version
// stamp built for this host, beside it in the directory sandboxtest removes.
func child(t *testing.T) string {
	t.Helper()

	if runtime.GOOS == "linux" {
		return sandboxtest.CavuBinary(t)
	}

	host.once.Do(func() {
		host.bin = filepath.Join(filepath.Dir(sandboxtest.CavuBinary(t)), "cavu-"+runtime.GOOS)

		cmd := exec.CommandContext(context.Background(), "go", "build", "-trimpath", "-ldflags", "-X main.version="+sandboxtest.CavuVersion, "-o", host.bin, "github.com/mhersson/cavu/cmd/cavu") //nolint:gosec // this tree, stamped with the test version
		cmd.Env = append(cmd.Environ(), "CGO_ENABLED=0")

		if out, err := cmd.CombinedOutput(); err != nil {
			host.err = fmt.Errorf("build cavu for %s: %w: %s", runtime.GOOS, err, out)
		}
	})

	require.NoError(t, host.err)

	return host.bin
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

// instance is a child cavu's data directory, its instance id, the runtime it
// drives and the fixture repository it is set up for. A path set is the
// child's whole PATH; out keeps what every child printed.
type instance struct {
	t    *testing.T
	rt   sandbox.Runtime
	dir  string
	id   string
	repo string
	path string
	out  strings.Builder
}

// result is what one child printed and its exit code.
type result struct {
	stdout, stderr string
	code           int
}

func (r result) String() string { return r.stdout + r.stderr }

// start starts the child on the instance's data directory with stdin as its
// input; wait waits for it to exit and returns its result.
func (in *instance) start(stdin string, args ...string) (cmd *exec.Cmd, wait func() result) {
	in.t.Helper()

	var stdout, stderr bytes.Buffer

	cmd = exec.CommandContext(in.t.Context(), child(in.t), args...)
	cmd.Env = append(os.Environ(), "CAVU_DATA_DIR="+in.dir)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = strings.NewReader(stdin), &stdout, &stderr

	if in.path != "" {
		cmd.Env = append(cmd.Env, "PATH="+in.path)
	}

	require.NoError(in.t, cmd.Start())

	return cmd, func() result {
		in.t.Helper()

		if err := cmd.Wait(); err != nil {
			var exit *exec.ExitError
			require.ErrorAs(in.t, err, &exit)
		}

		r := result{stdout: stdout.String(), stderr: stderr.String(), code: cmd.ProcessState.ExitCode()}
		in.out.WriteString(r.String())

		return r
	}
}

// cavu runs the child to its end.
func (in *instance) cavu(stdin string, args ...string) result {
	in.t.Helper()

	_, wait := in.start(stdin, args...)

	return wait()
}

// ok runs the child, requires exit code 0 and returns its standard output.
func (in *instance) ok(args ...string) string {
	in.t.Helper()

	r := in.cavu("", args...)
	require.Zero(in.t, r.code, r)

	return r.stdout
}

// setup serves the fixture repository to the forge token and sets a child up
// in key mode against endpoint, trusting the test CA. The first child builds
// the image.
func setup(t *testing.T, endpoint string) *instance {
	t.Helper()

	gitrepo.Isolate(t)

	rt := sandboxtest.Runtime(t)
	bare := strings.TrimPrefix(gitrepo.Bare(t, gitrepo.New(t, map[string]string{"notes.txt": "first line\n"})), "file://")
	repo := gitrepo.Handler(t, bare)
	// The user name is the one CAVU's git credential helper sends.
	port := sandboxtest.StartTLSServer(t, sandboxtest.HostAddr(t, rt), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, _ := r.BasicAuth(); u != "x-access-token" || p != forgeToken {
			w.Header().Set("WWW-Authenticate", `Basic realm="fixture"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)

			return
		}

		repo.ServeHTTP(w, r)
	}))

	in := &instance{
		t: t, rt: rt, dir: t.TempDir(),
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

	for _, s := range [][2]string{{"endpoint_credential", endpointKey}, {"forge_token", forgeToken}} {
		r := in.cavu(s[1], "secret", "set", s[0])
		require.Zero(t, r.code, r)
	}

	ids := values(in.ok("settings", "show"), "instance_id=")
	require.Len(t, ids, 1)
	in.id = ids[0]
	sandboxtest.RemoveInstanceAfter(t, rt, in.id)

	image.once.Do(func() {
		// What a person off Linux types: the host's cavu cannot run in the image.
		args := []string{"image", "build"}
		if runtime.GOOS != "linux" {
			args = append(args, "--cavu-binary", sandboxtest.CavuBinary(t))
		}

		if image.res = in.cavu("", args...); image.res.code == 0 {
			image.bin = rt.Bin
		}
	})
	require.Zero(t, image.res.code, image.res)

	return in
}

// project adds a project on the fixture repository and checks it.
func (in *instance) project(name string) {
	in.t.Helper()

	in.ok("project", "add", name, "--repo", in.repo, "--verify", verify)
	in.ok("project", "check", name)
}

// task adds a task to project and returns its id.
func (in *instance) task(project string) string {
	in.t.Helper()

	return strings.TrimSpace(in.ok("task", "add", project, "--title", title))
}

// pipeline adds and checks a project, then adds and runs a task on it. It
// returns the task's id and the run's result.
func (in *instance) pipeline(project string) (string, result) {
	in.t.Helper()

	in.project(project)
	id := in.task(project)

	return id, in.cavu("", "task", "run", id)
}

// container is the instance's one container of kind, such as agent or sidecar.
func (in *instance) container(kind string) string {
	in.t.Helper()

	all, _, err := in.rt.List(in.t.Context(), "cavu.instance="+in.id)
	require.NoError(in.t, err)

	named := slices.DeleteFunc(slices.Clone(all), func(c string) bool { return !strings.HasPrefix(c, "cavu-"+in.id[:8]+"-"+kind+"-") })
	require.Len(in.t, named, 1, all)

	return named[0]
}

// client runs the runtime's client and returns its standard output.
func (in *instance) client(args ...string) string {
	in.t.Helper()

	var stdout, stderr bytes.Buffer

	cmd := exec.CommandContext(in.t.Context(), in.rt.Bin, args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	require.NoError(in.t, cmd.Run(), stderr.String())

	return stdout.String()
}

// sh runs script with args in container as uid 1000, the user of every process
// there and of the files they write: root holds no capability in the container
// and would read less.
func (in *instance) sh(container, script string, args ...string) string {
	in.t.Helper()

	return in.client(append([]string{"exec", "--user=1000:1000", container, "sh", "-c", script, "sh"}, args...)...)
}

// procScan prints each process environment and command line that matches the
// extended pattern $1, and fails on one it cannot read while it exists.
const procScan = `s=0
for f in /proc/[0-9]*/environ /proc/[0-9]*/cmdline; do
	grep -qaE -e "$1" "$f" 2>/dev/null
	case $? in
	0) echo "$f" ;;
	2) if test -e "$f"; then echo "cannot read $f" >&2; s=1; fi ;;
	esac
done
exit $s`

// fileScan prints each file under the directories after $1 that matches the
// extended pattern $1, and fails on one it cannot read.
const fileScan = `p=$1; shift; grep -rlaE -D skip -e "$p" "$@" || test $? = 1`

// mark is a pattern for v that its own text does not match, so a scan never
// finds its own command line.
func mark(v string) string { return "[" + v[:1] + "]" + v[1:] }

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
	assert.Contains(t, strings.Split(in.ok("task", "diff", id), "\n"), "+"+marker)

	var sources []string
	for _, u := range values(show, "usage:") {
		sources = append(sources, strings.Fields(u)[0])
	}

	assert.Contains(t, sources, "proxy")

	reqs := mock.Requests()
	require.NotEmpty(t, reqs)

	for _, r := range reqs {
		assert.Equal(t, []string{"Bearer " + endpointKey}, r.Header.Values("Authorization"), r.Path)
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

func TestCredentialsNeverInSandbox(t *testing.T) {
	mock := mockapi.Start(t, []mockapi.Step{change, done})
	in := setup(t, mock.HostURL())

	in.project("notes")
	id := in.task("notes")

	release := mock.Hold()
	_, wait := in.start("", "task", "run", id)

	require.Eventually(t, func() bool {
		return slices.ContainsFunc(mock.Requests(), func(r mockapi.Request) bool { return r.ModelCall })
	}, 3*time.Minute, 200*time.Millisecond)

	agent := in.container("agent")
	info, err := in.rt.Inspect(t.Context(), agent)
	require.NoError(t, err)

	dirs := []string{"/tmp"}
	for _, m := range info.Mounts {
		dirs = append(dirs, m.Target)
	}

	files := func(pattern string) string { return in.sh(agent, fileScan, append([]string{pattern}, dirs...)...) }
	credentials := mark(endpointKey) + "|" + mark(forgeToken)

	assert.Empty(t, in.sh(agent, procScan, credentials))
	assert.Empty(t, files(credentials))

	pids := strings.Fields(in.sh(agent, "pgrep -x claude"))
	require.Len(t, pids, 1)

	var standIns []string

	for kv := range strings.SplitSeq(in.sh(agent, `cat "/proc/$1/environ"`, pids[0]), "\x00") {
		if _, v, _ := strings.Cut(kv, "="); standInForm.MatchString(v) {
			standIns = append(standIns, v)
		}
	}

	require.Len(t, standIns, 1)
	standIn := standIns[0]

	assert.Contains(t, strings.Split(in.sh(agent, procScan, mark(standIn)), "\n"), "/proc/"+pids[0]+"/environ")
	assert.NotEmpty(t, files(mark(title)))

	for _, c := range []string{agent, in.container("sidecar")} {
		out := in.client("container", "inspect", c)
		assert.NotContains(t, out, endpointKey, c)
		assert.NotContains(t, out, forgeToken, c)
		assert.Contains(t, out, in.id, c)
	}

	release()

	run := wait()
	require.Zero(t, run.code, run)
	assert.Equal(t, []string{"in_review"}, values(in.ok("task", "show", id), "status:"))

	for _, r := range mock.Requests() {
		assert.NotContains(t, fmt.Sprint(r.Header)+string(r.Body), standIn, r.Path)
	}

	var leaks, marked []string

	data := os.DirFS(in.dir)
	require.NoError(t, fs.WalkDir(data, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}

		b, err := fs.ReadFile(data, p)
		if bytes.Contains(b, []byte(endpointKey)) || bytes.Contains(b, []byte(forgeToken)) {
			leaks = append(leaks, p)
		}

		if bytes.Contains(b, []byte(marker)) {
			marked = append(marked, p)
		}

		return err
	}))
	assert.Empty(t, leaks)
	assert.True(t, slices.ContainsFunc(marked, func(p string) bool { return strings.HasPrefix(p, "artifacts/") }), marked)

	out := in.out.String()
	assert.NotContains(t, out, endpointKey)
	assert.NotContains(t, out, forgeToken)
	assert.Contains(t, out, in.id)
}

func TestHostRunsNoGit(t *testing.T) {
	// The first task's session, then the second's.
	mock := mockapi.Start(t, []mockapi.Step{change, done, change, done})
	in := setup(t, mock.HostURL())

	in.path = t.TempDir()
	require.NoError(t, os.Symlink(in.rt.Bin, filepath.Join(in.path, in.rt.Kind)))

	// Rootless Podman looks its network helper up on PATH.
	if pasta, err := exec.LookPath("pasta"); in.rt.Kind == "podman" && err == nil {
		require.NoError(t, os.Symlink(pasta, filepath.Join(in.path, "pasta")))
	}

	id, run := in.pipeline("notes")
	require.Zero(t, run.code, run)
	assert.Equal(t, []string{"in_review"}, values(in.ok("task", "show", id), "status:"))

	// A git first on the inherited PATH records each call and fails it.
	rec := t.TempDir()
	calls := filepath.Join(rec, "calls")
	require.NoError(t, os.WriteFile(filepath.Join(rec, "git"), []byte("#!/bin/sh\necho \"$@\" >> '"+calls+"'\nexit 1\n"), 0o700))
	in.path = rec + string(os.PathListSeparator) + os.Getenv("PATH")

	id = in.task("notes")
	run = in.cavu("", "task", "run", id)
	require.Zero(t, run.code, run)
	assert.Equal(t, []string{"in_review"}, values(in.ok("task", "show", id), "status:"))
	assert.NoFileExists(t, calls)
}
