package claudecode

import (
	"bytes"
	"context"
	"fmt"
	"regexp"
	"time"

	"github.com/mhersson/cavu/internal/driver"
	"github.com/mhersson/cavu/internal/proc"
)

const (
	// runRoot holds a fresh directory of files for each claude process, on the
	// sandbox's tmpfs.
	runRoot = "/run/cavu"
	// gateTimeout bounds the wait for the answer to the initialize request;
	// the prompt is written only after it.
	gateTimeout = 30 * time.Second
	// stopGrace is the SIGTERM-to-SIGKILL grace, and the time a process that
	// finished its work gets to exit once its stdin closes.
	stopGrace = 2 * time.Second
)

// claudeCode drives the unmodified claude binary; tests vary the fields.
type claudeCode struct {
	binary      string
	runRoot     string
	gateTimeout time.Duration
	grace       time.Duration
	// amend is nil outside tests; the conformance suite uses it for runs no
	// Spec describes, such as a subscription against a mock's base URL.
	amend func(*invocation)
}

// New returns the Claude Code driver.
func New() driver.Driver {
	return claudeCode{binary: "claude", runRoot: runRoot, gateTimeout: gateTimeout, grace: stopGrace}
}

var versionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+\S*`)

// Probe returns the release the binary reports; comparing it with a pin is
// the caller's job.
func (c claudeCode) Probe(ctx context.Context, t proc.Target) (string, error) {
	var out bytes.Buffer

	if err := proc.Run(ctx, t, proc.Command{Argv: []string{c.binary, "--version"}, Env: probeEnvironment(), Stdout: &out}, nil); err != nil {
		return "", fmt.Errorf("claudecode: %s --version: %w", c.binary, err)
	}

	first, _, _ := bytes.Cut(bytes.TrimSpace(out.Bytes()), []byte("\n"))

	version := versionPattern.Find(first)
	if version == nil {
		return "", fmt.Errorf("claudecode: %s --version printed %q, not a version", c.binary, first)
	}

	return string(version), nil
}
