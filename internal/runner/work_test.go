package runner

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/cavu/internal/driver"
	"github.com/mhersson/cavu/internal/engine"
	"github.com/mhersson/cavu/internal/forge"
	"github.com/mhersson/cavu/internal/proc"
	"github.com/mhersson/cavu/internal/prompts"
	"github.com/mhersson/cavu/internal/proxy"
	"github.com/mhersson/cavu/internal/sandbox"
	"github.com/mhersson/cavu/internal/sidecar"
	"github.com/mhersson/cavu/internal/store"
	"github.com/mhersson/cavu/internal/testkit/forgefake"
)

const (
	sessionUUID = "6ba7b810-9dad-11d1-80b4-00c04fd430c8"
	headCommit  = "89abcdef0123456789abcdef0123456789abcdef"
	fixHead     = "fedcba9876543210fedcba9876543210fedcba98" // the pull request's head a fix run starts at
)

func workConfig() engine.RunConfig {
	c := testConfig()
	c.Repo.BaseCommit = baseCommit
	c.Ecosystems = []string{"go"}
	c.Agent.Model, c.Agent.Effort, c.Agent.MaxTurns, c.Agent.Timeout = "claude-test", "high", 50, time.Minute
	c.Task.Title, c.Task.Body = "Fix login", "The login form rejects valid passwords."

	return c
}

var startWork = engine.WorkRequest{Mode: engine.ModeStart, SessionID: sessionUUID}

func noProgress(engine.Progress) {}

func structured(v map[string]any) json.RawMessage {
	b, _ := json.Marshal(v)

	return b
}

// freezeName is how the fake names the freeze of c's work under message.
func freezeName(c engine.RunConfig, message string) string {
	return stepName(scriptArgv(freezeScript, cmp.Or(c.Repo.Head, c.Repo.BaseCommit), c.Repo.Branch, message))
}

// failChecks makes fake's test and lint jobs fail on fixHead, the test job's
// log holding a planted secret.
func failChecks(fake *forgefake.Fake) {
	fake.SetChecks(fixHead,
		forgefake.CheckRun{ID: 1, App: "github-actions", Name: "test", Status: "completed", Conclusion: "failure"},
		forgefake.CheckRun{ID: 2, App: "github-actions", Name: "lint", Status: "completed", Conclusion: "failure"},
	)
	fake.SetJobLog(1, "--- FAIL: TestLogin with "+plantedSetup+"\n")
	fake.SetJobLog(2, "login.go:3: unused variable\n")
}

func render(t *testing.T, name string, data any) string {
	t.Helper()

	s, err := prompts.Render(name, data)
	require.NoError(t, err)

	return s
}

func TestWorkMapsResults(t *testing.T) {
	resets := time.Date(2026, 11, 16, 18, 0, 0, 0, time.UTC)
	done := structured(map[string]any{
		"status": "done", "title": "Fix the login form", "summary": "Trimmed the password.", "assumptions": []string{"Spaces are typos."},
		"where_to_look": []string{"login.go: the trim"},
	})
	doneResult := driver.Result{Reason: driver.ReasonDone, SessionID: "agent-id", Turns: 7, CostEstimateUSD: 0.5, Structured: done}
	doneWork := engine.WorkResult{
		Outcome: engine.OutcomeDone, Title: "Fix the login form", Summary: "Trimmed the password.",
		Assumptions: []string{"Spaces are typos."}, WhereToLook: []string{"login.go: the trim"}, Turns: 7, CostEstimateUSD: 0.5,
	}

	// What a rootless Podman client prints after an exec's own output.
	const warning = `time="2026-11-16T15:00:00Z" level=warning msg="The cgroupv2 manager is set to systemd but there is no systemd user session available"`

	changed, unchanged, refused, noCommit := doneWork, doneWork, doneWork, doneWork
	changed.Changed, changed.HeadCommit = true, headCommit
	unchanged.HeadCommit = baseCommit
	refused.Outcome, refused.Summary, refused.Detail = engine.OutcomeFailed, "HEAD left the base", "HEAD left the base"
	noCommit.Outcome, noCommit.Summary, noCommit.Detail = engine.OutcomeFailed, warning, warning

	tests := []struct {
		name   string
		res    driver.Result
		freeze *sandbox.Result // the freeze's answer; nil when none may run
		want   engine.WorkResult
	}{
		{
			name: "usage limit",
			res:  driver.Result{Reason: driver.ReasonUsageLimit, Detail: "429 usage", RateLimit: &driver.RateLimit{ResetsAt: resets}},
			want: engine.WorkResult{Outcome: engine.OutcomeUsageLimit, Detail: "429 usage", ResetsAt: resets},
		},
		{
			name: "max turns",
			res:  driver.Result{Reason: driver.ReasonMaxTurns, Turns: 50},
			want: engine.WorkResult{Outcome: engine.OutcomeMaxTurns, Turns: 50},
		},
		{
			name: "auth",
			res:  driver.Result{Reason: driver.ReasonAuth, Detail: "401"},
			want: engine.WorkResult{Outcome: engine.OutcomeEndpoint, Detail: "401"},
		},
		{
			name: "endpoint refused",
			res:  driver.Result{Reason: driver.ReasonEndpoint, Detail: "403"},
			want: engine.WorkResult{Outcome: engine.OutcomeEndpoint, Detail: "403"},
		},
		{
			name: "endpoint failed",
			res:  driver.Result{Reason: driver.ReasonEndpointFailed, Detail: "502"},
			want: engine.WorkResult{Outcome: engine.OutcomeEndpointFailed, Detail: "502"},
		},
		{
			name: "crashed, without a cost",
			res:  driver.Result{Reason: driver.ReasonCrashed, Detail: "exit 1", CostMissing: true},
			want: engine.WorkResult{Outcome: engine.OutcomeCrashed, Detail: "exit 1", CostMissing: true},
		},
		{name: "done", res: doneResult, freeze: &sandbox.Result{Output: headCommit + "\n"}, want: changed},
		{name: "done without a change", res: doneResult, freeze: &sandbox.Result{Output: baseCommit + "\n"}, want: unchanged},
		{name: "done but the freeze refuses", res: doneResult, freeze: &sandbox.Result{ExitCode: 1, Output: "HEAD left the base\n"}, want: refused},
		{name: "done past a runtime warning", res: doneResult, freeze: &sandbox.Result{Output: headCommit + "\n" + warning + "\n"}, want: changed},
		{name: "done with no commit printed", res: doneResult, freeze: &sandbox.Result{Output: warning + "\n"}, want: noCommit},
		{
			name: "blocked",
			res:  driver.Result{Reason: driver.ReasonDone, Structured: structured(map[string]any{"status": "blocked", "questions": []string{"Which form?"}})},
			want: engine.WorkResult{Outcome: engine.OutcomeBlocked, Questions: []string{"Which form?"}},
		},
		{
			name: "failed",
			res:  driver.Result{Reason: driver.ReasonDone, Structured: structured(map[string]any{"status": "failed", "summary": "No form."})},
			want: engine.WorkResult{Outcome: engine.OutcomeFailed, Summary: "No form."},
		},
		{
			name: "done without a result",
			res:  driver.Result{Reason: driver.ReasonDone, Text: "I will now"},
			want: engine.WorkResult{Outcome: engine.OutcomeUnfinished},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFake()
			c := workConfig()
			freeze := freezeName(c, "Fix the login form")

			if tt.freeze != nil {
				f.results[freeze] = *tt.freeze
			}

			res, err := f.workRunner(&fakeAgent{results: []driver.Result{tt.res}}).Work(t.Context(), c, startWork, noProgress)
			require.NoError(t, err)
			assert.Equal(t, tt.want, res)
			assert.Equal(t, tt.freeze != nil, ran(f, freeze), "the work is frozen only when the session is done")
		})
	}
}

