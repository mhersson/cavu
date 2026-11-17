//go:build integration

package claudecode

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/cavu/internal/driver"
	"github.com/mhersson/cavu/internal/env"
	"github.com/mhersson/cavu/internal/proc"
	"github.com/mhersson/cavu/internal/proxy"
	"github.com/mhersson/cavu/internal/sandbox"
	"github.com/mhersson/cavu/internal/sidecar"
	"github.com/mhersson/cavu/internal/testkit/mockapi"
	"github.com/mhersson/cavu/internal/testkit/sandboxtest"
)

// The conformance suite runs the pinned claude in the sandbox image's agent
// container, against mockapi through the bridge and the sidecar. The images it
// builds go once every test has run.
func init() { afterTests = sandboxtest.Cleanup }

const (
	confModel  = "claude-haiku-5-5"
	confHome   = "/home/agent"
	confConfig = confHome + "/.claude-cavu"
	confDir    = "/workspace"
	// confBound caps one claude run.
	confBound = 3 * time.Minute
)

// Markers the fixture repository plants, each in one file.
const (
	markerRootClaude = "CONFORMANCE-ROOT-CLAUDE-MD-5d1f"
	markerRootAgents = "CONFORMANCE-ROOT-AGENTS-MD-8a2c"
	markerDotClaude  = "CONFORMANCE-DOT-CLAUDE-MD-6b1e"
	markerRule       = "CONFORMANCE-RULE-4a7f"
	markerNested     = "CONFORMANCE-NESTED-CLAUDE-MD-3e7b"
	markerSkill      = "CONFORMANCE-SKILL-DESCRIPTION-9c4d"
	markerProjectEnv = "CONFORMANCE-PROJECT-ENV-1b6e"
	// markerImported is in a file the root CLAUDE.md imports with an @ line.
	markerImported = "CONFORMANCE-IMPORTED-7d2a"
)

// Files the fixture repository's hooks and MCP server create when they run.
const (
	sessionStartHookFile = confDir + "/.conformance-session-start-hook"
	postToolUseHookFile  = confDir + "/.conformance-post-tool-use-hook"
	mcpServerFile        = confDir + "/.conformance-mcp-server-ran"
)

// skillHookFiles are what the hook skill's frontmatter hooks create.
var skillHookFiles = []string{
	confDir + "/.conformance-skill-pre-tool-use-hook",
	confDir + "/.conformance-skill-post-tool-use-hook",
	confDir + "/.conformance-skill-stop-hook",
}

// repository maps each file under testdata/conformance to its path in the
// workspace. The stored names are ones no tool discovers, so no agent working
// in this repository loads them.
var repository = map[string]string{
	"root-claude-md.txt":        "CLAUDE.md",
	"root-agents-md.txt":        "AGENTS.md",
	"dot-claude-md.txt":         ".claude/CLAUDE.md",
	"rule-md.txt":               ".claude/rules/conformance.md",
	"pkg-claude-md.txt":         "pkg/CLAUDE.md",
	"pkg-notes.txt":             "pkg/notes.txt",
	"skill-md.txt":              ".claude/skills/conformance-marker/SKILL.md",
	"hook-skill-md.txt":         ".claude/skills/conformance-hook/SKILL.md",
	"project-settings-json.txt": ".claude/settings.json",
	"mcp-json.txt":              ".mcp.json",
	"docs-imported-md.txt":      "docs/imported.md",
}

// box is one check's sandbox: the phase container and its sidecar, the mock
// behind the proxy, and the fixture repository in the workspace.
type box struct {
	t       *testing.T
	rt      sandbox.Runtime
	sb      *sandbox.Sandbox
	req     sidecar.ProxyRequest // kept, so a change of phase keeps the sidecar
	c       sandbox.Container
	mock    *mockapi.Server
	cred    string // the real credential, which only the sidecar holds
	standIn string
	base    string // the fixture repository's commit
	sidecar lineLog
}

// keyBox is a key-mode box whose model route leads to a mock that answers
// steps.
func keyBox(t *testing.T, steps ...mockapi.Step) *box {
	t.Helper()

	return keyBoxWith(t, "bearer", steps...)
}

func keyBoxWith(t *testing.T, authHeader string, steps ...mockapi.Step) *box {
	t.Helper()

	r := sandboxtest.Runtime(t)
	image := sandboxtest.Image(t, r, Version, SHA512, nil)

	standIn := proxy.NewStandIn()
	b := &box{t: t, rt: r, mock: mockapi.Start(t, steps), cred: "conformance-credential-" + randomID(), standIn: standIn}
	b.up(image, sidecar.ProxyRequest{Model: &proxy.ModelRoute{
		BaseURL: b.mock.HostURL(), Credential: b.cred, AuthHeader: authHeader, StandIn: standIn,
	}})
	b.plant()

	return b
}

// up starts the sandbox of a fresh instance and removes all of it after the
// test: every container and volume with both the instance's label and its
// name prefix.
func (b *box) up(image string, req sidecar.ProxyRequest) {
	t := b.t
	instance := randomID()[:16]

	sb, err := sandbox.Open(sandbox.Options{
		Runtime: b.rt, Image: image, InstanceID: instance,
		RunID: positiveID(t), TaskID: positiveID(t), ProjectID: positiveID(t),
		Labels: sandboxtest.Labels(),
	})
	require.NoError(t, err)

	// Cleanups run last first: Down, then the removal.
	sandboxtest.RemoveInstanceAfter(t, b.rt, instance)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()

		assert.NoError(t, sb.Down(ctx))
	})

	require.NoError(t, sb.EnsureVolumes(t.Context()))

	b.sb, b.req = sb, req
	b.phase(sandbox.PhaseAgent)

	// The mock answers a side request without a step; no check should need it.
	if b.mock != nil {
		t.Cleanup(func() {
			assert.Len(t, modelCalls(b.mock), len(b.mock.Requests()), "a request was not a streaming model call")
		})
	}
}

// phase brings up the phase container of p in place of the current one.
func (b *box) phase(p sandbox.Phase) {
	b.t.Helper()

	c, err := b.sb.Up(b.t.Context(), p, b.req, b.sidecar.add)
	require.NoError(b.t, err)

	b.c = c
}

// plant writes the fixture repository into the workspace and commits it, as
// a clone would leave it.
func (b *box) plant() {
	files := map[string][]byte{}

	for stored, at := range repository {
		data, err := os.ReadFile(filepath.Join("testdata", "conformance", stored))
		require.NoError(b.t, err)

		files[path.Join(confDir, at)] = data
	}

	require.NoError(b.t, b.c.CopyIn(b.t.Context(), files))

	res := b.sh("git init -q -b main . && git add -A && " +
		"git -c user.name=conformance -c user.email=conformance@example.com commit -q -m fixture && " +
		"git rev-parse HEAD")
	require.Equal(b.t, 0, res.ExitCode, res.Output)

	b.base = strings.TrimSpace(res.Output)
}

