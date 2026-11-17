package runner

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/cavu/internal/driver"
	"github.com/mhersson/cavu/internal/engine"
	"github.com/mhersson/cavu/internal/proc"
	"github.com/mhersson/cavu/internal/prompts"
	"github.com/mhersson/cavu/internal/sandbox"
)

const (
	reviewUUID = "7ca7b810-9dad-11d1-80b4-00c04fd430c8"
	fixBase    = "456789abcdef0123456789abcdef0123456789ab"
)

// The files a review's diffs are written to, in the read-only container.
const (
	wantReviewPatch = "/run/cavu/review.patch"
	wantFixPatch    = "/run/cavu/fix.patch"
)

var (
	trimmed = engine.Finding{Severity: engine.SeverityCritical, Where: "login.go:12", What: "The password is trimmed.", Why: "Valid passwords fail."}
	untried = engine.Finding{Severity: engine.SeverityImportant, Where: "login_test.go", What: "No test has a space.", Why: "The fix is unproven."}
	wording = engine.Finding{Severity: engine.SeverityMinor, Where: "README.md", What: "A typo.", Why: "It reads badly."}
)

func fullReview() engine.ReviewRequest {
	return engine.ReviewRequest{
		SessionID: reviewUUID, Head: headCommit, Answers: []string{"The admin form."}, Assumptions: []string{"Spaces are typos."},
		Summary: "Trimmed the password.",
	}
}

func scopedReview() engine.ReviewRequest {
	r := fullReview()
	r.FixBase, r.Findings = fixBase, []engine.Finding{trimmed, untried}

	return r
}

func passing() driver.Result {
	return driver.Result{Reason: driver.ReasonDone, Structured: structured(map[string]any{
		"asked":    map[string]any{"pass": true, "reason": "It trims nothing now."},
		"built":    map[string]any{"pass": true, "reason": "Small and tested."},
		"findings": []any{},
	})}
}

// numbered is findings as the prompts number them.
func numbered(findings ...engine.Finding) []prompts.ReviewFinding {
	out := make([]prompts.ReviewFinding, len(findings))
	for i, f := range findings {
		out[i] = prompts.ReviewFinding{Number: i + 1, Severity: f.Severity, Where: f.Where, What: f.What, Why: f.Why}
	}

	return out
}

// diffName is how the fake names the writing of the diffs args name.
func diffName(args ...string) string {
	return stepName(scriptArgv(diffScript, args...))
}

func asMap(f engine.Finding) map[string]any {
	return map[string]any{"severity": f.Severity, "where": f.Where, "what": f.What, "why": f.Why}
}

// The diffs are written by one script in the read-only container, wrapped
// so it runs only in a sandbox, with git's diff options of the export and no
// binary patch, and every commit and path as an argument.
func TestReviewWritesTheDiffFiles(t *testing.T) {
	for _, tt := range []struct {
		name string
		r    engine.ReviewRequest
		args []string
	}{
		{name: "full", r: fullReview(), args: []string{baseCommit, headCommit, wantReviewPatch}},
		{
			name: "scoped", r: scopedReview(),
			args: []string{baseCommit, headCommit, wantReviewPatch, fixBase, headCommit, wantFixPatch},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newFake()

			_, err := f.workRunner(&fakeAgent{results: []driver.Result{passing()}}).Review(t.Context(), workConfig(), tt.r, noProgress)
			require.NoError(t, err)

			runs := f.runsIn(sandbox.PhaseReadOnly)
			require.Len(t, runs, 1)

			cmd := runs[0].cmd
			script := cmd.Argv[2]
			assert.True(t, strings.HasPrefix(script, proc.SandboxOnly("")), "the script runs only in a sandbox")

			for _, opt := range []string{"--no-ext-diff", "--no-textconv", "--ignore-submodules=none"} {
				assert.Contains(t, script, opt)
				assert.Contains(t, exportScript, opt, "the export's own option")
			}

			assert.NotContains(t, script, "--binary")
			assert.NotContains(t, script, "/run/cavu", "the paths go as arguments")
			assert.Equal(t, tt.args, cmd.Argv[4:])
			assert.Equal(t, workspace, cmd.Dir)
			assert.Equal(t, agentEnv([]string{"go"}, baseCommit), cmd.Env, "the export's environment")
		})
	}
}

