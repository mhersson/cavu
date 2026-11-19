package service

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

const label = "com.github.mhersson.cavu"

func agentPath(home string) string {
	return filepath.Join(home, "Library", "LaunchAgents", label+".plist")
}

func (m Manager) domain() string { return "gui/" + strconv.Itoa(m.UID) }

func (m Manager) installAgent(ctx context.Context, home, binary, dataDir string, linger bool) (string, bool, error) {
	if linger {
		return "", false, errors.New("linger is for a systemd user service only")
	}

	path := agentPath(home)
	env := m.environment(ctx, dataDir)
	env["HOME"] = home

	changed, err := write(path, renderAgent(binary, env, filepath.Join(home, "Library", "Logs", "cavu.log")))
	if err != nil {
		return path, false, err
	}

	if err := m.bootout(ctx); err != nil {
		return path, changed, err
	}

	return path, changed, m.run(ctx, []string{"launchctl", "bootstrap", m.domain(), path})
}

func (m Manager) uninstallAgent(ctx context.Context, home string) (string, bool, error) {
	path := agentPath(home)

	if ok, err := installed(path); !ok {
		return path, false, err
	}

	if err := m.bootout(ctx); err != nil {
		return path, false, err
	}

	if err := os.Remove(path); err != nil {
		return path, false, err
	}

	return path, true, nil
}

// bootout unloads the agent when launchctl print finds it loaded.
func (m Manager) bootout(ctx context.Context) error {
	target := m.domain() + "/" + label
	if _, err := m.Run(ctx, "launchctl", "print", target); err == nil {
		return m.run(ctx, []string{"launchctl", "bootout", target})
	}

	return nil
}

const agentHead = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>%s</string>

    <key>ProgramArguments</key>
    <array>
        <string>%s</string>
        <string>serve</string>
    </array>

    <key>EnvironmentVariables</key>
    <dict>
`

const agentTail = `    </dict>

    <key>RunAtLoad</key>
    <true/>

    <key>KeepAlive</key>
    <dict>
        <key>SuccessfulExit</key>
        <false/>
        <key>Crashed</key>
        <true/>
    </dict>

    <key>ThrottleInterval</key>
    <integer>10</integer>

    <key>ProcessType</key>
    <string>Interactive</string>

    <key>StandardOutPath</key>
    <string>%[1]s</string>

    <key>StandardErrorPath</key>
    <string>%[1]s</string>
</dict>
</plist>
`

func renderAgent(binary string, env map[string]string, logFile string) []byte {
	var b bytes.Buffer

	fmt.Fprintf(&b, agentHead, label, xmlText(binary))

	for _, k := range slices.Sorted(maps.Keys(env)) {
		fmt.Fprintf(&b, "        <key>%s</key>\n        <string>%s</string>\n", xmlText(k), xmlText(env[k]))
	}

	fmt.Fprintf(&b, agentTail, xmlText(logFile))

	return b.Bytes()
}

func xmlText(s string) string {
	var b strings.Builder

	_ = xml.EscapeText(&b, []byte(s)) // a strings.Builder never fails a write

	return b.String()
}