func ran(f *fakeSandbox, name string) bool {
	for _, r := range f.runsIn(sandbox.PhaseAgent) {
		if r.name == name {
			return true
		}
	}

	return false
}

// The freeze runs as one script in the agent container, wrapped so it runs
// only in a sandbox, with the commit the run started at, the branch and the
// message as arguments; its environment still names the base commit.
func TestWorkFreezeScript(t *testing.T) {
	for _, tt := range []struct{ name, title, message, head, start string }{
		{name: "the result's title", title: "Fix the login form", message: "Fix the login form", start: baseCommit},
		{name: "the task's title", message: "Fix login", start: baseCommit},
		{name: "a fix run", message: "Fix login", head: fixHead, start: fixHead},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newFake()
			c := workConfig()
			c.Repo.Head = tt.head
			f.results[freezeName(c, tt.message)] = sandbox.Result{Output: headCommit + "\n"}
			d := &fakeAgent{results: []driver.Result{{Reason: driver.ReasonDone, Structured: structured(map[string]any{"status": "done", "title": tt.title})}}}

			res, err := f.workRunner(d).Work(t.Context(), c, startWork, noProgress)
			require.NoError(t, err)
			assert.Equal(t, headCommit, res.HeadCommit)

			run := f.run(t, freezeName(c, tt.message))
			assert.Equal(t, sandbox.PhaseAgent, run.phase)
			assert.Equal(t, workspace, run.cmd.Dir)
			assert.True(t, strings.HasPrefix(run.cmd.Argv[2], proc.SandboxOnly("")), "the script runs only in a sandbox")
			assert.Equal(t, []string{tt.start, "cavu/fix-login", tt.message}, run.cmd.Argv[4:])
			assert.Contains(t, run.cmd.Env, "CAVU_BASE_COMMIT="+baseCommit)
		})
	}
}

// A fix run starts at the pull request's head, so a session that adds no
// commit leaves the change as it was, and one that adds a commit changed it.
func TestFixRunThatChangesNothingIsNotChanged(t *testing.T) {
	for _, tt := range []struct {
		name, frozen string
		changed      bool
	}{
		{name: "no commit", frozen: fixHead},
		{name: "a commit on top", frozen: headCommit, changed: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newFake()
			c := workConfig()
			c.Repo.Head = fixHead
			f.results[freezeName(c, c.Task.Title)] = sandbox.Result{Output: tt.frozen + "\n"}
			d := &fakeAgent{results: []driver.Result{{Reason: driver.ReasonDone, Structured: structured(map[string]any{"status": "done"})}}}
			w := engine.WorkRequest{Mode: engine.ModeComments, SessionID: sessionUUID, Resume: true}

			res, err := f.workRunner(d).Work(t.Context(), c, w, noProgress)
			require.NoError(t, err)
			assert.Equal(t, engine.OutcomeDone, res.Outcome, res.Detail)
			assert.Equal(t, tt.frozen, res.HeadCommit)
			assert.Equal(t, tt.changed, res.Changed)
		})
	}
}

