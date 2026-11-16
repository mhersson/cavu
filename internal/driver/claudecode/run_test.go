package claudecode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/cavu/internal/driver"
	"github.com/mhersson/cavu/internal/proc"
	"github.com/mhersson/cavu/internal/testkit/fakeclaude"
)

const (
	testToken     = "test-oauth-token-7f3a91" //nolint:gosec // a dummy
	testStandIn   = "test-stand-in-token-c41d"
	testBridgeURL = "http://127.0.0.1:3128"
	testPrompt    = "PROMPT-say-ok-5e2b"
	testInstr     = "INSTRUCTIONS-be-careful-9a0c"
	testSessionID = "0b9d3c5e-7a41-4f2e-9c8b-1d2e3f4a5b6c"
	testModel     = "claude-haiku-5-5"
)

// localTarget runs the fake claude on the host and nothing else. It refuses,
// as a test failure and without running anything, any other program and a
// command whose dir, files or config dir, or whose scenario's record or
// grandchild file, lie outside root. It creates the parent directories of a
// command's files, as a container's copy-in does, and runs a command that
// names no dir in root.
type localTarget struct {
	binary string                           // the fake claude: this test binary
	root   string                           // a t.TempDir() of the test; every path the fake uses
	env    []string                         // added to every command
	fail   func(format string, args ...any) // t.Errorf; the test of the refusals swaps it

	mu       sync.Mutex
	commands []proc.Command
	existed  []string // files already on disk when the command arrived
}

// newLocalTarget takes root from the caller's own t.TempDir(), which lies
// strictly below the system's temporary directory.
func newLocalTarget(t *testing.T, root string, env ...string) *localTarget {
	t.Helper()

	rel, err := filepath.Rel(os.TempDir(), root)
	require.NoError(t, err)
	require.True(t, filepath.IsAbs(root) && rel != "." && !strings.HasPrefix(rel, ".."),
		"root %q is not strictly below the temporary directory", root)

	bin, err := os.Executable()
	require.NoError(t, err)

	return &localTarget{binary: bin, root: root, env: env, fail: t.Errorf}
}

func (l *localTarget) Start(ctx context.Context, c proc.Command) (proc.Process, error) {
	if why := l.refusal(c); why != "" {
		l.fail("localTarget refused %q: %s", c.Argv, why)

		return nil, fmt.Errorf("localTarget refused %q: %s", c.Argv, why)
	}

	l.mu.Lock()
	l.commands = append(l.commands, c)

	for p := range c.Files {
		if _, err := os.Stat(p); err == nil {
			l.existed = append(l.existed, p)
		}
	}
	l.mu.Unlock()

	for p := range c.Files {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			return nil, err
		}
	}

	c.Env = append(slices.Clone(c.Env), l.env...)
	if c.Dir == "" {
		c.Dir = l.root
	}

	return proc.Local{}.Start(ctx, c)
}

// refusal says why c must not run on the host, empty when it may. Without
// fakeclaude.EnvVar the test binary would run the tests, not the fake; the
// fake writes its transcripts into CLAUDE_CONFIG_DIR.
func (l *localTarget) refusal(c proc.Command) string {
	env := append(slices.Clone(c.Env), l.env...)

	if len(c.Argv) == 0 || c.Argv[0] != l.binary {
		return "not the fake claude"
	}

	if !slices.ContainsFunc(env, func(kv string) bool { return strings.HasPrefix(kv, fakeclaude.EnvVar+"=") }) {
		return "no fake scenario"
	}

	if c.Dir != "" && !l.holds(c.Dir) {
		return "its dir is outside root"
	}

	for p := range c.Files {
		if !l.holds(p) {
			return "a file is outside root"
		}
	}

	for _, kv := range env {
		switch k, v, _ := strings.Cut(kv, "="); k {
		case "CLAUDE_CONFIG_DIR":
			if !l.holds(v) {
				return "its config dir is outside root"
			}
		case fakeclaude.EnvVar:
			if why := l.scenarioRefusal(v); why != "" {
				return why
			}
		}
	}

	return ""
}

