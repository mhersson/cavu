package git

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// writeGitWrapper writes a git wrapper script and returns its path and its log.
// The script appends its arguments, one call per line, to the log, runs
// extraShell (which may use $LOG), then execs the real git.
func writeGitWrapper(t *testing.T, extraShell string) (script, logPath string) {
	t.Helper()

	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not found")
	}

	dir := t.TempDir()
	logPath = filepath.Join(dir, "calls.log")
	body := "#!/bin/sh\nLOG='" + logPath + "'\nprintf '%s ' \"$@\" >> \"$LOG\"\necho >> \"$LOG\"\n" +
		extraShell + "\nexec '" + realGit + "' \"$@\"\n"
	script = filepath.Join(dir, "git")

	writeExecutable(t, script, body)

	return script, logPath
}

// writeExecutable writes an executable file; the fork lock keeps a parallel
// test's fork from inheriting the write descriptor (ETXTBSY, go.dev/issue/22315).
func writeExecutable(t *testing.T, path, body string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}

	syscall.ForkLock.RLock()

	err := os.WriteFile(path, []byte(body), 0o755)

	syscall.ForkLock.RUnlock()

	if err != nil {
		t.Fatal(err)
	}
}

// wrapGit returns a context whose git calls go through the logging wrapper.
// It leaves PATH alone, so the test can run in parallel.
func wrapGit(t *testing.T, extraShell string) (ctx context.Context, logPath string) {
	t.Helper()

	script, logPath := writeGitWrapper(t, extraShell)

	return context.WithValue(context.Background(), gitPathKey{}, script), logPath
}

var errRefusedSignal = errors.New("refused to signal")

// signalable refuses a pid or pgid read from a file unless it is above 1 and
// neither this process nor its group: as a group, 1 reaches every process the
// user owns and 0 this test's own group.
func signalable(id int) error {
	if id <= 1 || id == os.Getpid() || id == syscall.Getpgrp() {
		return fmt.Errorf("%w: %d", errRefusedSignal, id)
	}

	return nil
}

// killProcess sends SIGKILL to pid, read from a file, or fails the test
// without a signal when signalable refuses it.
func killProcess(t *testing.T, pid int) {
	t.Helper()

	if err := signalable(pid); err != nil {
		t.Error(err)

		return
	}

	_ = syscall.Kill(pid, syscall.SIGKILL)
}

// killGroupIn sends SIGKILL to the process group whose id the file at path
// holds, if it holds one, or fails the test without a signal when signalable
// refuses it.
func killGroupIn(t *testing.T, path string) {
	t.Helper()

	raw, err := os.ReadFile(path)
	if err != nil {
		return
	}

	pgid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		return
	}

	if err := signalable(pgid); err != nil {
		t.Error(err)

		return
	}

	_ = syscall.Kill(-pgid, syscall.SIGKILL)
}

func TestSignalableRefusesUnsafeIDs(t *testing.T) {
	t.Parallel()

	for _, id := range []int{-1, 0, 1, os.Getpid(), syscall.Getpgrp()} {
		if err := signalable(id); !errors.Is(err, errRefusedSignal) {
			t.Errorf("signalable(%d) = %v, want a refusal", id, err)
		}
	}

	// No pid reaches 1<<22, the largest pid_max Linux allows.
	if err := signalable(1 << 22); err != nil {
		t.Errorf("signalable(1<<22) = %v, want nil", err)
	}
}

// alive reports whether pid is a running process (a zombie is not).
func alive(pid int) bool {
	if syscall.Kill(pid, 0) != nil {
		return false
	}

	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return true
	}

	_, rest, _ := strings.Cut(string(stat), ") ")

	return !strings.HasPrefix(rest, "Z")
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()

	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if cond() {
			return
		}
	}

	t.Fatalf("timed out waiting for %s", what)
}

// cancelWhenWritten returns a context that is cancelled once path exists, so a
// test cancels a call at a known point rather than after a guessed delay.
func cancelWhenWritten(t *testing.T, path string) context.Context {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	go func() {
		for ctx.Err() == nil {
			if _, err := os.Stat(path); err == nil {
				cancel()

				return
			}

			time.Sleep(time.Millisecond)
		}
	}()

	return ctx
}

// lockFiles lists the *.lock files under dir, relative to it.
func lockFiles(t *testing.T, dir string) []string {
	t.Helper()

	var locks []string

	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if !d.IsDir() && strings.HasSuffix(d.Name(), ".lock") {
			rel, _ := filepath.Rel(dir, path)
			locks = append(locks, rel)
		}

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	return locks
}

func readPID(t *testing.T, path string) int {
	t.Helper()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}

	return pid
}
