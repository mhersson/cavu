// Package service installs `cavu serve` as a service of the user who runs it,
// a systemd user unit on Linux and a launchd agent on macOS, and removes it.
package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Runner runs a command on the host and returns its standard output; a
// non-zero exit is an error.
type Runner func(ctx context.Context, argv ...string) (string, error)

// Manager installs and removes the service. It starts commands only through Run.
type Manager struct {
	OS  string // as runtime.GOOS names it: linux for systemd, darwin for launchd
	UID int    // the user whose launchd domain takes the agent
	Run Runner
}

// Install writes the file that runs binary's serve verb on the data directory
// dataDir, an absolute path, and restarts the service from it, so installing
// again after the binary changed upgrades it. It returns the file's path and
// whether its text changed. With linger, on systemd only, it also enables
// lingering for the user.
func (m Manager) Install(ctx context.Context, binary, dataDir string, linger bool) (string, bool, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", false, err
	}

	switch m.OS {
	case "linux":
		return m.installUnit(ctx, home, binary, dataDir, linger)
	case "darwin":
		return m.installAgent(ctx, home, binary, dataDir, linger)
	}

	return "", false, m.unsupported()
}

// Uninstall stops the service and removes its file and nothing else. It
// returns the file's path and whether a service was installed.
func (m Manager) Uninstall(ctx context.Context) (string, bool, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", false, err
	}

	switch m.OS {
	case "linux":
		return m.uninstallUnit(ctx, home)
	case "darwin":
		return m.uninstallAgent(ctx, home)
	}

	return "", false, m.unsupported()
}

func (m Manager) unsupported() error {
	return fmt.Errorf("a service runs on Linux and macOS, not on %s", m.OS)
}

// environment is what the service's file carries but HOME. PATH is the
// installer's, so the service finds the runtime the person's shell found.
func (m Manager) environment(ctx context.Context, dataDir string) map[string]string {
	env := map[string]string{"CAVU_DATA_DIR": dataDir, "PATH": os.Getenv("PATH")}

	out, err := m.Run(ctx, "docker", "context", "inspect", "--format", "{{.Endpoints.docker.Host}}")
	if host := strings.TrimSpace(out); err == nil && host != "" && host != "unix:///var/run/docker.sock" {
		env["DOCKER_HOST"] = host
	}

	return env
}

// run runs the commands in turn and stops at the first that fails.
func (m Manager) run(ctx context.Context, cmds ...[]string) error {
	for _, argv := range cmds {
		if _, err := m.Run(ctx, argv...); err != nil {
			return fmt.Errorf("%s: %w", strings.Join(argv, " "), err)
		}
	}

	return nil
}

// write puts data at path unless the file holds it already, and reports
// whether it wrote.
func write(path string, data []byte) (bool, error) {
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, data) {
		return false, nil
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil { //nolint:gosec // G301: the unit and LaunchAgents directories' convention
		return false, err
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil { //nolint:gosec // G306: the file carries no secret
		return false, err
	}

	return true, os.Rename(tmp, path)
}

func installed(path string) (bool, error) {
	_, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}

	return err == nil, err
}
