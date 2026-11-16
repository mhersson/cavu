package claudecode

import (
	"encoding/json"
	"path"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/cavu/internal/driver"
	"github.com/mhersson/cavu/internal/testkit/fakeclaude"
)

// baseArgv is the flag set every invocation opens with.
func baseArgv(settings string) []string {
	return []string{
		"-p",
		"--output-format", "stream-json",
		"--input-format", "stream-json",
		"--verbose",
		"--include-partial-messages",
		"--setting-sources", "user",
		"--strict-mcp-config",
		"--system-prompt-snapshot", "off",
		"--permission-mode", "bypassPermissions",
		"--model", testModel,
		"--settings", settings,
	}
}

func requireKeptOutOfArgv(t *testing.T, args []string, values ...string) {
	t.Helper()

	for _, a := range args {
		for _, v := range values {
			require.NotContains(t, a, v)
		}
	}
}

// runFake runs f and requires a done result.
func runFake(t *testing.T, f *fakeRun) fakeclaude.Recorded {
	t.Helper()

	res, err := f.run(t.Context())
	require.NoError(t, err)
	require.Equal(t, driver.ReasonDone, res.Reason, res.Detail)

	return f.recorded(t)
}

func TestArgvKeyMode(t *testing.T) {
	f := newFakeRun(t, fakeclaude.Scenario{Replay: fixture(t, "real/t1.jsonl")})
	f.spec.Auth = keyAuth()
	f.spec.Instructions = testInstr
	f.spec.OutputSchema = json.RawMessage(`{"type": "object",
		"properties": {"ok": {"type": "boolean"}}}`)
	f.spec.MaxTurns = 7
	f.spec.Effort = "high"

	args := runFake(t, f).Args
	settings := flagValue(t, args, "--settings")
	instructions := flagValue(t, args, "--append-system-prompt-file")

	want := append(baseArgv(settings),
		"--disallowedTools", "WebFetch", "WebSearch",
		"--append-system-prompt-file", instructions,
		"--json-schema", `{"type":"object","properties":{"ok":{"type":"boolean"}}}`,
		"--session-id", testSessionID,
		"--max-turns", "7",
		"--effort", "high",
	)
	assert.Equal(t, want, args)
	assert.Equal(t, path.Dir(settings), path.Dir(instructions))
	requireKeptOutOfArgv(t, args, testPrompt, testInstr, testStandIn)
}

func TestArgvSubscriptionMode(t *testing.T) {
	f := newFakeRun(t, fakeclaude.Scenario{Replay: fixture(t, "real/t1.jsonl")})

	args := runFake(t, f).Args
	settings := flagValue(t, args, "--settings")

	want := append(baseArgv(settings),
		"--disallowedTools", "WebFetch", "WebSearch",
		"--session-id", testSessionID,
		"--max-turns", "10",
	)
	assert.Equal(t, want, args)
	assert.Equal(t, f.d.runRoot, path.Dir(path.Dir(settings)))
	requireKeptOutOfArgv(t, args, testPrompt, testToken)
}

// strayAuth is every auth and provider variable a caller could pass in
// Spec.Env.
var strayAuth = []string{
	"ANTHROPIC_API_KEY=stray", "ANTHROPIC_AUTH_TOKEN=stray", "ANTHROPIC_BASE_URL=http://127.0.0.1:1",
	"CLAUDE_CODE_OAUTH_TOKEN=stray", "ANTHROPIC_UNIX_SOCKET=/tmp/s", "CLAUDE_CODE_USE_BEDROCK=1",
	"CLAUDE_CODE_SIMPLE=1", "CLAUDE_CODE_RETRY_WATCHDOG=1",
}

func TestEnvKeyMode(t *testing.T) {
	f := newFakeRun(t, fakeclaude.Scenario{Replay: fixture(t, "real/t1.jsonl")})
	f.spec.Auth = keyAuth()
	f.spec.Env = append(f.spec.Env, strayAuth...)

	names := runFake(t, f).EnvNames
	assert.Subset(t, names, []string{"ANTHROPIC_BASE_URL", "ANTHROPIC_AUTH_TOKEN"})

	for _, name := range []string{
		"CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_API_KEY", "ANTHROPIC_UNIX_SOCKET",
		"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_SIMPLE", "CLAUDE_CODE_RETRY_WATCHDOG",
	} {
		assert.NotContains(t, names, name)
	}
}

