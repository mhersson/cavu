package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const dockerInspect = "docker context inspect --format {{.Endpoints.docker.Host}}"

var errExit = errors.New("exit 1")

// fake records each command and runs none. It returns out for a command,
// keyed by its argv joined with spaces, and errExit as well when fail names it.
type fake struct {
	calls [][]string
	out   map[string]string
	fail  map[string]bool
}

func (f *fake) run(_ context.Context, argv ...string) (string, error) {
	f.calls = append(f.calls, argv)

	key := strings.Join(argv, " ")
	if f.fail[key] {
		return f.out[key], errExit
	}

	return f.out[key], nil
}

// host sets a temporary home and PATH, and returns the home.
func host(t *testing.T, path string) string {
	t.Helper()

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", path)

	return home
}

func read(t *testing.T, path string) string {
	t.Helper()

	b, err := os.ReadFile(path)
	require.NoError(t, err)

	return string(b)
}

const wantUnit = `[Unit]
Description=CAVU server

[Service]
ExecStart=/usr/local/bin/cavu serve
Environment=CAVU_DATA_DIR=/srv/cavu
Environment=DOCKER_HOST=unix:///run/user/1000/docker.sock
Environment=PATH=/usr/local/bin:/usr/bin:/bin
KillMode=mixed
Restart=on-failure
RestartSec=10

[Install]
WantedBy=default.target
`

func TestUnitText(t *testing.T) {
	home := host(t, "/usr/local/bin:/usr/bin:/bin")
	f := &fake{out: map[string]string{dockerInspect: "unix:///run/user/1000/docker.sock\n"}}

	path, changed, err := Manager{OS: "linux", Run: f.run}.Install(t.Context(), "/usr/local/bin/cavu", "/srv/cavu", false)
	require.NoError(t, err)
	assert.True(t, changed)
	assert.Equal(t, filepath.Join(home, ".config", "systemd", "user", "cavu.service"), path)
	assert.Equal(t, wantUnit, read(t, path))
}

const wantAgent = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>com.github.mhersson.cavu</string>

    <key>ProgramArguments</key>
    <array>
        <string>/usr/local/bin/cavu</string>
        <string>serve</string>
    </array>

    <key>EnvironmentVariables</key>
    <dict>
        <key>CAVU_DATA_DIR</key>
        <string>/Users/u/.local/share/cavu</string>
        <key>HOME</key>
        <string>/Users/u</string>
        <key>PATH</key>
        <string>/opt/homebrew/bin:/usr/bin:/bin</string>
    </dict>

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
    <string>/Users/u/Library/Logs/cavu.log</string>

    <key>StandardErrorPath</key>
    <string>/Users/u/Library/Logs/cavu.log</string>