// scenarioRefusal says why the scenario file p must not reach the fake. The
// fake writes its record and its grandchild's pid beside their paths, with a
// ".tmp" suffix, then renames them into place, so each path must lie strictly
// below root. A missing file is allowed: the fake then fails to load it and
// writes nothing.
func (l *localTarget) scenarioRefusal(p string) string {
	if !filepath.IsAbs(p) {
		return "its scenario path is relative"
	}

	data, err := os.ReadFile(p) //nolint:gosec // a scenario the test wrote
	if errors.Is(err, fs.ErrNotExist) {
		return ""
	}

	var sc fakeclaude.Scenario
	if err != nil || json.Unmarshal(data, &sc) != nil {
		return "its scenario cannot be read"
	}

	for _, w := range []string{sc.Record, sc.Grandchild} {
		if w != "" && (!l.holds(w) || filepath.Clean(w) == filepath.Clean(l.root)) {
			return "its scenario writes outside root"
		}
	}

	return ""
}

// holds reports whether p is l.root or below it.
func (l *localTarget) holds(p string) bool {
	rel, err := filepath.Rel(l.root, p)

	return err == nil && filepath.IsAbs(p) && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func (l *localTarget) started() []proc.Command {
	l.mu.Lock()
	defer l.mu.Unlock()

	return slices.Clone(l.commands)
}

// routedTarget sends the fake claude to local and every other command to rec,
// so nothing but the fake runs on the host. It keeps every command in order.
type routedTarget struct {
	local *localTarget
	rec   *recordingTarget

	mu       sync.Mutex
	commands []proc.Command
}

func (r *routedTarget) Start(ctx context.Context, c proc.Command) (proc.Process, error) {
	r.mu.Lock()
	r.commands = append(r.commands, c)
	r.mu.Unlock()

	if len(c.Argv) > 0 && c.Argv[0] == r.local.binary {
		return r.local.Start(ctx, c)
	}

	return r.rec.Start(ctx, c)
}

func (r *routedTarget) started() []proc.Command {
	r.mu.Lock()
	defer r.mu.Unlock()

	return slices.Clone(r.commands)
}

// The kinds of command the recording target answers.
const (
	kindClear   = "clear"
	kindWrite   = "write"
	kindListing = "ls-tree"
	kindNested  = "nested"
	kindBatch   = "cat-file --batch"
)

// commandKind names c among the commands staging starts, empty for any other.
func commandKind(c proc.Command) string {
	argv := c.Argv

	switch {
	case len(argv) < 3:
		return ""
	case argv[2] == proc.SandboxOnly(clearScript):
		return kindClear
	case argv[2] == proc.SandboxOnly(writeScript):
		return kindWrite
	case argv[2] == nestedScript:
		return kindNested
	case argv[0] == "git" && argv[1] == "ls-tree":
		return kindListing
	case argv[0] == "git" && argv[1] == "cat-file":
		return kindBatch
	}

	return ""
}

// recordingTarget executes nothing and writes nothing. It records every
// command with the bytes written to its stdin and answers each kind from a
// script: the clear and the write exit 0 unless exits says otherwise, and the
// git reads answer for tree, a canned base commit.
type recordingTarget struct {
	t     *testing.T
	tree  fakeTree
	exits map[string]int // by command kind

	mu       sync.Mutex
	commands []*recorded
}

// recorded is one command the recording target saw; stdin is set once the
// command's stdin is closed.
type recorded struct {
	proc.Command

	kind  string
	stdin []byte
}

func newRecordingTarget(t *testing.T) *recordingTarget {
	t.Helper()

	return &recordingTarget{t: t, exits: map[string]int{}}
}

func (r *recordingTarget) Start(ctx context.Context, c proc.Command) (proc.Process, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	rec := &recorded{Command: c, kind: commandKind(c)}

	r.mu.Lock()
	r.commands = append(r.commands, rec)
	r.mu.Unlock()

	if rec.kind == "" {
		r.t.Errorf("the recording target has no answer for %q", c.Argv)

		return nil, errors.New("no scripted answer")
	}

	return &scriptedProcess{out: c.Stdout, done: make(chan struct{}), answer: func(stdin []byte) scripted {
		r.mu.Lock()
		rec.stdin = stdin
		r.mu.Unlock()

		return r.answer(rec.kind, c.Argv, stdin)
	}}, nil
}

func (r *recordingTarget) answer(kind string, argv []string, stdin []byte) scripted {
	if code := r.exits[kind]; code != 0 {
		return scripted{code: code, stderr: fmt.Sprintf("scripted exit %d", code)}
	}

	switch kind {
	case kindListing:
		return scripted{stdout: r.tree.listing(argv[slices.Index(argv, "--")+1:])}
	case kindNested:
		return scripted{stdout: r.tree.nested()}
	case kindBatch:
		return scripted{stdout: r.tree.batch(stdin)}
	}

	return scripted{}
}

// seen returns the commands recorded so far.
func (r *recordingTarget) seen() []*recorded {
	r.mu.Lock()
	defer r.mu.Unlock()

	return slices.Clone(r.commands)
}

// ofKind returns the commands of kind seen so far.
func (r *recordingTarget) ofKind(kind string) []*recorded {
	var out []*recorded

	for _, c := range r.seen() {
		if c.kind == kind {
			out = append(out, c)
		}
	}

	return out
}

// scripted is a recorded command's answer.
type scripted struct {
	stdout []byte
	code   int
	stderr string
}

// scriptedProcess answers once its stdin is closed, writing its stdout to out
// whole, or ends with 143 when stopped first.
type scriptedProcess struct {
	out    io.Writer
	answer func(stdin []byte) scripted

	mu    sync.Mutex
	stdin bytes.Buffer
	once  sync.Once
	done  chan struct{}
	res   scripted
	err   error
}

func (p *scriptedProcess) Stdin() io.WriteCloser { return scriptedStdin{p} }

func (p *scriptedProcess) Lines() <-chan []byte {
	lines := make(chan []byte)
	close(lines)

	return lines
}

func (p *scriptedProcess) StderrTail() string {
	select {
	case <-p.done:
		return p.res.stderr
	default:
		return ""
	}
}

func (p *scriptedProcess) Stop(time.Duration) error {
	p.end(func() { p.res = scripted{code: 143} })

	return nil
}

func (p *scriptedProcess) Wait() (int, error) {
	<-p.done

	return p.res.code, p.err
}

func (p *scriptedProcess) end(f func()) {
	p.once.Do(func() {
		f()
		close(p.done)
	})
}

type scriptedStdin struct{ p *scriptedProcess }

func (s scriptedStdin) Write(b []byte) (int, error) {
	s.p.mu.Lock()
	defer s.p.mu.Unlock()

	return s.p.stdin.Write(b)
}

func (s scriptedStdin) Close() error {
	s.p.end(func() {
		s.p.mu.Lock()
		in := bytes.Clone(s.p.stdin.Bytes())
		s.p.mu.Unlock()

		s.p.res = s.p.answer(in)
		if s.p.out != nil && len(s.p.res.stdout) > 0 {
			_, s.p.err = s.p.out.Write(s.p.res.stdout)
		}
	})

	return nil
}

// testDriver has no run root: a run's files then have relative paths, which
// localTarget refuses, until a fake run sets one below its root.
func testDriver(t *testing.T) claudeCode {
	t.Helper()

	bin, err := os.Executable()
	require.NoError(t, err)

	return claudeCode{binary: bin, gateTimeout: 10 * time.Second, grace: stopGrace}
}

func writeScenario(t *testing.T, sc fakeclaude.Scenario) string {
	t.Helper()

	b, err := json.Marshal(sc)
	require.NoError(t, err)

	p := filepath.Join(t.TempDir(), "scenario.json")
	require.NoError(t, os.WriteFile(p, b, 0o600))

	return p
}

// fakeEnv selects the fake with sc. A race-built fake would otherwise sleep a
// second at exit.
func fakeEnv(t *testing.T, sc fakeclaude.Scenario) []string {
	t.Helper()

	return []string{fakeclaude.EnvVar + "=" + writeScenario(t, sc), "GORACE=atexit_sleep_ms=0"}
}

// fixture is the absolute path of a testdata file; the fake runs in Spec.Dir.
func fixture(t *testing.T, rel string) string {
	t.Helper()

	p, err := filepath.Abs(filepath.Join("testdata", rel))
	require.NoError(t, err)

	return p
}

type fakeRun struct {
	d      claudeCode
	target *routedTarget
	spec   driver.Spec
	record string

	once   sync.Once
	first  chan struct{} // closed at the first event
	events []driver.Event
}

// newFakeRun returns a subscription-mode run of the fake claude with sc, in a
// root of its own.
func newFakeRun(t *testing.T, sc fakeclaude.Scenario) *fakeRun {
	t.Helper()

	return newFakeRunIn(t, t.TempDir(), sc)
}

// newFakeRunIn returns a subscription-mode run of the fake claude with sc.
// Its dir, home, config dir, run root and default record are in a fresh
// directory below root, the only tree its localTarget lets the fake use. Only
// the fake runs on the host; the staging's commands are recorded.
func newFakeRunIn(t *testing.T, root string, sc fakeclaude.Scenario) *fakeRun {
	t.Helper()

	base, err := os.MkdirTemp(root, "run")
	require.NoError(t, err)

	dir, home := filepath.Join(base, "dir"), filepath.Join(base, "home")
	require.NoError(t, os.Mkdir(dir, 0o700))
	require.NoError(t, os.Mkdir(home, 0o700))

	if sc.Record == "" {
		sc.Record = filepath.Join(base, "record.json")
	}

	if sc.Version == "" {
		sc.Version = "2.1.322"
	}

	d := testDriver(t)
	d.runRoot = filepath.Join(base, "run")

	return &fakeRun{
		d:      d,
		target: &routedTarget{local: newLocalTarget(t, root), rec: newRecordingTarget(t)},
		spec: driver.Spec{
			Auth:      driver.Auth{Mode: modeSubscription, Token: testToken},
			Model:     testModel,
			MaxTurns:  10,
			SessionID: testSessionID,
			Prompt:    testPrompt,
			Dir:       dir,
			Home:      home,
			ConfigDir: home + "/.claude-cavu",
			Env:       fakeEnv(t, sc),
		},
		record: sc.Record,
		first:  make(chan struct{}),
	}
}

func keyAuth() driver.Auth {
	return driver.Auth{Mode: modeKey, Token: testStandIn, BaseURL: testBridgeURL}
}

// testBound caps every wait in these tests, so a hang fails the test instead
// of running into the go test timeout.
const testBound = 30 * time.Second

func bounded(t *testing.T) context.Context {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), testBound)
	t.Cleanup(cancel)

	return ctx
}

