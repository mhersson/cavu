package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/mhersson/cavu/internal/auth"
)

func init() {
	commands["user"] = command{
		summary: "user set NAME < FILE: create the user, or change their name and password, read from standard input",
		run:     runUser,
	}
}

// runUser never takes the password as an argument, where the process list and
// the shell's history would show it.
func runUser(ctx context.Context, args []string, stdin io.Reader, _, stderr io.Writer) error {
	f := newVerbFlags("cavu user set NAME [--data-dir DIR] < FILE", stderr)
	if len(args) == 0 || args[0] != "set" {
		return f.helpOr(args)
	}

	name, err := f.parse(args[1:], 1)
	if err != nil {
		return err
	}

	if isTerminal(stdin) {
		return fmt.Errorf("the password is read from standard input, not a terminal: cavu user set %s < FILE", name[0])
	}

	data, err := io.ReadAll(stdin)
	if err != nil {
		return fmt.Errorf("standard input: %w", err)
	}

	a, err := f.open(ctx)
	if err != nil {
		return err
	}
	defer a.close()

	return auth.NewService(a.store, time.Now).SetUser(ctx, name[0], strings.TrimSuffix(string(data), "\n"))
}