func TestReviewSpec(t *testing.T) {
	c := workConfig()
	c.Endpoint.Mode = "subscription"
	c.Agent.Review.Model, c.Agent.Review.Effort = "claude-review", "max"
	c.Agent.Strong.Model, c.Agent.Strong.Effort = "claude-strong", "xhigh"

	for _, tt := range []struct {
		name   string
		r      engine.ReviewRequest
		diff   []string
		prompt string
		schema []byte
	}{
		{
			name: "full", r: fullReview(), schema: prompts.ReviewSchema(), diff: []string{baseCommit, headCommit, wantReviewPatch},
			prompt: render(t, prompts.Review, prompts.ReviewData{
				Title: c.Task.Title, Body: c.Task.Body, Answers: []string{"The admin form."}, Assumptions: []string{"Spaces are typos."},
				Summary: "Trimmed the password.", DiffFile: wantReviewPatch,
			}),
		},
		{
			name: "scoped", r: scopedReview(), schema: prompts.RereviewSchema(),
			diff: []string{baseCommit, headCommit, wantReviewPatch, fixBase, headCommit, wantFixPatch},
			prompt: render(t, prompts.Rereview, prompts.RereviewData{
				Title: c.Task.Title, Body: c.Task.Body, Findings: numbered(trimmed, untried), DiffFile: wantReviewPatch, FixDiffFile: wantFixPatch,
			}),
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newFake()
			d := &fakeAgent{results: []driver.Result{passing()}}

			_, err := f.workRunner(d).Review(t.Context(), c, tt.r, noProgress)
			require.NoError(t, err)
			require.Len(t, d.specs, 1)

			assert.Equal(t, []string{"volumes", "up/readonly", "run/" + diffName(tt.diff...)}, f.calls(), "the read-only phase, and the diffs written in it")

			// No instructions: a read-only session's prompt says all it needs.
			assert.Equal(t, driver.Spec{
				Auth:  driver.Auth{Mode: "subscription", Token: plantedEndpoint},
				Model: "claude-review", Effort: "max", MaxTurns: 50, SessionID: reviewUUID,
				Prompt: tt.prompt, OutputSchema: tt.schema,
				Dir: workspace, BaseCommit: baseCommit, Home: home, ConfigDir: "/home/agent/.claude-cavu",
				Env: withProjectVars(sessionEnv([]string{"go"}, baseCommit), c), ReadOnly: true,
			}, d.specs[0])
		})
	}
}