func TestEnvSubscriptionMode(t *testing.T) {
	f := newFakeRun(t, fakeclaude.Scenario{Replay: fixture(t, "real/t1.jsonl")})
	f.spec.Env = append(f.spec.Env, strayAuth...)

	names := runFake(t, f).EnvNames
	assert.Contains(t, names, "CLAUDE_CODE_OAUTH_TOKEN")

	for _, name := range []string{
		"ANTHROPIC_BASE_URL", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_API_KEY", "ANTHROPIC_UNIX_SOCKET",
		"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_SIMPLE", "CLAUDE_CODE_RETRY_WATCHDOG",
	} {
		assert.NotContains(t, names, name)
	}
}

func TestEnvIsNotInherited(t *testing.T) {
	t.Setenv("CAVU_TEST_INHERITED", "1")

	f := newFakeRun(t, fakeclaude.Scenario{Replay: fixture(t, "real/t1.jsonl")})
	f.spec.Env = append(f.spec.Env, "EXTRA=1")

	want := []string{
		"HOME", "CLAUDE_CONFIG_DIR", "PATH", "DISABLE_UPDATES", "DISABLE_AUTOUPDATER",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC", "CLAUDE_CODE_DISABLE_FAST_MODE",
		"CLAUDE_CODE_DISABLE_BACKGROUND_TASKS", "CLAUDE_CODE_DISABLE_WEB_FETCH", "CLAUDE_CODE_OAUTH_TOKEN",
	}

	for _, kv := range f.spec.Env {
		name, _, _ := strings.Cut(kv, "=")
		want = append(want, name)
	}

	rec := runFake(t, f)
	assert.ElementsMatch(t, want, rec.EnvNames)
	assert.Equal(t, f.spec.ConfigDir, rec.ConfigDir)
}

func TestWebToolsSwitch(t *testing.T) {
	for _, on := range []bool{false, true} {
		f := newFakeRun(t, fakeclaude.Scenario{Replay: fixture(t, "real/t1.jsonl")})
		f.spec.WebTools = on

		rec := runFake(t, f)
		assert.Equal(t, !on, slices.Contains(rec.Args, "--disallowedTools"), "web tools %v", on)
		assert.Equal(t, !on, slices.Contains(rec.EnvNames, "CLAUDE_CODE_DISABLE_WEB_FETCH"), "web tools %v", on)
	}
}

func TestSpecRefused(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*driver.Spec)
	}{
		{"no model", func(s *driver.Spec) { s.Model = "" }},
		{"no turns", func(s *driver.Spec) { s.MaxTurns = 0 }},
		{"session and resume", func(s *driver.Spec) { s.Resume = testSessionID }},
		{"neither session nor resume", func(s *driver.Spec) { s.SessionID = "" }},
		{"session id not a uuid", func(s *driver.Spec) { s.SessionID = "../x" }},
		{"resume id not a uuid", func(s *driver.Spec) { s.SessionID, s.Resume = "", "*" }},
		{"unknown effort", func(s *driver.Spec) { s.Effort = "minimal" }},
		{"effort case", func(s *driver.Spec) { s.Effort = "High" }},
		{"unknown auth mode", func(s *driver.Spec) { s.Auth.Mode = "oauth" }},
		{"subscription without token", func(s *driver.Spec) { s.Auth.Token = "" }},
		{"key mode without token", func(s *driver.Spec) { s.Auth = driver.Auth{Mode: modeKey, BaseURL: testBridgeURL} }},
		{"key mode without base url", func(s *driver.Spec) { s.Auth = driver.Auth{Mode: modeKey, Token: testStandIn} }},
		{"schema not json", func(s *driver.Spec) { s.OutputSchema = json.RawMessage(`{"type":`) }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeRun(t, fakeclaude.Scenario{Replay: fixture(t, "real/t1.jsonl")})
			tt.mutate(&f.spec)

			_, err := f.run(t.Context())
			require.Error(t, err)
			assert.Empty(t, f.target.started(), "a refused spec started a process")
		})
	}

	for _, effort := range []string{"", "low", "medium", "high", "xhigh", "max"} {
		f := newFakeRun(t, fakeclaude.Scenario{Replay: fixture(t, "real/t1.jsonl")})
		f.spec.Effort = effort

		_, err := f.run(t.Context())
		require.NoError(t, err, effort)
	}
}