</dict>
</plist>
`

func TestAgentText(t *testing.T) {
	home := host(t, "/opt/homebrew/bin:/usr/bin:/bin")
	f := &fake{fail: map[string]bool{"launchctl print gui/501/com.github.mhersson.cavu": true}}

	path, changed, err := Manager{OS: "darwin", UID: 501, Run: f.run}.Install(t.Context(), "/usr/local/bin/cavu", filepath.Join(home, ".local", "share", "cavu"), false)
	require.NoError(t, err)
	assert.True(t, changed)
	assert.Equal(t, filepath.Join(home, "Library", "LaunchAgents", "com.github.mhersson.cavu.plist"), path)
	assert.Equal(t, strings.ReplaceAll(wantAgent, "/Users/u", home), read(t, path))
}

func TestFileEscapesWhatSystemdAndXMLWouldRead(t *testing.T) {
	const path = `/opt/my tools/bin:/opt/100%/bin:/opt/a&"b"/bin`

	host(t, path)

	unitPath, _, err := Manager{OS: "linux", Run: (&fake{}).run}.Install(t.Context(), "/opt/my apps/cavu", "/srv/100%", false)
	require.NoError(t, err)

	unit := read(t, unitPath)
	assert.Contains(t, unit, "\nExecStart=\"/opt/my apps/cavu\" serve\n")
	assert.Contains(t, unit, "\nEnvironment=\"CAVU_DATA_DIR=/srv/100%%\"\n")
	assert.Contains(t, unit, "\nEnvironment=\"PATH=/opt/my tools/bin:/opt/100%%/bin:/opt/a&\\\"b\\\"/bin\"\n")

	agentPath, _, err := Manager{OS: "darwin", UID: 501, Run: (&fake{}).run}.Install(t.Context(), "/opt/a&b/cavu", "/srv/100%", false)
	require.NoError(t, err)

	agent := read(t, agentPath)
	assert.Contains(t, agent, "\n        <string>/opt/a&amp;b/cavu</string>\n")
	assert.Contains(t, agent, "\n        <string>/opt/my tools/bin:/opt/100%/bin:/opt/a&amp;&#34;b&#34;/bin</string>\n")
}

func TestFileEnvironment(t *testing.T) {
	cases := []struct {
		name   string
		docker string
		fails  bool
		want   []string
	}{
		{"a non-default docker context", "tcp://10.0.0.5:2376\n", false, []string{"CAVU_DATA_DIR=/srv/cavu", "DOCKER_HOST=tcp://10.0.0.5:2376", "PATH=/bin"}},
		{"the default socket", "unix:///var/run/docker.sock\n", false, []string{"CAVU_DATA_DIR=/srv/cavu", "PATH=/bin"}},
		{"docker failing", "tcp://10.0.0.5:2376\n", true, []string{"CAVU_DATA_DIR=/srv/cavu", "PATH=/bin"}},
		{"docker printing nothing", "", false, []string{"CAVU_DATA_DIR=/srv/cavu", "PATH=/bin"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			host(t, "/bin")

			f := &fake{out: map[string]string{dockerInspect: tc.docker}, fail: map[string]bool{dockerInspect: tc.fails}}

			path, _, err := Manager{OS: "linux", Run: f.run}.Install(t.Context(), "/usr/local/bin/cavu", "/srv/cavu", false)
			require.NoError(t, err)

			var env []string

			for line := range strings.Lines(read(t, path)) {
				if v, ok := strings.CutPrefix(line, "Environment="); ok {
					env = append(env, strings.TrimSuffix(v, "\n"))
				}
			}

			assert.Equal(t, tc.want, env)
		})
	}
}

func userctl(args ...string) []string { return append([]string{"systemctl", "--user"}, args...) }

func TestSystemdInstall(t *testing.T) {
	docker := strings.Fields(dockerInspect)
	reload, enable, restart := userctl("daemon-reload"), userctl("enable", "cavu.service"), userctl("restart", "cavu.service")

	cases := []struct {
		name    string
		again   bool
		linger  bool
		changed bool
		want    [][]string
	}{
		{"a new file", false, false, true, [][]string{docker, reload, enable, restart}},
		{"an unchanged file", true, false, false, [][]string{docker, enable, restart}},
		{"with linger", false, true, true, [][]string{docker, reload, enable, restart, {"loginctl", "enable-linger"}}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			host(t, "/bin")

			if tc.again {
				_, _, err := Manager{OS: "linux", Run: (&fake{}).run}.Install(t.Context(), "/usr/local/bin/cavu", "/srv/cavu", false)
				require.NoError(t, err)
			}

			f := &fake{}

			_, changed, err := Manager{OS: "linux", Run: f.run}.Install(t.Context(), "/usr/local/bin/cavu", "/srv/cavu", tc.linger)
			require.NoError(t, err)
			assert.Equal(t, tc.changed, changed)
			assert.Equal(t, tc.want, f.calls)
		})
	}
}

func TestSystemdInstallReturnsAFailingCall(t *testing.T) {
	host(t, "/bin")

	f := &fake{fail: map[string]bool{"systemctl --user enable cavu.service": true}}

	_, _, err := Manager{OS: "linux", Run: f.run}.Install(t.Context(), "/usr/local/bin/cavu", "/srv/cavu", false)
	require.ErrorIs(t, err, errExit)
	assert.Equal(t, userctl("enable", "cavu.service"), f.calls[len(f.calls)-1], "nothing runs after the failing call")
}

func TestSystemdUninstall(t *testing.T) {
	disable := userctl("disable", "--now", "cavu.service")

	cases := []struct {
		name      string
		installed bool
		fail      bool
		removed   bool
		want      [][]string
	}{
		{"installed", true, false, true, [][]string{disable, userctl("daemon-reload")}},
		{"not installed", false, false, false, nil},
		{"a failing disable", true, true, false, [][]string{disable}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := host(t, "/bin")
			unit := filepath.Join(home, ".config", "systemd", "user", "cavu.service")
			other := filepath.Join(home, ".config", "systemd", "user", "other.service")

			require.NoError(t, os.MkdirAll(filepath.Dir(unit), 0o755))
			require.NoError(t, os.WriteFile(other, []byte("[Unit]\n"), 0o644))

			if tc.installed {
				require.NoError(t, os.WriteFile(unit, []byte(wantUnit), 0o644))
			}

			f := &fake{fail: map[string]bool{strings.Join(disable, " "): tc.fail}}

			path, removed, err := Manager{OS: "linux", Run: f.run}.Uninstall(t.Context())
			if tc.fail {
				require.ErrorIs(t, err, errExit)
			} else {
				require.NoError(t, err)
			}

			assert.Equal(t, unit, path)
			assert.Equal(t, tc.removed, removed)
			assert.Equal(t, tc.want, f.calls)
			assert.Equal(t, tc.installed && !tc.removed, fileExists(unit))
			assert.FileExists(t, other)
		})
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)

	return err == nil
}

func TestLaunchdInstall(t *testing.T) {
	const printCmd, target = "launchctl print gui/501/com.github.mhersson.cavu", "gui/501/com.github.mhersson.cavu"

	install := func(t *testing.T, f *fake) string {
		t.Helper()

		path, _, err := Manager{OS: "darwin", UID: 501, Run: f.run}.Install(t.Context(), "/usr/local/bin/cavu", "/srv/cavu", false)
		require.NoError(t, err)

		return path
	}

	t.Run("not loaded", func(t *testing.T) {
		home := host(t, "/bin")
		f := &fake{fail: map[string]bool{printCmd: true}}

		path := install(t, f)

		assert.Equal(t, [][]string{
			strings.Fields(dockerInspect), strings.Fields(printCmd), {"launchctl", "bootstrap", "gui/501", path},
		}, f.calls)
		assert.Equal(t, filepath.Join(home, "Library", "LaunchAgents", "com.github.mhersson.cavu.plist"), path)
		assert.DirExists(t, filepath.Join(home, "Library", "Logs"))
	})

	t.Run("loaded and changed", func(t *testing.T) {
		host(t, "/bin")

		path := install(t, &fake{fail: map[string]bool{printCmd: true}})
		require.NoError(t, os.WriteFile(path, []byte("old"), 0o644))

		f := &fake{}
		install(t, f)

		assert.Equal(t, [][]string{
			strings.Fields(dockerInspect), strings.Fields(printCmd),
			{"launchctl", "bootout", target},
			{"launchctl", "bootstrap", "gui/501", path},
		}, f.calls)
	})

	t.Run("loaded and unchanged", func(t *testing.T) {
		host(t, "/bin")

		install(t, &fake{fail: map[string]bool{printCmd: true}})

		f := &fake{}
		install(t, f)

		assert.Equal(t, [][]string{
			strings.Fields(dockerInspect), strings.Fields(printCmd), {"launchctl", "kickstart", "-k", target},
		}, f.calls)
	})
}

func TestLaunchdRefusesLinger(t *testing.T) {
	home := host(t, "/bin")
	f := &fake{}

	_, _, err := Manager{OS: "darwin", UID: 501, Run: f.run}.Install(t.Context(), "/usr/local/bin/cavu", "/srv/cavu", true)
	require.Error(t, err)
	assert.Empty(t, f.calls)

	entries, err := os.ReadDir(home)
	require.NoError(t, err)
	assert.Empty(t, entries)
}

func TestLaunchdUninstall(t *testing.T) {
	const printCmd, target = "launchctl print gui/501/com.github.mhersson.cavu", "gui/501/com.github.mhersson.cavu"

	cases := []struct {
		name      string
		installed bool
		loaded    bool
		want      [][]string
	}{
		{"loaded", true, true, [][]string{strings.Fields(printCmd), {"launchctl", "bootout", target}}},
		{"not loaded", true, false, [][]string{strings.Fields(printCmd)}},
		{"not installed", false, true, nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := host(t, "/bin")
			agent := filepath.Join(home, "Library", "LaunchAgents", "com.github.mhersson.cavu.plist")

			if tc.installed {
				require.NoError(t, os.MkdirAll(filepath.Dir(agent), 0o755))
				require.NoError(t, os.WriteFile(agent, []byte(wantAgent), 0o644))
			}

			f := &fake{fail: map[string]bool{printCmd: !tc.loaded}}

			path, removed, err := Manager{OS: "darwin", UID: 501, Run: f.run}.Uninstall(t.Context())
			require.NoError(t, err)
			assert.Equal(t, agent, path)
			assert.Equal(t, tc.installed, removed)
			assert.Equal(t, tc.want, f.calls)
			assert.NoFileExists(t, agent)
		})
	}
}

func TestOtherSystemsAreRefused(t *testing.T) {
	home := host(t, "/bin")
	f := &fake{}
	m := Manager{OS: "windows", Run: f.run}

	_, _, err := m.Install(t.Context(), "/usr/local/bin/cavu", "/srv/cavu", false)
	require.Error(t, err)

	_, _, err = m.Uninstall(t.Context())
	require.Error(t, err)

	assert.Empty(t, f.calls)

	entries, err := os.ReadDir(home)
	require.NoError(t, err)
	assert.Empty(t, entries)
}