func (f *fakeRun) run(ctx context.Context) (driver.Result, error) {
	ctx, cancel := context.WithTimeout(ctx, testBound)
	defer cancel()

	return f.d.Run(ctx, f.target, f.spec, func(ev driver.Event) {
		f.events = append(f.events, ev)
		f.once.Do(func() { close(f.first) })
	})
}

func (f *fakeRun) recorded(t *testing.T) fakeclaude.Recorded {
	t.Helper()

	data, err := os.ReadFile(f.record)
	require.NoError(t, err)

	var rec fakeclaude.Recorded
	require.NoError(t, json.Unmarshal(data, &rec))

	return rec
}

// flagValue returns the element after flag, failing when absent.
func flagValue(t *testing.T, args []string, flag string) string {
	t.Helper()

	i := slices.Index(args, flag)
	require.GreaterOrEqual(t, i, 0, "missing %s in %v", flag, args)
	require.Less(t, i+1, len(args), "no value after %s", flag)

	return args[i+1]
}

// processGone reports whether pid no longer runs; an unreaped zombie counts.
func processGone(pid int) bool {
	if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
		return true
	}

	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}

	_, after, ok := bytes.Cut(stat, []byte(") "))

	return ok && len(after) > 0 && after[0] == 'Z'
}

func readPID(t *testing.T, p string) int {
	t.Helper()

	data, err := os.ReadFile(p)
	require.NoError(t, err)

	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	require.NoError(t, err)

	return pid
}

