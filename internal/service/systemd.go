package service

import (
	"bytes"
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

const unitName = "cavu.service"

func unitPath(home string) string { return filepath.Join(home, ".config", "systemd", "user", unitName) }

func systemctl(args ...string) []string { return append([]string{"systemctl", "--user"}, args...) }

func (m Manager) installUnit(ctx context.Context, home, binary, dataDir string, linger bool) (string, bool, error) {
	path := unitPath(home)

	changed, err := write(path, renderUnit(binary, m.environment(ctx, dataDir)))
	if err != nil {
		return path, false, err
	}

	var cmds [][]string
	if changed {
		cmds = append(cmds, systemctl("daemon-reload"))
	}

	cmds = append(cmds, systemctl("enable", unitName), systemctl("restart", unitName))
	if linger {
		cmds = append(cmds, []string{"loginctl", "enable-linger"})
	}

	return path, changed, m.run(ctx, cmds...)
}

func (m Manager) uninstallUnit(ctx context.Context, home string) (string, bool, error) {
	path := unitPath(home)

	if ok, err := installed(path); !ok {
		return path, false, err
	}

	if err := m.run(ctx, systemctl("disable", "--now", unitName)); err != nil {
		return path, false, err
	}

	if err := os.Remove(path); err != nil {
		return path, false, err
	}

	return path, true, m.run(ctx, systemctl("daemon-reload"))
}

func renderUnit(binary string, env map[string]string) []byte {
	var b bytes.Buffer

	fmt.Fprintf(&b, "[Unit]\nDescription=CAVU server\n\n[Service]\nExecStart=%s serve\n", systemdWord(binary))

	for _, k := range slices.Sorted(maps.Keys(env)) {
		fmt.Fprintf(&b, "Environment=%s\n", systemdWord(k+"="+env[k]))
	}

	// mixed sends the SIGTERM to cavu serve alone, which winds its own children down.
	b.WriteString("KillMode=mixed\nRestart=on-failure\nRestartSec=10\n\n[Install]\nWantedBy=default.target\n")

	return b.Bytes()
}

// systemdWord renders one word of a unit line: % is doubled so systemd does
// not expand it as a specifier, and a word that whitespace or % would
// otherwise split or mangle is double-quoted. A plain word renders as is.
func systemdWord(v string) string {
	if !strings.ContainsAny(v, " \t%") {
		return v
	}

	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "%", "%%").Replace(v) + `"`
}
