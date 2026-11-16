//go:build unix

package proc

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const helperEnv = "PROC_HELPER"

func TestMain(m *testing.M) {
	if mode := os.Getenv(helperEnv); mode != "" {
		runHelper(mode)
		os.Exit(0)
	}

	os.Exit(m.Run())
}

func spawnSleeper() *exec.Cmd {
	gc := exec.Command(os.Args[0]) //nolint:gosec // re-execs the test binary

	gc.Env = append(os.Environ(), helperEnv+"=sleep")
	gc.Stdout = os.Stdout

	return gc
}

// block parks the helper; a bare select{} would die of the runtime's deadlock
// detector instead of waiting for a signal.
func block() {
	for {
		time.Sleep(time.Hour)
	}
}

func runHelper(mode string) {
	switch mode {
	case "lines":
		fmt.Println("one")
		fmt.Println("two")
		fmt.Println("three")
	case "long":
		fmt.Println(strings.Repeat("x", 5<<20))
		fmt.Println("ok")
	case "long2k":
		fmt.Println(strings.Repeat("y", 2048))
		// Keep writing so a reader that stopped draining would hang the child.
		for {
			fmt.Println(strings.Repeat("y", 2048))
		}
	case "group":
		gc := spawnSleeper()
		if err := gc.Start(); err != nil {
			os.Exit(2)
		}

		_ = os.WriteFile(os.Getenv("PROC_PIDFILE"), []byte(strconv.Itoa(gc.Process.Pid)), 0o600) //nolint:gosec // test-supplied path

		fmt.Println("ready")

		block()
	case "leaderexits":
		gc := spawnSleeper()
		if err := gc.Start(); err != nil {
			os.Exit(2)
		}

		_ = os.WriteFile(os.Getenv("PROC_PIDFILE"), []byte(strconv.Itoa(gc.Process.Pid)), 0o600) //nolint:gosec // test-supplied path

		fmt.Println("hi")
	case "ignoreterm":
		signal.Ignore(syscall.SIGTERM)
		fmt.Println("ready")

		block()
	case "stderr":
		for range 40 {
			fmt.Fprint(os.Stderr, strings.Repeat("a", 1023)+"\n")
		}

		fmt.Fprint(os.Stderr, "END")
	case "flood":
		for {
			fmt.Println("flood")
		}
	case "sleep":
		time.Sleep(300 * time.Second)
	case "feed":
		_, _ = io.Copy(os.Stdout, os.NewFile(3, "feed"))
	case "cat":
		_, _ = io.Copy(os.Stdout, os.Stdin)
	case "env":
		fmt.Print(strings.Join(os.Environ(), "\n"))
	case "exit3":
		os.Exit(3)
	case "selfkill":
		_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)

		block()
	}
}

func helperCommand(t *testing.T, mode string, extra ...string) Command {
	t.Helper()

	exe, err := os.Executable()
	require.NoError(t, err)

	return Command{
		Argv: []string{exe},
		Env:  append(append(os.Environ(), helperEnv+"="+mode), extra...),
	}
}

func startHelper(t *testing.T, mode string, extra ...string) Process {
	t.Helper()

	return startCommand(t, helperCommand(t, mode, extra...))
}

func startCommand(t *testing.T, c Command) Process {
	t.Helper()

	p, err := Local{}.Start(t.Context(), c)
	require.NoError(t, err)

	t.Cleanup(func() { _ = p.Stop(100 * time.Millisecond) })

	return p
}

func readPID(t *testing.T, path string) int {
	t.Helper()

	var pid int

	require.Eventually(t, func() bool {
		b, err := os.ReadFile(path)
		if err != nil {
			return false
		}

		pid, err = strconv.Atoi(string(b))

		return err == nil
	}, 5*time.Second, 20*time.Millisecond)

	return pid
}

func requireGone(t *testing.T, pid int) {
	t.Helper()

	require.Eventually(t, func() bool {
		return syscall.Kill(pid, 0) == syscall.ESRCH
	}, 2*time.Second, 20*time.Millisecond)
}

type exit struct {
	code int
	err  error
}

func waitWithin(t *testing.T, p Process, d time.Duration) exit {
	t.Helper()

	ch := make(chan exit, 1)

	go func() {
		code, err := p.Wait()
		ch <- exit{code, err}
	}()

	select {
	case e := <-ch:
		return e
	case <-time.After(d):
		require.FailNow(t, "Wait did not return in time")

		return exit{}
	}
}