func TestWorkSpec(t *testing.T) {
	// Every session runs at a pull request's head, whose test and lint jobs
	// failed; the project ignores lint.
	client, fake, url := fakeForge(t)
	failChecks(fake)

	repo, err := forge.ParseRepo(url)
	require.NoError(t, err)

	logs, err := client.FailedLogs(t.Context(), repo, forge.Access{Token: plantedForge}, fixHead, []string{"lint"})
	require.NoError(t, err)
	require.Contains(t, logs, "--- FAIL: TestLogin")

	comments := []engine.Comment{
		{ID: "PRRC_1", Author: "alice", Path: "login.go", Line: 12, Body: "Trim only the newline."},
		{ID: "PRR_2", Author: "bob", Body: "Add a test."},
	}

	tests := []struct {
		name   string
		w      engine.WorkRequest
		prompt func(t *testing.T, c engine.RunConfig) string
	}{
		{
			name: "start", w: startWork,
			prompt: func(t *testing.T, c engine.RunConfig) string {
				return render(t, prompts.Work, prompts.WorkData{Title: c.Task.Title, Body: c.Task.Body})
			},
		},
		{
			name: "continue", w: engine.WorkRequest{Mode: engine.ModeContinue, SessionID: sessionUUID, Resume: true},
			prompt: func(t *testing.T, _ engine.RunConfig) string { return render(t, prompts.Continue, nil) },
		},
		{
			name: "fix verify", w: engine.WorkRequest{Mode: engine.ModeFixVerify, SessionID: sessionUUID, Resume: true, Detail: "ok\n--- FAIL: TestLogin\n"},
			prompt: func(t *testing.T, c engine.RunConfig) string {
				return render(t, prompts.FixVerify, prompts.FixVerifyData{Command: c.Verify.Command, Output: "ok\n--- FAIL: TestLogin\n"})
			},
		},
		{
			name: "answer", w: engine.WorkRequest{Mode: engine.ModeAnswer, SessionID: sessionUUID, Resume: true, Detail: "The admin form."},
			prompt: func(t *testing.T, _ engine.RunConfig) string {
				return render(t, prompts.Answer, prompts.AnswerData{Answer: "The admin form."})
			},
		},
		{
			name: "fix ci", w: engine.WorkRequest{Mode: engine.ModeFixCI, SessionID: sessionUUID, Resume: true},
			prompt: func(t *testing.T, c engine.RunConfig) string {
				return render(t, prompts.FixCI, prompts.FixCIData{Logs: masker(c)(logs)})
			},
		},
		{
			name: "comments", w: engine.WorkRequest{Mode: engine.ModeComments, SessionID: sessionUUID, Resume: true, Comments: comments},
			prompt: func(t *testing.T, _ engine.RunConfig) string {
				return render(t, prompts.Comments, prompts.CommentsData{Comments: []prompts.ReviewComment{
					{Author: "alice", Path: "login.go", Line: 12, Body: "Trim only the newline."},
					{Author: "bob", Body: "Add a test."},
				}})
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFake()
			c := workConfig()
			c.Endpoint.Mode = "subscription"
			c.Agent.WebTools = true
			c.Repo.URL, c.Repo.Head, c.Repo.IgnoredChecks = url, fixHead, []string{"lint"}
			d := &fakeAgent{results: []driver.Result{{Reason: driver.ReasonDone}}}
			r := f.workRunner(d)
			r.forge = client

			_, err := r.Work(t.Context(), c, tt.w, noProgress)
			require.NoError(t, err)
			require.Len(t, d.specs, 1)

			want := driver.Spec{
				Auth:  driver.Auth{Mode: "subscription", Token: plantedEndpoint},
				Model: "claude-test", Effort: "high", MaxTurns: 50,
				Prompt: tt.prompt(t, c), Instructions: render(t, prompts.Instructions, nil), OutputSchema: prompts.ResultSchema(),
				Dir: workspace, BaseCommit: baseCommit, Home: home, ConfigDir: "/home/agent/.claude-cavu",
				Env: withProjectVars(sessionEnv([]string{"go"}, baseCommit), c), WebTools: true,
			}
			if tt.w.Resume {
				want.Resume = sessionUUID
			} else {
				want.SessionID = sessionUUID
			}

			assert.Equal(t, want, d.specs[0])
			assert.Contains(t, d.specs[0].Env, "CAVU_BASE_COMMIT="+baseCommit)
			assertMasked(t, d.specs[0].Prompt)
		})
	}
}

// Logs of the failed checks that the forge cannot give fail the call, which
// the engine retries, and no session runs without them.
func TestWorkFailsWithoutTheFailedLogs(t *testing.T) {
	client, fake, url := fakeForge(t)
	fake.Fail(1)

	c := workConfig()
	c.Repo.URL, c.Repo.Head = url, fixHead
	d := &fakeAgent{results: []driver.Result{{Reason: driver.ReasonDone}}}
	r := newFake().workRunner(d)
	r.forge = client

	_, err := r.Work(t.Context(), c, engine.WorkRequest{Mode: engine.ModeFixCI, SessionID: sessionUUID, Resume: true}, noProgress)
	require.Error(t, err)
	assert.Empty(t, d.specs)
}

func TestWorkSpecCarriesNoRealCredential(t *testing.T) {
	f := newFake()
	d := &fakeAgent{results: []driver.Result{{Reason: driver.ReasonDone}}}

	_, err := f.workRunner(d).Work(t.Context(), workConfig(), startWork, noProgress)
	require.NoError(t, err)
	require.Len(t, d.specs, 1)

	standIn := f.requests(sandbox.PhaseAgent)[0].Model.StandIn
	assert.Equal(t, driver.Auth{Mode: "key", Token: standIn, BaseURL: sandbox.BridgeURL}, d.specs[0].Auth)

	raw, err := json.Marshal(d.specs[0])
	require.NoError(t, err)

	for _, secret := range []string{plantedEndpoint, plantedForge, plantedSetup} {
		assert.NotContains(t, string(raw), secret)
	}
}

// AgentPhase builds what a run starts its agent phase and a session with, but
// the session's prompt and id.
func TestAgentPhaseIsWhatARunBuilds(t *testing.T) {
	for _, mode := range []string{"key", "subscription"} {
		t.Run(mode, func(t *testing.T) {
			f := newFake()
			c := workConfig()
			c.Endpoint.Mode = mode
			d := &fakeAgent{results: []driver.Result{{Reason: driver.ReasonDone}}}

			_, err := f.workRunner(d).Work(t.Context(), c, startWork, noProgress)
			require.NoError(t, err)
			require.Len(t, d.specs, 1)

			req, spec, err := AgentPhase(c, d)
			require.NoError(t, err)

			up := f.requests(sandbox.PhaseAgent)
			require.Len(t, up, 1)

			if mode == "key" {
				// Each start of the phase has its own stand-in, which the agent holds.
				assert.Equal(t, req.Model.StandIn, spec.Auth.Token)
				req.Model.StandIn, spec.Auth.Token = up[0].Model.StandIn, up[0].Model.StandIn
			}

			spec.Prompt, spec.SessionID = d.specs[0].Prompt, d.specs[0].SessionID

			assert.Equal(t, up[0], req)
			assert.Equal(t, d.specs[0], spec)
		})
	}
}

