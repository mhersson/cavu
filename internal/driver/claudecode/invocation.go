package claudecode

import (
	"bytes"
	"cmp"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/mhersson/cavu/internal/driver"
	"github.com/mhersson/cavu/internal/proc"
)

// Auth modes.
const (
	modeSubscription = "subscription"
	modeKey          = "key"
)

// basePath is the PATH a command gets unless Spec.Env sets one.
const basePath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// settings turns attribution off (empty texts drop the Co-Authored-By trailer
// and the PR footer, sessionUrl false the session link line) and every hook,
// a staged skill's own included: CAVU uses none.
const settings = `{"attribution":{"commit":"","pr":"","sessionUrl":false},"disableAllHooks":true}`

// minTokenLen is the shortest secret internal/redact masks; a shorter token
// would reach the record unmasked.
const minTokenLen = 8

var effortLevels = []string{"low", "medium", "high", "xhigh", "max"}

// uuidPattern guards --session-id and --resume: the pinned release reads a
// --resume value that is no UUID as a session name or a picker search term.
var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// authEnv are never taken from Spec.Env: each one carries a credential, moves
// requests off the endpoint the auth mode names, or makes a usage limit wait.
var authEnv = []string{
	"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL", "ANTHROPIC_UNIX_SOCKET",
	"CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CODE_API_KEY_FILE_DESCRIPTOR", "CLAUDE_CODE_HOST_AUTH_ENV_VAR",
	"CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST", "CLAUDE_CODE_SIMPLE",
	"CLAUDE_CODE_USE_ANTHROPIC_AWS", "CLAUDE_CODE_USE_ANTHROPIC_GOOGLE_CLOUD", "CLAUDE_CODE_USE_BEDROCK",
	"CLAUDE_CODE_USE_FOUNDRY", "CLAUDE_CODE_USE_GATEWAY", "CLAUDE_CODE_USE_MANTLE", "CLAUDE_CODE_USE_VERTEX",
	"CLAUDE_CODE_RETRY_WATCHDOG",
}

// invocation is one claude process: its argv, environment, the files it
// reads, and the user message written after the initialize gate.
type invocation struct {
	argv   []string
	env    []string
	files  map[string][]byte
	prompt []byte
}

func validate(s driver.Spec) error {
	switch {
	case s.Auth.Mode != modeSubscription && s.Auth.Mode != modeKey:
		return fmt.Errorf("claudecode: auth mode %q is not %s or %s", s.Auth.Mode, modeSubscription, modeKey)
	case utf8.RuneCountInString(s.Auth.Token) < minTokenLen:
		return fmt.Errorf("claudecode: %s mode needs a token of at least %d characters", s.Auth.Mode, minTokenLen)
	case s.Auth.Mode == modeKey && s.Auth.BaseURL == "":
		return errors.New("claudecode: key mode without a base URL")
	case s.Model == "":
		return errors.New("claudecode: no model")
	// Every session clears the config dir, so these hold before any command
	// is composed; an empty value would also override the image's and lose
	// the transcript.
	case !cleanAbs(s.Home) || s.Home == "/":
		return fmt.Errorf("claudecode: home %q is not an absolute, clean path below /", s.Home)
	case !cleanAbs(s.ConfigDir) || !strings.HasPrefix(s.ConfigDir, s.Home+"/"):
		return fmt.Errorf("claudecode: config dir %q is not a clean path below the home %q", s.ConfigDir, s.Home)
	case s.Dir != "" && !cleanAbs(s.Dir):
		return fmt.Errorf("claudecode: dir %q is not an absolute, clean path", s.Dir)
	case s.Dir == s.ConfigDir || strings.HasPrefix(s.Dir, s.ConfigDir+"/"):
		return fmt.Errorf("claudecode: the config dir %q holds the dir %q", s.ConfigDir, s.Dir)
	case s.Prompt == "":
		return errors.New("claudecode: no prompt")
	case s.MaxTurns < 1:
		return fmt.Errorf("claudecode: max turns %d is below 1", s.MaxTurns)
	case (s.SessionID == "") == (s.Resume == ""):
		return errors.New("claudecode: exactly one of session id and resume must be set")
	case !uuidPattern.MatchString(cmp.Or(s.SessionID, s.Resume)):
		return fmt.Errorf("claudecode: session id %q is not a UUID", cmp.Or(s.SessionID, s.Resume))
	case s.Effort != "" && !slices.Contains(effortLevels, s.Effort):
		return fmt.Errorf("claudecode: effort %q is not one of %v", s.Effort, effortLevels)
	case s.BaseCommit != "" && !commitPattern.MatchString(s.BaseCommit):
		return fmt.Errorf("claudecode: base commit %q is not a full commit id", s.BaseCommit)
	case s.BaseCommit != "" && s.Dir == "":
		return errors.New("claudecode: a base commit needs a dir")
	}

	return nil
}

func cleanAbs(p string) bool {
	return path.IsAbs(p) && path.Clean(p) == p
}

