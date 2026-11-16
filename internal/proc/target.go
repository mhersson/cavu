//go:build unix

package proc

import (
	"context"
	"io"
	"time"
)

// Command is one process to start on a Target.
type Command struct {
	Argv []string
	// Env is the complete KEY=VALUE list; in a container, the container's own
	// environment is also visible to the command.
	Env   []string
	Dir   string
	Files map[string][]byte // absolute path -> content, written 0600 before the start; Container only, Local ignores it
	Feed  []byte            // served to the child on file descriptor 3; Local only, a Container refuses it
	// Stdout, when set, receives stdout whole and Lines closes at once.
	Stdout io.Writer
	// Stderr, when set, also receives stderr as it arrives; StderrTail is kept
	// either way. Local only, a Container refuses it. A failing writer loses
	// the copy, never the tail.
	Stderr io.Writer
}

// Process is a started command.
type Process interface {
	Stdin() io.WriteCloser
	// Lines yields stdout one line per item and is closed at the end of output.
	Lines() <-chan []byte
	// StderrTail is the last 16 KiB of stderr.
	StderrTail() string
	Stop(grace time.Duration) error
	// Wait returns the exit code, 128 plus the signal for a death by signal.
	// The error is only for a failure to run or read, never for the code.
	Wait() (exitCode int, err error)
}

// Target is where a Command runs.
type Target interface {
	Start(ctx context.Context, c Command) (Process, error)
}

// Local runs commands on this machine.
type Local struct{}

// Start starts c with c.Feed on file descriptor 3. The context bounds only
// the start; the process lives until Stop or Wait.
func (Local) Start(ctx context.Context, c Command) (Process, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	p, err := start(c, maxLine)
	if err != nil {
		return nil, err
	}

	return p, nil
}
