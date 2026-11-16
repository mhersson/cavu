//go:build unix

package git

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/mhersson/cavu/internal/testkit/gitrepo"
)

// A ref write cancelled while git holds its locks leaves none behind: git
// removes them on SIGTERM but not on SIGKILL, and a packed-refs.lock left in
// a mirror or workspace volume fails every later fetch into it.
func TestCancelledRefWriteLeavesNoLock(t *testing.T) {
	t.Parallel()

	root := gitrepo.New(t, map[string]string{"f": "x\n"})

	oid := gitrepo.Git(t, root, "rev-parse", "HEAD")
	gitrepo.Git(t, root, "update-ref", "refs/test/a", oid)
	gitrepo.Git(t, root, "update-ref", "refs/test/b", oid)
	gitrepo.Git(t, root, "pack-refs", "--all")

	// git runs the hook with "prepared" once it holds the transaction's locks.
	// Its arguments are git's, so it writes its marker next to itself, from $0.
	hooks := filepath.Join(root, ".git", "hooks")
	started := filepath.Join(hooks, "started")
	writeExecutable(t, filepath.Join(hooks, "reference-transaction"),
		"#!/bin/sh\n[ \"$1\" = prepared ] || exit 0\n: > \"${0%/*}/started\"\nexec sleep 30\n")

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

// A cancel reaches git's children as well as git: the whole process group gets
// the SIGTERM.
func TestCancelStopsTheProcessGroup(t *testing.T) {
	t.Parallel()

	root := gitrepo.New(t, map[string]string{"f": "x\n"})
	script, logPath := writeGitWrapper(t, `sleep 30 &
echo $! > "$LOG.tmp" && mv "$LOG.tmp" "$LOG.pid"
wait`)
	ctx := context.WithValue(cancelWhenWritten(t, logPath+".pid"), gitPathKey{}, script)

	_, err := run(ctx, call{dir: root, args: []string{"update-ref", "refs/test/x", "HEAD"}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want canceled", err)
	}

	pid := readPID(t, logPath+".pid")
	t.Cleanup(func() { killProcess(t, pid) })

	eventually(t, "the child to die", func() bool { return !alive(pid) })
}