func TestPromptAfterInitialize(t *testing.T) {
	f := newFakeRun(t, fakeclaude.Scenario{Replay: fixture(t, "real/t1.jsonl"), InitDelay: 300 * time.Millisecond})

	res, err := f.run(t.Context())
	require.NoError(t, err)
	assert.Equal(t, driver.ReasonDone, res.Reason)

	rec := f.recorded(t)
	assert.False(t, rec.EarlyInput, "a user line was written before claude answered initialize")
	assert.JSONEq(t, string(userMessage(testPrompt)), rec.Stdin)
}

func TestInitializeTimeout(t *testing.T) {
	f := newFakeRun(t, fakeclaude.Scenario{NoInit: true})
	f.d.gateTimeout = 300 * time.Millisecond

	start := time.Now()
	res, err := f.run(t.Context())
	elapsed := time.Since(start)

	require.NoError(t, err)
	assert.Equal(t, driver.ReasonCrashed, res.Reason)
	assert.True(t, res.IsError)
	assert.NotEmpty(t, res.Detail)
	assert.GreaterOrEqual(t, elapsed, f.d.gateTimeout, "the run ended before the gate timed out")
	assert.Less(t, elapsed, f.d.gateTimeout+f.d.grace)

	rec := f.recorded(t)
	assert.Empty(t, rec.Stdin)
	assert.True(t, processGone(rec.PID))
}

