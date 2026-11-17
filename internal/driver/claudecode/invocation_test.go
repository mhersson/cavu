package claudecode

import (
	"encoding/json"
	"path"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/cavu/internal/driver"
	"github.com/mhersson/cavu/internal/redact"
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
func runFake(t *testing.T, f *fakeRun) fakeRecorded {
	t.Helper()

	res, err := f.run(t.Context())
	require.NoError(t, err)
	require.Equal(t, driver.ReasonDone, res.Reason, res.Detail)

	return f.recorded(t)
}

func TestArgvKeyMode(t *testing.T) {
	f := newFakeRun(t, fakeScenario{Replay: fixture(t, "real/t1.jsonl")})
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
	f := newFakeRun(t, fakeScenario{Replay: fixture(t, "real/t1.jsonl")})

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
	f := newFakeRun(t, fakeScenario{Replay: fixture(t, "real/t1.jsonl")})
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
	f := newFakeRun(t, fakeScenario{Replay: fixture(t, "real/t1.jsonl")})
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

	f := newFakeRun(t, fakeScenario{Replay: fixture(t, "real/t1.jsonl")})
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
		f := newFakeRun(t, fakeScenario{Replay: fixture(t, "real/t1.jsonl")})
		f.spec.WebTools = on

		rec := runFake(t, f)
		assert.Equal(t, !on, slices.Contains(rec.Args, "--disallowedTools"), "web tools %v", on)
		assert.Equal(t, !on, slices.Contains(rec.EnvNames, "CLAUDE_CODE_DISABLE_WEB_FETCH"), "web tools %v", on)
	}
}

// splitDisallowed returns the --disallowedTools list, which like the CLI's
// variadic option ends at the next flag, and the rest of argv with the
// invocation's own directory as a placeholder.
func splitDisallowed(t *testing.T, argv []string) (tools, rest []string) {
	t.Helper()

	dir := path.Dir(flagValue(t, argv, "--settings"))

	for i := 0; i < len(argv); i++ {
		if argv[i] != "--disallowedTools" {
			rest = append(rest, strings.ReplaceAll(argv[i], dir, "<dir>"))

			continue
		}

		for i+1 < len(argv) && !strings.HasPrefix(argv[i+1], "--") {
			i++
			tools = append(tools, argv[i])
		}
	}

	return tools, rest
}

// TestInvocationReadOnly: a read-only spec's argv differs from the same spec's
// without the flag only in the editing tools it disallows.
func TestInvocationReadOnly(t *testing.T) {
	for _, web := range []bool{false, true} {
		t.Run("web tools "+strconv.FormatBool(web), func(t *testing.T) {
			d := claudeCode{binary: "claude", runRoot: runRoot}
			s := driver.Spec{
				Auth: driver.Auth{Mode: modeSubscription, Token: testToken}, Model: testModel, MaxTurns: 1,
				SessionID: testSessionID, Prompt: testPrompt, Instructions: testInstr, WebTools: web,
				OutputSchema: json.RawMessage(`{"type":"object"}`), Home: "/home/agent", ConfigDir: "/home/agent/.claude-cavu",
			}

			plain, err := d.newInvocation(s)
			require.NoError(t, err)

			s.ReadOnly = true

			readOnly, err := d.newInvocation(s)
			require.NoError(t, err)

			plainTools, plainRest := splitDisallowed(t, plain.argv)
			tools, rest := splitDisallowed(t, readOnly.argv)
			assert.Equal(t, plainRest, rest)
			assert.Equal(t, append(plainTools, "Edit", "Write", "NotebookEdit"), tools)
		})
	}
}

