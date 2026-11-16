package runner

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/cavu/internal/driver"
	"github.com/mhersson/cavu/internal/engine"
	"github.com/mhersson/cavu/internal/proc"
	"github.com/mhersson/cavu/internal/prompts"
	"github.com/mhersson/cavu/internal/proxy"
	"github.com/mhersson/cavu/internal/sandbox"
	"github.com/mhersson/cavu/internal/sidecar"
	"github.com/mhersson/cavu/internal/store"
)

const (
	sessionUUID = "6ba7b810-9dad-11d1-80b4-00c04fd430c8"
	headCommit  = "89abcdef0123456789abcdef0123456789abcdef"
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
	return stepName(scriptArgv(freezeScript, c.Repo.BaseCommit, c.Repo.Branch, message))
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
	})
	doneResult := driver.Result{Reason: driver.ReasonDone, SessionID: "agent-id", Turns: 7, CostEstimateUSD: 0.5, Structured: done}
	doneWork := engine.WorkResult{
		Outcome: engine.OutcomeDone, Title: "Fix the login form", Summary: "Trimmed the password.",
		Assumptions: []string{"Spaces are typos."}, Turns: 7, CostEstimateUSD: 0.5,
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
			name: "endpoint",
			res:  driver.Result{Reason: driver.ReasonEndpoint, Detail: "502"},
			want: engine.WorkResult{Outcome: engine.OutcomeEndpoint, Detail: "502"},
		},
		{
			name: "crashed",
			res:  driver.Result{Reason: driver.ReasonCrashed, Detail: "exit 1"},
			want: engine.WorkResult{Outcome: engine.OutcomeCrashed, Detail: "exit 1"},
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
// only in a sandbox, with the base, the branch and the message as arguments.
func TestWorkFreezeScript(t *testing.T) {
	for _, tt := range []struct{ name, title, message string }{
		{name: "the result's title", title: "Fix the login form", message: "Fix the login form"},
		{name: "the task's title", message: "Fix login"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newFake()
			c := workConfig()
			f.results[freezeName(c, tt.message)] = sandbox.Result{Output: headCommit + "\n"}
			d := &fakeAgent{results: []driver.Result{{Reason: driver.ReasonDone, Structured: structured(map[string]any{"status": "done", "title": tt.title})}}}

			res, err := f.workRunner(d).Work(t.Context(), c, startWork, noProgress)
			require.NoError(t, err)
			assert.Equal(t, headCommit, res.HeadCommit)

			run := f.run(t, freezeName(c, tt.message))
			assert.Equal(t, sandbox.PhaseAgent, run.phase)
			assert.Equal(t, workspace, run.cmd.Dir)
			assert.True(t, strings.HasPrefix(run.cmd.Argv[2], proc.SandboxOnly("")), "the script runs only in a sandbox")
			assert.Equal(t, []string{baseCommit, "cavu/fix-login", tt.message}, run.cmd.Argv[4:])
		})
	}
}

func TestWorkSpec(t *testing.T) {
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
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFake()
			c := workConfig()
			c.Endpoint.Mode = "subscription"
			c.Agent.WebTools = true
			d := &fakeAgent{results: []driver.Result{{Reason: driver.ReasonDone}}}

			_, err := f.workRunner(d).Work(t.Context(), c, tt.w, noProgress)
			require.NoError(t, err)
			require.Len(t, d.specs, 1)

			want := driver.Spec{
				Auth:  driver.Auth{Mode: "subscription", Token: plantedEndpoint},
				Model: "claude-test", Effort: "high", MaxTurns: 50,
				Prompt: tt.prompt(t, c), Instructions: render(t, prompts.Instructions, nil), OutputSchema: prompts.ResultSchema(),
				Dir: workspace, BaseCommit: baseCommit, Home: home, ConfigDir: "/home/agent/.claude-cavu",
				Env: agentEnv([]string{"go"}), WebTools: true,
			}
			if tt.w.Resume {
				want.Resume = sessionUUID
			} else {
				want.SessionID = sessionUUID
			}

			assert.Equal(t, want, d.specs[0])
		})
	}
}

func TestWorkSpecCarriesNoRealCredential(t *testing.T) {
	f := newFake()
	d := &fakeAgent{results: []driver.Result{{Reason: driver.ReasonDone}}}

	_, err := f.workRunner(d).Work(t.Context(), workConfig(), startWork, noProgress)
	require.NoError(t, err)
	require.Len(t, d.specs, 1)

	standIn := f.requests(sandbox.PhaseAgent)[0].Model.StandIn
	assert.Equal(t, driver.Auth{Mode: "key", Token: standIn, BaseURL: bridgeURL}, d.specs[0].Auth)

	raw, err := json.Marshal(d.specs[0])
	require.NoError(t, err)

	for _, secret := range []string{plantedEndpoint, plantedForge, plantedSetup} {
		assert.NotContains(t, string(raw), secret)
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
		"status": "blocked", "title": leak, "summary": leak, "assumptions": []string{leak}, "questions": []string{leak},
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
