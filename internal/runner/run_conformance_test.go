//go:build integration

package runner

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/cavu/internal/engine"
	"github.com/mhersson/cavu/internal/testkit/mockapi"
)

// These tests run the pinned claude in the sandbox image's agent container, in
// key mode with a dummy credential, against the scripted mock Messages API.

// done ends a session with the structured result of finished work.
var done = mockapi.Step{Tool: mockapi.Structured, Input: map[string]any{
	"status": "done", "title": "Add a line", "summary": "Added a line to notes.txt.",
}}

func bash(command string) mockapi.Step {
	return mockapi.Step{Tool: "Bash", Input: map[string]any{"command": command}}
}

// agentRun prepares a key-mode run of a repository holding notes.txt, whose
// model endpoint is a mock that answers steps.
func agentRun(t *testing.T, steps ...mockapi.Step) (*Runner, engine.RunConfig, *mockapi.Server) {
	t.Helper()

	r, c, _ := fixtureRun(t, map[string]string{"notes.txt": "first line\n"})
	mock := mockapi.Start(t, steps)

	c.Endpoint.Mode, c.Endpoint.URL, c.Endpoint.AuthHeader = "key", mock.HostURL(), "bearer"
	c.Endpoint.Credential = "dummy-credential-" + randomInstance(t)
	c.Agent.Model, c.Agent.MaxTurns, c.Agent.Timeout = "claude-haiku-5-5", 10, 5*time.Minute
	c.Task.Title, c.Task.Body = "Add a line", "Add a line to notes.txt."
	c.Verify.Command, c.Verify.Timeout = "test -f notes.txt", time.Minute

	res, err := r.Prepare(t.Context(), c)
	require.NoError(t, err)
	require.Empty(t, res.FailedStep, res.Output)

	c.Repo.BaseCommit, c.Ecosystems = res.BaseCommit, res.Ecosystems

	return r, c, mock
}

func newSession(t *testing.T) engine.WorkRequest {
	t.Helper()

	b := make([]byte, 16)
	_, err := rand.Read(b)
	require.NoError(t, err)

	return engine.WorkRequest{Mode: engine.ModeStart, SessionID: fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:])}
}

func work(t *testing.T, r *Runner, c engine.RunConfig, w engine.WorkRequest) engine.WorkResult {
	t.Helper()

	res, err := r.Work(t.Context(), c, w, func(engine.Progress) {})
	require.NoError(t, err)

	return res
}

func export(t *testing.T, r *Runner, c engine.RunConfig, res engine.WorkResult) engine.ExportResult {
	t.Helper()

	ex, err := r.Export(t.Context(), c, res.HeadCommit)
	require.NoError(t, err)
	require.Empty(t, ex.Problem)
	assert.Equal(t, res.HeadCommit, ex.HeadCommit)

	return ex
}

// git runs git in the run's workspace and returns its trimmed output.
func git(t *testing.T, r *Runner, c engine.RunConfig, args ...string) string {
	t.Helper()

	out := runIn(t, r.state(c.RunID), append([]string{"git"}, args...)...)
	require.Equal(t, 0, out.ExitCode, out.Output)

	return strings.TrimSpace(out.Output)
}

// assertBundle checks in the agent container that git accepts bundle and that
// it holds the task branch at head.
func assertBundle(t *testing.T, r *Runner, c engine.RunConfig, bundle []byte, head string) {
	t.Helper()

	const at = "/tmp/check.bundle"

	require.NoError(t, r.state(c.RunID).agent.CopyIn(t.Context(), map[string][]byte{at: bundle}))
	git(t, r, c, "bundle", "verify", "-q", at)
	assert.Equal(t, head+" refs/heads/"+c.Repo.Branch, git(t, r, c, "bundle", "list-heads", at))
}

func TestRunProducesDiff(t *testing.T) {
	r, c, _ := agentRun(t, bash("echo CAVU-ADDED-LINE >> notes.txt && git commit -qam 'docs(notes): add a line'"), done)

	res := work(t, r, c, newSession(t))
	require.Equal(t, engine.OutcomeDone, res.Outcome, res.Detail)
	assert.True(t, res.Changed)

	vr, err := r.Verify(t.Context(), c)
	require.NoError(t, err)
	assert.Equal(t, engine.ResultPassed, vr.Result, vr.Output)

	ex := export(t, r, c, res)
	assert.Contains(t, string(ex.Diff), "\n+CAVU-ADDED-LINE\n")
	assert.Equal(t, 1, ex.FilesChanged)
	assert.Equal(t, []string{"notes.txt | 1 +", "1 file changed, 1 insertion(+)"}, ex.DiffStat)
	assertBundle(t, r, c, ex.Bundle, res.HeadCommit)
}

