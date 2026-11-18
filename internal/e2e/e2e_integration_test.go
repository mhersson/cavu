//go:build integration

package e2e

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"

	"github.com/mhersson/cavu/internal/sandbox"
	"github.com/mhersson/cavu/internal/testkit/gitrepo"
	"github.com/mhersson/cavu/internal/testkit/mockapi"
	"github.com/mhersson/cavu/internal/testkit/sandboxtest"
)

const (
	// verify passes on the fixture repository and on every change that keeps its first line,
	// while CAVU_BASE_COMMIT names a commit in the workspace.
	verify = `grep -qx 'first line' notes.txt && git cat-file -e "$CAVU_BASE_COMMIT^{commit}"`
	// marker is the line the change step adds; title is every task's.
	marker = "CAVU-E2E-LINE"
	title  = "Add a line"
	// endpointKey and forgeToken are the credentials every child holds.
	endpointKey = "sk-test-REALKEY-0123456789"
	forgeToken  = "ghp_TESTFORGETOKEN0123456789" //nolint:gosec // a test value
)

// change and done are one session's model calls: a commit that adds a line to
// notes.txt, then the structured result of finished work. ready is a
// readiness turn's one model call, which finds nothing missing. readDiff and
// passed are a review's: it reads the diff CAVU wrote for it, then passes the
// change.
var (
	change = mockapi.Step{Tool: "Bash", Input: map[string]any{
		"command": "echo " + marker + " >> notes.txt && git commit -qam 'docs(notes): add a line'",
	}}
	done = mockapi.Step{Tool: mockapi.Structured, Input: map[string]any{
		"status": "done", "title": title, "summary": "Added a line to notes.txt.",
	}}
	ready    = mockapi.Step{Tool: mockapi.Structured, Input: map[string]any{"status": "ready"}}
	readDiff = mockapi.Step{Tool: "Bash", Input: map[string]any{"command": "cat /run/cavu/review.patch"}}
	passed   = mockapi.Step{Tool: mockapi.Structured, Input: map[string]any{
		"asked": map[string]any{"pass": true, "reason": "It adds the line."}, "built": map[string]any{"pass": true, "reason": "One line."},
		"findings": []any{},
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
// input; out, unless nil, also gets its standard output as it is written.
// ended is closed when it exits, and wait waits for that and returns its
// result, which it keeps the first time. The test's end interrupts a child
// still running, as Ctrl-C does, and kills it a minute later.
func (in *instance) start(stdin string, out io.Writer, args ...string) (cmd *exec.Cmd, ended <-chan struct{}, wait func() result) {
	in.t.Helper()

	var stdout, stderr bytes.Buffer

	cmd = exec.CommandContext(in.t.Context(), child(in.t), args...)
	cmd.Env = append(os.Environ(), "CAVU_DATA_DIR="+in.dir)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = strings.NewReader(stdin), &stdout, &stderr
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = time.Minute

	if out != nil {
		cmd.Stdout = io.MultiWriter(&stdout, out)
	}

	if in.path != "" {
		cmd.Env = append(cmd.Env, "PATH="+in.path)
	}

	require.NoError(in.t, cmd.Start())

	var (
		err  error
		kept sync.Once
	)

	done := make(chan struct{})

	go func() { err = cmd.Wait(); close(done) }()

	return cmd, done, func() result {
		in.t.Helper()

		<-done

		// An interrupted child that exits 0 waits with the context's error.
		if err != nil && !errors.Is(err, context.Canceled) {
			var exit *exec.ExitError
			require.ErrorAs(in.t, err, &exit)
		}

		r := result{stdout: stdout.String(), stderr: stderr.String(), code: cmd.ProcessState.ExitCode()}

		kept.Do(func() { in.out.WriteString(r.String()) })

		return r
	}
}

// cavu runs the child to its end.
func (in *instance) cavu(stdin string, args ...string) result {
	in.t.Helper()

	_, _, wait := in.start(stdin, nil, args...)

	return wait()
}

// runHeld starts task run on id with the mock's first model call held, and
// returns once that call is logged. A run that ends first fails the test with
// its output.
func (in *instance) runHeld(mock *mockapi.Server, id string) (release func(), cmd *exec.Cmd, wait func() result) {
	in.t.Helper()

	release = mock.Hold()
	cmd, ended, wait := in.start("", nil, "task", "run", id)

	if !modelCalled(mock, ended) {
		_ = cmd.Process.Kill() // a run that has not ended is past the bound

		in.t.Fatalf("task run ended, or made no model call within 3 minutes:\n%s", wait())
	}

	return release, cmd, wait
}

// modelCalls is every model call mock has logged, in arrival order.
func modelCalls(mock *mockapi.Server) []mockapi.Request {
	return slices.DeleteFunc(mock.Requests(), func(r mockapi.Request) bool { return !r.ModelCall })
}

// modelCalled waits until mock has logged a model call, and reports false
// once the child whose ended is given ends or 3 minutes pass first.
func modelCalled(mock *mockapi.Server, ended <-chan struct{}) bool {
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()

	limit := time.After(3 * time.Minute)

	for len(modelCalls(mock)) == 0 {
		select {
		case <-ended:
			return false
		case <-limit:
			return false
		case <-tick.C:
		}
	}

	return true
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

// project adds a project on the fixture repository and checks it. The
// readiness turn and the review are off, since no test but the pipeline's is
// about them, unless switches, each NAME=on or NAME=off, turn them on.
func (in *instance) project(name string, switches ...string) {
	in.t.Helper()

	in.ok("project", "add", name, "--repo", in.repo, "--verify", verify)

	set := []string{"project", "set", name, "--switch", "readiness=off", "--switch", "review=off"}
	for _, s := range switches {
		set = append(set, "--switch", s)
	}

	in.ok(set...)
	in.ok("project", "check", name)
}

// task adds a task to project and returns its id.
func (in *instance) task(project string) string {
	in.t.Helper()

	return strings.TrimSpace(in.ok("task", "add", project, "--title", title))
}

// pipeline adds and checks a project with switches, then adds and runs a task
// on it. It returns the task's id and the run's result.
func (in *instance) pipeline(project string, switches ...string) (string, result) {
	in.t.Helper()

	in.project(project, switches...)
	id := in.task(project)

	return id, in.cavu("", "task", "run", id)
}

// containers lists the instance's containers.
func (in *instance) containers() []string {
	in.t.Helper()

	all, _, err := in.rt.List(in.t.Context(), "cavu.instance="+in.id)
	require.NoError(in.t, err)

	return all
}

// container is the instance's one container of kind, such as agent or sidecar.
func (in *instance) container(kind string) string {
	in.t.Helper()

	all := in.containers()
	named := slices.DeleteFunc(slices.Clone(all), func(c string) bool { return !strings.HasPrefix(c, "cavu-"+in.id[:8]+"-"+kind+"-") })
	require.Len(in.t, named, 1, all)

	return named[0]
}

// runs counts the task's run rows in the child's database.
func (in *instance) runs(task string) int {
	in.t.Helper()

	db, err := sql.Open("sqlite", filepath.Join(in.dir, "cavu.db"))
	require.NoError(in.t, err)

	defer func() { _ = db.Close() }()

	var n int
	require.NoError(in.t, db.QueryRowContext(in.t.Context(), "SELECT count(*) FROM runs WHERE task_id = ?", task).Scan(&n))

	return n
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

// setupScan is a setup command that runs procScan and fileScan in the setup
// container and prints what they find. It fails on a match of found, on what
// they cannot read, and when no process holds a match of planted.
func setupScan(found, planted string) string {
	return fmt.Sprintf(`c=$(sh -c '%[1]s' sh '%[3]s') &&
f=$(sh -c '%[2]s' sh '%[3]s' /workspace /handoff /cache /home/agent /tmp /run/cavu) &&
echo "$c" "$f" &&
test -z "$c$f" &&
p=$(sh -c '%[1]s' sh '%[4]s') &&
test -n "$p"`, procScan, fileScan, found, planted)
}

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

// sessionStart is what the tests read of a session_started event.
type sessionStart struct {
	Mode   string
	Resume bool
}

// apiEvent is what the tests read of an event as the API gives it.
type apiEvent struct {
	ID      int64
	Type    string
	Payload json.RawMessage
}

// shown is the events in out, the output of task show --events, where an
// event line is its id, time, type, actor and payload; another line with five
// fields reads as an event of a type no test asks for.
func shown(out string) []apiEvent {
	var events []apiEvent

	for line := range strings.Lines(out) {
		if e := strings.SplitN(strings.TrimSpace(line), " ", 5); len(e) == 5 {
			events = append(events, apiEvent{Type: e[2], Payload: json.RawMessage(e[4])})
		}
	}

	return events
}

// payloads decodes into T the payload of every event of kind.
func payloads[T any](t *testing.T, events []apiEvent, kind string) []T {
	t.Helper()

	var ps []T

	for _, e := range events {
		if e.Type == kind {
			var p T
			require.NoError(t, json.Unmarshal(e.Payload, &p))
			ps = append(ps, p)
		}
	}

	return ps
}

// sessionStarts returns every session_started event among events.
func sessionStarts(t *testing.T, events []apiEvent) []sessionStart {
	t.Helper()

	return payloads[sessionStart](t, events, "session_started")
}

func TestPipelineEndToEnd(t *testing.T) {
	// The readiness turn, then the work that resumes its session, then a review.
	mock := mockapi.Start(t, []mockapi.Step{ready, change, done, readDiff, passed})
	in := setup(t, mock.HostURL())

	id, run := in.pipeline("notes", "readiness=on", "review=on")
	require.Zero(t, run.code, run)

	show := in.ok("task", "show", id, "--events")
	assert.Equal(t, []string{"in_review"}, values(show, "status:"))
	assert.Equal(t, []sessionStart{{Mode: "readiness"}, {Mode: "start", Resume: true}}, sessionStarts(t, shown(show)))
	assert.Contains(t, strings.Split(in.ok("task", "diff", id), "\n"), "+"+marker)

	var reviews []string

	for _, r := range payloads[struct{ Outcome string }](t, shown(show), "review_ended") {
		reviews = append(reviews, r.Outcome)
	}

	assert.Equal(t, []string{"reviewed"}, reviews, show)

	// The review's second model call carries what its shell read, so the diff
	// was written in its read-only container; the added line is in no earlier call.
	calls := modelCalls(mock)
	require.Len(t, calls, 5)

	for i, r := range calls {
		assert.Equal(t, i == 4, bytes.Contains(r.Body, []byte("+"+marker)), "model call %d", i+1)
	}

	changed := values(show, "changed:")
	require.NotEmpty(t, changed, show)
	assert.Equal(t, "notes.txt", strings.Fields(changed[0])[0], show)

	turns := values(show, "turns:")
	require.Len(t, turns, 1, show)
	n, err := strconv.Atoi(turns[0])
	require.NoError(t, err, show)
	assert.Positive(t, n)

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
	col := strings.Index(lines[header], "VERIFY")
	require.GreaterOrEqual(t, col, 0, r)
	assert.Equal(t, "passed", strings.Fields(rows[0][col:])[0])
}

func TestCredentialsNeverInSandbox(t *testing.T) {
	mock := mockapi.Start(t, []mockapi.Step{change, done})
	in := setup(t, mock.HostURL())

	const planted = "sk-test-PLANTED-0123456789"

	credentials := mark(endpointKey) + "|" + mark(forgeToken)
	in.ok("project", "add", "notes", "--repo", in.repo, "--verify", verify, "--setup", setupScan(credentials, mark(planted)))

	// Without the planted value the setup command fails, so a check that passes ran it.
	r := in.cavu("", "project", "check", "notes")
	require.Equal(t, 1, r.code, r)
	assert.Equal(t, []string{"setup"}, values(r.stdout, "failed step:"))

	// The readiness turn and the review are off: the held call must be the agent phase's.
	in.ok("project", "set", "notes", "--env", "E2E_PLANTED="+planted, "--switch", "readiness=off", "--switch", "review=off")
	in.ok("project", "check", "notes")

	// The run's prepare runs the setup command again, so its exit 0 covers that setup container too.
	id := in.task("notes")

	release, _, wait := in.runHeld(mock, id)

	agent := in.container("agent")
	info, err := in.rt.Inspect(t.Context(), agent)
	require.NoError(t, err)

	dirs := []string{"/tmp"}
	for _, m := range info.Mounts {
		dirs = append(dirs, m.Target)
	}

	files := func(pattern string) string { return in.sh(agent, fileScan, append([]string{pattern}, dirs...)...) }

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

func TestVerifyGateBlocks(t *testing.T) {
	// The change drops the line verify wants; each fix session changes nothing.
	breaks := mockapi.Step{Tool: "Bash", Input: map[string]any{
		"command": "echo " + marker + " > notes.txt && git commit -qam 'docs(notes): rewrite the notes'",
	}}
	mock := mockapi.Start(t, []mockapi.Step{breaks, done, done, done})
	in := setup(t, mock.HostURL())

	id, run := in.pipeline("notes")
	require.Equal(t, 3, run.code, run)

	show := in.ok("task", "show", id, "--events")
	assert.Equal(t, []string{"blocked"}, values(show, "status:"))
	assert.Equal(t, []string{"verify_failed"}, values(show, "stop:"))

	modes := map[string]int{}
	for _, s := range sessionStarts(t, shown(show)) {
		modes[s.Mode]++
	}

	assert.Equal(t, map[string]int{"start": 1, "fix_verify": 2}, modes)
	assert.Equal(t, 1, in.cavu("", "task", "diff", id).code)

	diffs, err := filepath.Glob(filepath.Join(in.dir, "artifacts", "*", "*", "diff.patch"))
	require.NoError(t, err)
	assert.Empty(t, diffs)
}

func TestGatewayBehindCustomCA(t *testing.T) {
	mock := mockapi.StartTLSWith(t, []mockapi.Step{change, done}, sandboxtest.ServerCertificate(t))
	in := setup(t, mock.HostURL())

	id, run := in.pipeline("notes")
	require.Zero(t, run.code, run)
	assert.Equal(t, []string{"in_review"}, values(in.ok("task", "show", id), "status:"))
	assert.Len(t, modelCalls(mock), 2)

	// A network adds a CA, so the configured bundle is no longer the image's.
	ca, _, _ := sandboxtest.CA(t)
	added, _, _ := sandboxtest.CAFromSeed(t, "cavu-test-ca-seed-another-net-ca")
	bundle := filepath.Join(t.TempDir(), "ca.pem")
	require.NoError(t, os.WriteFile(bundle, append(ca, added...), 0o600))
	in.ok("settings", "set", "ca_bundle_path", bundle)

	id = in.task("notes")
	r := in.cavu("", "task", "run", id)
	assert.Equal(t, 1, r.code, r)
	assert.Contains(t, r.stderr, "cavu image build")
	assert.Equal(t, []string{"draft"}, values(in.ok("task", "show", id), "status:"))
	assert.Empty(t, in.containers())
}

// resumed is a session cut off in its first model call, then resumed.
var resumed = []sessionStart{{Mode: "start"}, {Mode: "continue", Resume: true}}

func TestKilledProcessResumes(t *testing.T) {
	// The held call takes the first step, which the killed run never receives.
	mock := mockapi.Start(t, []mockapi.Step{change, change, done})
	in := setup(t, mock.HostURL())

	in.project("notes")
	id := in.task("notes")

	_, cmd, wait := in.runHeld(mock, id)
	require.NoError(t, cmd.Process.Kill())
	wait()

	run := in.cavu("", "task", "run", id)
	require.Zero(t, run.code, run)

	show := in.ok("task", "show", id, "--events")
	assert.Equal(t, []string{"in_review"}, values(show, "status:"))
	assert.Equal(t, resumed, sessionStarts(t, shown(show)))
	assert.Equal(t, 1, in.runs(id))
	assert.Empty(t, in.containers())
}

func TestKilledSidecarResumes(t *testing.T) {
	// The held call takes the first step, which the cut session never receives.
	mock := mockapi.Start(t, []mockapi.Step{change, change, done})
	in := setup(t, mock.HostURL())

	in.project("notes")
	id := in.task("notes")

	_, _, wait := in.runHeld(mock, id)
	in.client("kill", in.container("sidecar"))

	// The runner fault stops the task, and the same task run retries it after a pause.
	run := wait()
	require.Zero(t, run.code, run)

	show := in.ok("task", "show", id, "--events")
	assert.Equal(t, []string{"in_review"}, values(show, "status:"))
	assert.Equal(t, resumed, sessionStarts(t, shown(show)))
	assert.Equal(t, 1, in.runs(id))
	assert.Empty(t, in.containers())

	var steps []string

	for line := range strings.Lines(show) {
		e := strings.SplitN(strings.TrimSpace(line), " ", 5)
		if len(e) < 5 {
			continue
		}

		switch e[2] {
		case "session_started":
			steps = append(steps, e[2])
		case "stopped":
			var stop struct{ Reason string }
			require.NoError(t, json.Unmarshal([]byte(e[4]), &stop))
			steps = append(steps, e[2]+" "+stop.Reason)
		}
	}

	assert.Equal(t, []string{"session_started", "stopped sandbox", "session_started"}, steps,
		"the sandbox stop comes between the cut session and the resumed one")
}
