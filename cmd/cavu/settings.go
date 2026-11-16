package main

import (
	"context"
	"fmt"
	"io"
	"maps"
	"slices"

	"github.com/mhersson/cavu/internal/engine"
)

func init() {
	commands["settings"] = command{
		summary: "settings set KEY VALUE | settings show: set one setting, or print them all",
		run:     runSettings,
	}
}

const settingsUsage = "cavu settings set KEY VALUE [--data-dir DIR]\n       cavu settings show [--data-dir DIR]"

func runSettings(ctx context.Context, args []string, _ io.Reader, stdout, stderr io.Writer) error {
	f := newVerbFlags(settingsUsage, stderr)

	switch {
	case len(args) > 0 && args[0] == "set":
		return settingsSet(ctx, f, args[1:])
	case len(args) > 0 && args[0] == "show":
		return settingsShow(ctx, f, args[1:], stdout)
	}

	return f.usageError()
}

func settingsSet(ctx context.Context, f verbFlags, args []string) error {
	kv, err := f.parse(args, 2)
	if err != nil {
		return err
	}

	if err := engine.CheckSetting(kv[0], kv[1]); err != nil {
		return err
	}

	a, err := f.open(ctx)
	if err != nil {
		return err
	}
	defer a.close()

	return a.store.SetSetting(ctx, kv[0], kv[1])
}

// settingsShow prints every setting as stored: no secret is a setting.
func settingsShow(ctx context.Context, f verbFlags, args []string, stdout io.Writer) error {
	if _, err := f.parse(args, 0); err != nil {
		return err
	}

	a, err := f.open(ctx)
	if err != nil {
		return err
	}
	defer a.close()

	all, err := a.store.Settings(ctx)
	if err != nil {
		return err
	}

	for _, k := range slices.Sorted(maps.Keys(all)) {
		fmt.Fprintf(stdout, "%s=%s\n", k, all[k])
	}

	return nil
}