// newInvocation builds the process for s. Its files go under a fresh
// directory in c.runRoot; the prompt, the instructions and the token never
// enter argv.
func (c claudeCode) newInvocation(s driver.Spec) (invocation, error) {
	if err := validate(s); err != nil {
		return invocation{}, err
	}

	var schema bytes.Buffer
	if len(s.OutputSchema) > 0 {
		if err := json.Compact(&schema, s.OutputSchema); err != nil {
			return invocation{}, fmt.Errorf("claudecode: output schema: %w", err)
		}
	}

	dir := path.Join(c.runRoot, randomID())
	settingsPath := path.Join(dir, "settings.json")
	files := map[string][]byte{settingsPath: []byte(settings)}

	argv := []string{
		c.binary,
		"-p",
		"--output-format", "stream-json",
		"--input-format", "stream-json",
		"--verbose",
		"--include-partial-messages",
		"--setting-sources", "user",
		"--strict-mcp-config",
		"--system-prompt-snapshot", "off",
		"--permission-mode", "bypassPermissions",
		"--model", s.Model,
		"--settings", settingsPath,
	}

	if !s.WebTools {
		argv = append(argv, "--disallowedTools", "WebFetch", "WebSearch")
	}

	if s.Instructions != "" {
		p := path.Join(dir, "instructions.md")
		files[p] = []byte(s.Instructions)
		argv = append(argv, "--append-system-prompt-file", p)
	}

	if schema.Len() > 0 {
		argv = append(argv, "--json-schema", schema.String())
	}

	if s.SessionID != "" {
		argv = append(argv, "--session-id", s.SessionID)
	} else {
		argv = append(argv, "--resume", s.Resume)
	}

	argv = append(argv, "--max-turns", strconv.Itoa(s.MaxTurns))

	if s.Effort != "" {
		argv = append(argv, "--effort", s.Effort)
	}

	return invocation{argv: argv, env: environment(s), files: files, prompt: userMessage(s.Prompt)}, nil
}

func randomID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)

	return hex.EncodeToString(b)
}

// quietEnv is what every command gets: a PATH, and no self-update or traffic
// beyond the model calls.
func quietEnv() map[string]string {
	return map[string]string{
		"PATH":                basePath,
		"DISABLE_UPDATES":     "1",
		"DISABLE_AUTOUPDATER": "1",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
	}
}

func probeEnvironment() []string {
	return proc.BuildEnv(nil, nil, quietEnv())
}

// environment builds the whole process environment: nothing is inherited, a
// Spec.Env entry overrides a base entry, and only the auth mode sets the auth
// variables.
func environment(s driver.Spec) []string {
	set := quietEnv()
	set["HOME"] = s.Home
	set["CLAUDE_CONFIG_DIR"] = s.ConfigDir
	set["CLAUDE_CODE_DISABLE_FAST_MODE"] = "1"
	// A background task finishing after the result starts a turn nobody reads,
	// and a timed-out foreground command would move to the background.
	set["CLAUDE_CODE_DISABLE_BACKGROUND_TASKS"] = "1"

	if !s.WebTools {
		set["CLAUDE_CODE_DISABLE_WEB_FETCH"] = "1"
	}

	for _, kv := range s.Env {
		if k, v, _ := strings.Cut(kv, "="); !slices.Contains(authEnv, k) {
			set[k] = v
		}
	}

	switch s.Auth.Mode {
	case modeSubscription:
		set["CLAUDE_CODE_OAUTH_TOKEN"] = s.Auth.Token
	case modeKey:
		set["ANTHROPIC_BASE_URL"] = s.Auth.BaseURL
		set["ANTHROPIC_AUTH_TOKEN"] = s.Auth.Token
	}

	maps.Copy(set, noProxyEnv(s.Env))

	return proc.BuildEnv(nil, nil, set)
}

// proxyEnv are the variables that name a proxy. The pinned release sends
// loopback through it too, so a proxied run needs loopbackHosts in NO_PROXY.
var (
	proxyEnv      = []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"}
	loopbackHosts = []string{"127.0.0.1", "localhost", "::1"}
)

// noProxyEnv returns NO_PROXY and no_proxy with loopbackHosts added when env
// names a proxy, nil otherwise. Each form keeps its own entries, or the other
// form's when unset or empty, so a reader preferring either form still skips
// them.
func noProxyEnv(env []string) map[string]string {
	vals := map[string]string{}
	proxied := false

	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")

		switch {
		case slices.Contains(proxyEnv, k):
			proxied = proxied || v != ""
		case k == "NO_PROXY" || k == "no_proxy":
			vals[k] = v
		}
	}

	if !proxied {
		return nil
	}

	out := map[string]string{}

	for _, k := range []string{"NO_PROXY", "no_proxy"} {
		other := "no_proxy"
		if k == other {
			other = "NO_PROXY"
		}

		v := vals[k]
		if v == "" {
			v = vals[other]
		}

		out[k] = withLoopback(v)
	}

	return out
}

// withLoopback appends each loopback host the comma-separated list lacks.
func withLoopback(list string) string {
	have := map[string]bool{}
	for e := range strings.SplitSeq(list, ",") {
		have[strings.ToLower(strings.TrimSpace(e))] = true
	}

	entries := []string{}
	if list = strings.TrimRight(list, ", "); list != "" {
		entries = append(entries, list)
	}

	for _, h := range loopbackHosts {
		if !have[h] {
			entries = append(entries, h)
		}
	}

	return strings.Join(entries, ",")
}

// userMessage renders one stream-json user line, newline-terminated.
func userMessage(text string) []byte {
	type block struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}

	type body struct {
		Role    string  `json:"role"`
		Content []block `json:"content"`
	}

	b, _ := json.Marshal(struct { //nolint:errchkjson // strings always marshal
		Type    string `json:"type"`
		Message body   `json:"message"`
	}{Type: "user", Message: body{Role: "user", Content: []block{{Type: "text", Text: text}}}})

	return append(b, '\n')
}
