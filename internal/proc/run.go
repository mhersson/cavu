//go:build unix

package proc

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"
)

// stopGrace is the time between TERM and KILL when Wait stops a command.
const stopGrace = 2 * time.Second

// ExitError is a command that ended with a non-zero exit code.
type ExitError struct {
	Code   int
	Stderr string // the stderr tail, trimmed
}

func (e *ExitError) Error() string { return fmt.Sprintf("exit %d: %s", e.Code, e.Stderr) }

// Run starts c on t, copies stdin (unless nil) to it and closes it, and waits
// for its end as Wait does. The copy outlives Run until a read or write of it
// fails, so a caller that streams stdin closes the stream once Run returns. A
// non-zero exit is an *ExitError.
func Run(ctx context.Context, t Target, c Command, stdin io.Reader) error {
	p, err := t.Start(ctx, c)
	if err != nil {
		return err
	}

	// A command that never reads its stdin must not block the wait.
	go func() {
		if stdin != nil {
			_, _ = io.Copy(p.Stdin(), stdin)
		}

		_ = p.Stdin().Close()
	}()

	code, err := Wait(ctx, p)
	if err != nil {
		return err
	}

	if code != 0 {
		return &ExitError{Code: code, Stderr: strings.TrimSpace(p.StderrTail())}
	}

	return nil
}

// Wait waits for p to end. When ctx ends first it stops p, waits for it and
// returns ctx's error.
func Wait(ctx context.Context, p Process) (int, error) {
	var (
		code int
		err  error
	)

	done := make(chan struct{})

	go func() {
		code, err = p.Wait()

		close(done)
	}()

	select {
	case <-done:
		return code, err
	case <-ctx.Done():
		_ = p.Stop(stopGrace)

		<-done

		return 0, ctx.Err()
	}
}
