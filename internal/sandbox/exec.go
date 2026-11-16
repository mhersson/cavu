package sandbox

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mhersson/cavu/internal/proc"
)

// pidDir must exist and be writable by the exec user in every container a
// command runs in; CAVU mounts a tmpfs there.
const pidDir = "/run/cavu"

// execSlack bounds a signalling exec beyond the grace it waits out.
const execSlack = 10 * time.Second

// stopScript waits for the pid file, sends TERM to the command's process
// group, then KILL if the group outlives the grace ($2 in tenths of a second).
// Busybox's kill takes no "--", so the group goes as a bare negative pid.
const stopScript = `f=$1 n=$2
while [ ! -s "$f" ] && [ "$n" -gt 0 ]; do sleep 0.1; n=$((n - 1)); done
[ -s "$f" ] || exit 3
pg=-$(cat "$f")
kill -TERM "$pg" 2>/dev/null || exit 0
while [ "$n" -gt 0 ] && kill -0 "$pg" 2>/dev/null; do sleep 0.1; n=$((n - 1)); done
kill -KILL "$pg" 2>/dev/null
exit 0`

// Container is a running container; it is a proc.Target.
type Container struct {
	r    Runtime
	name string
}

// Container returns the running container called name.
func (r Runtime) Container(name string) Container { return Container{r: r, name: name} }

// Start writes cmd.Files with CopyIn, then execs cmd in the container with
// stdin attached. The context bounds only the start.
func (c Container) Start(ctx context.Context, cmd proc.Command) (proc.Process, error) {
	p, err := c.start(ctx, cmd, false)
	if err != nil {
		return nil, err
	}

	return p, nil
}

// start execs cmd behind a wrapper that records its pid, so Stop can signal
// the command's group from a second exec; an exec'd command leads its own
// group. merge sends the command's stderr to its stdout.
func (c Container) start(ctx context.Context, cmd proc.Command, merge bool) (*execProcess, error) {
	if err := checkArg("container name", c.name); err != nil {
		return nil, err
	}

	if len(cmd.Argv) == 0 || strings.HasPrefix(cmd.Argv[0], "-") {
		return nil, errors.New("command argv: empty or starts with -")
	}

	if len(cmd.Feed) > 0 {
		return nil, errors.New("a container command takes no feed")
	}

	if cmd.Dir != "" && !path.IsAbs(cmd.Dir) {
		return nil, fmt.Errorf("command dir %q: want an absolute path", cmd.Dir)
	}

	feed, err := envFile(cmd.Env)
	if err != nil {
		return nil, err
	}

	if len(cmd.Files) > 0 {
		if err := c.CopyIn(ctx, cmd.Files); err != nil {
			return nil, err
		}
	}

	pidFile := pidDir + "/" + rand.Text() + ".pid"

	// A command whose pid file cannot be written would be unstoppable, so it
	// does not run; when merging, the shell's own complaint lands in stdout.
	script := `echo $$ > ` + pidFile + ` && exec "$@"`
	if merge {
		script = "exec 2>&1; " + script
	}

	args := []string{c.r.Bin, "exec", "--interactive"}

	if len(feed) > 0 {
		args = append(args, envFileArg)
	}

	if cmd.Dir != "" {
		args = append(args, "--workdir="+cmd.Dir)
	}

	args = append(args, c.name, "sh", "-c", script, "sh")

	p, err := proc.Local{}.Start(ctx, proc.Command{
		Argv:   append(args, cmd.Argv...),
		Env:    os.Environ(),
		Feed:   feed,
		Stdout: cmd.Stdout,
	})
	if err != nil {
		return nil, fmt.Errorf("exec in %s: %w", c.name, err)
	}

	return &execProcess{Process: p, c: c, pidFile: pidFile, exited: make(chan struct{})}, nil
}

// execProcess is a command exec'd in a container. Its Process is the local
// client, and ending the client alone leaves the command running.
type execProcess struct {
	proc.Process

	c       Container
	pidFile string

	waitOnce sync.Once
	exited   chan struct{}
	code     int
	err      error
}

// wait starts the one Wait on the client and returns a channel closed when
// it returns.
func (p *execProcess) wait() <-chan struct{} {
	p.waitOnce.Do(func() {
		go func() {
			p.code, p.err = p.Process.Wait()
			close(p.exited)
		}()
	})

	return p.exited
}

func (p *execProcess) Wait() (int, error) {
	<-p.wait()

	return p.code, p.err
}

// Stop signals the command's process group inside the container, TERM and
// then KILL after grace, waiting at most grace for the pid file. It returns
// nil once the command has ended. Otherwise it ends the client and reports
// that the command may run on until the container is removed.
func (p *execProcess) Stop(grace time.Duration) error {
	exited := p.wait()

	select {
	case <-exited:
		return nil
	default:
	}

	ctx, cancel := context.WithTimeout(context.Background(), grace+execSlack)
	tenths := strconv.Itoa(int(grace / (100 * time.Millisecond)))
	signalErr := p.c.r.run(ctx, call{args: []string{"exec", p.c.name, "sh", "-c", stopScript, "sh", p.pidFile, tenths}})

	cancel()

	timer := time.NewTimer(killGrace)
	defer timer.Stop()

	select {
	case <-exited:
		return nil
	case <-timer.C:
	}

	_ = p.Process.Stop(killGrace)

	<-exited

	return errors.Join(fmt.Errorf("command in %s did not end; it may run until the container is removed", p.c.name), signalErr)
}

// Result is a command run to its end.
type Result struct {
	ExitCode int
	Output   string
	TimedOut bool
}

// Run runs a command to its end. Output is the command's stdout and stderr
// together, then anything the runtime client itself reported, capped at
// 64 KiB (head and tail kept). At the timeout it kills the command inside the
// container. The error is for a command that could not be started, read or
// stopped.
func (c Container) Run(ctx context.Context, cmd proc.Command, timeout time.Duration) (Result, error) {
	out := proc.NewCapture(proc.CaptureCap)
	cmd.Stdout = out

	p, err := c.start(ctx, cmd, true)
	if err != nil {
		return Result{}, err
	}

	_ = p.Stdin().Close()

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	var res Result

	select {
	case <-p.wait():
	case <-timer.C:
		res.TimedOut = true

		if err := p.Stop(killGrace); err != nil {
			return Result{}, fmt.Errorf("run in %s: timed out: %w", c.name, err)
		}
	case <-ctx.Done():
		_ = p.Stop(killGrace)

		return Result{}, fmt.Errorf("run in %s: %w", c.name, ctx.Err())
	}

	code, err := p.Wait()
	if err != nil {
		return Result{}, fmt.Errorf("run in %s: %w", c.name, err)
	}

	// The wrapper sends all the command's output to stdout, so the client's
	// stderr is the runtime's own reason for a failed exec.
	_, _ = io.WriteString(out, p.StderrTail())

	res.ExitCode = code
	res.Output = out.String()

	return res, nil
}