// The driver adds the loopback hosts to a session's NO_PROXY, so the session
// gets none of the runner's own; the commands the runner runs keep theirs.
func TestWorkSessionLeavesNoProxyToTheDriver(t *testing.T) {
	f := newFake()
	c := workConfig()
	f.results[freezeName(c, c.Task.Title)] = sandbox.Result{Output: headCommit + "\n"}
	d := &fakeAgent{results: []driver.Result{{Reason: driver.ReasonDone, Structured: structured(map[string]any{"status": "done"})}}}

	_, err := f.workRunner(d).Work(t.Context(), c, startWork, noProgress)
	require.NoError(t, err)
	require.Len(t, d.specs, 1)

	names := func(env []string) []string {
		var out []string

		for _, kv := range env {
			k, _, _ := strings.Cut(kv, "=")
			out = append(out, k)
		}

		return out
	}

	session, freeze := names(d.specs[0].Env), names(f.run(t, freezeName(c, c.Task.Title)).cmd.Env)
	assert.Contains(t, session, "HTTPS_PROXY")

	for _, k := range []string{"NO_PROXY", "no_proxy"} {
		assert.NotContains(t, session, k)
		assert.Contains(t, freeze, k)
	}
}

// A refused resume runs once more as a new session under the same id, with
// the task's opening prompt, whatever the mode.
func TestWorkResumeRefusedStartsFresh(t *testing.T) {
	f := newFake()
	c := workConfig()
	d := &fakeAgent{results: []driver.Result{{Reason: driver.ReasonResumeRefused}, {Reason: driver.ReasonDone}}}
	w := engine.WorkRequest{Mode: engine.ModeFixVerify, SessionID: sessionUUID, Resume: true, Detail: "--- FAIL: TestLogin\n"}

	res, err := f.workRunner(d).Work(t.Context(), c, w, noProgress)
	require.NoError(t, err)
	assert.Equal(t, engine.OutcomeUnfinished, res.Outcome)

	require.Len(t, d.specs, 2)
	assert.Equal(t, sessionUUID, d.specs[0].Resume)
	assert.Empty(t, d.specs[0].SessionID)
	assert.Equal(t, sessionUUID, d.specs[1].SessionID)
	assert.Empty(t, d.specs[1].Resume)
	assert.Equal(t, render(t, prompts.Work, prompts.WorkData{Title: c.Task.Title, Body: c.Task.Body}), d.specs[1].Prompt)
}

var readinessWork = engine.WorkRequest{Mode: engine.ModeReadiness, SessionID: sessionUUID}

func readyResult() driver.Result {
	return driver.Result{Reason: driver.ReasonDone, Structured: structured(map[string]any{"status": "ready"})}
}

func TestReadinessSpec(t *testing.T) {
	f := newFake()
	c := workConfig()
	c.Endpoint.Mode = "subscription"
	d := &fakeAgent{results: []driver.Result{readyResult()}}

	res, err := f.workRunner(d).Work(t.Context(), c, readinessWork, noProgress)
	require.NoError(t, err)
	assert.Equal(t, engine.OutcomeReady, res.Outcome)
	assert.Equal(t, []string{"volumes", "up/readonly", goSeed}, f.calls(), "a ready result runs no command in the container but the home's seed")
	require.Len(t, d.specs, 1)

	assert.Equal(t, driver.Spec{
		Auth:  driver.Auth{Mode: "subscription", Token: plantedEndpoint},
		Model: "claude-test", Effort: "high", MaxTurns: 50, SessionID: sessionUUID,
		// No instructions: they would tell a session that changes nothing to commit its work.
		Prompt:       render(t, prompts.Readiness, prompts.WorkData{Title: c.Task.Title, Body: c.Task.Body}),
		OutputSchema: prompts.ReadinessSchema(),
		Dir:          workspace, BaseCommit: baseCommit, Home: home, ConfigDir: "/home/agent/.claude-cavu",
		Env: withProjectVars(sessionEnv([]string{"go"}, baseCommit), c), ReadOnly: true,
	}, d.specs[0])
}

// The session phases share the run's proxy request, so a switch between them
// keeps the sidecar; whatever writes runs in the agent phase.
func TestPhasesSwitch(t *testing.T) {
	t.Run("the work after a readiness turn", func(t *testing.T) {
		f := newFake()
		c := workConfig()
		freeze := freezeName(c, c.Task.Title)
		f.results[freeze] = sandbox.Result{Output: headCommit + "\n"}
		d := &fakeAgent{results: []driver.Result{
			readyResult(), {Reason: driver.ReasonDone, Structured: structured(map[string]any{"status": "done"})},
		}}
		r := f.workRunner(d)

		_, err := r.Work(t.Context(), c, readinessWork, noProgress)
		require.NoError(t, err)

		res, err := r.Work(t.Context(), c, engine.WorkRequest{Mode: engine.ModeStart, SessionID: sessionUUID, Resume: true}, noProgress)
		require.NoError(t, err)
		assert.Equal(t, headCommit, res.HeadCommit)

		assert.Equal(t, []string{"volumes", "up/readonly", goSeed, "up/agent", goSeed, "run/" + freeze}, f.calls())
		assert.Equal(t, sandbox.PhaseAgent, f.run(t, freeze).phase)

		readOnly, agent := f.requests(sandbox.PhaseReadOnly), f.requests(sandbox.PhaseAgent)
		require.Len(t, readOnly, 1)
		require.Len(t, agent, 1)
		assert.Equal(t, readOnly[0], agent[0], "the same request, so the sidecar stays")
		require.Len(t, d.specs, 2)
		assert.Equal(t, d.specs[0].Auth, d.specs[1].Auth, "both sessions hold the one stand-in")
	})

	t.Run("a verify after a readiness turn", func(t *testing.T) {
		f := newFake()
		c := workConfig()
		r := f.workRunner(&fakeAgent{results: []driver.Result{readyResult()}})

		_, err := r.Work(t.Context(), c, readinessWork, noProgress)
		require.NoError(t, err)

		_, err = r.Verify(t.Context(), c)
		require.NoError(t, err)

		assert.Equal(t, []string{"volumes", "up/readonly", goSeed, "up/agent", goSeed, "run/make test"}, f.calls(), "the agent phase starts once")
		assert.Equal(t, sandbox.PhaseAgent, f.run(t, "make test").phase, "the verify runs in the agent phase, never the read-only one")
		assert.Equal(t, f.requests(sandbox.PhaseReadOnly), f.requests(sandbox.PhaseAgent))
	})
}

