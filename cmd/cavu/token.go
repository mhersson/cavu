package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/mhersson/cavu/internal/auth"
)

func init() {
	commands["token"] = command{
		summary: "token add | list | remove: add an API token and print it once, list the tokens, or remove one",
		run:     runToken,
	}
}

const (
	apiAddUsage    = "cavu token add NAME [--data-dir DIR]"
	apiListUsage   = "cavu token list [--data-dir DIR]"
	apiRemoveUsage = "cavu token remove NAME [--data-dir DIR]"
)

func runToken(ctx context.Context, args []string, _ io.Reader, stdout, stderr io.Writer) error {
	if len(args) > 0 {
		switch args[0] {
		case "add":
			return tokenAdd(ctx, args[1:], stdout, stderr)
		case "list":
			return tokenList(ctx, args[1:], stdout, stderr)
		case "remove":
			return tokenRemove(ctx, args[1:], stderr)
		}
	}

	usage := strings.Join([]string{apiAddUsage, apiListUsage, apiRemoveUsage}, "\n       ")

	return newVerbFlags(usage, stderr).helpOr(args)
}

// tokenAdd prints the token alone: it is readable only now.
func tokenAdd(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	f := newVerbFlags(apiAddUsage, stderr)

	name, err := f.parse(args, 1)
	if err != nil {
		return err
	}

	a, err := f.open(ctx)
	if err != nil {
		return err
	}
	defer a.close()

	token, err := auth.NewService(a.store, time.Now).NewAPIToken(ctx, name[0])
	if err != nil {
		return fmt.Errorf("token %q: %w", name[0], err)
	}

	_, err = fmt.Fprintln(stdout, token)

	return err
}

func tokenList(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	f := newVerbFlags(apiListUsage, stderr)
	if _, err := f.parse(args, 0); err != nil {
		return err
	}

	a, err := f.open(ctx)
	if err != nil {
		return err
	}
	defer a.close()

	tokens, err := a.store.APITokens(ctx)
	if err != nil {
		return err
	}

	w := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tCREATED\tLAST USED")

	for _, t := range tokens {
		used := "never"
		if t.LastUsedAt != nil {
			used = t.LastUsedAt.Format(time.RFC3339)
		}

		fmt.Fprintf(w, "%s\t%s\t%s\n", t.Name, t.CreatedAt.Format(time.RFC3339), used)
	}

	return w.Flush()
}

func tokenRemove(ctx context.Context, args []string, stderr io.Writer) error {
	f := newVerbFlags(apiRemoveUsage, stderr)

	name, err := f.parse(args, 1)
	if err != nil {
		return err
	}

	a, err := f.open(ctx)
	if err != nil {
		return err
	}
	defer a.close()

	if err := a.store.DeleteAPIToken(ctx, name[0]); err != nil {
		return fmt.Errorf("token %q: %w", name[0], err)
	}

	return nil
}
