package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func runCavu(args ...string) (code int, stdout, stderr string) {
	var out, errOut bytes.Buffer

	code = run(args, bytes.NewReader(nil), &out, &errOut)

	return code, out.String(), errOut.String()
}

func register(t *testing.T, name string, internal bool, fn func(context.Context, []string, io.Reader, io.Writer, io.Writer) error) {
	t.Helper()

	commands[name] = command{summary: "test verb", internal: internal, run: fn}

	t.Cleanup(func() { delete(commands, name) })
}

func TestVersion(t *testing.T) {
	code, stdout, stderr := runCavu("version")

	assert.Equal(t, 0, code)
	assert.Equal(t, "cavu dev\n", stdout)
	assert.Empty(t, stderr)
}

func TestUnknownVerb(t *testing.T) {
	code, stdout, stderr := runCavu("frobnicate")

	assert.Equal(t, 2, code)
	assert.Empty(t, stdout)
	assert.Contains(t, stderr, "frobnicate")
}

func TestNoArguments(t *testing.T) {
	code, stdout, stderr := runCavu()

	assert.Equal(t, 2, code)
	assert.Empty(t, stdout)

	require.NotEmpty(t, commands)

	for name := range commands {
		assert.Contains(t, stderr, name)
	}
}

func TestHelpExitsZero(t *testing.T) {
	for _, arg := range []string{"help", "-h", "--help"} {
		t.Run(arg, func(t *testing.T) {
			code, stdout, stderr := runCavu(arg)

			assert.Equal(t, 0, code)
			assert.Empty(t, stderr)

			for name := range commands {
				assert.Contains(t, stdout, name)
			}
		})
	}
}

func TestInternalVerbListedAndDispatched(t *testing.T) {
	register(t, "hidden", true, func(_ context.Context, _ []string, _ io.Reader, stdout, _ io.Writer) error {
		_, err := io.WriteString(stdout, "ran\n")

		return err
	})

	code, _, stderr := runCavu()
	assert.Equal(t, 2, code)
	assert.Contains(t, stderr, "hidden")

	code, stdout, _ := runCavu("hidden")
	assert.Equal(t, 0, code)
	assert.Equal(t, "ran\n", stdout)
}

func TestCommandErrorExitsOne(t *testing.T) {
	register(t, "failing", false, func(context.Context, []string, io.Reader, io.Writer, io.Writer) error {
		return errors.New("boom")
	})

	code, stdout, stderr := runCavu("failing")

	assert.Equal(t, 1, code)
	assert.Empty(t, stdout)
	assert.Equal(t, "cavu: boom\n", stderr)
}

func TestCommandExitErrorCarriesCode(t *testing.T) {
	register(t, "coded", false, func(context.Context, []string, io.Reader, io.Writer, io.Writer) error {
		return exitError{code: 7, err: errors.New("stopped")}
	})

	code, _, stderr := runCavu("coded")

	assert.Equal(t, 7, code)
	assert.Empty(t, stderr)
}

func TestExitErrorUnwraps(t *testing.T) {
	cause := errors.New("cause")

	assert.ErrorIs(t, exitError{code: 3, err: cause}, cause)
}

func TestExitErrorWithoutCauseDoesNotPanic(t *testing.T) {
	assert.NotPanics(t, func() { _ = exitError{code: 4}.Error() })
}

func TestSidecarAndBridgeAreInternalVerbs(t *testing.T) {
	_, _, stderr := runCavu()

	_, internal, found := strings.Cut(stderr, "internal:")
	require.True(t, found)

	assert.Contains(t, internal, "sidecar")
	assert.Contains(t, internal, "bridge")
}

func TestSidecarReportsABadRequestAsAnErrorLine(t *testing.T) {
	var out, errOut bytes.Buffer

	code := run([]string{"sidecar"}, strings.NewReader(`{}`), &out, &errOut)

	assert.Equal(t, 1, code)
	assert.Contains(t, out.String(), `"Type":"error"`)
}

func TestBridgeRejectsAnExtraArgument(t *testing.T) {
	code, _, stderr := runCavu("bridge", "extra")

	assert.Equal(t, 1, code)
	assert.NotEmpty(t, stderr)
}

func TestVerbHelpPrintsOnStdoutAndExitsZero(t *testing.T) {
	for _, args := range [][]string{
		{"task", "-h"}, {"task", "run", "-h"}, {"settings", "set", "--help"},
		{"project", "set", "-h"}, {"secret", "-h"}, {"image", "build", "-h"}, {"bridge", "-h"},
	} {
		var stdout, stderr bytes.Buffer

		assert.Equal(t, 0, run(args, strings.NewReader(""), &stdout, &stderr), args)
		assert.NotEmpty(t, stdout.String(), args)
		assert.Empty(t, stderr.String(), args)
	}
}

func TestVerbUsageErrorStaysOnStderr(t *testing.T) {
	for _, args := range [][]string{{"task", "run"}, {"task", "run", "1", "--bogus"}, {"settings", "set", "k"}} {
		var stdout, stderr bytes.Buffer

		assert.Equal(t, 2, run(args, strings.NewReader(""), &stdout, &stderr), args)
		assert.Empty(t, stdout.String(), args)
		assert.NotEmpty(t, stderr.String(), args)
	}
}