// sh runs script in the workspace of the phase container.
func (b *box) sh(script string) sandbox.Result {
	b.t.Helper()

	res, err := b.c.Run(b.t.Context(), proc.Command{Argv: []string{"sh", "-c", script}, Dir: confDir}, time.Minute)
	require.NoError(b.t, err)
	require.False(b.t, res.TimedOut, script)

	return res
}

// exists reports whether p exists in the phase container.
func (b *box) exists(p string) bool {
	return b.sh("test -e "+p).ExitCode == 0
}

// spec is a key-mode session in the workspace, staged from the fixture
// repository's commit, with the environment the runner passes in the agent
// phase.
func (b *box) spec() driver.Spec {
	return driver.Spec{
		Auth:       driver.Auth{Mode: modeKey, Token: b.standIn, BaseURL: testBridgeURL},
		Model:      confModel,
		MaxTurns:   10,
		SessionID:  newSessionID(b.t),
		Prompt:     "conformance check",
		Dir:        confDir,
		BaseCommit: b.base,
		Home:       confHome,
		ConfigDir:  confConfig,
		Env:        agentEnv(),
	}
}

// agentEnv is the agent phase's environment as the runner passes it: the
// trust and cache variables, and the proxy through the bridge.
func agentEnv(extra ...string) []string {
	out := env.AgentEnv(nil, confHome)
	out = append(out, "HTTPS_PROXY="+testBridgeURL, "HTTP_PROXY="+testBridgeURL)

	return append(out, extra...)
}

// outcome is one claude run as the driver and the stream saw it.
type outcome struct {
	res    driver.Result
	events []driver.Event
	lines  [][]byte
	stderr string
}

func conformanceDriver() claudeCode {
	return New().(claudeCode) //nolint:forcetypeassert // New returns a claudeCode
}

// run runs s in the phase container with d.
func (b *box) run(d claudeCode, s driver.Spec) outcome {
	b.t.Helper()

	o, err := b.try(d, s)
	require.NoError(b.t, err)

	return o
}

// try is run for a goroutine other than the test's.
func (b *box) try(d claudeCode, s driver.Spec) (outcome, error) {
	ctx, cancel := context.WithTimeout(b.t.Context(), confBound)
	defer cancel()

	rec := &recorder{Target: b.c}

	var o outcome

	res, err := d.Run(ctx, rec, s, func(ev driver.Event) { o.events = append(o.events, ev) })
	if err != nil {
		return outcome{}, err
	}

	o.res = res
	o.lines, o.stderr = rec.output()

	return o, nil
}

// requireDone fails unless the run ended done.
func requireDone(t *testing.T, o outcome) {
	t.Helper()

	require.Equal(t, driver.ReasonDone, o.res.Reason, "detail: %s\nstderr: %s", o.res.Detail, o.stderr)
}

// line returns the first raw stdout line of type and subtype ("" for any).
func (o outcome) line(t *testing.T, typ, subtype string) map[string]any {
	t.Helper()

	for _, raw := range o.lines {
		var l map[string]any
		if json.Unmarshal(raw, &l) == nil && l["type"] == typ && (subtype == "" || l["subtype"] == subtype) {
			return l
		}
	}

	require.Failf(t, "no such line", "no %s/%s line in %d lines", typ, subtype, len(o.lines))

	return nil
}

// modelCalls are the requests the mock's script answered.
func modelCalls(m *mockapi.Server) []mockapi.Request {
	var out []mockapi.Request

	for _, r := range m.Requests() {
		if r.ModelCall {
			out = append(out, r)
		}
	}

	return out
}

func carries(r mockapi.Request, marker string) bool {
	return bytes.Contains(r.Body, []byte(marker))
}

// carriedBy names the requests, by their number among all requests, that
// hold marker anywhere.
func carriedBy(m *mockapi.Server, marker string) []int {
	var out []int

	for i, r := range m.Requests() {
		if carries(r, marker) || slices.ContainsFunc(headerValues(r), func(v string) bool { return strings.Contains(v, marker) }) {
			out = append(out, i+1)
		}
	}

	return out
}

func headerValues(r mockapi.Request) []string {
	var out []string
	for _, vs := range r.Header {
		out = append(out, vs...)
	}

	return out
}

// recorder keeps every stdout line and the stderr tail of the processes it
// starts, while the driver reads them as usual.
type recorder struct {
	proc.Target

	mu    sync.Mutex
	lines [][]byte
	procs []*teeProcess
}

func (r *recorder) Start(ctx context.Context, c proc.Command) (proc.Process, error) {
	p, err := r.Target.Start(ctx, c)
	if err != nil {
		return nil, err
	}

	tp := &teeProcess{Process: p, lines: make(chan []byte), quit: make(chan struct{}), done: make(chan struct{})}

	go func() {
		defer close(tp.done)
		defer close(tp.lines)

		for l := range p.Lines() {
			r.mu.Lock()
			r.lines = append(r.lines, l)
			r.mu.Unlock()

			select {
			case tp.lines <- l:
			case <-tp.quit:
			}
		}
	}()

	r.mu.Lock()
	r.procs = append(r.procs, tp)
	r.mu.Unlock()

	return tp, nil
}

// output is called once the driver has waited on every process, so each
// copier ends as soon as it has kept its last line.
func (r *recorder) output() ([][]byte, string) {
	r.mu.Lock()
	procs := slices.Clone(r.procs)
	r.mu.Unlock()

	var stderr strings.Builder

	for _, p := range procs {
		<-p.done

		stderr.WriteString(p.StderrTail())
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	return slices.Clone(r.lines), stderr.String()
}

// teeProcess hands on the lines its recorder copies; once stopped or waited
// on, it stops handing them on, as proc does.
type teeProcess struct {
	proc.Process

	lines chan []byte
	quit  chan struct{}
	once  sync.Once
	done  chan struct{} // closed when the copier has ended
}

func (p *teeProcess) Lines() <-chan []byte { return p.lines }

func (p *teeProcess) Stop(grace time.Duration) error {
	p.once.Do(func() { close(p.quit) })

	return p.Process.Stop(grace)
}

func (p *teeProcess) Wait() (int, error) {
	p.once.Do(func() { close(p.quit) })

	return p.Process.Wait()
}

type lineLog struct {
	mu    sync.Mutex
	lines []sidecar.Line
}

func (l *lineLog) add(line sidecar.Line) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.lines = append(l.lines, line)
}

// of returns the lines of type typ.
func (l *lineLog) of(typ string) []sidecar.Line {
	l.mu.Lock()
	defer l.mu.Unlock()

	var out []sidecar.Line

	for _, line := range l.lines {
		if line.Type == typ {
			out = append(out, line)
		}
	}

	return out
}

func positiveID(t *testing.T) int64 {
	t.Helper()

	n, err := rand.Int(rand.Reader, big.NewInt(1<<40))
	require.NoError(t, err)

	return n.Int64() + 1
}