func TestReviewResultMapping(t *testing.T) {
	resets := time.Date(2026, 11, 17, 18, 0, 0, 0, time.UTC)
	leak := " token " + plantedSetup
	mask := masker(workConfig())

	review := func(asked, built bool, findings ...map[string]any) driver.Result {
		return driver.Result{Reason: driver.ReasonDone, Turns: 4, CostEstimateUSD: 0.25, Structured: structured(map[string]any{
			"asked": map[string]any{"pass": asked, "reason": "asked-reason"}, "built": map[string]any{"pass": built, "reason": "built-reason"},
			"findings": findings,
		})}
	}

	leaky := asMap(untried)
	leaky["where"], leaky["what"], leaky["why"] = "where"+leak, "what"+leak, "why"+leak
	leakyReview := review(false, true, leaky)
	leakyReview.Structured = structured(map[string]any{
		"asked":    map[string]any{"pass": false, "reason": "asked" + leak},
		"built":    map[string]any{"pass": true, "reason": "built" + leak},
		"findings": []any{leaky},
	})

	badSeverity := asMap(trimmed)
	badSeverity["severity"] = "blocker"

	tests := []struct {
		name    string
		r       engine.ReviewRequest
		res     driver.Result
		timeout bool // the session runs past its time limit
		want    engine.ReviewResult
	}{
		{
			name: "a full review with three findings", r: fullReview(), res: review(false, true, asMap(trimmed), asMap(untried), asMap(wording)),
			want: engine.ReviewResult{
				Outcome: engine.ReviewReviewed, Asked: engine.Verdict{Reason: "asked-reason"}, Built: engine.Verdict{Pass: true, Reason: "built-reason"},
				Findings: []engine.Finding{trimmed, untried, wording}, Turns: 4, CostEstimateUSD: 0.25,
			},
		},
		{
			name: "a result holding a stored secret, masked", r: fullReview(), res: leakyReview,
			want: engine.ReviewResult{
				Outcome: engine.ReviewReviewed, Asked: engine.Verdict{Reason: mask("asked" + leak)}, Built: engine.Verdict{Pass: true, Reason: mask("built" + leak)},
				Findings: []engine.Finding{{Severity: untried.Severity, Where: mask("where" + leak), What: mask("what" + leak), Why: mask("why" + leak)}},
				Turns:    4, CostEstimateUSD: 0.25,
			},
		},
		{
			name: "scoped verdicts placed by their number", r: scopedReview(),
			res: driver.Result{Reason: driver.ReasonDone, Turns: 2, Structured: structured(map[string]any{
				"verdicts": []map[string]any{
					{"finding": 2, "addressed": true, "evidence": "login_test.go:30"},
					{"finding": 1, "addressed": false, "evidence": "still trimmed"},
					{"finding": 9, "addressed": true, "evidence": "no such finding"},
				},
				"breakage": []any{asMap(wording)},
			})},
			want: engine.ReviewResult{Outcome: engine.ReviewReviewed, Addressed: []bool{false, true}, Findings: []engine.Finding{wording}, Turns: 2},
		},
		{
			name: "a scoped finding without a verdict", r: scopedReview(),
			res: driver.Result{Reason: driver.ReasonDone, Structured: structured(map[string]any{
				"verdicts": []map[string]any{{"finding": 1, "addressed": true, "evidence": "fixed"}}, "breakage": []any{},
			})},
			want: engine.ReviewResult{Outcome: engine.ReviewReviewed, Addressed: []bool{true, false}, Findings: []engine.Finding{}},
		},
		{
			name: "no structured result", r: fullReview(), res: driver.Result{Reason: driver.ReasonDone, Text: "Looks fine."},
			want: engine.ReviewResult{Outcome: engine.ReviewFailed},
		},
		{
			name: "a full result without its verdicts", r: fullReview(),
			res:  driver.Result{Reason: driver.ReasonDone, Structured: structured(map[string]any{"findings": []any{}})},
			want: engine.ReviewResult{Outcome: engine.ReviewFailed},
		},
		{
			name: "a scoped result without its verdicts", r: scopedReview(),
			res:  driver.Result{Reason: driver.ReasonDone, Structured: structured(map[string]any{"breakage": []any{}})},
			want: engine.ReviewResult{Outcome: engine.ReviewFailed},
		},
		{
			name: "a severity outside the three", r: fullReview(), res: review(true, true, badSeverity),
			want: engine.ReviewResult{Outcome: engine.ReviewFailed, Turns: 4, CostEstimateUSD: 0.25},
		},
		{
			name: "a crashed session, with its detail and without a cost", r: fullReview(),
			res:  driver.Result{Reason: driver.ReasonCrashed, Detail: "exit 1" + leak, CostMissing: true},
			want: engine.ReviewResult{Outcome: engine.ReviewFailed, Detail: mask("exit 1" + leak), CostMissing: true},
		},
		{
			name: "a usage limit with its reset", r: fullReview(),
			res:  driver.Result{Reason: driver.ReasonUsageLimit, Detail: "429 usage", RateLimit: &driver.RateLimit{ResetsAt: resets}},
			want: engine.ReviewResult{Outcome: engine.OutcomeUsageLimit, Detail: "429 usage", ResetsAt: resets},
		},
		{
			name: "an endpoint failure", r: fullReview(), res: driver.Result{Reason: driver.ReasonEndpoint, Detail: "502"},
			want: engine.ReviewResult{Outcome: engine.OutcomeEndpoint, Detail: "502"},
		},
		{
			name: "an authentication failure", r: fullReview(), res: driver.Result{Reason: driver.ReasonAuth, Detail: "401"},
			want: engine.ReviewResult{Outcome: engine.OutcomeEndpoint, Detail: "401"},
		},
		{
			name: "used-up turns", r: fullReview(), res: driver.Result{Reason: driver.ReasonMaxTurns, Turns: 50},
			want: engine.ReviewResult{Outcome: engine.OutcomeMaxTurns, Turns: 50},
		},
		{
			name: "a timeout", r: fullReview(), res: driver.Result{Reason: driver.ReasonInterrupted}, timeout: true,
			want: engine.ReviewResult{Outcome: engine.OutcomeTimeout},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := workConfig()
			d := &fakeAgent{results: []driver.Result{tt.res}}

			if tt.timeout {
				c.Agent.Timeout = 20 * time.Millisecond
				d.during = func(ctx context.Context, _ func(driver.Event)) { <-ctx.Done() }
			}

			res, err := newFake().workRunner(d).Review(t.Context(), c, tt.r, noProgress)
			require.NoError(t, err)
			assert.Equal(t, tt.want, res)
		})
	}
}