func TestInitializeErrorAnswer(t *testing.T) {
	f := newFakeRun(t, fakeclaude.Scenario{
		Replay: fixture(t, "real/t1.jsonl"), InitError: "initialize refused for " + testToken,
	})

	res, err := f.run(t.Context())
	require.NoError(t, err)
	assert.Equal(t, driver.ReasonCrashed, res.Reason)
	assert.True(t, res.IsError)
	assert.Contains(t, res.Detail, "initialize refused for", "the detail carries claude's error")
	assert.NotContains(t, res.Detail, testToken)

	rec := f.recorded(t)
	assert.Empty(t, rec.Stdin, "the prompt went to a process whose initialize failed")
	assert.True(t, processGone(rec.PID))
	assert.Empty(t, ofKind(f.events, driver.KindStarted))
}

func TestResume(t *testing.T) {
	root := t.TempDir()

	a := newFakeRunIn(t, root, fakeclaude.Scenario{Replay: fixture(t, "real/t1.jsonl")})

	res, err := a.run(t.Context())
	require.NoError(t, err)
	require.Equal(t, driver.ReasonDone, res.Reason)

	b := newFakeRunIn(t, root, fakeclaude.Scenario{Replay: fixture(t, "real/t1.jsonl")})
	b.spec.Dir, b.spec.Home, b.spec.ConfigDir = a.spec.Dir, a.spec.Home, a.spec.ConfigDir
	b.spec.SessionID, b.spec.Resume = "", testSessionID

	res, err = b.run(t.Context())
	require.NoError(t, err)
	assert.Equal(t, driver.ReasonDone, res.Reason)
	assert.Equal(t, testSessionID, res.SessionID)

	args := b.recorded(t).Args
	assert.Equal(t, testSessionID, flagValue(t, args, "--resume"))
	assert.NotContains(t, args, "--session-id")
}

func TestResumeRefused(t *testing.T) {
	f := newFakeRun(t, fakeclaude.Scenario{Replay: fixture(t, "real/t1.jsonl")})
	f.spec.SessionID, f.spec.Resume = "", testSessionID

	res, err := f.run(t.Context())
	require.NoError(t, err)
	assert.Equal(t, driver.ReasonResumeRefused, res.Reason)
	assert.True(t, res.IsError)

	// The same words from a new session are a crash.
	g := newFakeRun(t, fakeclaude.Scenario{
		ExitAtStart: true, ExitCode: 1, Stderr: "No conversation found with session ID: " + testSessionID + "\n",
	})

	res, err = g.run(t.Context())
	require.NoError(t, err)
	assert.Equal(t, driver.ReasonCrashed, res.Reason)
}

