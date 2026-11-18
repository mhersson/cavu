//go:build unix

// Package git runs the git commands a run needs inside the sandbox's containers:
// it fetches a repository into a mirror, clones a workspace from it and pushes
// a task's branch from it, with the forge token reaching git only through the
// environment. Only the sidecar calls it, inside a container; host-side code
// never runs git.
package git

import (
	"os/exec"
	"syscall"
)

// setProcessGroup starts the command in its own process group, which a cancel
// sends one SIGTERM: git removes its locks on it, and a process that ignores it
// ends when the clone's container is removed.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }
}
