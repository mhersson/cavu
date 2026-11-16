package git

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

const (
	waitDelay   = 2 * time.Second
	stderrLimit = 64 << 10
)

// gitPathKey is a context key whose value is the path of a git executable.
// Only tests set it, to wrap git without changing PATH.
type gitPathKey struct{}

// gitPath is the git executable ctx asks for, else git on PATH.
func gitPath(ctx context.Context) string {
	if path, ok := ctx.Value(gitPathKey{}).(string); ok {
		return path
	}

	return "git"
}

// call describes one git invocation. All git in this package goes through run.
type call struct {
	dir     string
	args    []string
	env     []string // added to the inherited environment
	read    bool     // adds GIT_OPTIONAL_LOCKS=0
	noHooks bool     // core.hooksPath=/dev/null: a repository must not run code
	net     bool     // network command: no terminal prompts
}

// run executes git and returns its stdout. A non-zero exit carries the first
// line of stderr in the error.
func run(ctx context.Context, c call) ([]byte, error) {
	cmd := c.command(ctx)
	out := &bytes.Buffer{}
	stderr := &cappedBuffer{limit: stderrLimit}
	cmd.Stdout = out
	cmd.Stderr = stderr

	err := cmd.Run()

	switch {
	case ctx.Err() != nil:
		return nil, fmt.Errorf("git %s: %w", c.name(), ctx.Err())
	case err != nil:
		return out.Bytes(), fmt.Errorf("git %s: %w%s", c.name(), err, firstLine(stderr.Bytes()))
	}

	return out.Bytes(), nil
}

func (c call) name() string {
	for _, a := range c.args {
		if !strings.HasPrefix(a, "-") {
			return a
		}
	}

	return "command"
}

// command builds the process: config as -c arguments before the subcommand,
// its own process group and a bounded wait.
func (c call) command(ctx context.Context) *exec.Cmd {
	// Maintenance finishes inside the call: a detached gc would die with the
	// container and leave its lock files behind.
	args := []string{
		"-c", "core.fsmonitor=false", "-c", "core.quotePath=false",
		"-c", "gc.autoDetach=false", "-c", "maintenance.autoDetach=false",
	}
	if c.noHooks {
		args = append(args, "-c", "core.hooksPath=/dev/null")
	}

	args = append(args, c.args...)
	cmd := exec.CommandContext(ctx, gitPath(ctx), args...) //nolint:gosec // gitPath is git or a test wrapper
	cmd.Dir = c.dir
	// The sidecar's container holds no variable but the runtime's own, so
	// nothing inherited names another repository.
	cmd.Env = append(os.Environ(), c.env...)

	if c.read {
		cmd.Env = append(cmd.Env, "GIT_OPTIONAL_LOCKS=0")
	}

	if c.net {
		cmd.Env = append(cmd.Env, "GIT_TERMINAL_PROMPT=0", "GCM_INTERACTIVE=never")
	}

	setProcessGroup(cmd)
	cmd.WaitDelay = waitDelay

	return cmd
}

// firstLine formats stderr's first line as a ": ..." suffix, or "" when empty.
func firstLine(b []byte) string {
	line, _, _ := strings.Cut(strings.TrimSpace(string(b)), "\n")
	if line == "" {
		return ""
	}

	return ": " + strings.TrimSpace(line)
}

// cappedBuffer keeps at most limit bytes and drops the rest.
type cappedBuffer struct {
	buf   bytes.Buffer
	limit int
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if room := b.limit - b.buf.Len(); room > 0 {
		b.buf.Write(p[:min(len(p), room)])
	}

	return len(p), nil
}

func (b *cappedBuffer) Bytes() []byte { return b.buf.Bytes() }