func TestReadinessResultMapping(t *testing.T) {
	leak := " token " + plantedSetup
	mask := masker(workConfig())

	tests := []struct {
		name string
		out  map[string]any
		want engine.WorkResult
	}{
		{name: "ready", out: map[string]any{"status": "ready"}, want: engine.WorkResult{Outcome: engine.OutcomeReady}},
		{
			name: "blocked with pairs, masked",
			out: map[string]any{"status": "blocked", "questions": []map[string]string{
				{"question": "Which port?" + leak, "suggested_answer": "8080" + leak},
				{"question": "Is IPv6 needed?", "suggested_answer": ""},
			}},
			want: engine.WorkResult{
				Outcome: engine.OutcomeBlocked, Questions: []string{mask("Which port?" + leak), "Is IPv6 needed?"},
				SuggestedAnswers: []string{mask("8080" + leak), ""},
			},
		},
		{name: "blocked without a question", out: map[string]any{"status": "blocked"}, want: engine.WorkResult{Outcome: engine.OutcomeUnfinished}},
		{
			name: "blocked with an empty list", out: map[string]any{"status": "blocked", "questions": []string{}},
			want: engine.WorkResult{Outcome: engine.OutcomeUnfinished},
		},
		{
			name: "questions that are not pairs", out: map[string]any{"status": "blocked", "questions": []string{"Which port?"}},
			want: engine.WorkResult{Outcome: engine.OutcomeUnfinished},
		},
		{name: "a work result", out: map[string]any{"status": "done"}, want: engine.WorkResult{Outcome: engine.OutcomeUnfinished}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFake()
			d := &fakeAgent{results: []driver.Result{{Reason: driver.ReasonDone, Structured: structured(tt.out)}}}

			res, err := f.workRunner(d).Work(t.Context(), workConfig(), readinessWork, noProgress)
			require.NoError(t, err)
			assert.Equal(t, tt.want, res)

			readOnly := f.runsIn(sandbox.PhaseReadOnly)
			require.Len(t, readOnly, 1, "a readiness turn is never frozen")
			assert.Equal(t, goSeed, "run/"+readOnly[0].name, "only the home's seed runs")
		})
	}
}

func TestResumedStartUsesProceed(t *testing.T) {
	c := workConfig()

	for _, tt := range []struct {
		name   string
		w      engine.WorkRequest
		prompt string
	}{
		{"resumed", engine.WorkRequest{Mode: engine.ModeStart, SessionID: sessionUUID, Resume: true}, render(t, prompts.Proceed, nil)},
		{"new", startWork, render(t, prompts.Work, prompts.WorkData{Title: c.Task.Title, Body: c.Task.Body})},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := &fakeAgent{results: []driver.Result{{Reason: driver.ReasonDone}}}

			_, err := newFake().workRunner(d).Work(t.Context(), c, tt.w, noProgress)
			require.NoError(t, err)
			require.Len(t, d.specs, 1)
			assert.Equal(t, tt.prompt, d.specs[0].Prompt)
		})
	}
}

func TestWorkTimeout(t *testing.T) {
	t.Run("the session's time limit", func(t *testing.T) {
		c := workConfig()
		c.Agent.Timeout = 50 * time.Millisecond

		var cause error

		d := &fakeAgent{results: []driver.Result{{Reason: driver.ReasonInterrupted}}, during: func(ctx context.Context, _ func(driver.Event)) {
			<-ctx.Done()
			cause = ctx.Err()
		}}

		started := time.Now()
		res, err := newFake().workRunner(d).Work(t.Context(), c, startWork, noProgress)
		require.NoError(t, err)
		assert.Equal(t, engine.OutcomeTimeout, res.Outcome)
		require.ErrorIs(t, cause, context.DeadlineExceeded, "the driver's context ends")
		assert.GreaterOrEqual(t, time.Since(started), c.Agent.Timeout)
	})

	t.Run("a cancelled call", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		d := &fakeAgent{results: []driver.Result{{Reason: driver.ReasonInterrupted}}, during: func(ctx context.Context, _ func(driver.Event)) {
			cancel()
			<-ctx.Done()
		}}

		_, err := newFake().workRunner(d).Work(ctx, workConfig(), startWork, noProgress)
		require.ErrorIs(t, err, context.Canceled)
	})
}

func TestWorkFaults(t *testing.T) {
	t.Run("a driver that cannot run", func(t *testing.T) {
		d := &fakeAgent{err: errors.New("claudecode: start: exec failed")}

		_, err := newFake().workRunner(d).Work(t.Context(), workConfig(), startWork, noProgress)
		require.ErrorIs(t, err, d.err)
	})

	t.Run("a sidecar that ends during the session", func(t *testing.T) {
		f := newFake()
		d := &fakeAgent{results: []driver.Result{{Reason: driver.ReasonInterrupted}}, during: func(ctx context.Context, _ func(driver.Event)) {
			f.fail("sandbox: the sidecar does not answer: it exited: killed")
			<-ctx.Done()
		}}

		_, err := f.workRunner(d).Work(t.Context(), workConfig(), startWork, noProgress)
		require.Error(t, err)
		require.NotErrorIs(t, err, context.Canceled)
	})
}

