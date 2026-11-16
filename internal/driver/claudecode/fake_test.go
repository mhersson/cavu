package claudecode

// The fake stands in for the claude CLI in tests: the test binary
// re-executes itself as claude, answers the initialize request and replays
// recorded stream-json the way the pinned release does.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// fakeEnvVar names the fakeScenario JSON file; the test binary dispatches to fakeMain when
// it is set.
const fakeEnvVar = "CAVU_FAKE_CLAUDE"

// fakeScenario scripts one fake claude process. A user line that arrives before
// the initialize answer is dropped and recorded as early input, so a test
// catches a prompt written too soon.
type fakeScenario struct {
	Version   string // printed for --version
	Replay    string // fixture JSONL written to stdout after the first user message
	Stderr    string // written to stderr before exiting
	ExitCode  int
	Hang      bool          // block after the replay until killed
	Record    string        // write fakeRecorded JSON here
	InitDelay time.Duration // answer the initialize request after this long
	NoInit    bool          // never answer the initialize request
	InitError string        // answer the initialize request with this error instead
	// Account replaces the initialize answer's account, which by default
	// names the credential variable the environment holds; NoAccount leaves
	// it out.
	Account     json.RawMessage
	NoAccount   bool
	ExitAtStart bool // write Stderr and exit with ExitCode before reading stdin
}

// fakeRecorded is what the fake saw.
type fakeRecorded struct {
	Args       []string
	EnvNames   []string // names only, never values
	ConfigDir  string   // CLAUDE_CONFIG_DIR
	Stdin      string   // the first user line answered
	EarlyInput bool     // a user line arrived before the initialize answer and was dropped
	// RunFiles holds the contents of the files --settings and
	// --append-system-prompt-file name, by flag.
	RunFiles map[string]string
	PID      int
}

// fakeMain reads the scenario, acts and exits; it never returns.
func fakeMain() {
	os.Exit(fakeExec())
}

func fakeExec() int {
	sc, err := fakeLoad(os.Getenv(fakeEnvVar))
	if err != nil {
		return fakeFail(err)
	}

	if slices.Contains(os.Args[1:], "--version") {
		fmt.Println(sc.Version + " (Claude Code)")

		return 0
	}

	p := &fakeProc{sc: sc, flags: fakeParseFlags(os.Args[1:]), configDir: os.Getenv("CLAUDE_CONFIG_DIR")}

	return p.run()
}

func fakeFail(err error) int {
	fmt.Fprintln(os.Stderr, "fake claude:", err)

	return 2
}

func fakeLoad(path string) (fakeScenario, error) {
	var sc fakeScenario

	data, err := os.ReadFile(path) //nolint:gosec // the test names the scenario file
	if err != nil {
		return sc, fmt.Errorf("read scenario: %w", err)
	}

	if err := json.Unmarshal(data, &sc); err != nil {
		return sc, fmt.Errorf("decode scenario: %w", err)
	}

	return sc, nil
}

// fakeFileFlags name files whose contents fakeRecorded.RunFiles keeps.
var fakeFileFlags = []string{"--settings", "--append-system-prompt-file"}

func fakeParseFlags(args []string) map[string]string {
	values := map[string]string{}

	for i, a := range args {
		switch a {
		case "--settings", "--append-system-prompt-file", "--session-id", "--resume":
			if i+1 < len(args) {
				values[a] = args[i+1]
			}
		}
	}

	return values
}

type fakeProc struct {
	sc        fakeScenario
	flags     map[string]string
	configDir string
	sid       string
	in        <-chan fakeInLine
}

type fakeInLine struct {
	data []byte
	at   time.Time
}

