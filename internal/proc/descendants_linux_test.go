//go:build linux

package proc

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestProcStopKillsEscapedDescendant(t *testing.T) {
	t.Parallel()

	pidFile := t.TempDir() + "/pid"
	p := startHelper(t, "escape", "PROC_PIDFILE="+pidFile)
	pid := readPID(t, pidFile)

	t.Cleanup(func() {
		if !t.Failed() {
			return
		}

		if err := signalable(pid); err != nil {
			t.Error(err)

			return
		}

		_ = syscall.Kill(pid, syscall.SIGKILL)
	})

	select {
	case <-p.Lines():
	case <-time.After(5 * time.Second):
		require.FailNow(t, "helper not ready")
	}

	require.NoError(t, p.Stop(200*time.Millisecond))
	requireGone(t, pid)
}

var errRefusedSignal = errors.New("refused to signal")

// signalable refuses a pid read from a file unless it is above 1 and neither
// this process nor its process group.
func signalable(pid int) error {
	if pid <= 1 || pid == os.Getpid() || pid == syscall.Getpgrp() {
		return fmt.Errorf("%w: %d", errRefusedSignal, pid)
	}

	return nil
}

func TestSignalableRefusesUnsafeIDs(t *testing.T) {
	t.Parallel()

	for _, pid := range []int{-1, 0, 1, os.Getpid(), syscall.Getpgrp()} {
		require.ErrorIs(t, signalable(pid), errRefusedSignal, pid)
	}

	// No pid reaches 1<<22, the largest pid_max Linux allows.
	require.NoError(t, signalable(1<<22))
}
