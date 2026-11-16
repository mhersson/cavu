package sandbox

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/cavu/internal/proc"
)

func TestContainerStartRefuses(t *testing.T) {
	cases := map[string]proc.Command{
		"empty argv":           {},
		"argv starting with -": {Argv: []string{"-x"}},
		"a feed":               {Argv: []string{"true"}, Feed: []byte("x")},
		"relative dir":         {Argv: []string{"true"}, Dir: "workspace"},
		"relative file":        {Argv: []string{"true"}, Files: map[string][]byte{"run/cavu/a": nil}},
	}

	for name, cmd := range cases {
		t.Run(name, func(t *testing.T) {
			r, f := fakeRuntime(t, "docker")

			p, err := r.Container("cavu-agent-r1").Start(t.Context(), cmd)
			require.Error(t, err)

			// A typed nil would make a later p != nil check pass.
			if p != nil {
				t.Errorf("a failed start returned a non-nil %T", p)
			}

			_, err = r.Container("cavu-agent-r1").Run(t.Context(), cmd, time.Minute)
			require.Error(t, err)

			assert.Empty(t, f.calls(t), "the runtime was called")
		})
	}
}

func TestContainerStartArgv(t *testing.T) {
	r, f := fakeRuntime(t, "docker")

	p, err := r.Container("cavu-agent-r1").Start(t.Context(), proc.Command{
		Argv: []string{"git", "status", "--short"},
		Env:  []string{"A=1"},
		Dir:  "/workspace",
	})
	require.NoError(t, err)
	require.NoError(t, p.Stdin().Close())

	code, err := p.Wait()
	require.NoError(t, err)
	assert.Equal(t, 0, code)

	argv := f.only(t).argv
	assert.Equal(t, []string{"exec", "--interactive"}, argv[:2])
	assert.Subset(t, argv, []string{"--env-file=/dev/fd/3", "--workdir=/workspace"})
	assert.Equal(t, []string{"git", "status", "--short"}, argv[len(argv)-3:])

	name := slices.Index(argv, "cavu-agent-r1")
	require.Positive(t, name)
	assert.Equal(t, []string{"sh", "-c"}, argv[name+1:name+3])
}

func TestRunRefusesNonPositiveTimeout(t *testing.T) {
	for _, timeout := range []time.Duration{0, -time.Second} {
		t.Run(timeout.String(), func(t *testing.T) {
			r, f := fakeRuntime(t, "docker")

			_, err := r.Container("cavu-agent-r1").Run(t.Context(), proc.Command{Argv: []string{"true"}}, timeout)
			require.Error(t, err)
			assert.Empty(t, f.calls(t), "the runtime was called")
		})
	}
}

func TestRunHonoursContext(t *testing.T) {
	r, f := fakeRuntime(t, "docker")
	f.set(t, "sleep", []byte("slow-target"))

	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()

	begun := time.Now()
	_, err := r.Container("cavu-agent-r1").Run(ctx, proc.Command{Argv: []string{"make", "slow-target"}}, time.Minute)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, time.Since(begun), 10*time.Second)
}

func TestRunReportsExitAndOutput(t *testing.T) {
	r, f := fakeRuntime(t, "docker")
	f.set(t, "stdout", []byte("hello\n"))
	f.set(t, "exit", []byte("7"))

	res, err := r.Container("cavu-agent-r1").Run(t.Context(), proc.Command{Argv: []string{"make", "test"}}, time.Minute)
	require.NoError(t, err)
	assert.Equal(t, Result{ExitCode: 7, Output: "hello\n"}, res)
}

func TestRunOutputHoldsClientStderr(t *testing.T) {
	const fromClient = "client-stderr-marker\n"

	t.Run("after the command's output", func(t *testing.T) {
		r, f := fakeRuntime(t, "docker")
		f.set(t, "stdout", []byte("partial\n"))
		f.set(t, "stderr", []byte(fromClient))
		f.set(t, "exit", []byte("125"))

		res, err := r.Container("cavu-agent-r1").Run(t.Context(), proc.Command{Argv: []string{"make", "test"}}, time.Minute)
		require.NoError(t, err)
		assert.Equal(t, Result{ExitCode: 125, Output: "partial\n" + fromClient}, res)
	})

	t.Run("within the cap", func(t *testing.T) {
		r, f := fakeRuntime(t, "docker")
		f.set(t, "stdout", bytes.Repeat([]byte("line\n"), 40000))
		f.set(t, "stderr", []byte(fromClient))
		f.set(t, "exit", []byte("1"))

		res, err := r.Container("cavu-agent-r1").Run(t.Context(), proc.Command{Argv: []string{"make", "test"}}, time.Minute)
		require.NoError(t, err)
		assert.LessOrEqual(t, len(res.Output), proc.CaptureCap)
		assert.True(t, strings.HasPrefix(res.Output, "line\n"))
		assert.True(t, strings.HasSuffix(res.Output, "line\n"+fromClient))
	})
}