func TestProcLines(t *testing.T) {
	t.Parallel()

	p := startHelper(t, "lines")

	var got []string
	for l := range p.Lines() {
		got = append(got, string(l))
	}

	assert.Equal(t, []string{"one", "two", "three"}, got)

	code, err := p.Wait()
	require.NoError(t, err)
	assert.Equal(t, 0, code)
}

func TestProcLongLines(t *testing.T) {
	t.Parallel()

	t.Run("within cap", func(t *testing.T) {
		t.Parallel()

		p := startHelper(t, "long")

		var got [][]byte
		for l := range p.Lines() {
			got = append(got, l)
		}

		require.Len(t, got, 2)
		assert.Equal(t, bytes.Repeat([]byte("x"), 5<<20), got[0])
		assert.Equal(t, "ok", string(got[1]))

		_, err := p.Wait()
		require.NoError(t, err)
	})

	t.Run("over cap", func(t *testing.T) {
		t.Parallel()

		p, err := start(helperCommand(t, "long2k"), 1024)
		require.NoError(t, err)
		t.Cleanup(func() { _ = p.Stop(100 * time.Millisecond) })

		closed := make(chan struct{})

		go func() {
			for range p.Lines() {
			}

			close(closed)
		}()

		select {
		case <-closed:
		case <-time.After(5 * time.Second):
			require.FailNow(t, "Lines did not close")
		}

		e := waitWithin(t, p, 5*time.Second)
		require.Error(t, e.err)
		assert.Contains(t, e.err.Error(), "1024")
	})
}

func TestProcStopKillsGroup(t *testing.T) {
	t.Parallel()

	pidFile := t.TempDir() + "/pid"
	p := startHelper(t, "group", "PROC_PIDFILE="+pidFile)
	pid := readPID(t, pidFile)

	require.NoError(t, p.Stop(500*time.Millisecond))
	requireGone(t, pid)
}

func TestProcStopEscalates(t *testing.T) {
	t.Parallel()

	p := startHelper(t, "ignoreterm")

	select {
	case <-p.Lines():
	case <-time.After(5 * time.Second):
		require.FailNow(t, "helper not ready")
	}

	require.NoError(t, p.Stop(200*time.Millisecond))

	e := waitWithin(t, p, 2*time.Second)
	assert.Equal(t, 128+int(syscall.SIGKILL), e.code)
}

func TestProcStopIdempotentAfterExit(t *testing.T) {
	t.Parallel()

	p := startHelper(t, "lines")

	for range p.Lines() {
	}

	_, err := p.Wait()
	require.NoError(t, err)
	require.NoError(t, p.Stop(100*time.Millisecond))
	require.NoError(t, p.Stop(100*time.Millisecond))
}

func TestProcStderrTail(t *testing.T) {
	t.Parallel()

	p := startHelper(t, "stderr")

	for range p.Lines() {
	}

	_, err := p.Wait()
	require.NoError(t, err)

	tail := p.StderrTail()
	assert.Len(t, tail, 16<<10)
	assert.True(t, strings.HasSuffix(tail, "aaa\nEND"))
}

func TestProcWaitKillsLeftoverGroup(t *testing.T) {
	t.Parallel()

	pidFile := t.TempDir() + "/pid"
	p := startHelper(t, "leaderexits", "PROC_PIDFILE="+pidFile)
	pid := readPID(t, pidFile)

	require.NoError(t, waitWithin(t, p, 5*time.Second).err)
	requireGone(t, pid)
}

func TestProcWaitAfterStopWithoutReader(t *testing.T) {
	t.Parallel()

	p := startHelper(t, "flood")

	// Let the reader block on an undelivered line.
	time.Sleep(200 * time.Millisecond)

	require.NoError(t, p.Stop(200*time.Millisecond))
	_ = waitWithin(t, p, 5*time.Second)
}

func TestProcWaitExitCode(t *testing.T) {
	t.Parallel()

	t.Run("non-zero exit is not an error", func(t *testing.T) {
		t.Parallel()

		e := waitWithin(t, startHelper(t, "exit3"), 5*time.Second)
		require.NoError(t, e.err)
		assert.Equal(t, 3, e.code)
	})

	t.Run("death by signal is 128 plus the signal", func(t *testing.T) {
		t.Parallel()

		e := waitWithin(t, startHelper(t, "selfkill"), 5*time.Second)
		require.NoError(t, e.err)
		assert.Equal(t, 128+int(syscall.SIGKILL), e.code)
	})
}