// The freeze commits what a session left uncommitted and follows a HEAD that
// moved ahead of the base; any other state fails the session.
func TestFreezeGitStates(t *testing.T) {
	tests := []struct {
		name, command, line, outcome string
	}{
		{name: "uncommitted file only", command: "echo CAVU-UNCOMMITTED > new.txt", line: "+CAVU-UNCOMMITTED", outcome: engine.OutcomeDone},
		{
			name: "detached HEAD ahead of base", command: "git checkout -q --detach && echo CAVU-DETACHED >> notes.txt && git commit -qam detached",
			line: "+CAVU-DETACHED", outcome: engine.OutcomeDone,
		},
		{
			name: "another branch ahead of base", command: "git checkout -q -b other && echo CAVU-OTHER >> notes.txt && git commit -qam other",
			line: "+CAVU-OTHER", outcome: engine.OutcomeDone,
		},
		{
			name: "history that does not descend from base", command: `git reset -q --hard "$(git commit-tree -m orphan 'HEAD^{tree}')"`,
			outcome: engine.OutcomeFailed,
		},
		{name: "no .git", command: "rm -rf .git", outcome: engine.OutcomeFailed},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, c, _ := agentRun(t, bash(tt.command), done)

			res := work(t, r, c, newSession(t))
			require.Equal(t, tt.outcome, res.Outcome, res.Detail)

			if tt.outcome == engine.OutcomeFailed {
				assert.NotEmpty(t, res.Detail)
				assert.Empty(t, res.HeadCommit)

				return
			}

			assert.True(t, res.Changed)
			assert.Equal(t, res.HeadCommit, git(t, r, c, "rev-parse", "refs/heads/"+c.Repo.Branch), "the task branch holds the frozen work")
			assert.Empty(t, git(t, r, c, "status", "--porcelain"))
			assert.Contains(t, string(export(t, r, c, res).Diff), "\n"+tt.line+"\n")
		})
	}
}

func TestFreezeRunsNoMonitor(t *testing.T) {
	r, c, _ := agentRun(t, bash("echo CAVU-ADDED-LINE >> notes.txt && git commit -qam 'docs(notes): add a line' && "+
		`printf '#!/bin/sh\ntouch /tmp/cavu-monitor-ran\nexit 1\n' > /tmp/monitor && chmod +x /tmp/monitor && `+
		"git config core.fsmonitor /tmp/monitor && echo CAVU-LEFT > left.txt"), done)

	res := work(t, r, c, newSession(t))
	require.Equal(t, engine.OutcomeDone, res.Outcome, res.Detail)
	require.True(t, res.Changed)

	assert.NotEqual(t, 0, runIn(t, r.state(c.RunID), "test", "-e", "/tmp/cavu-monitor-ran").ExitCode, "the monitor ran")
}

func TestExportIgnoresChangesAfterFreeze(t *testing.T) {
	r, c, _ := agentRun(t, bash("echo CAVU-FROZEN >> notes.txt && git commit -qam 'docs(notes): add a line'"), done)

	res := work(t, r, c, newSession(t))
	require.Equal(t, engine.OutcomeDone, res.Outcome, res.Detail)

	c.Verify.Command = "echo CAVU-LATER >> notes.txt && git commit -qam later && git checkout -q -b later && " +
		"{ setsid sh -c 'while :; do echo CAVU-REWRITTEN > /handoff/diff.patch; sleep 0.01; done' </dev/null >/dev/null 2>&1 & }"

	vr, err := r.Verify(t.Context(), c)
	require.NoError(t, err)
	require.Equal(t, engine.ResultPassed, vr.Result, vr.Output)
	require.NotEqual(t, res.HeadCommit, git(t, r, c, "rev-parse", "refs/heads/"+c.Repo.Branch), "the verify command moved the branch")

	ex := export(t, r, c, res)
	assert.Contains(t, string(ex.Diff), "\n+CAVU-FROZEN\n")
	assert.NotContains(t, string(ex.Diff), "CAVU-LATER")
	assert.NotContains(t, string(ex.Diff), "CAVU-REWRITTEN")
	assertBundle(t, r, c, ex.Bundle, res.HeadCommit)
}

// forgeCommitGraph points the head commit's entry in the commit-graph at the parent's tree.
const forgeCommitGraph = `g=.git/objects/info/commit-graph
git --no-replace-objects commit-graph write --reachable && chmod u+w "$g" &&
perl -e 'open(F, "+<:raw", $ARGV[0]) or die; local $/; my $d = <F>; my %c;
for my $i (0 .. ord(substr($d, 6, 1)) - 1) { my ($id, $at) = unpack("a4 Q>", substr($d, 8 + 12 * $i, 12)); $c{$id} = $at }
my $n = 0; $n++ while $c{OIDL} + 20 * $n < $c{CDAT} && substr($d, $c{OIDL} + 20 * $n, 20) ne pack("H*", $ARGV[1]);
seek(F, $c{CDAT} + 36 * $n, 0); print F pack("H*", $ARGV[2])' "$g" "$(git rev-parse HEAD)" "$(git rev-parse 'HEAD~1^{tree}')"`