// groupScript leads its own process group, as proc.Local starts it. It has a
// second member, writes the pid file only after a delay, and both it and the
// member record the TERM they get in the directory $2. The leader waits for
// the member in its trap, so the member's record is never cut short.
const groupScript = `sh -c 'trap "echo > \"$1/member\"; exit 0" TERM; while :; do sleep 0.1; done' sh "$2" &
member=$!
trap 'wait "$member"; echo > "$2/leader"; exit 0' TERM
sleep 0.5
echo $$ > "$1"
while :; do sleep 0.1; done`

func waitFor(t *testing.T, p proc.Process, d time.Duration) int {
	t.Helper()

	type exit struct {
		code int
		err  error
	}

	ch := make(chan exit, 1)

	go func() {
		code, err := p.Wait()
		ch <- exit{code, err}
	}()

	select {
	case e := <-ch:
		require.NoError(t, e.err)

		return e.code
	case <-time.After(d):
		require.FailNow(t, "the process did not end in time")

		return 0
	}
}

func TestStopScriptWaitsForPidFile(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "cmd.pid")

	start := func(argv ...string) proc.Process {
		p, err := proc.Local{}.Start(t.Context(), proc.Command{Argv: argv, Env: os.Environ(), Stdout: io.Discard})
		require.NoError(t, err)
		t.Cleanup(func() { _ = p.Stop(time.Second) })

		return p
	}

	stop := start("sh", "-c", stopScript, "sh", pidFile, "50")
	require.NoFileExists(t, pidFile)

	group := start("sh", "-c", groupScript, "sh", pidFile, dir)

	assert.Equal(t, 0, waitFor(t, stop, 10*time.Second), "stop script")
	assert.Equal(t, 0, waitFor(t, group, 10*time.Second), "group leader")
	assert.FileExists(t, filepath.Join(dir, "leader"), "the leader did not finish its TERM trap")
	assert.FileExists(t, filepath.Join(dir, "member"), "TERM did not reach the other member")
}

// recordKills goes before the stop script and replaces kill, a regular
// builtin, with a function that only records its arguments in $3 and fails,
// so no case can signal anything. With $4 set it first writes its own pid to
// the pid file.
const recordKills = `log=$3
kill() { echo "$*" >> "$log"; return 1; }
[ -z "$4" ] || echo $$ > "$1"
`

func TestStopScriptRefusesAPidFileThatNamesNoGroup(t *testing.T) {
	run := func(t *testing.T, pid string, own bool) (code int, kills string) {
		t.Helper()

		dir := t.TempDir()
		pidFile, log := filepath.Join(dir, "cmd.pid"), filepath.Join(dir, "kills")
		argv := []string{"sh", "-c", recordKills + stopScript, "sh", pidFile, "1", log}

		if own {
			argv = append(argv, "own")
		} else {
			require.NoError(t, os.WriteFile(pidFile, []byte(pid), 0o600))
		}

		p, err := proc.Local{}.Start(t.Context(), proc.Command{Argv: argv, Env: os.Environ(), Dir: dir, Stdout: io.Discard})
		require.NoError(t, err)
		t.Cleanup(func() { _ = p.Stop(time.Second) })

		code = waitFor(t, p, 10*time.Second)

		b, err := os.ReadFile(log)
		if errors.Is(err, fs.ErrNotExist) {
			return code, ""
		}

		require.NoError(t, err)

		return code, string(b)
	}

	// Every later case would signal for real if both its refusal and the
	// replaced kill failed, so they run only once the replacement records.
	require.True(t, t.Run("a pid above 1 reaches kill", func(t *testing.T) {
		code, kills := run(t, "99999999\n", false)
		assert.Equal(t, 0, code)
		require.Equal(t, "-TERM -99999999\n", kills)
	}))

	for name, pid := range map[string]string{
		"empty":          "\n",
		"not a number":   "abc\n",
		"zero":           "0\n",
		"one":            "1\n",
		"negative":       "-5\n",
		"two numbers":    "1 2\n",
		"a leading zero": "01\n",
	} {
		t.Run(name, func(t *testing.T) {
			code, kills := run(t, pid, false)
			assert.Equal(t, stopRefused, code)
			assert.Empty(t, kills, "a refused pid reached kill")
		})
	}

	t.Run("its own pid", func(t *testing.T) {
		code, kills := run(t, "", true)
		assert.Equal(t, stopRefused, code)
		assert.Empty(t, kills, "its own group reached kill")
	})
}