func TestCancelInterrupts(t *testing.T) {
	root := t.TempDir()
	pidPath := filepath.Join(root, "grandchild.pid")
	f := newFakeRunIn(t, root, fakeclaude.Scenario{
		Replay: fixture(t, "synthetic/crash.jsonl"), Hang: true, Grandchild: pidPath,
	})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	cancelledAt := make(chan time.Time, 1)

	go func() {
		select {
		case <-f.first:
			cancelledAt <- time.Now()

			cancel()
		case <-ctx.Done():
		}
	}()

	res, err := f.run(ctx)
	returned := time.Now()

	require.NoError(t, err)
	assert.Equal(t, driver.ReasonInterrupted, res.Reason)
	assert.True(t, res.IsError)

	// The cancel follows the send, so an interrupted run finds the time here.
	select {
	case at := <-cancelledAt:
		assert.Less(t, returned.Sub(at), f.d.grace)
	default:
		t.Fatal("the run ended before its first event")
	}

	grandchild := readPID(t, pidPath)
	assert.Eventually(t, func() bool { return processGone(grandchild) }, 2*time.Second, 20*time.Millisecond,
		"grandchild %d outlived the run", grandchild)
	assert.True(t, processGone(f.recorded(t).PID))
}

func TestOutcomeIgnoresExitCode(t *testing.T) {
	tests := []struct {
		name    string
		sc      fakeclaude.Scenario
		reason  string
		isError bool
	}{
		{"success result exit 1", fakeclaude.Scenario{Replay: "real/t1.jsonl", ExitCode: 1}, driver.ReasonDone, false},
		{"error result exit 0", fakeclaude.Scenario{Replay: "synthetic/max_turns.jsonl"}, driver.ReasonMaxTurns, true},
		{"no result exit 0", fakeclaude.Scenario{ExitAtStart: true}, driver.ReasonCrashed, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.sc.Replay != "" {
				tt.sc.Replay = fixture(t, tt.sc.Replay)
			}

			res, err := newFakeRun(t, tt.sc).run(t.Context())
			require.NoError(t, err)
			assert.Equal(t, tt.reason, res.Reason)
			assert.Equal(t, tt.isError, res.IsError)
		})
	}
}

// settingsJSON is the settled --settings file: attribution and every hook off.
const settingsJSON = `{"attribution":{"commit":"","pr":"","sessionUrl":false},"disableAllHooks":true}`

func TestFilesWrittenThroughCommand(t *testing.T) {
	f := newFakeRun(t, fakeclaude.Scenario{Replay: fixture(t, "real/t1.jsonl")})
	f.spec.Instructions = testInstr

	res, err := f.run(t.Context())
	require.NoError(t, err)
	require.Equal(t, driver.ReasonDone, res.Reason)

	rec := f.recorded(t)
	settings := flagValue(t, rec.Args, "--settings")
	instructions := flagValue(t, rec.Args, "--append-system-prompt-file")

	assert.Equal(t, "settings.json", path.Base(settings))
	assert.Equal(t, path.Dir(settings), path.Dir(instructions))
	assert.Equal(t, f.d.runRoot, path.Dir(path.Dir(settings)))

	// The config dir's clear, then claude.
	cmds := f.target.started()
	require.Len(t, cmds, 2)
	assert.Equal(t, kindClear, commandKind(cmds[0]))
	assert.Empty(t, cmds[0].Files)

	claude := cmds[1]
	assert.Equal(t, slices.Sorted(slices.Values([]string{settings, instructions})), slices.Sorted(maps.Keys(claude.Files)))
	assert.JSONEq(t, settingsJSON, string(claude.Files[settings]))
	assert.Equal(t, testInstr, string(claude.Files[instructions]))
	assert.Empty(t, f.target.local.existed, "the driver wrote a file itself")

	assert.JSONEq(t, settingsJSON, rec.RunFiles["--settings"])
	assert.Equal(t, testInstr, rec.RunFiles["--append-system-prompt-file"])
}

