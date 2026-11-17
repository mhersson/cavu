package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
)

func init() {
	commands["secret"] = command{
		summary: "secret set NAME < FILE: store a secret read from standard input",
		run:     runSecret,
	}
}

// runSecret never prints the value, and never takes it as an argument, where
// the process list and the shell's history would show it.
func runSecret(ctx context.Context, args []string, stdin io.Reader, _, stderr io.Writer) error {
	f := newVerbFlags("cavu secret set NAME [--data-dir DIR] < FILE", stderr)
	if len(args) == 0 || args[0] != "set" {
		return f.helpOr(args)
	}

	name, err := f.parse(args[1:], 1)
	if err != nil {
		return err
	}

	// Typed on a terminal, the value would show on the screen.
	if isTerminal(stdin) {
		return fmt.Errorf("the value is read from standard input, not a terminal: cavu secret set %s < FILE", name[0])
	}

	data, err := io.ReadAll(stdin)
	if err != nil {
		return fmt.Errorf("standard input: %w", err)
	}

	value := strings.TrimSuffix(string(data), "\n")
	if value == "" {
		return errors.New("the value on standard input is empty")
	}

	a, err := f.open(ctx)
	if err != nil {
		return err
	}
	defer a.close()

	return a.vault.Set(ctx, name[0], value)
}
