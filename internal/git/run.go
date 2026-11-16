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
	env     []string // added after the cleaned environment
	read    bool     // adds GIT_OPTIONAL_LOCKS=0
	noHooks bool     // core.hooksPath=/dev/null: a repository must not run code
	net     bool     // network command: new session, no terminal prompts, URLs hidden in errors
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
		return out.Bytes(), fmt.Errorf("git %s: %w%s", c.name(), err, c.stderrSuffix(stderr.Bytes()))
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

// stderrSuffix is firstLine of stderr. A network call also has every URL in it
// replaced by "<url>", since a remote URL may carry credentials.
func (c call) stderrSuffix(stderr []byte) string {
	if c.net {
		return hideURLs(firstLine(stderr))
	}

	return firstLine(stderr)
}

// hideURLs replaces each <scheme>://... token, up to whitespace or a quote.
func hideURLs(s string) string {
	var b strings.Builder

	for {
		i := strings.Index(s, "://")
		if i < 0 {
			b.WriteString(s)

			return b.String()
		}

		start := i
		for start > 0 && isSchemeByte(s[start-1]) {
			start--
		}

		end := i + len("://")
		for end < len(s) && !strings.ContainsRune(" \t\r\n'\"", rune(s[end])) {
			end++
		}

		b.WriteString(s[:start])
		b.WriteString("<url>")

		s = s[end:]
	}
}

func isSchemeByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '+' || c == '-' || c == '.'
}

// command builds the process: config as -c arguments before the subcommand,
// the cleaned environment, its own process group (a network call: its own
// session, so no controlling terminal) and a bounded wait.
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
	cmd.Env = append(cleanEnv(os.Environ()), c.env...)

	if c.read {
		cmd.Env = append(cmd.Env, "GIT_OPTIONAL_LOCKS=0")
	}

	if c.net {
		cmd.Env = append(cmd.Env, "GIT_TERMINAL_PROMPT=0", "GCM_INTERACTIVE=never")
		setSession(cmd)
	} else {
		setProcessGroup(cmd)
	}

	// A read takes no locks (GIT_OPTIONAL_LOCKS), so it skips SIGTERM's grace.
	stop := stopProcessGroup
	if c.read {
		stop = killProcessGroup
	}

	cmd.Cancel = func() error { return stop(cmd.Process) }
	cmd.WaitDelay = waitDelay

	return cmd
}

// cleanEnv drops git's repository-local variables from inherited, so a call
// reads only the repository it was pointed at. The list is from
// `git rev-parse --local-env-vars` (git 2.56); GIT_CONFIG_COUNT also removes its
// KEY_<n> and VALUE_<n> pairs.
func cleanEnv(inherited []string) []string {
	env := make([]string, 0, len(inherited))

	for _, kv := range inherited {
		name, _, _ := strings.Cut(kv, "=")
		if !repoLocalVar(name) {
			env = append(env, kv)
		}
	}

	return env
}

func repoLocalVar(name string) bool {
	switch name {
	case "GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_CONFIG", "GIT_CONFIG_PARAMETERS",
		"GIT_CONFIG_COUNT", "GIT_OBJECT_DIRECTORY", "GIT_DIR", "GIT_WORK_TREE",
		"GIT_IMPLICIT_WORK_TREE", "GIT_GRAFT_FILE", "GIT_INDEX_FILE",
		"GIT_NO_REPLACE_OBJECTS", "GIT_REPLACE_REF_BASE", "GIT_PREFIX",
		"GIT_SHALLOW_FILE", "GIT_COMMON_DIR", "GIT_OPTIONAL_LOCKS":
		return true
	}

	return strings.HasPrefix(name, "GIT_CONFIG_KEY_") || strings.HasPrefix(name, "GIT_CONFIG_VALUE_")
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