func (p *fakeProc) run() int {
	rec := p.record()

	// Written again once stdin delivers the prompt, so a test whose run never
	// gets that far still learns the PID.
	if err := p.write(rec); err != nil {
		return fakeFail(err)
	}

	if p.sc.ExitAtStart {
		_, _ = os.Stderr.WriteString(p.sc.Stderr)

		return p.sc.ExitCode
	}

	if id := p.flags["--resume"]; id != "" {
		if matches, _ := filepath.Glob(filepath.Join(p.configDir, "projects", "*", id+".jsonl")); len(matches) == 0 {
			text := "No conversation found with session ID: " + id
			fakeWriteJSONLine(map[string]any{
				"type": "result", "subtype": "error_during_execution", "is_error": true,
				"num_turns": 0, "session_id": id, "errors": []string{text},
			})
			fmt.Fprintln(os.Stderr, text)

			return 1
		}
	}

	p.in = fakeReadLines(os.Stdin)

	first, early, ok := p.awaitUser()
	if !ok {
		return 0
	}

	rec.Stdin, rec.EarlyInput = string(first), early

	if err := p.write(rec); err != nil {
		return fakeFail(err)
	}

	replay, err := p.loadFixture(p.sc.Replay)
	if err != nil {
		return fakeFail(err)
	}

	if err := p.persist(append([][]byte{first}, replay...)); err != nil {
		return fakeFail(err)
	}

	for _, line := range replay {
		fakeWriteLine(line)
	}

	return p.exit(fakeHasResult(replay))
}

// exit writes the scripted stderr and returns the exit code, hanging first
// when told to. Like the CLI, a finished replay waits for the next message
// until stdin closes when waitInput is set.
func (p *fakeProc) exit(waitInput bool) int {
	_, _ = os.Stderr.WriteString(p.sc.Stderr)

	if p.sc.Hang {
		for {
			time.Sleep(time.Hour)
		}
	}

	if waitInput {
		p.drainStdin()
	}

	return p.sc.ExitCode
}

func fakeHasResult(lines [][]byte) bool {
	return slices.ContainsFunc(lines, func(line []byte) bool { return fakeLineType(line) == "result" })
}

func (p *fakeProc) write(rec fakeRecorded) error {
	if p.sc.Record == "" {
		return nil
	}

	b, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("encode record: %w", err)
	}

	tmp := p.sc.Record + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("write record: %w", err)
	}

	if err := os.Rename(tmp, p.sc.Record); err != nil {
		return fmt.Errorf("publish record: %w", err)
	}

	return nil
}

func (p *fakeProc) record() fakeRecorded {
	rec := fakeRecorded{
		Args:      os.Args[1:],
		ConfigDir: p.configDir,
		RunFiles:  map[string]string{},
		PID:       os.Getpid(),
	}

	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		rec.EnvNames = append(rec.EnvNames, name)
	}

	slices.Sort(rec.EnvNames)

	for _, flag := range fakeFileFlags {
		if path := p.flags[flag]; path != "" {
			data, _ := os.ReadFile(path) //nolint:gosec // the invocation names the file
			rec.RunFiles[flag] = string(data)
		}
	}

	return rec
}

func fakeReadLines(r io.Reader) <-chan fakeInLine {
	ch := make(chan fakeInLine)

	go func() {
		defer close(ch)

		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 64<<10), 64<<20)

		for sc.Scan() {
			ch <- fakeInLine{data: bytes.Clone(sc.Bytes()), at: time.Now()}
		}
	}()

	return ch
}

// account is the initialize answer's account: the scenario's, or the shape
// the credential variable in the environment would give.
func (p *fakeProc) account() any {
	switch {
	case p.sc.Account != nil:
		return p.sc.Account
	case os.Getenv("CLAUDE_CODE_OAUTH_TOKEN") != "":
		return map[string]string{"tokenSource": "CLAUDE_CODE_OAUTH_TOKEN", "apiProvider": "firstParty"} //nolint:gosec // names the token's source, holds no credential
	case os.Getenv("ANTHROPIC_AUTH_TOKEN") != "":
		return map[string]string{"tokenSource": "ANTHROPIC_AUTH_TOKEN", "apiProvider": "firstParty"} //nolint:gosec // names the token's source, holds no credential
	default:
		return map[string]string{"tokenSource": "none", "apiProvider": "firstParty"}
	}
}