// envValue returns the value of name in env, and whether env holds it.
func envValue(t *testing.T, env []string, name string) (string, bool) {
	t.Helper()

	var value string

	found := false

	for _, kv := range env {
		if k, v, _ := strings.Cut(kv, "="); k == name {
			require.False(t, found, "%s appears twice", name)

			value, found = v, true
		}
	}

	return value, found
}

func TestEnvProxyBypassesLoopback(t *testing.T) {
	const loopback = "127.0.0.1,localhost,::1"

	spec := func(env ...string) driver.Spec {
		return driver.Spec{Auth: keyAuth(), Home: "/home/agent", ConfigDir: "/home/agent/.claude-cavu", Env: env}
	}

	for _, name := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"} {
		t.Run(name, func(t *testing.T) {
			env := environment(spec("PATH=/bin", name+"=http://proxy:3128"))

			for _, np := range []string{"NO_PROXY", "no_proxy"} {
				v, ok := envValue(t, env, np)
				require.True(t, ok, np)
				assert.Equal(t, loopback, v, np)
			}

			assert.Contains(t, env, name+"=http://proxy:3128")
		})
	}

	for _, tc := range []struct {
		name         string
		env          []string
		upper, lower string
		set          bool
	}{
		{name: "no proxy", env: []string{"PATH=/bin"}},
		{name: "empty proxy", env: []string{"PATH=/bin", "HTTP_PROXY="}},
		{name: "no proxy keeps NO_PROXY as is", env: []string{"NO_PROXY=corp"}, upper: "corp", set: true},
		{
			name: "NO_PROXY kept and seeds no_proxy", env: []string{"HTTPS_PROXY=http://p:1", "NO_PROXY=corp.internal"},
			upper: "corp.internal," + loopback, lower: "corp.internal," + loopback, set: true,
		},
		{
			name: "no_proxy kept and seeds NO_PROXY", env: []string{"https_proxy=http://p:1", "no_proxy=.corp"},
			upper: ".corp," + loopback, lower: ".corp," + loopback, set: true,
		},
		{
			name: "each form keeps its own entries", env: []string{"ALL_PROXY=socks5://p:1", "NO_PROXY=a", "no_proxy=b, localhost"},
			upper: "a," + loopback, lower: "b, localhost,127.0.0.1,::1", set: true,
		},
		{
			name: "empty no_proxy starts from NO_PROXY", env: []string{"HTTPS_PROXY=http://p:1", "NO_PROXY=corp", "no_proxy="},
			upper: "corp," + loopback, lower: "corp," + loopback, set: true,
		},
		{
			name: "loopback already listed", env: []string{"HTTP_PROXY=http://p:1", "NO_PROXY=::1,LOCALHOST,127.0.0.1"},
			upper: "::1,LOCALHOST,127.0.0.1", lower: "::1,LOCALHOST,127.0.0.1", set: true,
		},
		{
			name: "trailing comma", env: []string{"HTTP_PROXY=http://p:1", "NO_PROXY=corp,"},
			upper: "corp," + loopback, lower: "corp," + loopback, set: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := environment(spec(tc.env...))

			upper, upperOK := envValue(t, env, "NO_PROXY")
			lower, lowerOK := envValue(t, env, "no_proxy")

			if !tc.set {
				assert.False(t, upperOK, "NO_PROXY=%s", upper)
				assert.False(t, lowerOK, "no_proxy=%s", lower)

				return
			}

			assert.Equal(t, tc.upper, upper, "NO_PROXY")

			if tc.lower == "" {
				assert.False(t, lowerOK, "no_proxy=%s", lower)

				return
			}

			assert.Equal(t, tc.lower, lower, "no_proxy")
		})
	}
}

func TestEnvSpecOverridesBase(t *testing.T) {
	env := environment(driver.Spec{
		Auth: driver.Auth{Mode: modeSubscription, Token: testToken}, Home: "/home/agent", ConfigDir: "/c",
		Env: []string{"PATH=/opt/go/bin:/usr/bin", "CLAUDE_CODE_OAUTH_TOKEN=stray"},
	})

	v, ok := envValue(t, env, "PATH")
	require.True(t, ok)
	assert.Equal(t, "/opt/go/bin:/usr/bin", v)

	v, ok = envValue(t, env, "CLAUDE_CODE_OAUTH_TOKEN")
	require.True(t, ok)
	assert.Equal(t, testToken, v)
}
