// Package main is the cavu command: it dispatches the first argument to the
// verb registered under that name and turns the result into an exit code.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"syscall"
)

// command is one verb of the cavu command. Each verb registers itself in the
// commands map from an init function in its own file.
type command struct {
	summary  string
	internal bool
	run      func(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error
}

var commands = map[string]command{}

// exitError makes a command end with a specific exit code. The command has
// already reported, so run prints nothing for it. It is returned by value.
type exitError struct {
	code int
	err  error
}

func (e exitError) Error() string {
	if e.err == nil {
		return fmt.Sprintf("exit %d", e.code)
	}

	return e.err.Error()
}

func (e exitError) Unwrap() error { return e.err }

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// run dispatches args to a verb under a context that ends on interrupt or
// termination, and returns the exit code.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if len(args) == 0 {
		usage(stderr)

		return 2
	}

	cmd, ok := commands[args[0]]
	if !ok {
		fmt.Fprintf(stderr, "cavu: unknown command %q\n\n", args[0])
		usage(stderr)

		return 2
	}

	err := cmd.run(ctx, args[1:], stdin, stdout, stderr)
	if err == nil {
		return 0
	}

	if ee, ok := errors.AsType[exitError](err); ok {
		return ee.code
	}

	fmt.Fprintf(stderr, "cavu: %v\n", err)

	return 1
}

func usage(w io.Writer) {
	var public, internal []string

	for name, cmd := range commands {
		if cmd.internal {
			internal = append(internal, name)
		} else {
			public = append(public, name)
		}
	}

	sort.Strings(public)
	sort.Strings(internal)

	fmt.Fprintln(w, "usage: cavu <command> [arguments]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "commands:")
	printCommands(w, public)

	if len(internal) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "internal:")
		printCommands(w, internal)
	}
}

func printCommands(w io.Writer, names []string) {
	for _, name := range names {
		fmt.Fprintf(w, "  %-12s %s\n", name, commands[name].summary)
	}
}
