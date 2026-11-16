package main

import (
	"bytes"
	"context"
	"errors"
	"io"
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

	code, _, _ := runCavu("coded")

	assert.Equal(t, 7, code)
}

func TestExitErrorUnwraps(t *testing.T) {
	cause := errors.New("cause")

	assert.ErrorIs(t, exitError{code: 3, err: cause}, cause)
}

func TestExitErrorWithoutCauseDoesNotPanic(t *testing.T) {
	assert.NotPanics(t, func() { _ = exitError{code: 4}.Error() })
}