// A diff the script cannot write leaves the review without a verdict, which
// never blocks the task.
func TestReviewWithoutItsDiff(t *testing.T) {
	f := newFake()
	c := workConfig()
	f.results[diffName(c.Repo.BaseCommit, headCommit, wantReviewPatch)] = sandbox.Result{ExitCode: 128, Output: "fatal: bad object" + plantedSetup + "\n"}
	d := &fakeAgent{results: []driver.Result{passing()}}

	res, err := f.workRunner(d).Review(t.Context(), c, fullReview(), noProgress)
	require.NoError(t, err)
	assert.Equal(t, engine.ReviewResult{Outcome: engine.ReviewFailed, Detail: masker(c)("fatal: bad object" + plantedSetup)}, res)
	assert.Empty(t, d.specs, "no session runs without the diff")
}

func TestReviewChangesNothing(t *testing.T) {
	for _, r := range []engine.ReviewRequest{fullReview(), scopedReview()} {
		f := newFake()
		d := &fakeAgent{results: []driver.Result{passing()}}

		_, err := f.workRunner(d).Review(t.Context(), workConfig(), r, noProgress)
		require.NoError(t, err)

		runs := f.runsIn(sandbox.PhaseReadOnly)
		require.Len(t, runs, 1, "the diffs are the one command")
		assert.Empty(t, f.runsIn(sandbox.PhaseAgent), "no freeze, and nothing in the agent phase")
		assert.NotContains(t, f.calls(), "restart")

		// Every third argument is a file the script writes.
		args := runs[0].cmd.Argv[4:]
		for i := 2; i < len(args); i += 3 {
			assert.True(t, strings.HasPrefix(args[i], "/run/cavu/"), args[i])
		}
	}
}

func TestReviewForwardsProgressMasked(t *testing.T) {
	leak := "token " + plantedSetup
	d := &fakeAgent{results: []driver.Result{passing()}, during: func(_ context.Context, sink func(driver.Event)) {
		sink(driver.Event{Kind: driver.KindText, Text: "Reading the diff " + leak})
	}}

	var got []engine.Progress

	_, err := newFake().workRunner(d).Review(t.Context(), workConfig(), fullReview(), func(p engine.Progress) { got = append(got, p) })
	require.NoError(t, err)
	assert.Equal(t, []engine.Progress{
		{Type: engine.AgentText, Payload: engine.AgentTextPayload{Text: masker(workConfig())("Reading the diff " + leak)}},
	}, got)
}