func newSessionID(t *testing.T) string {
	t.Helper()

	b := make([]byte, 16)
	_, err := rand.Read(b)
	require.NoError(t, err)

	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80

	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// TestInstructionsLoaded: the root CLAUDE.md and AGENTS.md, .claude/CLAUDE.md
// and the repository's rule reach the first model call, and so does
// pkg/CLAUDE.md's path, but never its content.
func TestInstructionsLoaded(t *testing.T) {
	b := keyBox(t, mockapi.Step{Text: "done"})

	requireDone(t, b.run(conformanceDriver(), b.spec()))

	calls := modelCalls(b.mock)
	require.NotEmpty(t, calls)

	for _, m := range []string{markerRootClaude, markerDotClaude, markerRootAgents, markerRule, "pkg/CLAUDE.md"} {
		assert.True(t, carries(calls[0], m), "%s is not in the first model call", m)
	}

	assert.Empty(t, carriedBy(b.mock, markerNested), "pkg/CLAUDE.md's content reached a request")

	// A finding, not a gate: what an @ import in the staged CLAUDE.md loads.
	t.Logf("the imported file reached requests %v", carriedBy(b.mock, markerImported))
}

// TestInstructionsNeedStaging is the check that retires the staging: without
// it the pinned release loads none of the repository's instruction files. A
// release for which this fails loads them without the project source.
func TestInstructionsNeedStaging(t *testing.T) {
	b := keyBox(t,
		mockapi.Step{Tool: "Read", Input: map[string]any{"file_path": confDir + "/pkg/notes.txt"}},
		mockapi.Step{Text: "done"},
	)

	s := b.spec()
	s.BaseCommit = ""
	requireDone(t, b.run(conformanceDriver(), s))

	require.Len(t, modelCalls(b.mock), 2)

	for _, m := range []string{markerRootClaude, markerDotClaude, markerRootAgents, markerRule, markerNested, markerSkill} {
		assert.Empty(t, carriedBy(b.mock, m), "%s reached a request without staging", m)
	}
}

// TestStagingTakesBaseCommit: what the workspace holds beyond the base commit
// is never staged.
func TestStagingTakesBaseCommit(t *testing.T) {
	const written = "CONFORMANCE-WORKING-TREE-1f9b"

	b := keyBox(t, mockapi.Step{Text: "done"})
	require.Equal(t, 0, b.sh("echo "+written+" > CLAUDE.md").ExitCode)

	requireDone(t, b.run(conformanceDriver(), b.spec()))

	calls := modelCalls(b.mock)
	require.NotEmpty(t, calls)
	assert.True(t, carries(calls[0], markerRootClaude), "the base commit's CLAUDE.md is not in the first model call")
	assert.Empty(t, carriedBy(b.mock, written), "the working tree's CLAUDE.md reached a request")
}

// TestStagingFollowsRootLink: a root CLAUDE.md that links to AGENTS.md, a
// common layout, puts AGENTS.md's content in the first model call once, and a
// .claude/CLAUDE.md linking to another file, relative to its directory, puts
// that file's content there.
func TestStagingFollowsRootLink(t *testing.T) {
	b := keyBox(t, mockapi.Step{Text: "done"})

	s := b.spec()
	s.BaseCommit = lastLine(t, b.sh("ln -sf AGENTS.md CLAUDE.md && ln -sf ../docs/imported.md .claude/CLAUDE.md && "+
		"git add CLAUDE.md .claude/CLAUDE.md && git "+confIdentity+" commit -q -m link && git rev-parse HEAD"))
	requireDone(t, b.run(conformanceDriver(), s))

	calls := modelCalls(b.mock)
	require.NotEmpty(t, calls)
	assert.Equal(t, 1, bytes.Count(calls[0].Body, []byte(markerRootAgents)))
	assert.True(t, carries(calls[0], markerImported), "the linked docs/imported.md is not in the first model call")

	for _, m := range []string{markerRootClaude, markerDotClaude} {
		assert.Empty(t, carriedBy(b.mock, m), "the content a link replaced reached a request")
	}
}

// TestSkillsLoaded: a skill under .claude/skills reaches the request's skill
// listing by its description.
func TestSkillsLoaded(t *testing.T) {
	b := keyBox(t, mockapi.Step{Text: "done"})

	requireDone(t, b.run(conformanceDriver(), b.spec()))

	calls := modelCalls(b.mock)
	require.NotEmpty(t, calls)
	assert.True(t, carries(calls[0], markerSkill), "%s is not in the first model call", markerSkill)
}

// TestProjectSettingsNotLoaded: the repository's .claude/settings.json hooks,
// deny rule and env all stay out of the session.
func TestProjectSettingsNotLoaded(t *testing.T) {
	const ran = confDir + "/.conformance-bash-ran"

	b := keyBox(t,
		mockapi.Step{Tool: "Bash", Input: map[string]any{"command": "printenv CONFORMANCE_PROJECT_ENV; touch " + ran}},
		mockapi.Step{Text: "done"},
	)

	o := b.run(conformanceDriver(), b.spec())
	requireDone(t, o)

	assert.False(t, b.exists(sessionStartHookFile), "the SessionStart hook ran")
	assert.False(t, b.exists(postToolUseHookFile), "the PostToolUse hook ran")
	assert.True(t, b.exists(ran), "the Bash call did not run")
	assert.Equal(t, []bool{false}, toolErrors(o))
	assert.Empty(t, carriedBy(b.mock, markerProjectEnv), "the env entry reached a request")
}

// TestSkillHooksOff: a staged skill's frontmatter hooks never run, though the
// skill is invoked and a Bash call follows; the same run with the settings
// file's hooks switch left out runs them, which proves the check sees them.
func TestSkillHooksOff(t *testing.T) {
	for _, tc := range []struct {
		name     string
		settings string
		run      bool
	}{
		{"settings file", settings, false},
		{"hooks left on", `{"attribution":{"commit":"","pr":"","sessionUrl":false}}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := keyBox(t,
				mockapi.Step{Tool: "Skill", Input: map[string]any{"skill": "conformance-hook"}},
				mockapi.Step{Tool: "Bash", Input: map[string]any{"command": "true"}},
				mockapi.Step{Text: "done"},
			)

			d := conformanceDriver()
			d.amend = func(inv *invocation) {
				for p := range inv.files {
					if path.Base(p) == "settings.json" {
						inv.files[p] = []byte(tc.settings)
					}
				}
			}

			o := b.run(d, b.spec())
			requireDone(t, o)
			assert.Equal(t, []bool{false, false}, toolErrors(o), "the Skill or the Bash call failed")

			var ran []string

			for _, f := range skillHookFiles {
				if b.exists(f) {
					ran = append(ran, f)
				}
			}

			t.Logf("skill hooks that ran: %v", ran)

			if tc.run {
				assert.NotEmpty(t, ran, "no skill hook ran with hooks on, so the check cannot see one")
			} else {
				assert.Empty(t, ran, "a skill hook ran")
			}
		})
	}
}

// TestConfigDirCleared: what a session writes into the config dir for the
// user source to load (here a settings.json with an env entry) is gone before
// the next session; written after the clear, as the control does, it reaches
// the session's environment.
func TestConfigDirCleared(t *testing.T) {
	const (
		name   = "CONFORMANCE_USER_ENV"
		marker = "CONFORMANCE-USER-ENV-9d4f"
	)

	planted := []byte(`{"env":{"` + name + `":"` + marker + `"}}`)

	for _, control := range []bool{false, true} {
		t.Run(fmt.Sprintf("written after the clear %v", control), func(t *testing.T) {
			b := keyBox(t,
				mockapi.Step{Text: "done"},
				mockapi.Step{Tool: "Bash", Input: map[string]any{"command": "printenv " + name + "; true"}},
				mockapi.Step{Text: "done"},
			)
			d := conformanceDriver()

			first := b.spec()
			requireDone(t, b.run(d, first))

			require.NoError(t, b.c.CopyIn(t.Context(), map[string][]byte{
				confConfig + "/settings.json": planted, confConfig + "/agents/planted.md": []byte(marker + "\n"),
			}))

			if control {
				d.amend = func(inv *invocation) { inv.files[confConfig+"/settings.json"] = planted }
			}

			second := b.spec()
			second.SessionID, second.Resume = "", first.SessionID

			o := b.run(d, second)
			requireDone(t, o)
			assert.Equal(t, []bool{false}, toolErrors(o))
			assert.Equal(t, control, len(carriedBy(b.mock, marker)) > 0, "the planted env reached a request")
			assert.False(t, b.exists(confConfig+"/agents/planted.md"), "the planted agent survived the clear")
		})
	}
}

// TestConfigDirSymlinkRefused: a config dir the agent turned into a symlink
// stops Run before any request, and the clear deletes nothing on either side
// of the link. Without a base commit no write follows the clear, so the clear
// alone must refuse.
func TestConfigDirSymlinkRefused(t *testing.T) {
	const elsewhere = confHome + "/elsewhere"

	for _, tc := range []struct{ name, target string }{
		{"to a directory", elsewhere},
		{"relative", "elsewhere"},
		{"to the root", "/"},
		{"to nothing", confHome + "/missing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := keyBox(t, mockapi.Step{Text: "done"})

			kept := []string{elsewhere + "/settings.json", elsewhere + "/projects/-earlier/transcript.jsonl"}
			require.NoError(t, b.c.CopyIn(t.Context(), map[string][]byte{kept[0]: []byte("{}\n"), kept[1]: []byte("{}\n")}))

			// -T makes ln fail rather than link inside an existing directory.
			res, err := b.c.Run(t.Context(), proc.Command{Argv: []string{"ln", "-s", "-T", "--", tc.target, confConfig}}, time.Minute)
			require.NoError(t, err)
			require.Equal(t, 0, res.ExitCode, res.Output)

			s := b.spec()
			s.BaseCommit = ""

			_, err = b.try(conformanceDriver(), s)

			var ee *proc.ExitError
			require.ErrorAs(t, err, &ee, "the clear did not refuse")
			require.NotErrorIs(t, err, errNotInSandbox)
			assert.Empty(t, b.mock.Requests(), "a session started")

			assert.Equal(t, 0, b.sh("test -L "+confConfig).ExitCode, "the link went")

			for _, p := range append(kept, confDir+"/CLAUDE.md", proc.SandboxMarker) {
				assert.True(t, b.exists(p), "%s went", p)
			}
		})
	}
}

// TestStagingReplacesEarlierStaging: what an earlier session left in the
// config dir goes but its transcript, and a skill script the commit marks
// executable is staged executable, every other file not.
func TestStagingReplacesEarlierStaging(t *testing.T) {
	const (
		transcript = "projects/-earlier/0b9d3c5e-0000-4000-8000-000000000000.jsonl"
		script     = "skills/conformance-marker/scripts/run.sh"
	)

	b := keyBox(t)

	base := lastLine(t, b.sh(`set -e
mkdir -p .claude/skills/conformance-marker/scripts
printf 'echo run\n' > .claude/`+script+`
chmod 755 .claude/`+script+`
git add .claude/skills
git `+confIdentity+` commit -q -m script
git rev-parse HEAD`))

	planted := map[string][]byte{path.Join(confConfig, transcript): []byte("{}\n")}
	for _, rel := range []string{
		"CLAUDE.md", "skills/stale/SKILL.md", "rules/stale.md", "settings.json", "settings.local.json",
		"agents/planted.md", "commands/planted.md", "output-styles/planted.md", ".claude.json",
	} {
		planted[path.Join(confConfig, rel)] = []byte("stale\n")
	}

	require.NoError(t, b.c.CopyIn(t.Context(), planted))

	s := b.spec()
	s.BaseCommit = base
	require.NoError(t, b.prepare(s))

	res := b.sh("cd " + confConfig + " && find . -mindepth 1 -maxdepth 1 -printf '%P\\n' | sort")
	require.Equal(t, 0, res.ExitCode, res.Output)
	assert.Equal(t, "CLAUDE.md\nprojects\nrules\nskills\n", res.Output)

	res = b.sh("cd " + confConfig + " && find . -type f -printf '%P %m\\n' | sort")
	require.Equal(t, 0, res.ExitCode, res.Output)

	files := map[string]bool{}

	for line := range strings.Lines(res.Output) {
		name, mode, _ := strings.Cut(strings.TrimSpace(line), " ")
		perm, err := strconv.ParseUint(mode, 8, 32)
		require.NoError(t, err, line)

		files[name] = perm&0o100 != 0
	}

	assert.Equal(t, map[string]bool{
		"CLAUDE.md":                          false,
		transcript:                           false,
		"rules/conformance.md":               false,
		"skills/conformance-marker/SKILL.md": false,
		"skills/conformance-hook/SKILL.md":   false,
		script:                               true,
	}, files)
	assert.Equal(t, "{}\n", b.readConfig(transcript))
}

// TestInstanceInstructionsAndSkillsReachTheAgent: the instance's instructions
// reach the first model call ahead of the repository's root CLAUDE.md, and its
// skills are listed by name and description, but for one of the name of the
// repository's skill, whose own is listed. The staging keeps an instance
// script executable and its other files not.
func TestInstanceInstructionsAndSkillsReachTheAgent(t *testing.T) {
	const (
		markerInstance      = "CONFORMANCE-INSTANCE-INSTRUCTIONS-2b8e"
		instanceSkill       = "conformance-instance"
		markerInstanceSkill = "CONFORMANCE-INSTANCE-SKILL-DESCRIPTION-5a3c"
		markerShadowed      = "CONFORMANCE-INSTANCE-SHADOWED-DESCRIPTION-8f1d"
	)

	skill := func(name, description string) driver.File {
		return driver.File{Data: []byte("---\nname: " + name + "\ndescription: " + description + "\n---\n\nUse it.\n")}
	}

	b := keyBox(t, mockapi.Step{Text: "done"})

	s := b.spec()
	s.InstanceInstructions = "# Every task\n\n" + markerInstance + "\n"
	s.InstanceSkills = map[string]driver.File{
		instanceSkill + "/SKILL.md":       skill(instanceSkill, markerInstanceSkill),
		instanceSkill + "/scripts/run.sh": {Data: []byte("echo run\n"), Executable: true},
		"conformance-marker/SKILL.md":     skill("conformance-marker", markerShadowed),
	}

	requireDone(t, b.run(conformanceDriver(), s))

	calls := modelCalls(b.mock)
	require.NotEmpty(t, calls)

	instance, root := bytes.Index(calls[0].Body, []byte(markerInstance)), bytes.Index(calls[0].Body, []byte(markerRootClaude))
	require.GreaterOrEqual(t, instance, 0, "the instance's instructions are not in the first model call")
	require.GreaterOrEqual(t, root, 0, "the repository's CLAUDE.md is not in the first model call")
	assert.Less(t, instance, root, "the repository's CLAUDE.md comes first")

	for _, m := range []string{instanceSkill, markerInstanceSkill, markerSkill} {
		assert.True(t, carries(calls[0], m), "%s is not in the first model call", m)
	}

	assert.Empty(t, carriedBy(b.mock, markerShadowed), "the instance's skill of the repository's name reached a request")

	require.NoError(t, b.prepare(s))

	res := b.sh("cd " + confConfig + "/skills/" + instanceSkill + " && find . -type f -printf '%P %m\\n' | sort")
	require.Equal(t, 0, res.ExitCode, res.Output)

	files := map[string]bool{}

	for line := range strings.Lines(res.Output) {
		name, mode, _ := strings.Cut(strings.TrimSpace(line), " ")
		perm, err := strconv.ParseUint(mode, 8, 32)
		require.NoError(t, err, line)

		files[name] = perm&0o100 != 0
	}

	assert.Equal(t, map[string]bool{"SKILL.md": false, "scripts/run.sh": true}, files)
	assert.Contains(t, b.readConfig("skills/conformance-marker/SKILL.md"), markerSkill, "the repository's skill was not written over the instance's")
}

// TestSubagentIsCounted settles what a subagent counts toward. The main
// session calls the Agent tool; the subagent, whose requests open with the
// prompt it was given, runs a Bash call and ends; then the main session ends.
// Every call costs 10 input and 5 output tokens.
func TestSubagentIsCounted(t *testing.T) {
	const marker = "CONFORMANCE-SUBAGENT-PROMPT-3c9e"

	withSubagent := []mockapi.Step{
		{Tool: "Agent", Input: map[string]any{"description": "Conformance subagent", "prompt": marker}},
		{Tool: "Bash", Input: map[string]any{"command": "true"}},
		{Text: "subagent done"},
		{Text: "done"},
	}

	// session runs steps under maxTurns, logs every count, and returns the
	// outcome, which requests were the subagent's, and the proxy's count.
	session := func(t *testing.T, maxTurns int, steps ...mockapi.Step) (outcome, []bool, proxy.Usage) {
		t.Helper()

		b := keyBox(t, steps...)
		s := b.spec()
		s.MaxTurns = maxTurns

		o := b.run(conformanceDriver(), s)

		var bySubagent []bool
		for _, r := range modelCalls(b.mock) {
			bySubagent = append(bySubagent, len(r.Messages) > 0 && bytes.Contains(r.Messages[0], []byte(marker)))
		}

		var proxied proxy.Usage

		lines := b.sidecar.of("usage")
		for _, l := range lines {
			proxied.Input, proxied.Output = proxied.Input+l.Usage.Input, proxied.Output+l.Usage.Output
		}

		type perModel map[string]struct {
			Input  int64 `json:"inputTokens"`
			Output int64 `json:"outputTokens"`
		}

		var result struct {
			Usage struct {
				Input  int64 `json:"input_tokens"`
				Output int64 `json:"output_tokens"`
			} `json:"usage"`
			ModelUsage perModel `json:"modelUsage"`
			SinceLast  perModel `json:"model_usage_since_last_result"`
		}

		require.NoError(t, json.Unmarshal([]byte(rawLine(o, "result")), &result))
		t.Logf("max turns %d: %s, turns %d, usage %+v; requests by the subagent %v; result line usage %+v, modelUsage %+v, "+
			"model_usage_since_last_result %+v; proxy %d lines, %d input, %d output", maxTurns, o.res.Reason, o.res.Turns, o.res.Usage,
			bySubagent, result.Usage, result.ModelUsage, result.SinceLast, len(lines), proxied.Input, proxied.Output)

		return o, bySubagent, proxied
	}

	t.Run("counts", func(t *testing.T) {
		o, bySubagent, proxied := session(t, 10, withSubagent...)
		requireDone(t, o)
		require.Equal(t, []bool{false, true, true, false}, bySubagent, "the steps did not line up as scripted")

		var total driver.Usage
		for _, u := range o.res.Usage {
			total.Input, total.Output = total.Input+u.Input, total.Output+u.Output
		}

		assert.Equal(t, [2]int64{40, 20}, [2]int64{total.Input, total.Output}, "the result's usage lacks the subagent's calls")
		assert.Equal(t, [2]int64{40, 20}, [2]int64{proxied.Input, proxied.Output}, "the proxy's count lacks a call")
		assert.Equal(t, 2, o.res.Turns, "the subagent's turns count in num_turns")
	})

	// The main session's own two turns fit the limit, as the control shows;
	// with the subagent's two the run still ends done.
	t.Run("turn limit", func(t *testing.T) {
		control, _, _ := session(t, 2, mockapi.Step{Tool: "Bash", Input: map[string]any{"command": "true"}}, mockapi.Step{Text: "done"})
		require.Equal(t, driver.ReasonDone, control.res.Reason, "a two-turn session does not fit the limit")

		o, bySubagent, _ := session(t, 2, withSubagent...)
		assert.Equal(t, driver.ReasonDone, o.res.Reason, "the subagent's turns count toward --max-turns")
		assert.Equal(t, 2, o.res.Turns, "the subagent's turns count in num_turns")
		assert.Equal(t, []bool{false, true, true, false}, bySubagent, "the steps did not line up as scripted")
	})
}

// confIdentity names the committer of the checks' own commits.
const confIdentity = "-c user.name=conformance -c user.email=conformance@example.com"

// prepare runs the driver's clear and staging for s in the agent container,
// with no session after them.
func (b *box) prepare(s driver.Spec) error {
	ctx, cancel := context.WithTimeout(b.t.Context(), confBound)
	defer cancel()

	return conformanceDriver().prepare(ctx, b.c, s)
}

// readConfig returns a file below the config dir of the agent container.
func (b *box) readConfig(rel string) string {
	b.t.Helper()

	var out bytes.Buffer
	require.NoError(b.t, b.c.CopyOut(b.t.Context(), path.Join(confConfig, rel), &out))

	return out.String()
}

// lastLine returns the last line of a script's output, failing unless the
// script succeeded.
func lastLine(t *testing.T, res sandbox.Result) string {
	t.Helper()

	require.Equal(t, 0, res.ExitCode, res.Output)

	lines := strings.Split(strings.TrimSpace(res.Output), "\n")

	return lines[len(lines)-1]
}

// TestProjectMCPNotLoaded: the server .mcp.json names neither starts nor
// offers a tool.
func TestProjectMCPNotLoaded(t *testing.T) {
	b := keyBox(t, mockapi.Step{Text: "done"})

	o := b.run(conformanceDriver(), b.spec())
	requireDone(t, o)

	for _, r := range modelCalls(b.mock) {
		for _, tool := range r.Tools {
			assert.False(t, strings.HasPrefix(tool, "mcp__"), "tool %s offered", tool)
		}
	}

	var servers []struct {
		Name string `json:"name"`
	}

	raw, err := json.Marshal(o.line(t, "system", "init")["mcp_servers"])
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &servers))
	assert.Empty(t, servers)
	assert.False(t, b.exists(mcpServerFile), "the MCP server's command ran")
}

// attributionTexts are what the pinned release adds to a request so that the
// model signs commits and pull requests.
var attributionTexts = []string{"Co-Authored-By", "Generated with [Claude Code]"}

// TestUserSettingsStillApply: the --settings file turns attribution off; the
// same run with an empty one carries the texts, which proves the check sees
// them.
func TestUserSettingsStillApply(t *testing.T) {
	for _, tc := range []struct {
		name     string
		settings string
		want     bool
	}{
		{"settings file", settings, false},
		{"empty settings", "{}", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := keyBox(t, mockapi.Step{Tool: "Bash", Input: map[string]any{"command": "true"}}, mockapi.Step{Text: "done"})

			d := conformanceDriver()
			d.amend = func(inv *invocation) {
				for p := range inv.files {
					if path.Base(p) == "settings.json" {
						inv.files[p] = []byte(tc.settings)
					}
				}
			}

			requireDone(t, b.run(d, b.spec()))

			for _, text := range attributionTexts {
				assert.Equal(t, tc.want, len(carriedBy(b.mock, text)) > 0, "%q in a request", text)
			}
		})
	}
}

// TestBypassPermissions: a Bash call that writes runs without a denial, as
// uid 1000.
func TestBypassPermissions(t *testing.T) {
	const out = confDir + "/.conformance-bypass"

	b := keyBox(t,
		mockapi.Step{Tool: "Bash", Input: map[string]any{"command": "id -u > " + out}},
		mockapi.Step{Text: "done"},
	)

	o := b.run(conformanceDriver(), b.spec())
	requireDone(t, o)

	assert.Equal(t, []bool{false}, toolErrors(o))
	assert.Empty(t, o.line(t, "result", "")["permission_denials"])

	res := b.sh("cat " + out + " && stat -c %u " + out)
	require.Equal(t, 0, res.ExitCode, res.Output)
	assert.Equal(t, "1000\n1000\n", res.Output)
}

// TestWebToolsAbsent: WebFetch and WebSearch are offered only with the switch
// on. The pinned release defers both: init lists them and the request names
// them among the tools to load on demand, never in its tool definitions.
func TestWebToolsAbsent(t *testing.T) {
	for _, on := range []bool{false, true} {
		t.Run(fmt.Sprintf("web tools %v", on), func(t *testing.T) {
			b := keyBox(t, mockapi.Step{Text: "done"})

			s := b.spec()
			s.WebTools = on

			o := b.run(conformanceDriver(), s)
			requireDone(t, o)

			var tools []string

			raw, err := json.Marshal(o.line(t, "system", "init")["tools"])
			require.NoError(t, err)
			require.NoError(t, json.Unmarshal(raw, &tools))

			calls := modelCalls(b.mock)
			require.NotEmpty(t, calls)

			for _, tool := range []string{"WebFetch", "WebSearch"} {
				assert.Equal(t, on, slices.Contains(tools, tool), "%s in init", tool)
				assert.Equal(t, on, carries(calls[0], tool), "%s named in the first model call", tool)

				for _, r := range calls {
					assert.NotContains(t, r.Tools, tool)
				}

				if !on {
					assert.Empty(t, carriedBy(b.mock, tool), "%s named in a request", tool)
				}
			}
		})
	}
}

// TestStructuredResult: with a schema, the structured-output tool's input is
// Result.Structured.
func TestStructuredResult(t *testing.T) {
	b := keyBox(t, mockapi.Step{Tool: mockapi.Structured, Input: map[string]any{"verdict": "pass", "count": 2}})

	s := b.spec()
	s.OutputSchema = json.RawMessage(`{"type":"object","properties":{"verdict":{"type":"string"},"count":{"type":"integer"}},` +
		`"required":["verdict","count"],"additionalProperties":false}`)

	o := b.run(conformanceDriver(), s)
	requireDone(t, o)
	assert.JSONEq(t, `{"verdict":"pass","count":2}`, string(o.res.Structured))
}

// TestResultFields: the result line carries is_error and subtype, and a run
// cut by --max-turns ends max_turns.
func TestResultFields(t *testing.T) {
	t.Run("done", func(t *testing.T) {
		b := keyBox(t, mockapi.Step{Text: "done"})
		s := b.spec()

		o := b.run(conformanceDriver(), s)
		requireDone(t, o)

		res := o.line(t, "result", "")
		assert.IsType(t, true, res["is_error"])
		assert.IsType(t, "", res["subtype"])
		// --session-id follows --disallowedTools, whose list must end there.
		assert.Equal(t, s.SessionID, o.res.SessionID)
	})

	t.Run("max turns", func(t *testing.T) {
		b := keyBox(t, mockapi.Step{Tool: "Bash", Input: map[string]any{"command": "true"}})

		s := b.spec()
		s.MaxTurns = 1

		o := b.run(conformanceDriver(), s)
		assert.Equal(t, driver.ReasonMaxTurns, o.res.Reason, o.res.Detail)
		assert.True(t, o.res.IsError)

		res := o.line(t, "result", "")
		assert.Equal(t, true, res["is_error"])
		assert.Equal(t, "error_max_turns", res["subtype"])
	})
}

// TestKeyModeTraffic: the mock sees only the real credential, in the header
// the route names, and the sidecar refuses no host during a full run.
func TestKeyModeTraffic(t *testing.T) {
	for _, header := range []string{"bearer", "x-api-key"} {
		t.Run(header, func(t *testing.T) {
			b := keyBoxWith(t, header,
				mockapi.Step{Tool: "Read", Input: map[string]any{"file_path": confDir + "/pkg/notes.txt"}},
				mockapi.Step{Text: "done"},
			)

			o := b.run(conformanceDriver(), b.spec())
			requireDone(t, o)

			reqs := b.mock.Requests()
			require.NotEmpty(t, reqs)

			for i, r := range reqs {
				switch header {
				case "bearer":
					assert.Equal(t, "Bearer "+b.cred, r.Header.Get("Authorization"), "request %d", i+1)
					assert.Empty(t, r.Header.Values("X-Api-Key"), "request %d", i+1)
				default:
					assert.Equal(t, b.cred, r.Header.Get("X-Api-Key"), "request %d", i+1)
					assert.Empty(t, r.Header.Values("Authorization"), "request %d", i+1)
				}
			}

			assert.Empty(t, carriedBy(b.mock, b.standIn), "the stand-in reached the endpoint")
			assert.Empty(t, b.sidecar.of("denied"))
			requireNoSecretOutput(t, o, b.standIn)
		})
	}
}

// TestResumeAcrossContainers: a session resumed in a fresh agent container on
// the same volumes sends the earlier turn.
func TestResumeAcrossContainers(t *testing.T) {
	const (
		firstPrompt = "CONFORMANCE-FIRST-PROMPT-6a3d"
		firstAnswer = "CONFORMANCE-FIRST-ANSWER-0e5c"
		gone        = "/tmp/.conformance-before-restart"
	)

	b := keyBox(t, mockapi.Step{Text: firstAnswer}, mockapi.Step{Text: "done"})
	d := conformanceDriver()

	first := b.spec()
	first.Prompt = firstPrompt
	requireDone(t, b.run(d, first))

	require.Equal(t, 0, b.sh("touch "+gone).ExitCode)

	c, err := b.sb.Restart(t.Context())
	require.NoError(t, err)

	b.c = c
	require.False(t, b.exists(gone), "the agent container was not replaced")

	second := b.spec()
	second.SessionID, second.Resume = "", first.SessionID

	o := b.run(d, second)
	requireDone(t, o)
	assert.Equal(t, first.SessionID, o.res.SessionID)

	calls := modelCalls(b.mock)
	require.Len(t, calls, 2)
	assert.True(t, carries(calls[1], firstPrompt), "the resumed call lacks the earlier prompt")
	assert.True(t, carries(calls[1], firstAnswer), "the resumed call lacks the earlier answer")
}

// TestResumeUnknownSession: resuming a session with no transcript is refused,
// and a new session under the same id then starts cleanly, as the runner
// starts one.
func TestResumeUnknownSession(t *testing.T) {
	b := keyBox(t, mockapi.Step{Text: "done"})
	d := conformanceDriver()
	id := newSessionID(t)

	s := b.spec()
	s.SessionID, s.Resume = "", id

	o := b.run(d, s)
	assert.Equal(t, driver.ReasonResumeRefused, o.res.Reason, "detail: %s\nstderr: %s", o.res.Detail, o.stderr)
	assert.Empty(t, modelCalls(b.mock))

	s.SessionID, s.Resume = id, ""

	o = b.run(d, s)
	requireDone(t, o)
	assert.Equal(t, id, o.res.SessionID)
	assert.Len(t, modelCalls(b.mock), 1)
}

// verdictSchema asks for a one-field structured result.
const verdictSchema = `{"type":"object","properties":{"verdict":{"type":"string"}},"required":["verdict"],"additionalProperties":false}`

// TestReadOnlySessionChangesNothing: a read-only session in the read-only
// phase is offered no editing tool, changes nothing in the workspace through
// the editing tools or the shell, and still searches with the shell.
func TestReadOnlySessionChangesNothing(t *testing.T) {
	b := keyBox(t,
		mockapi.Step{Tool: "Write", Input: map[string]any{"file_path": confDir + "/.conformance-written", "content": "written\n"}},
		mockapi.Step{Tool: "Edit", Input: map[string]any{
			"file_path": confDir + "/pkg/notes.txt", "old_string": "conformance fixture file", "new_string": "edited",
		}},
		mockapi.Step{Tool: "Bash", Input: map[string]any{"command": "touch probe"}},
		// The command holds the marker's tail; only its output holds the whole.
		mockapi.Step{Tool: "Bash", Input: map[string]any{"command": "grep -rh " + strings.TrimPrefix(markerNested, "CONFORMANCE-") + " pkg"}},
		mockapi.Step{Tool: mockapi.Structured, Input: map[string]any{"verdict": "pass"}},
	)
	b.phase(sandbox.PhaseReadOnly)

	s := b.spec()
	s.ReadOnly = true
	s.OutputSchema = json.RawMessage(verdictSchema)

	o := b.run(conformanceDriver(), s)
	requireDone(t, o)
	assert.JSONEq(t, `{"verdict":"pass"}`, string(o.res.Structured))

	var tools []string

	raw, err := json.Marshal(o.line(t, "system", "init")["tools"])
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &tools))

	calls := modelCalls(b.mock)
	require.Len(t, calls, 5)

	for _, tool := range []string{"Edit", "Write", "NotebookEdit"} {
		assert.NotContains(t, tools, tool, "in init")

		for i, r := range calls {
			assert.NotContains(t, r.Tools, tool, "model call %d", i+1)
		}
	}

	for i, r := range calls {
		assert.Equal(t, i == 4, carries(r, markerNested), "model call %d carries the grep's output", i+1)
	}

	b.phase(sandbox.PhaseAgent)

	res := b.sh("git status --porcelain")
	require.Equal(t, 0, res.ExitCode, res.Output)
	assert.Empty(t, res.Output, "the read-only session changed the workspace")
}

// TestReadOnlyThenResumeWrites: a read-only session resumed in the agent phase
// without the flag has its editing tools back.
func TestReadOnlyThenResumeWrites(t *testing.T) {
	const written = confDir + "/.conformance-resumed"

	b := keyBox(t,
		mockapi.Step{Tool: mockapi.Structured, Input: map[string]any{"verdict": "pass"}},
		mockapi.Step{Tool: "Write", Input: map[string]any{"file_path": written, "content": "resumed\n"}},
		mockapi.Step{Text: "done"},
	)
	d := conformanceDriver()

	b.phase(sandbox.PhaseReadOnly)

	first := b.spec()
	first.ReadOnly = true
	first.OutputSchema = json.RawMessage(verdictSchema)
	requireDone(t, b.run(d, first))

	b.phase(sandbox.PhaseAgent)

	second := b.spec()
	second.SessionID, second.Resume = "", first.SessionID

	o := b.run(d, second)
	requireDone(t, o)
	assert.Equal(t, first.SessionID, o.res.SessionID)

	res := b.sh("cat " + written)
	require.Equal(t, 0, res.ExitCode, res.Output)
	assert.Equal(t, "resumed\n", res.Output)
}

// TestTrustsCustomCAThroughTunnel is a subscription run on a network that
// inspects TLS: the endpoint's certificate is signed by the instance's CA, and
// claude reaches it through a CONNECT tunnel. With the CA in the image the run
// succeeds; without it, it ends in an error.
func TestTrustsCustomCAThroughTunnel(t *testing.T) {
	r := sandboxtest.Runtime(t)
	caPEM, _, _ := sandboxtest.CA(t)

	for _, tc := range []struct {
		name string
		ca   []byte
	}{
		{"trusting image", caPEM},
		{"plain image", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			image := sandboxtest.Image(t, r, Version, SHA512, tc.ca)
			mock := mockapi.StartTLSWith(t, []mockapi.Step{{Text: "done"}}, sandboxtest.ServerCertificate(t))

			b := &box{t: t, rt: r, mock: mock}
			b.up(image, sidecar.ProxyRequest{AllowHosts: []string{strings.TrimPrefix(mock.HostURL(), "https://")}})
			b.plant()

			d := conformanceDriver()
			d.amend = func(inv *invocation) { inv.env = append(inv.env, "ANTHROPIC_BASE_URL="+mock.HostURL()) }

			s := b.spec()
			s.Auth = driver.Auth{Mode: modeSubscription, Token: dummySubscriptionToken}
			s.Env = agentEnv(boundedRetries...)

			o := b.run(d, s)
			t.Logf("reason %s, detail %q, denied %v", o.res.Reason, o.res.Detail, b.sidecar.of("denied"))
			requireNoSecretOutput(t, o, dummySubscriptionToken)

			if tc.ca != nil {
				requireDone(t, o)
				assert.NotEmpty(t, modelCalls(mock))
				assert.Empty(t, b.sidecar.of("denied"))

				return
			}

			assert.NotEqual(t, driver.ReasonDone, o.res.Reason)
			assert.True(t, o.res.IsError)
			assert.Empty(t, mock.Requests(), "a request got through without the CA")
		})
	}
}

const dummySubscriptionToken = "conformance-dummy-subscription-token" //nolint:gosec // a dummy

// boundedRetries keeps a check from waiting out the default retry backoff.
var boundedRetries = []string{"CLAUDE_CODE_MAX_RETRIES=1"}

// TestNoCredentialInContainer: while claude waits on a held answer, the real
// credential is in no process's environment or command line and in no file
// the agent can reach.
func TestNoCredentialInContainer(t *testing.T) {
	b := keyBox(t, mockapi.Step{Text: "done"})
	release := b.mock.Hold()

	t.Cleanup(release)

	type ended struct {
		o   outcome
		err error
	}

	done := make(chan ended, 1)

	go func() {
		o, err := b.try(conformanceDriver(), b.spec())
		done <- ended{o, err}
	}()

	require.Eventually(t, func() bool { return len(modelCalls(b.mock)) > 0 }, confBound, 100*time.Millisecond)
	require.Equal(t, 0, b.sh("pgrep -x claude").ExitCode, "claude is not running during the scan")

	// The bracket keeps the pattern from matching the scan's own command line.
	pattern := "[" + b.cred[:1] + "]" + b.cred[1:]

	res := b.sh(`for f in /proc/[0-9]*/environ /proc/[0-9]*/cmdline; do grep -alE -e '` + pattern + `' "$f" 2>/dev/null; done; ` +
		`grep -alrE -D skip -e '` + pattern + `' /home/agent /workspace /handoff /run/cavu /tmp 2>/dev/null; true`)
	assert.Empty(t, res.Output, "the credential is readable in the agent container")

	res = b.sh(`grep -alE -e '` + "[" + b.standIn[:1] + "]" + b.standIn[1:] + `' /proc/[0-9]*/environ 2>/dev/null; true`)
	assert.NotEmpty(t, res.Output, "the scan does not see the stand-in in claude's environment")

	release()

	select {
	case e := <-done:
		require.NoError(t, e.err)
		requireDone(t, e.o)
	case <-time.After(confBound):
		t.Fatal("the run did not end after release")
	}
}

// TestErrorResults: how the pinned release reports an endpoint error, with
// retries bounded: the result is an error and each retry carries its status.
func TestErrorResults(t *testing.T) {
	for _, tc := range []struct {
		status int
		reason string
	}{
		{http.StatusUnauthorized, driver.ReasonAuth},
		{http.StatusTooManyRequests, driver.ReasonEndpoint},
		{http.StatusInternalServerError, driver.ReasonEndpoint},
		{529, driver.ReasonEndpoint},
	} {
		t.Run(strconv.Itoa(tc.status), func(t *testing.T) {
			steps := slices.Repeat([]mockapi.Step{{Status: tc.status}}, 4)
			b := keyBox(t, steps...)

			s := b.spec()
			s.Env = agentEnv(boundedRetries...)

			o := b.run(conformanceDriver(), s)
			t.Logf("result: %s", rawLine(o, "result"))

			for _, raw := range o.lines {
				if bytes.Contains(raw, []byte(`"api_retry"`)) {
					t.Logf("api_retry: %s", raw)
				}
			}

			assert.Equal(t, tc.reason, o.res.Reason, o.res.Detail)
			assert.True(t, o.res.IsError)
			requireNoSecretOutput(t, o, b.standIn)

			// CLAUDE_CODE_MAX_RETRIES=1: the call and one retry.
			var retries []driver.Retry

			for _, ev := range o.events {
				if ev.Kind == driver.KindRetry {
					retries = append(retries, *ev.Retry)
				}
			}

			require.Len(t, retries, 1)
			assert.Equal(t, tc.status, retries[0].Status)
			assert.Equal(t, 1, retries[0].Attempt)
			assert.Equal(t, 1, retries[0].Max)
			assert.Len(t, modelCalls(b.mock), 2)
		})
	}
}

// TestVersion: --version names the pinned release on its first line.
func TestVersion(t *testing.T) {
	b := keyBox(t)

	v, err := conformanceDriver().Probe(t.Context(), b.c)
	require.NoError(t, err)
	assert.Equal(t, Version, v)
}

// TestInstructionsAndEffort: the instructions file reaches the system prompt
// and the effort the request.
func TestInstructionsAndEffort(t *testing.T) {
	const marker = "CONFORMANCE-INSTRUCTIONS-4c2e"

	b := keyBox(t, mockapi.Step{Text: "done"})

	s := b.spec()
	s.Instructions = marker
	s.Effort = "high"
	requireDone(t, b.run(conformanceDriver(), s))

	calls := modelCalls(b.mock)
	require.NotEmpty(t, calls)
	assert.Contains(t, calls[0].System, marker)

	var body struct {
		OutputConfig struct {
			Effort string `json:"effort"`
		} `json:"output_config"`
	}

	require.NoError(t, json.Unmarshal(calls[0].Body, &body))
	assert.Equal(t, "high", body.OutputConfig.Effort)
}

// requireNoSecretOutput fails if claude wrote secret on stdout or stderr,
// before any masking.
func requireNoSecretOutput(t *testing.T, o outcome, secret string) {
	t.Helper()

	assert.NotContains(t, o.stderr, secret, "the secret is on stderr")

	for _, l := range o.lines {
		require.NotContains(t, string(l), secret, "the secret is on stdout")
	}
}

// toolErrors lists the error flag of each tool result, in order.
func toolErrors(o outcome) []bool {
	var out []bool

	for _, ev := range o.events {
		if ev.Kind == driver.KindToolResult {
			out = append(out, ev.ToolError)
		}
	}

	return out
}

// rawLine is the first stdout line of typ, as written.
func rawLine(o outcome, typ string) string {
	for _, raw := range o.lines {
		var l struct {
			Type string `json:"type"`
		}

		if json.Unmarshal(raw, &l) == nil && l.Type == typ {
			return string(raw)
		}
	}

	return ""
}
