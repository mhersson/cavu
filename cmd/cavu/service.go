package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"

	"github.com/mhersson/cavu/internal/proc"
	"github.com/mhersson/cavu/internal/service"
)

func init() {
	commands["service"] = command{
		summary: "service install | uninstall: run cavu serve as a service of your user, or stop and remove it",
		run:     runService,
	}
}

const (
	serviceInstallUsage   = "cavu service install [--linger] [--data-dir DIR]"
	serviceUninstallUsage = "cavu service uninstall"
)

// services is the seam the unit tests replace: the real one runs systemctl,
// launchctl, loginctl and docker on this host.
var services = service.Manager{OS: runtime.GOOS, UID: os.Getuid(), Run: runOnHost}

func runOnHost(ctx context.Context, argv ...string) (string, error) {
	var out bytes.Buffer

	err := proc.Run(ctx, proc.Local{}, proc.Command{Argv: argv, Env: os.Environ(), Stdout: &out}, nil)

	return out.String(), err
}

func runService(ctx context.Context, args []string, _ io.Reader, stdout, stderr io.Writer) error {
	if len(args) > 0 {
		switch args[0] {
		case "install":
			return serviceInstall(ctx, args[1:], stdout, stderr)
		case "uninstall":
			return serviceUninstall(ctx, args[1:], stdout, stderr)
		}
	}

	return newFlags(serviceInstallUsage+"\n       "+serviceUninstallUsage, stderr).helpOr(args)
}

func serviceInstall(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	f := newVerbFlags(serviceInstallUsage, stderr)
	linger := f.Bool("linger", false, "also enable lingering with loginctl: systemd then runs your user manager from boot and after you log out (Linux only)")

	if _, err := f.parse(args, 0); err != nil {
		return err
	}

	dir, err := dataDir(*f.dataDir)
	if err != nil {
		return err
	}

	if dir, err = filepath.Abs(dir); err != nil {
		return err
	}

	exe, err := os.Executable()
	if err != nil {
		return err
	}

	path, changed, err := services.Install(ctx, exe, dir, *linger)
	if err != nil {
		return err
	}

	state := "unchanged"
	if changed {
		state = "written"
	}

	_, err = fmt.Fprintf(stdout, "%s %s; cavu serve started from it\n", path, state)

	return err
}

func serviceUninstall(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if _, err := newFlags(serviceUninstallUsage, stderr).parse(args, 0); err != nil {
		return err
	}

	path, removed, err := services.Uninstall(ctx)
	if err != nil {
		return err
	}

	if !removed {
		_, err = fmt.Fprintf(stdout, "no service is installed: %s does not exist\n", path)

		return err
	}

	_, err = fmt.Fprintf(stdout, "stopped cavu serve and removed %s\n", path)

	return err
}