func TestFixReviewSessions(t *testing.T) {
	c := workConfig()
	c.Endpoint.Mode = "subscription"
	c.Agent.Strong.Model, c.Agent.Strong.Effort = "claude-strong", "xhigh"
	findings := []engine.Finding{trimmed, untried}

	t.Run("resumed", func(t *testing.T) {
		f := newFake()
		d := &fakeAgent{results: []driver.Result{{Reason: driver.ReasonDone}}}
		w := engine.WorkRequest{Mode: engine.ModeFixReview, SessionID: sessionUUID, Resume: true, Role: engine.RoleWork, Findings: findings}

		_, err := f.workRunner(d).Work(t.Context(), c, w, noProgress)
		require.NoError(t, err)
		require.Len(t, d.specs, 1)

		spec := d.specs[0]
		assert.Equal(t, sessionUUID, spec.Resume)
		assert.Empty(t, spec.SessionID)
		assert.Equal(t, []string{"claude-test", "high"}, []string{spec.Model, spec.Effort})
		assert.Equal(t, render(t, prompts.FixReview, prompts.FixReviewData{Findings: numbered(trimmed, untried)}), spec.Prompt)
		assert.Equal(t, render(t, prompts.Instructions, nil), spec.Instructions)
		assert.Empty(t, f.runsIn(sandbox.PhaseAgent), "the resumed session has the diff in its history")

		for _, x := range findings {
			for _, v := range []string{x.Where, x.What, x.Why} {
				assert.Contains(t, spec.Prompt, v)
			}
		}
	})

	t.Run("fresh", func(t *testing.T) {
		f := newFake()
		diff := diffName(c.Repo.BaseCommit, "refs/heads/"+c.Repo.Branch, wantReviewPatch)

		var before []string

		d := &fakeAgent{results: []driver.Result{{Reason: driver.ReasonDone}}, during: func(context.Context, func(driver.Event)) {
			before = f.calls()
		}}
		w := engine.WorkRequest{Mode: engine.ModeFixReview, SessionID: sessionUUID, Role: engine.RoleStrong, Findings: findings}

		_, err := f.workRunner(d).Work(t.Context(), c, w, noProgress)
		require.NoError(t, err)
		require.Len(t, d.specs, 1)
		assert.Equal(t, []string{"volumes", "up/agent", "run/" + diff}, before, "the diff of the task branch is written before the session")

		run := f.run(t, diff)
		assert.Equal(t, sandbox.PhaseAgent, run.phase)
		assert.True(t, strings.HasPrefix(run.cmd.Argv[2], proc.SandboxOnly("")), "the script runs only in a sandbox")

		spec := d.specs[0]
		assert.Equal(t, sessionUUID, spec.SessionID)
		assert.Empty(t, spec.Resume)
		assert.Equal(t, []string{"claude-strong", "xhigh"}, []string{spec.Model, spec.Effort})
		assert.Equal(t, render(t, prompts.FixReviewFresh, prompts.FixReviewFreshData{
			Title: c.Task.Title, Body: c.Task.Body, Findings: numbered(trimmed, untried), DiffFile: wantReviewPatch,
		}), spec.Prompt)
		assert.Equal(t, render(t, prompts.Instructions, nil), spec.Instructions)
		assert.Equal(t, []byte(prompts.ResultSchema()), []byte(spec.OutputSchema))
	})

	t.Run("fresh without its diff", func(t *testing.T) {
		f := newFake()
		f.results[diffName(c.Repo.BaseCommit, "refs/heads/"+c.Repo.Branch, wantReviewPatch)] = sandbox.Result{ExitCode: 128, Output: "fatal: bad ref\n"}
		d := &fakeAgent{results: []driver.Result{{Reason: driver.ReasonDone}}}
		w := engine.WorkRequest{Mode: engine.ModeFixReview, SessionID: sessionUUID, Role: engine.RoleStrong, Findings: findings}

		res, err := f.workRunner(d).Work(t.Context(), c, w, noProgress)
		require.NoError(t, err)
		assert.Equal(t, engine.WorkResult{Outcome: engine.OutcomeFailed, Summary: "fatal: bad ref", Detail: "fatal: bad ref"}, res)
		assert.Empty(t, d.specs)
	})
}

// The role of a session picks its model and effort.
func TestRolePicksTheModel(t *testing.T) {
	c := workConfig()
	c.Agent.Review.Model, c.Agent.Review.Effort = "claude-review", "max"
	c.Agent.Strong.Model, c.Agent.Strong.Effort = "claude-strong", "xhigh"

	for role, want := range map[string][]string{
		engine.RoleWork: {"claude-test", "high"}, engine.RoleStrong: {"claude-strong", "xhigh"},
	} {
		d := &fakeAgent{results: []driver.Result{{Reason: driver.ReasonDone}}}

		_, err := newFake().workRunner(d).Work(t.Context(), c, engine.WorkRequest{Mode: engine.ModeContinue, SessionID: sessionUUID, Resume: true, Role: role}, noProgress)
		require.NoError(t, err)
		require.Len(t, d.specs, 1)
		assert.Equal(t, want, []string{d.specs[0].Model, d.specs[0].Effort}, role)
	}
}
