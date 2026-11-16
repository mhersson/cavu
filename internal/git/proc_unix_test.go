//go:build unix

package git

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/mhersson/cavu/internal/testkit/gitrepo"
)

// A ref write cancelled while git holds its locks leaves none behind: git
// removes them on SIGTERM but not on SIGKILL, and a packed-refs.lock left in
// the user's repository fails their own git until they delete it.
func TestCancelledRefWriteLeavesNoLock(t *testing.T) {
	t.Parallel()

	root := gitrepo.New(t, map[string]string{"f": "x\n"})

	oid := gitrepo.Git(t, root, "rev-parse", "HEAD")
	gitrepo.Git(t, root, "update-ref", "refs/test/a", oid)
	gitrepo.Git(t, root, "update-ref", "refs/test/b", oid)
	gitrepo.Git(t, root, "pack-refs", "--all")

	// git runs the hook with "prepared" once it holds the transaction's locks.
	started := filepath.Join(t.TempDir(), "started")
	writeExecutable(t, filepath.Join(root, ".git", "hooks", "reference-transaction"),
		"#!/bin/sh\n[ \"$1\" = prepared ] || exit 0\n: > '"+started+"'\nexec sleep 30\n")

	_, err := run(cancelWhenWritten(t, started), call{
		dir:  root,
		args: []string{"update-ref", "-d", "refs/test/a", oid},
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want canceled", err)
	}

	if locks := lockFiles(t, filepath.Join(root, ".git")); len(locks) > 0 {
		t.Errorf("lock files left in the repository: %q", locks)
	}
}

// A write call whose processes ignore SIGTERM is still stopped: the kill
// follows once the grace is over.
func TestCancelKillsAGroupThatIgnoresTerm(t *testing.T) {
	t.Parallel()

	root := gitrepo.New(t, map[string]string{"f": "x\n"})
	script, logPath := writeGitWrapper(t, `trap '' TERM
sleep 30 &
echo $! > "$LOG.tmp" && mv "$LOG.tmp" "$LOG.pid"
wait`)
	ctx := context.WithValue(cancelWhenWritten(t, logPath+".pid"), gitPathKey{}, script)

	_, err := run(ctx, call{dir: root, args: []string{"update-ref", "refs/test/x", "HEAD"}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want canceled", err)
	}

	pid := readPID(t, logPath+".pid")
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })

	eventually(t, "the child that ignores SIGTERM to die", func() bool { return !alive(pid) })
}

// A cancelled read holds no lock, so it is killed at once without SIGTERM's
// grace, and the re-kill still reaches children forked during the kill.
func TestCancelledReadGetsNoTerm(t *testing.T) {
	t.Parallel()

	root := gitrepo.New(t, map[string]string{"f": "x\n"})
	script, logPath := writeGitWrapper(t, `trap ': > "$LOG.term"' TERM
echo $$ > "$LOG.pgid"
i=0
while [ $i -lt 300 ]; do
	sh -c 'echo $$ >> "$1"; exec sleep 30' sh "$LOG.pids" &
	i=$((i+1))
done
wait`)

	t.Cleanup(func() {
		if raw, err := os.ReadFile(logPath + ".pgid"); err == nil {
			if pgid, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil {
				_ = syscall.Kill(-pgid, syscall.SIGKILL)
			}
		}
	})

	// The first child's pid lands while the wrapper is still forking.
	ctx := context.WithValue(cancelWhenWritten(t, logPath+".pids"), gitPathKey{}, script)

	_, err := run(ctx, call{dir: root, args: []string{"status"}, read: true})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want canceled", err)
	}

	if _, err := os.Stat(logPath + ".term"); err == nil {
		t.Error("a cancelled read got SIGTERM")
	}

	raw, err := os.ReadFile(logPath + ".pids")
	if err != nil {
		t.Fatal(err)
	}

	for field := range strings.FieldsSeq(string(raw)) {
		pid, err := strconv.Atoi(field)
		if err != nil {
			t.Fatal(err)
		}

		eventually(t, "child "+field+" to die", func() bool { return !alive(pid) })
	}
}