// A session the sidecar's end cuts still used what the proxy counted, which
// comes back with the fault for the record.
func TestWorkReturnsUsageWithAFault(t *testing.T) {
	cut := func(f *fakeSandbox) *fakeAgent {
		return &fakeAgent{results: []driver.Result{{Reason: driver.ReasonInterrupted, Turns: 4}}, during: func(ctx context.Context, _ func(driver.Event)) {
			f.send(sidecar.Line{Type: "usage", Usage: &proxy.Usage{Model: "claude-opus", Input: 10, Output: 5}})
			f.send(sidecar.Line{Type: "usage", Usage: &proxy.Usage{Model: "claude-opus", Input: 20, Output: 7, CacheRead: 3}})
			f.fail("sandbox: the sidecar does not answer: it exited: killed")
			<-ctx.Done()
		}}
	}
	want := []store.Usage{{Source: "proxy", Model: "claude-opus", Input: 30, Output: 12, CacheRead: 3}}

	t.Run("work", func(t *testing.T) {
		f := newFake()

		res, err := f.workRunner(cut(f)).Work(t.Context(), workConfig(), startWork, noProgress)
		require.Error(t, err)
		assert.Equal(t, want, res.Usage)
		assert.Equal(t, 4, res.Turns)
	})

	t.Run("review", func(t *testing.T) {
		f := newFake()

		res, err := f.workRunner(cut(f)).Review(t.Context(), workConfig(), fullReview(), noProgress)
		require.Error(t, err)
		assert.Equal(t, want, res.Usage)
		assert.Equal(t, 4, res.Turns)
	})
}

// cappedConfig is workConfig under a money cap that leaves microUSD.
func cappedConfig(microUSD int64) engine.RunConfig {
	c := workConfig()
	c.Budget.Capped, c.Budget.MicroUSD = true, microUSD
	c.Budget.Prices = map[string]engine.Price{"*": {Input: 3, Output: 15, CacheRead: 0.3, CacheWrite: 3.75}}

	return c
}

// The proxy's budget line ends the session in flight at once, and what the
// responses in flight still used is counted.
func TestBudgetLineEndsTheSession(t *testing.T) {
	// cut sends the line with reason during the session, between the usage of
	// two responses, and keeps the cause its context ended with.
	cut := func(f *fakeSandbox, reason string, cause *error) *fakeAgent {
		return &fakeAgent{results: []driver.Result{{Reason: driver.ReasonInterrupted, Turns: 4}}, during: func(ctx context.Context, _ func(driver.Event)) {
			f.send(sidecar.Line{Type: "usage", Usage: &proxy.Usage{Model: "claude-opus", Input: 10, Output: 5}})
			f.send(sidecar.Line{Type: "budget", Error: reason})
			f.send(sidecar.Line{Type: "usage", Usage: &proxy.Usage{Model: "claude-opus", Input: 20, Output: 7}})
			<-ctx.Done()
			*cause = context.Cause(ctx)
		}}
	}
	usage := []store.Usage{{Source: "proxy", Model: "claude-opus", Input: 30, Output: 12}}
	unpriced := `Model "claude-opus" has no price; ` + plantedSetup

	for _, tt := range []struct {
		name, reason, outcome, detail string
	}{
		{name: "the cap is spent", outcome: engine.OutcomeBudget},
		{name: "the spend cannot be counted", reason: unpriced, outcome: engine.OutcomeUnmetered, detail: masker(workConfig())(unpriced)},
	} {
		t.Run("work: "+tt.name, func(t *testing.T) {
			f := newFake()
			c := cappedConfig(1_000_000)

			var cause error

			res, err := f.workRunner(cut(f, tt.reason, &cause)).Work(t.Context(), c, startWork, noProgress)
			require.NoError(t, err)
			require.Error(t, cause, "the driver's context ends")
			require.NotErrorIs(t, cause, errSessionTimeout)
			assert.Equal(t, tt.outcome, res.Outcome)
			assert.Equal(t, usage, res.Usage)
			assert.Equal(t, 4, res.Turns)
			assert.False(t, ran(f, freezeName(c, c.Task.Title)), "nothing is frozen")

			if tt.detail != "" {
				assert.Equal(t, tt.detail, res.Detail)
			}
		})

		t.Run("review: "+tt.name, func(t *testing.T) {
			f := newFake()

			var cause error

			res, err := f.workRunner(cut(f, tt.reason, &cause)).Review(t.Context(), cappedConfig(1_000_000), fullReview(), noProgress)
			require.NoError(t, err)
			require.Error(t, cause, "the driver's context ends")
			assert.Equal(t, tt.outcome, res.Outcome)
			assert.Equal(t, usage, res.Usage)

			if tt.detail != "" {
				assert.Equal(t, tt.detail, res.Detail)
			}
		})
	}

	t.Run("a line after the session's result", func(t *testing.T) {
		f := newFake()
		c := cappedConfig(1_000_000)
		freeze := freezeName(c, c.Task.Title)
		f.results[freeze] = sandbox.Result{Output: headCommit + "\n"}
		f.onRun = func(name string) {
			if name == freeze {
				f.send(sidecar.Line{Type: "budget"})
			}
		}
		r := f.workRunner(&fakeAgent{results: []driver.Result{{Reason: driver.ReasonDone, Structured: structured(map[string]any{"status": "done"})}}})

		res, err := r.Work(t.Context(), c, startWork, noProgress)
		require.NoError(t, err)
		assert.Equal(t, engine.OutcomeDone, res.Outcome)
		assert.Equal(t, headCommit, res.HeadCommit, "the freeze ran to its end")

		_, err = r.Verify(t.Context(), c)
		require.NoError(t, err)

		agent := f.requests(sandbox.PhaseAgent)
		require.Len(t, agent, 2, "the next action's sidecar counts anew")
		assert.NotEqual(t, agent[0].Model.StandIn, agent[1].Model.StandIn)
	})
}