// stopExecs counts the calls that ran the stop script.
func stopExecs(t *testing.T, f *fake) int {
	t.Helper()

	n := 0

	for _, c := range f.calls(t) {
		if slices.Contains(c.argv, stopScript) {
			n++
		}
	}

	return n
}

// TestStopAfterTheClientDied kills the client with proc's 64 MiB line cap,
// which in a container would leave the command running, so Stop must ask
// the stop exec rather than trust the client's end.
func TestStopAfterTheClientDied(t *testing.T) {
	r, f := fakeRuntime(t, "docker")
	f.set(t, "out.big-line", bytes.Repeat([]byte("x"), 65<<20))

	killed := func() proc.Process {
		p, err := r.Container("cavu-agent-r1").Start(t.Context(), proc.Command{Argv: []string{"emit", "big-line"}})
		require.NoError(t, err)
		require.NoError(t, p.Stdin().Close())

		_, err = p.Wait()
		require.Error(t, err, "the line cap kills the client")

		return p
	}

	require.NoError(t, killed().Stop(time.Second), "the stop exec found the group gone")
	assert.Equal(t, 1, stopExecs(t, f))

	p := killed()

	f.set(t, "exit", []byte("3"))

	err := p.Stop(time.Second)
	require.Error(t, err, "the stop exec could not signal")
	require.NotErrorIs(t, err, errSignalRefused)
	assert.Equal(t, 2, stopExecs(t, f))

	f.set(t, "exit", []byte("0"))

	p = killed()

	f.set(t, "exit", []byte(strconv.Itoa(stopRefused)))
	require.ErrorIs(t, p.Stop(time.Second), errSignalRefused)
	assert.Equal(t, 3, stopExecs(t, f))
}

// TestStopThatHadToKillTheClient holds the command exec while the stop exec
// answers at once without ending it, as when something outside the group
// keeps the output open.
func TestStopThatHadToKillTheClient(t *testing.T) {
	r, f := fakeRuntime(t, "docker")
	f.set(t, "sleep", []byte("hold"))

	p, err := r.Container("cavu-agent-r1").Start(t.Context(), proc.Command{Argv: []string{"serve", "hold"}})
	require.NoError(t, err)
	require.NoError(t, p.Stdin().Close())

	require.Error(t, p.Stop(100*time.Millisecond), "the client had to be killed")
	require.Error(t, p.Stop(100*time.Millisecond), "a repeated Stop")
	assert.Equal(t, 2, stopExecs(t, f))
}

func TestRunReportsAFailedStopAtTheTimeout(t *testing.T) {
	r, f := fakeRuntime(t, "docker")
	f.set(t, "sleep", []byte("hold"))

	_, err := r.Container("cavu-agent-r1").Run(t.Context(), proc.Command{Argv: []string{"serve", "hold"}}, 200*time.Millisecond)
	require.Error(t, err, "the command may still run, so Run must not report a plain timeout")
	assert.Equal(t, 1, stopExecs(t, f))
}

// TestStopAfterACleanEndSendsNothing pins the early return that keeps a late
// Stop from signalling a pid the container may have reused.
func TestStopAfterACleanEndSendsNothing(t *testing.T) {
	r, f := fakeRuntime(t, "docker")

	p, err := r.Container("cavu-agent-r1").Start(t.Context(), proc.Command{Argv: []string{"true"}})
	require.NoError(t, err)
	require.NoError(t, p.Stdin().Close())

	code, err := p.Wait()
	require.NoError(t, err)
	require.Equal(t, 0, code)

	require.NoError(t, p.Stop(time.Second))
	require.NoError(t, p.Stop(time.Second))
	assert.Equal(t, 0, stopExecs(t, f))
}