func TestSpecRefused(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*driver.Spec)
	}{
		{"no model", func(s *driver.Spec) { s.Model = "" }},
		{"no home", func(s *driver.Spec) { s.Home = "" }},
		{"home is /", func(s *driver.Spec) { s.Home, s.ConfigDir = "/", "/.claude-cavu" }},
		{"relative home", func(s *driver.Spec) { s.Home, s.ConfigDir = "home", "home/.claude-cavu" }},
		{"unclean home", func(s *driver.Spec) { s.Home += "/" }},
		{"no config dir", func(s *driver.Spec) { s.ConfigDir = "" }},
		{"config dir /", func(s *driver.Spec) { s.ConfigDir = "/" }},
		{"config dir is home", func(s *driver.Spec) { s.ConfigDir = s.Home }},
		{"config dir is home/./", func(s *driver.Spec) { s.ConfigDir = s.Home + "/./" }},
		{"config dir above home", func(s *driver.Spec) { s.ConfigDir = path.Dir(s.Home) }},
		{"config dir beside home", func(s *driver.Spec) { s.ConfigDir = s.Home + "-beside" }},
		{"config dir escaping home with ..", func(s *driver.Spec) { s.ConfigDir = s.Home + "/../escaped" }},
		{"config dir is home/", func(s *driver.Spec) { s.ConfigDir = s.Home + "/" }},
		{"config dir two levels below home", func(s *driver.Spec) { s.ConfigDir = s.Home + "/nested/.claude-cavu" }},
		{"relative config dir", func(s *driver.Spec) { s.ConfigDir = ".claude-cavu" }},
		{"dir is the config dir", func(s *driver.Spec) { s.Dir = s.ConfigDir }},
		{"dir below the config dir", func(s *driver.Spec) { s.Dir = s.ConfigDir + "/repo" }},
		{"relative dir", func(s *driver.Spec) { s.Dir = "repo" }},
		{"unclean dir", func(s *driver.Spec) { s.Dir += "/../repo" }},
		{"short base commit", func(s *driver.Spec) { s.BaseCommit = "abc123" }},
		{"option as base commit", func(s *driver.Spec) { s.BaseCommit = "--output=/tmp/x" }},
		{"base commit without a dir", func(s *driver.Spec) { s.BaseCommit, s.Dir = testBase, "" }},
		{"no prompt", func(s *driver.Spec) { s.Prompt = "" }},
		{"no turns", func(s *driver.Spec) { s.MaxTurns = 0 }},
		{"session and resume", func(s *driver.Spec) { s.Resume = testSessionID }},
		{"neither session nor resume", func(s *driver.Spec) { s.SessionID = "" }},
		{"session id not a uuid", func(s *driver.Spec) { s.SessionID = "../x" }},
		{"resume id not a uuid", func(s *driver.Spec) { s.SessionID, s.Resume = "", "*" }},
		{"unknown effort", func(s *driver.Spec) { s.Effort = "minimal" }},
		{"effort case", func(s *driver.Spec) { s.Effort = "High" }},
		{"unknown auth mode", func(s *driver.Spec) { s.Auth.Mode = "oauth" }},
		{"subscription without token", func(s *driver.Spec) { s.Auth.Token = "" }},
		{"subscription token too short to mask", func(s *driver.Spec) { s.Auth.Token = "åbcdefg" }},
		{"key mode token too short to mask", func(s *driver.Spec) {
			s.Auth = driver.Auth{Mode: modeKey, Token: "abcdefg", BaseURL: testBridgeURL}
		}},
		{"key mode without token", func(s *driver.Spec) { s.Auth = driver.Auth{Mode: modeKey, BaseURL: testBridgeURL} }},
		{"key mode without base url", func(s *driver.Spec) { s.Auth = driver.Auth{Mode: modeKey, Token: testStandIn} }},
		{"schema not json", func(s *driver.Spec) { s.OutputSchema = json.RawMessage(`{"type":`) }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeRun(t, fakeScenario{Replay: fixture(t, "real/t1.jsonl")})
			tt.mutate(&f.spec)

			_, err := f.run(t.Context())
			require.Error(t, err)
			assert.Empty(t, f.target.started(), "a refused spec started a process")
			assert.Empty(t, f.target.rec.seen(), "a refused spec reached the recording target")
		})
	}

	for _, effort := range []string{"", "low", "medium", "high", "xhigh", "max"} {
		f := newFakeRun(t, fakeScenario{Replay: fixture(t, "real/t1.jsonl")})
		f.spec.Effort = effort

		_, err := f.run(t.Context())
		require.NoError(t, err, effort)
	}
}