func TestWorkForwardsProgress(t *testing.T) {
	resets := time.Date(2026, 11, 16, 18, 0, 0, 0, time.UTC)
	f := newFake()
	d := &fakeAgent{results: []driver.Result{{Reason: driver.ReasonDone}}, during: func(_ context.Context, sink func(driver.Event)) {
		sink(driver.Event{Kind: driver.KindStarted, SessionID: sessionUUID})
		sink(driver.Event{Kind: driver.KindText, Text: "Reading the form."})
		f.send(sidecar.Line{Type: "denied", Host: "evil.example:443"})
		sink(driver.Event{Kind: driver.KindToolUse, ToolName: "Bash", ToolInput: json.RawMessage(`{"command":"make test"}`)})
		sink(driver.Event{Kind: driver.KindToolResult, ToolName: "Bash", Text: "ok"})
		sink(driver.Event{Kind: driver.KindRateLimit, RateLimit: &driver.RateLimit{Window: "five_hour", Status: "allowed_warning", ResetsAt: resets}})
		sink(driver.Event{Kind: driver.KindUsage, Usage: &driver.Usage{Model: "claude-test", Input: 1}})
	}}

	var got []engine.Progress

	_, err := f.workRunner(d).Work(t.Context(), workConfig(), startWork, func(p engine.Progress) { got = append(got, p) })
	require.NoError(t, err)

	f.send(sidecar.Line{Type: "denied", Host: "late.example:443"})

	assert.Equal(t, []engine.Progress{
		{Type: engine.AgentText, Payload: engine.AgentTextPayload{Text: "Reading the form."}},
		{Type: engine.EgressDenied, Payload: engine.EgressDeniedPayload{Host: "evil.example:443"}},
		{Type: engine.AgentTool, Payload: engine.AgentToolPayload{Name: "Bash", Summary: `{"command":"make test"}`}},
		{Type: engine.RateLimit, Payload: engine.RateLimitPayload{Window: "five_hour", Status: "allowed_warning", ResetsAt: resets}},
	}, got, "nothing is sent after Work returns")
}

func TestWorkUsageSourcePerMode(t *testing.T) {
	totals := []driver.Usage{
		{Model: "claude-haiku", Input: 90, Output: 9, CostEstimateUSD: 0.01},
		{Model: "claude-opus", Input: 900, Output: 90, CostEstimateUSD: 0.75},
	}

	t.Run("key", func(t *testing.T) {
		f := newFake()
		d := &fakeAgent{results: []driver.Result{{Reason: driver.ReasonDone, Usage: totals}}, during: func(context.Context, func(driver.Event)) {
			for _, u := range []sidecar.Line{
				{Type: "usage", Usage: &proxy.Usage{Model: "claude-opus", Input: 10, Output: 5, CacheRead: 1, CacheWrite: 2}},
				{Type: "usage", Usage: &proxy.Usage{Model: "claude-haiku", Input: 3, Output: 1}},
				{Type: "usage", Usage: &proxy.Usage{Model: "claude-opus", Input: 20, Output: 7}},
			} {
				f.send(u)
			}
		}}

		res, err := f.workRunner(d).Work(t.Context(), workConfig(), startWork, noProgress)
		require.NoError(t, err)
		assert.Equal(t, []store.Usage{
			{Source: "proxy", Model: "claude-haiku", Input: 3, Output: 1, CostEstimateUSD: 0.01},
			{Source: "proxy", Model: "claude-opus", Input: 30, Output: 12, CacheRead: 1, CacheWrite: 2, CostEstimateUSD: 0.75},
		}, res.Usage)
	})

	t.Run("subscription", func(t *testing.T) {
		c := workConfig()
		c.Endpoint.Mode = "subscription"
		d := &fakeAgent{results: []driver.Result{{Reason: driver.ReasonDone, Usage: totals}}}

		res, err := newFake().workRunner(d).Work(t.Context(), c, startWork, noProgress)
		require.NoError(t, err)
		assert.Equal(t, []store.Usage{
			{Source: "agent", Model: "claude-haiku", Input: 90, Output: 9, CostEstimateUSD: 0.01},
			{Source: "agent", Model: "claude-opus", Input: 900, Output: 90, CostEstimateUSD: 0.75},
		}, res.Usage)
	})
}

func TestWorkOutputIsRedacted(t *testing.T) {
	leak := "token " + plantedSetup + " key " + plantedEndpoint + " forge " + plantedForge

	f := newFake()
	d := &fakeAgent{results: []driver.Result{{Reason: driver.ReasonDone, Structured: structured(map[string]any{
		"status": "blocked", "title": leak, "summary": leak, "assumptions": []string{leak}, "questions": []string{leak}, "where_to_look": []string{leak},
	})}}, during: func(_ context.Context, sink func(driver.Event)) {
		sink(driver.Event{Kind: driver.KindText, Text: leak})
		sink(driver.Event{Kind: driver.KindToolUse, ToolName: "Bash", ToolInput: json.RawMessage(`"` + leak + `"`)})
	}}

	var progress []string

	res, err := f.workRunner(d).Work(t.Context(), workConfig(), startWork, func(p engine.Progress) {
		raw, _ := json.Marshal(p.Payload)
		progress = append(progress, string(raw))
	})
	require.NoError(t, err)

	raw, err := json.Marshal(res)
	require.NoError(t, err)
	assertMasked(t, string(raw))
	assertMasked(t, strings.Join(progress, "\n"))
}

// echoAgent repeats the token its session holds in every text it hands back,
// as an agent that read its own environment would.
type echoAgent struct {
	fakeDriver

	fail  bool
	token string
}

func (d *echoAgent) Run(_ context.Context, _ proc.Target, s driver.Spec, sink func(driver.Event)) (driver.Result, error) {
	d.token = s.Auth.Token

	sink(driver.Event{Kind: driver.KindText, Text: d.token})
	sink(driver.Event{Kind: driver.KindToolUse, ToolName: d.token, ToolInput: json.RawMessage(`"` + d.token + `"`)})
	sink(driver.Event{Kind: driver.KindRateLimit, RateLimit: &driver.RateLimit{Window: d.token, Status: d.token}})

	if d.fail {
		return driver.Result{}, errors.New(d.token)
	}

	return driver.Result{Reason: driver.ReasonCrashed, IsError: true, Detail: d.token, Usage: []driver.Usage{{Model: d.token, Input: 1}}}, nil
}

