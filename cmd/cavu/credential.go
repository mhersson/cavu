package main

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/mhersson/cavu/internal/engine"
)

func init() {
	commands["credential"] = command{
		summary: "credential set | list | remove: store a named GitHub credential read from standard input, list them, or remove one",
		run:     runCredential,
	}
}

const (
	githubSetUsage    = "cavu credential set NAME [--app-id N] [--data-dir DIR] < FILE"
	githubListUsage   = "cavu credential list [--data-dir DIR]"
	githubRemoveUsage = "cavu credential remove NAME [--data-dir DIR]"
)

func runCredential(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) > 0 {
		switch args[0] {
		case "set":
			return credentialSet(ctx, args[1:], stdin, stderr)
		case "list":
			return credentialList(ctx, args[1:], stdout, stderr)
		case "remove":
			return credentialRemove(ctx, args[1:], stderr)
		}
	}

	usage := strings.Join([]string{githubSetUsage, githubListUsage, githubRemoveUsage}, "\n       ")

	return newVerbFlags(usage, stderr).helpOr(args)
}

// credentialSet reads the value as cavu secret set does. With --app-id it is
// the App's private key in PEM, else a personal access token.
func credentialSet(ctx context.Context, args []string, stdin io.Reader, stderr io.Writer) error {
	f := newVerbFlags(githubSetUsage, stderr)
	appID := f.Int64("app-id", 0, "the GitHub App's id; standard input is then its private key in PEM, else a personal access token")

	name, err := f.parse(args, 1)
	if err != nil {
		return err
	}

	value, err := readValue(stdin, "cavu credential set "+name[0]+" < FILE")
	if err != nil {
		return err
	}

	c := engine.NewCredential{Token: value}
	if f.given()["app-id"] {
		c = engine.NewCredential{AppID: *appID, AppKey: value}
	}

	a, err := f.open(ctx)
	if err != nil {
		return err
	}
	defer a.close()

	return engine.ChangeCredentials(ctx, a.store, a.vault, map[string]*engine.NewCredential{name[0]: &c})
}

func credentialList(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	f := newVerbFlags(githubListUsage, stderr)
	if _, err := f.parse(args, 0); err != nil {
		return err
	}

	a, err := f.open(ctx)
	if err != nil {
		return err
	}
	defer a.close()

	cs, err := a.store.Credentials(ctx)
	if err != nil {
		return err
	}

	w := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tKIND\tAPP ID")

	for _, c := range cs {
		appID := ""
		if c.Kind == engine.CredentialApp {
			appID = strconv.FormatInt(c.AppID, 10)
		}

		fmt.Fprintf(w, "%s\t%s\t%s\n", c.Name, c.Kind, appID)
	}

	return w.Flush()
}

func credentialRemove(ctx context.Context, args []string, stderr io.Writer) error {
	f := newVerbFlags(githubRemoveUsage, stderr)

	name, err := f.parse(args, 1)
	if err != nil {
		return err
	}

	a, err := f.open(ctx)
	if err != nil {
		return err
	}
	defer a.close()

	return engine.ChangeCredentials(ctx, a.store, a.vault, map[string]*engine.NewCredential{name[0]: nil})
}
