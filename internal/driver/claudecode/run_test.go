package claudecode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// localTarget creates the parent directories of a command's files, as a
// container's copy-in does, then starts it with proc.Local.
type localTarget struct {
	env []string // added to every command

	mu       sync.Mutex
	commands []proc.Command
	existed  []string // files already on disk when the command arrived
}

func (l *localTarget) Start(ctx context.Context, c proc.Command) (proc.Process, error) {
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

	return proc.Local{}.Start(ctx, c)
}

func (l *localTarget) started() []proc.Command {
	l.mu.Lock()
	defer l.mu.Unlock()

	return slices.Clone(l.commands)
}

func testDriver(t *testing.T) claudeCode {
	t.Helper()

	bin, err := os.Executable()
	require.NoError(t, err)

	return claudeCode{binary: bin, runRoot: t.TempDir(), gateTimeout: 10 * time.Second, grace: stopGrace}
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
	target *localTarget
	spec   driver.Spec
	record string

	once   sync.Once
	first  chan struct{} // closed at the first event
	events []driver.Event
}

// newFakeRun returns a subscription-mode run of the fake claude with sc.
func newFakeRun(t *testing.T, sc fakeclaude.Scenario) *fakeRun {
	t.Helper()

	if sc.Record == "" {
		sc.Record = filepath.Join(t.TempDir(), "record.json")
	}

	if sc.Version == "" {
		sc.Version = "2.1.322"
	}

	return &fakeRun{
		d:      testDriver(t),
		target: &localTarget{},
		spec: driver.Spec{
			Auth:      driver.Auth{Mode: modeSubscription, Token: testToken},
			Model:     testModel,
			MaxTurns:  10,
			SessionID: testSessionID,
			Prompt:    testPrompt,
			Dir:       t.TempDir(),
			Home:      t.TempDir(),
			ConfigDir: t.TempDir(),
			Env:       fakeEnv(t, sc),
		},
		record: sc.Record,
		first:  make(chan struct{}),
	}
}

func keyAuth() driver.Auth {
	return driver.Auth{Mode: modeKey, Token: testStandIn, BaseURL: testBridgeURL}
}

func (f *fakeRun) run(ctx context.Context) (driver.Result, error) {
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
	assert.Contains(t, res.Detail, "initialize")
	assert.Less(t, elapsed, f.d.gateTimeout+f.d.grace)

	rec := f.recorded(t)
	assert.Empty(t, rec.Stdin)
	assert.True(t, processGone(rec.PID))
}

func TestResume(t *testing.T) {
	dir, configDir := t.TempDir(), t.TempDir()

	a := newFakeRun(t, fakeclaude.Scenario{Replay: fixture(t, "real/t1.jsonl")})
	a.spec.Dir, a.spec.ConfigDir = dir, configDir

	res, err := a.run(t.Context())
	require.NoError(t, err)
	require.Equal(t, driver.ReasonDone, res.Reason)

	b := newFakeRun(t, fakeclaude.Scenario{Replay: fixture(t, "real/t1.jsonl")})
	b.spec.Dir, b.spec.ConfigDir = dir, configDir
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
	pidPath := filepath.Join(t.TempDir(), "grandchild.pid")
	f := newFakeRun(t, fakeclaude.Scenario{
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
	assert.Less(t, returned.Sub(<-cancelledAt), f.d.grace)

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

	cmds := f.target.started()
	require.Len(t, cmds, 1)
	assert.Equal(t, slices.Sorted(slices.Values([]string{settings, instructions})), slices.Sorted(maps.Keys(cmds[0].Files)))
	assert.JSONEq(t, `{"attribution":{"commit":"","pr":"","sessionUrl":false}}`, string(cmds[0].Files[settings]))
	assert.Equal(t, testInstr, string(cmds[0].Files[instructions]))
	assert.Empty(t, f.target.existed, "the driver wrote a file itself")

	assert.JSONEq(t, `{"attribution":{"commit":"","pr":"","sessionUrl":false}}`, rec.RunFiles["--settings"])
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