// The runner's mask is the one that knows the token the agent holds; in key
// mode that is the run's stand-in, which no vault holds.
func TestWorkMasksTheAgentsToken(t *testing.T) {
	for _, tc := range []struct {
		name, mode string
		fail       bool
	}{
		{"key mode, a result", "key", false},
		{"key mode, an error from the driver", "key", true},
		{"subscription mode, a result", "subscription", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := workConfig()
			c.Endpoint.Mode = tc.mode
			d := &echoAgent{fail: tc.fail}
			r := newFake().runner()
			r.driver = d

			var out []string

			res, err := r.Work(t.Context(), c, startWork, func(p engine.Progress) {
				raw, _ := json.Marshal(p.Payload)
				out = append(out, string(raw))
			})
			require.Len(t, out, 3, "the text, the tool use and the rate limit")

			if tc.fail {
				require.Error(t, err)
				out = append(out, err.Error())
			} else {
				require.NoError(t, err)
				assert.NotEmpty(t, res.Detail)

				raw, err := json.Marshal(res)
				require.NoError(t, err)

				out = append(out, string(raw))
			}

			require.NotEmpty(t, d.token)
			assert.NotContains(t, strings.Join(out, "\n"), d.token)
		})
	}
}

// Every session gets the instance's files, the read-only readiness turn and
// review as well as a work session.
func TestSessionsCarryTheInstanceFiles(t *testing.T) {
	c := workConfig()
	c.Agent.Instance.Instructions = "INSTANCE-INSTRUCTIONS-4c1d"
	c.Agent.Instance.Skills = map[string]engine.File{
		"one/SKILL.md":       {Data: []byte("one\n")},
		"one/scripts/run.sh": {Data: []byte("echo run\n"), Executable: true},
	}

	want := map[string]driver.File{
		"one/SKILL.md":       {Data: []byte("one\n")},
		"one/scripts/run.sh": {Data: []byte("echo run\n"), Executable: true},
	}

	for name, tc := range map[string]struct {
		result  driver.Result
		session func(ctx context.Context, r *Runner) error
	}{
		"work": {driver.Result{Reason: driver.ReasonDone}, func(ctx context.Context, r *Runner) error {
			_, err := r.Work(ctx, c, startWork, noProgress)

			return err
		}},
		"readiness": {readyResult(), func(ctx context.Context, r *Runner) error {
			_, err := r.Work(ctx, c, readinessWork, noProgress)

			return err
		}},
		"review": {passing(), func(ctx context.Context, r *Runner) error {
			_, err := r.Review(ctx, c, fullReview(), noProgress)

			return err
		}},
	} {
		t.Run(name, func(t *testing.T) {
			d := &fakeAgent{results: []driver.Result{tc.result}}
			require.NoError(t, tc.session(t.Context(), newFake().workRunner(d)))
			require.Len(t, d.specs, 1)
			assert.Equal(t, c.Agent.Instance.Instructions, d.specs[0].InstanceInstructions)
			assert.Equal(t, want, d.specs[0].InstanceSkills)
		})
	}
}

// Every session gets the task's images as files outside the workspace, in
// place before it starts, a resumed one too, whose container may not be the
// one an earlier session read them in; a prompt that carries the task names
// the paths they were copied to.
func TestImagesStayOutOfTheWorkspace(t *testing.T) {
	c := workConfig()
	c.Task.Images = []engine.TaskImage{{Name: "save-button.png", Data: []byte("png-1")}, {Name: "save-button.png", Data: []byte("png-2")}}
	want := map[string][]byte{"/run/cavu/images/1-save-button.png": []byte("png-1"), "/run/cavu/images/2-save-button.png": []byte("png-2")}

	work := func(w engine.WorkRequest) func(ctx context.Context, r *Runner) error {
		return func(ctx context.Context, r *Runner) error {
			_, err := r.Work(ctx, c, w, noProgress)

			return err
		}
	}

	review := func(rq engine.ReviewRequest) func(ctx context.Context, r *Runner) error {
		return func(ctx context.Context, r *Runner) error {
			_, err := r.Review(ctx, c, rq, noProgress)

			return err
		}
	}

	for _, tc := range []struct {
		name    string
		session func(ctx context.Context, r *Runner) error
		named   bool // the prompt carries the task
	}{
		{"readiness", work(readinessWork), true},
		{"work", work(startWork), true},
		{"the work after a readiness turn", work(engine.WorkRequest{Mode: engine.ModeStart, SessionID: sessionUUID, Resume: true}), false},
		{"the fix after a review", work(engine.WorkRequest{Mode: engine.ModeFixReview, SessionID: sessionUUID, Findings: []engine.Finding{trimmed}}), true},
		{"review", review(fullReview()), true},
		{"scoped review", review(scopedReview()), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := reviewed()

			var before map[string][]byte

			d := &fakeAgent{results: []driver.Result{{Reason: driver.ReasonDone}}, during: func(context.Context, func(driver.Event)) {
				f.mu.Lock()
				before = maps.Clone(f.copied)
				f.mu.Unlock()
			}}

			require.NoError(t, tc.session(t.Context(), f.workRunner(d)))
			require.Len(t, d.specs, 1)

			got := map[string][]byte{}

			for p, data := range before {
				if slices.ContainsFunc(c.Task.Images, func(img engine.TaskImage) bool { return bytes.Equal(img.Data, data) }) {
					got[p] = data
				}
			}

			assert.Equal(t, want, got, "each image is in place before the session, at its path and nowhere else")

			for p := range want {
				assert.Equal(t, tc.named, strings.Contains(d.specs[0].Prompt, p), p)
			}
		})
	}
}