// The diff and the summary show the committed change whatever diff drivers, replace refs, commit-graph or submodule setting the agent left.
func TestExportShowsCommittedChange(t *testing.T) {
	r, c, _ := agentRun(t, bash("git config --global diff.external true && "+
		"echo 'notes.txt diff=silent' >> .git/info/attributes && git config diff.silent.textconv true && "+
		"echo CAVU-ADDED-LINE >> notes.txt && "+
		`mkdir sub && git update-index --add --cacheinfo 160000,"$(git rev-parse HEAD)",sub && git config diff.ignoreSubmodules all && `+
		"git commit -qam 'docs(notes): add a line' && "+
		"git replace \"$(git rev-parse HEAD:notes.txt)\" \"$(git rev-parse HEAD~1:notes.txt)\""), done)

	res := work(t, r, c, newSession(t))
	require.Equal(t, engine.OutcomeDone, res.Outcome, res.Detail)

	forged := runIn(t, r.state(c.RunID), "sh", "-c", forgeCommitGraph)
	require.Equal(t, 0, forged.ExitCode, forged.Output)

	ex := export(t, r, c, res)
	assert.Contains(t, string(ex.Diff), "\n+CAVU-ADDED-LINE\n")
	assert.Contains(t, string(ex.Diff), "\n+Subproject commit ")
	assert.Equal(t, []string{"notes.txt | 1 +", "sub       | 1 +", "2 files changed, 2 insertions(+)"}, ex.DiffStat)
}

func TestVerifyRunsInFreshContainer(t *testing.T) {
	const left = "^sleep 987654$"

	r, c, _ := agentRun(t, bash("{ setsid sleep 987654 </dev/null >/dev/null 2>&1 & } && echo CAVU-STARTED >> notes.txt"), done)

	res := work(t, r, c, newSession(t))
	require.Equal(t, engine.OutcomeDone, res.Outcome, res.Detail)

	running := runIn(t, r.state(c.RunID), "pgrep", "-f", left)
	require.Equal(t, 0, running.ExitCode, "the session left its background process running: %s", running.Output)

	c.Verify.Command = "! pgrep -f '" + left + "'"

	vr, err := r.Verify(t.Context(), c)
	require.NoError(t, err)
	assert.Equal(t, engine.ResultPassed, vr.Result, vr.Output)
}

func TestSessionResumesAfterDown(t *testing.T) {
	const (
		firstPrompt = "CAVU-FIRST-PROMPT-8e2d"
		firstAnswer = "CAVU-FIRST-ANSWER-4b1c"
	)

	r, c, mock := agentRun(t, mockapi.Step{Text: firstAnswer}, done, done)
	c.Task.Title = firstPrompt

	w := newSession(t)
	work(t, r, c, w)

	n := len(mock.Requests())

	require.NoError(t, r.Release(t.Context(), refOf(c), false))

	res := work(t, r, c, engine.WorkRequest{Mode: engine.ModeContinue, SessionID: w.SessionID, Resume: true})
	require.Equal(t, engine.OutcomeDone, res.Outcome, res.Detail)

	reqs := mock.Requests()
	require.Greater(t, len(reqs), n, "the resumed session called the model")

	resumed := reqs[len(reqs)-1]
	assert.True(t, carries(resumed, firstPrompt), "the resumed call lacks the first prompt")
	assert.True(t, carries(resumed, firstAnswer), "the resumed call lacks the first answer")
}

func carries(r mockapi.Request, marker string) bool {
	for _, m := range r.Messages {
		if bytes.Contains(m, []byte(marker)) {
			return true
		}
	}

	return false
}

// A fresh fix of a review gets the whole change as a file in the agent
// container, and the new session reads it.
func TestFreshFixReadsReviewPatch(t *testing.T) {
	const line = "CAVU-DIFF-ONLY-LINE-5d3a"

	r, c, mock := agentRun(t,
		bash("echo "+line+" >> notes.txt && git commit -qam 'docs(notes): add a line'"), done,
		bash("cat /run/cavu/review.patch"), done)

	res := work(t, r, c, newSession(t))
	require.Equal(t, engine.OutcomeDone, res.Outcome, res.Detail)
	require.True(t, res.Changed)

	before := len(mock.Requests())

	fix := newSession(t)
	fix.Mode, fix.Role = engine.ModeFixReview, engine.RoleStrong
	// As the settings fill an unset model_strong with the work model.
	c.Agent.Strong.Model = c.Agent.Model
	fix.Findings = []engine.Finding{{Severity: "important", Where: "notes.txt", What: "The new line is unexplained.", Why: "A reader cannot tell why it is there."}}

	res = work(t, r, c, fix)
	require.Equal(t, engine.OutcomeDone, res.Outcome, res.Detail)

	reqs := mock.Requests()[before:]
	require.Len(t, reqs, 2, "the fresh session read the patch and then finished")
	assert.False(t, carries(reqs[0], line), "the diff reached the model before the read")
	assert.True(t, carries(reqs[1], line), "the diff did not reach the model after the read")
}