func TestUsageLimitStopsTheProcess(t *testing.T) {
	f := newFakeRun(t, fakeclaude.Scenario{Replay: fixture(t, "synthetic/rejected.jsonl"), Hang: true})

	start := time.Now()
	res, err := f.run(t.Context())

	require.NoError(t, err)
	assert.Less(t, time.Since(start), f.d.grace+time.Second)
	assert.Equal(t, driver.ReasonUsageLimit, res.Reason)
	assert.True(t, processGone(f.recorded(t).PID))
}

func TestRunMasksToken(t *testing.T) {
	f := newFakeRun(t, fakeclaude.Scenario{
		ExitAtStart: true, ExitCode: 1, Stderr: "bad credentials " + testToken + "\n",
	})

	res, err := f.run(t.Context())
	require.NoError(t, err)
	assert.Equal(t, driver.ReasonCrashed, res.Reason)
	assert.Contains(t, res.Detail, "bad credentials")
	assert.NotContains(t, res.Detail, testToken)
}

// requireNoFragment fails when s holds any run of n characters of secret.
func requireNoFragment(t *testing.T, s, secret string, n int) {
	t.Helper()

	for i := 0; i+n <= len(secret); i++ {
		require.NotContains(t, s, secret[i:i+n])
	}
}

// A stderr tail cut at a byte count can open with the end of the token,
// which masking alone cannot recognise.
func TestRunDropsCutToken(t *testing.T) {
	f := newFakeRun(t, fakeclaude.Scenario{
		ExitAtStart: true, ExitCode: 1, Stderr: testToken[6:] + " trailing\nnext " + testToken + "\n",
	})

	res, err := f.run(t.Context())
	require.NoError(t, err)
	assert.Equal(t, driver.ReasonCrashed, res.Reason)
	requireNoFragment(t, res.Detail, testToken, 8)
	assert.Contains(t, res.Detail, "trailing")
	assert.Contains(t, res.Detail, "next")
}

func TestStderrDetail(t *testing.T) {
	tests := []struct {
		name, tail, want string
	}{
		{"cut token end", testToken[3:] + " rest\n", "rest"},
		{"shortest stripped end", testToken[len(testToken)-minFragment:] + " rest", "rest"},
		{"short coincidental match kept", testToken[len(testToken)-minFragment+1:] + " rest", testToken[len(testToken)-minFragment+1:] + " rest"},
		{"last character kept", testToken[len(testToken)-1:] + "rror: connect", testToken[len(testToken)-1:] + "rror: connect"},
		{"whole token stays for the mask", testToken + " rest", testToken + " rest"},
		{"no token", "plain failure\n", "plain failure"},
		{"invalid utf-8", "\xa9broken \xff tail", "broken  tail"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, stderrDetail(tt.tail, testToken))
		})
	}

	assert.Equal(t, "rest", stderrDetail(" rest ", ""), "an empty secret strips nothing")
}

func TestCancelBeforeStart(t *testing.T) {
	f := newFakeRun(t, fakeclaude.Scenario{Replay: fixture(t, "real/t1.jsonl")})

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	res, err := f.run(ctx)
	require.NoError(t, err)
	assert.Equal(t, driver.ReasonInterrupted, res.Reason)
	assert.True(t, res.IsError)
	assert.Empty(t, f.target.started(), "a cancelled run started a process")
}

