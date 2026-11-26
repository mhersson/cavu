//go:build unix

package proc

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// keptTarget starts on the host and keeps the process it started.
type keptTarget struct{ p Process }

func (k *keptTarget) Start(ctx context.Context, c Command) (Process, error) {
	p, err := Local{}.Start(ctx, c)
	k.p = p

	return p, err
}

func shell(script string) Command {
	return Command{Argv: []string{"sh", "-c", script}, Env: os.Environ()}
}

func TestRunExitCode(t *testing.T) {
	t.Parallel()

	t.Run("zero is no error", func(t *testing.T) {
		t.Parallel()

		require.NoError(t, Run(t.Context(), Local{}, shell("exit 0"), nil))
	})

	t.Run("non-zero is an ExitError with the stderr tail", func(t *testing.T) {
		t.Parallel()

		err := Run(t.Context(), Local{}, shell("echo why >&2; exit 3"), nil)

		e, ok := errors.AsType[*ExitError](err)
		require.True(t, ok, "%v", err)
		assert.Equal(t, 3, e.Code)
		assert.Equal(t, "why", e.Stderr)
	})

	t.Run("a command that cannot start is no ExitError", func(t *testing.T) {
		t.Parallel()

		err := Run(t.Context(), Local{}, Command{Argv: []string{"/nonexistent/binary"}}, nil)
		require.Error(t, err)

		_, ok := errors.AsType[*ExitError](err)
		assert.False(t, ok)
	})
}

func TestRunWritesAndClosesStdin(t *testing.T) {
	t.Parallel()

	// cat ends only once its stdin is closed.
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	var out bytes.Buffer

	c := helperCommand(t, "cat")
	c.Stdout = &out

	require.NoError(t, Run(ctx, Local{}, c, strings.NewReader("in\n")))
	assert.Equal(t, "in\n", out.String())
}

func TestRunStopsWhenContextEnds(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()

	target := &keptTarget{}
	begun := time.Now()

	require.ErrorIs(t, Run(ctx, target, helperCommand(t, "sleep"), nil), context.DeadlineExceeded)
	assert.Less(t, time.Since(begun), stopGrace+5*time.Second)

	// The command has ended, and by the TERM a stop sends first.
	assert.Equal(t, 128+int(syscall.SIGTERM), waitWithin(t, target.p, time.Second).code)
}