// The token floor must be redact's: a token validate accepts is one the mask
// covers, and one rune less is one it would leave in the record.
func TestTokenFloorIsRedacts(t *testing.T) {
	spec := func(token string) driver.Spec {
		return driver.Spec{
			Auth: driver.Auth{Mode: modeSubscription, Token: token}, Model: testModel, MaxTurns: 1,
			SessionID: testSessionID, Prompt: testPrompt, Home: "/home/agent", ConfigDir: "/home/agent/.claude-cavu",
		}
	}

	// Multi-byte runes, so a floor counted in bytes on either side fails too.
	floor := strings.Repeat("å", minTokenLen)
	require.NoError(t, validate(spec(floor)))
	assert.NotContains(t, redact.New(floor)(floor), floor, "redact masks a token at the floor")

	below := strings.Repeat("å", minTokenLen-1)
	require.Error(t, validate(spec(below)))
	assert.Equal(t, below, redact.New(below)(below), "redact leaves a token below the floor unmasked")
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

func TestEnvProxyAppendsLoopback(t *testing.T) {
	const loopback = "127.0.0.1,localhost,::1"

	spec := func(env ...string) driver.Spec {
		return driver.Spec{Auth: keyAuth(), Home: "/home/agent", ConfigDir: "/home/agent/.claude-cavu", Env: env}
	}

	for _, name := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"} {
		env := environment(spec(name + "=http://proxy:3128"))

		for _, np := range []string{"NO_PROXY", "no_proxy"} {
			v, ok := envValue(t, env, np)
			require.True(t, ok, "%s with %s", np, name)
			assert.Equal(t, loopback, v, "%s with %s", np, name)
		}
	}

	env := environment(spec("HTTPS_PROXY=http://p:1", "NO_PROXY=corp", "no_proxy=.lan"))
	upper, _ := envValue(t, env, "NO_PROXY")
	lower, _ := envValue(t, env, "no_proxy")
	assert.Equal(t, "corp,"+loopback, upper)
	assert.Equal(t, ".lan,"+loopback, lower)

	for _, e := range [][]string{{"PATH=/bin"}, {"HTTP_PROXY="}} {
		env := environment(spec(e...))
		_, upperOK := envValue(t, env, "NO_PROXY")
		_, lowerOK := envValue(t, env, "no_proxy")
		assert.False(t, upperOK || lowerOK, "%v", e)
	}
}

// Spec.Env overrides the base, but every auth value comes from Spec.Auth
// whatever Spec.Env holds.
func TestEnvValues(t *testing.T) {
	tests := []struct {
		name   string
		auth   driver.Auth
		want   map[string]string
		absent []string
	}{
		{
			name:   "subscription",
			auth:   driver.Auth{Mode: modeSubscription, Token: testToken},
			want:   map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": testToken},
			absent: []string{"ANTHROPIC_BASE_URL", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_API_KEY"},
		},
		{
			name:   "key",
			auth:   keyAuth(),
			want:   map[string]string{"ANTHROPIC_BASE_URL": testBridgeURL, "ANTHROPIC_AUTH_TOKEN": testStandIn},
			absent: []string{"CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_API_KEY"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := environment(driver.Spec{
				Auth: tt.auth, Home: "/home/agent", ConfigDir: "/home/agent/.claude-cavu",
				Env: append([]string{"PATH=/opt/go/bin:/usr/bin"}, strayAuth...),
			})

			tt.want["PATH"] = "/opt/go/bin:/usr/bin"
			tt.want["HOME"] = "/home/agent"
			tt.want["CLAUDE_CONFIG_DIR"] = "/home/agent/.claude-cavu"

			for name, want := range tt.want {
				v, ok := envValue(t, env, name)
				require.True(t, ok, name)
				assert.Equal(t, want, v, name)
			}

			for _, name := range tt.absent {
				_, ok := envValue(t, env, name)
				assert.False(t, ok, name)
			}
		})
	}
}