// localTarget refuses, as a test failure and without starting anything, any
// program but the fake and any path the fake would use outside its root. Each
// refused command would be harmless if it ran, and the path outside is in a
// directory this test created.
func TestLocalTargetRefuses(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "refused")

	scenario := func(t *testing.T, sc fakeclaude.Scenario) []string {
		t.Helper()

		sc.Version = "2.1.325"

		return fakeEnv(t, sc)
	}

	for _, tc := range []struct {
		name   string
		mutate func(*testing.T, *localTarget, *proc.Command)
	}{
		{"another program", func(_ *testing.T, _ *localTarget, c *proc.Command) { c.Argv = []string{"true"} }},
		{"a shell", func(_ *testing.T, _ *localTarget, c *proc.Command) { c.Argv = []string{"sh", "-c", "exit 0"} }},
		{"no program", func(_ *testing.T, _ *localTarget, c *proc.Command) { c.Argv = nil }},
		// Run without a scenario, the test binary would run no test.
		{"no fake scenario", func(_ *testing.T, l *localTarget, c *proc.Command) {
			l.env, c.Argv = nil, []string{l.binary, "-test.run=^$"}
		}},
		{"a dir outside", func(_ *testing.T, _ *localTarget, c *proc.Command) { c.Dir = "/" }},
		{"a dir escaping with ..", func(_ *testing.T, l *localTarget, c *proc.Command) { c.Dir = l.root + "/.." }},
		{"a relative dir", func(_ *testing.T, _ *localTarget, c *proc.Command) { c.Dir = "." }},
		{"a file outside", func(_ *testing.T, _ *localTarget, c *proc.Command) {
			c.Files = map[string][]byte{outside: []byte("x")}
		}},
		{"a config dir outside", func(_ *testing.T, _ *localTarget, c *proc.Command) { c.Env = []string{"CLAUDE_CONFIG_DIR=/"} }},
		{"a record outside", func(t *testing.T, l *localTarget, _ *proc.Command) {
			l.env = scenario(t, fakeclaude.Scenario{Record: outside})
		}},
		{"a relative record", func(t *testing.T, l *localTarget, _ *proc.Command) {
			l.env = scenario(t, fakeclaude.Scenario{Record: "record.json"})
		}},
		// The fake would write the record's temporary file beside the root.
		{"the root as the record", func(t *testing.T, l *localTarget, _ *proc.Command) {
			l.env = scenario(t, fakeclaude.Scenario{Record: l.root})
		}},
		{"a grandchild outside", func(t *testing.T, l *localTarget, _ *proc.Command) {
			l.env = scenario(t, fakeclaude.Scenario{Grandchild: outside})
		}},
		{"a relative scenario", func(_ *testing.T, l *localTarget, _ *proc.Command) {
			l.env = []string{fakeclaude.EnvVar + "=scenario.json"}
		}},
		{"a scenario that does not decode", func(t *testing.T, l *localTarget, _ *proc.Command) {
			p := filepath.Join(t.TempDir(), "scenario.json")
			require.NoError(t, os.WriteFile(p, []byte("{"), 0o600))

			l.env = []string{fakeclaude.EnvVar + "=" + p}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := newLocalTarget(t, t.TempDir(), scenario(t, fakeclaude.Scenario{})...)

			var failures []string

			l.fail = func(format string, args ...any) { failures = append(failures, fmt.Sprintf(format, args...)) }

			c := proc.Command{Argv: []string{l.binary, "--version"}, Dir: l.root}
			tc.mutate(t, l, &c)

			p, err := l.Start(bounded(t), c)
			require.Error(t, err)
			assert.Nil(t, p)
			assert.Len(t, failures, 1, "the refusal is no test failure")
			assert.Empty(t, l.started())
			assert.NoFileExists(t, outside)
			assert.NoFileExists(t, outside+".tmp")
		})
	}

	// Started, a relative file would land in the package directory, so this
	// refusal is checked without Start.
	t.Run("a relative file", func(t *testing.T) {
		l := newLocalTarget(t, t.TempDir(), scenario(t, fakeclaude.Scenario{})...)

		c := proc.Command{Argv: []string{l.binary, "--version"}, Dir: l.root, Files: map[string][]byte{"refused.txt": []byte("x")}}
		assert.NotEmpty(t, l.refusal(c))
	})

	t.Run("the fake whose files are in its root runs", func(t *testing.T) {
		root := t.TempDir()
		env := scenario(t, fakeclaude.Scenario{
			Record: filepath.Join(root, "record.json"), Grandchild: filepath.Join(root, "grandchild.pid"),
		})

		got, err := testDriver(t).Probe(bounded(t), newLocalTarget(t, root, env...))
		require.NoError(t, err)
		assert.Equal(t, "2.1.325", got)
	})
}