func TestStdoutWriterTakesWholeOutput(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer

	c := helperCommand(t, "long")
	c.Stdout = &out
	p := startCommand(t, c)

	select {
	case _, open := <-p.Lines():
		assert.False(t, open, "Lines must be closed when Stdout is set")
	case <-time.After(time.Second):
		require.FailNow(t, "Lines did not close")
	}

	require.NoError(t, waitWithin(t, p, 5*time.Second).err)
	assert.Equal(t, strings.Repeat("x", 5<<20)+"\nok\n", out.String())
}

func TestFeedArrivesOnFd3(t *testing.T) {
	t.Parallel()

	t.Run("read by the child", func(t *testing.T) {
		t.Parallel()

		// Larger than a pipe buffer, with NUL bytes and newlines.
		feed := bytes.Repeat([]byte("KEY=value\x00\n"), 20000)

		var out bytes.Buffer

		c := helperCommand(t, "feed")
		c.Feed = feed
		c.Stdout = &out

		require.NoError(t, waitWithin(t, startCommand(t, c), 5*time.Second).err)
		assert.Equal(t, feed, out.Bytes())
	})

	t.Run("unread by the child", func(t *testing.T) {
		t.Parallel()

		c := helperCommand(t, "lines")
		c.Feed = bytes.Repeat([]byte("x"), 1<<20)
		c.Stdout = io.Discard

		require.NoError(t, waitWithin(t, startCommand(t, c), 5*time.Second).err)
	})
}

func TestEnvIsComplete(t *testing.T) {
	t.Parallel()

	exe, err := os.Executable()
	require.NoError(t, err)

	var out bytes.Buffer

	p := startCommand(t, Command{Argv: []string{exe}, Env: []string{helperEnv + "=env", "ONLY=this"}, Stdout: &out})
	require.NoError(t, waitWithin(t, p, 5*time.Second).err)

	assert.ElementsMatch(t, []string{helperEnv + "=env", "ONLY=this"}, strings.Split(out.String(), "\n"))
}

func TestLocalRefusesBadCommands(t *testing.T) {
	t.Parallel()

	good := helperCommand(t, "lines")

	cases := map[string]Command{
		"empty argv":     {Env: good.Env},
		"missing binary": {Argv: []string{"/nonexistent/binary"}},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			p, err := Local{}.Start(t.Context(), c)
			require.Error(t, err)

			// A typed nil would make a later p != nil check pass.
			if p != nil {
				t.Errorf("a failed start returned a non-nil %T", p)
			}
		})
	}

	t.Run("done context", func(t *testing.T) {
		t.Parallel()

		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		_, err := Local{}.Start(ctx, good)
		require.ErrorIs(t, err, context.Canceled)
	})
}

func TestLocalContextBoundsOnlyTheStart(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())

	c := helperCommand(t, "cat")
	p, err := Local{}.Start(ctx, c)
	require.NoError(t, err)
	t.Cleanup(func() { _ = p.Stop(100 * time.Millisecond) })

	cancel()

	_, err = io.WriteString(p.Stdin(), "still here\n")
	require.NoError(t, err)

	select {
	case l := <-p.Lines():
		assert.Equal(t, "still here", string(l))
	case <-time.After(5 * time.Second):
		require.FailNow(t, "process did not outlive its start context")
	}

	require.NoError(t, p.Stdin().Close())
	require.NoError(t, waitWithin(t, p, 5*time.Second).err)
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestStderrWriterAndTail(t *testing.T) {
	for name, w := range map[string]io.Writer{"works": &bytes.Buffer{}, "fails": failingWriter{}} {
		t.Run(name, func(t *testing.T) {
			p, err := Local{}.Start(t.Context(), Command{
				Argv:   []string{"sh", "-c", "echo one >&2; echo two >&2"},
				Env:    os.Environ(),
				Stderr: w,
			})
			require.NoError(t, err)

			code, err := p.Wait()
			require.NoError(t, err)
			assert.Equal(t, 0, code)
			assert.Equal(t, "one\ntwo\n", p.StderrTail())

			if b, ok := w.(*bytes.Buffer); ok {
				assert.Equal(t, "one\ntwo\n", b.String())
			}
		})
	}
}