// awaitUser answers initialize requests until the first user line arrives
// after the answer; early reports that an earlier one was dropped.
func (p *fakeProc) awaitUser() (line []byte, early, ok bool) {
	var answeredAt time.Time

	for l := range p.in {
		var msg struct {
			Type      string `json:"type"`
			RequestID string `json:"request_id"`
			Request   struct {
				Subtype string `json:"subtype"`
			} `json:"request"`
		}

		if json.Unmarshal(l.data, &msg) != nil {
			continue
		}

		switch {
		case msg.Type == "control_request" && msg.Request.Subtype == "initialize":
			if p.sc.NoInit {
				continue
			}

			time.Sleep(p.sc.InitDelay)

			answeredAt = time.Now()

			if p.sc.InitError != "" {
				fakeWriteJSONLine(map[string]any{"type": "control_response", "response": map[string]any{
					"subtype": "error", "request_id": msg.RequestID, "error": p.sc.InitError,
				}})

				continue
			}

			answer := map[string]any{"pid": os.Getpid(), "claude_code_version": p.sc.Version}
			if !p.sc.NoAccount {
				answer["account"] = p.account()
			}

			fakeWriteJSONLine(map[string]any{"type": "control_response", "response": map[string]any{
				"subtype": "success", "request_id": msg.RequestID, "response": answer,
			}})
		case msg.Type == "user":
			if answeredAt.IsZero() || l.at.Before(answeredAt) {
				early = true

				continue
			}

			return l.data, early, true
		}
	}

	return nil, early, false
}

func (p *fakeProc) drainStdin() {
	for range p.in { //nolint:revive // runs until the caller closes stdin
	}
}

// loadFixture returns the fixture's lines (none for an empty path) with its
// session ID replaced by this process's.
func (p *fakeProc) loadFixture(path string) ([][]byte, error) {
	var lines [][]byte

	if path != "" {
		data, err := os.ReadFile(path) //nolint:gosec // the test names the fixture
		if err != nil {
			return nil, fmt.Errorf("read fixture: %w", err)
		}

		for line := range bytes.Lines(data) {
			if line = bytes.TrimSpace(line); len(line) > 0 {
				lines = append(lines, line)
			}
		}
	}

	fixtureSID := ""

	for _, line := range lines {
		var l struct {
			SessionID string `json:"session_id"`
		}

		if json.Unmarshal(line, &l) == nil && l.SessionID != "" {
			fixtureSID = l.SessionID

			break
		}
	}

	p.sid = fakeFirstSet(p.flags["--session-id"], p.flags["--resume"], fixtureSID, "fake-session")

	if fixtureSID != "" && fixtureSID != p.sid {
		for i, line := range lines {
			lines[i] = bytes.ReplaceAll(line, []byte(fixtureSID), []byte(p.sid))
		}
	}

	return lines, nil
}

func fakeFirstSet(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}

	return ""
}

// fakeProjectKey is the CLI's projects/ directory name for a working directory.
func fakeProjectKey(dir string) string {
	return fakeNonAlnum.ReplaceAllString(dir, "-")
}

var fakeNonAlnum = regexp.MustCompile(`[^A-Za-z0-9]`)

// persist appends lines to the session transcript under the config dir, as
// the CLI does, so a later --resume finds it.
func (p *fakeProc) persist(lines [][]byte) error {
	if p.configDir == "" {
		return nil
	}

	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getwd: %w", err)
	}

	dir := filepath.Join(p.configDir, "projects", fakeProjectKey(cwd))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("transcript dir: %w", err)
	}

	path := filepath.Join(dir, p.sid+".jsonl")

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600) //nolint:gosec // under CLAUDE_CONFIG_DIR
	if err != nil {
		return fmt.Errorf("open transcript: %w", err)
	}

	for _, line := range lines {
		if _, err := f.Write(append(bytes.Clone(line), '\n')); err != nil {
			_ = f.Close()

			return fmt.Errorf("write transcript: %w", err)
		}
	}

	if err := f.Close(); err != nil {
		return fmt.Errorf("close transcript: %w", err)
	}

	return nil
}

func fakeLineType(line []byte) string {
	var l struct {
		Type string `json:"type"`
	}

	_ = json.Unmarshal(line, &l)

	return l.Type
}

func fakeWriteLine(line []byte) {
	_, _ = os.Stdout.Write(append(bytes.Clone(line), '\n'))
}

func fakeWriteJSONLine(v any) {
	b, _ := json.Marshal(v)
	fakeWriteLine(b)
}
