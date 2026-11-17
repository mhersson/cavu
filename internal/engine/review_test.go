package engine_test

import (
	"encoding/json"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/mhersson/cavu/internal/engine"
	"github.com/mhersson/cavu/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	critical  = engine.Finding{Severity: engine.SeverityCritical, Where: "login.go:12", What: "The password is logged.", Why: "Logs are widely read."}
	important = engine.Finding{Severity: engine.SeverityImportant, Where: "login.go:30", What: "No test covers a wrong password.", Why: "The error path can regress."}
	minor     = engine.Finding{Severity: engine.SeverityMinor, Where: "login.go:4", What: "A comment is stale.", Why: "It misleads a reader."}
	pass      = engine.Verdict{Pass: true}

	reviewing     = ev(engine.ReviewStarted, `{"session_id":"r1","head":"def456"}`)
	autoFixReview = ev(engine.SessionStarted, `{"mode":"fix_review","session_id":"s1","resume":true,"automatic":true,"role":"work"}`)
	fixedFed      = ev(engine.SessionEnded, `{"outcome":"done","changed":true,"head_commit":"fed789"}`)
	reviewingFix  = ev(engine.ReviewStarted, `{"session_id":"r2","scoped":true,"head":"fed789"}`)

	// found is a review that found one important finding.
	found = engine.ReviewResult{Outcome: engine.ReviewReviewed, Asked: pass, Built: pass, Findings: []engine.Finding{important}}
)

// reviewEnd is a review_ended event with p as its payload.
func reviewEnd(t *testing.T, p engine.ReviewEndedPayload) store.Event {
	t.Helper()

	raw, err := json.Marshal(p)
	require.NoError(t, err)

	return store.Event{Type: engine.ReviewEnded, Payload: raw}
}

func stopEv(t *testing.T, p engine.StoppedPayload) store.Event {
	t.Helper()

	raw, err := json.Marshal(p)
	require.NoError(t, err)

	return store.Event{Type: engine.Stopped, Payload: raw}
}

// fullReview is a full review of def456 with both verdicts passed.
func fullReview(t *testing.T, findings ...engine.Finding) store.Event {
	t.Helper()

	return reviewEnd(t, engine.ReviewEndedPayload{Outcome: engine.ReviewReviewed, Head: "def456", Asked: pass, Built: pass, Findings: findings})
}

// reviewedRun is a run whose session froze def456 and passed verify, followed by more.
func reviewedRun(more ...store.Event) []store.Event {
	return slices.Concat(preparedRun(firstSession, doneChanged, verifying, verifyPassed), more)
}

// fixRound is an automatic fix session that froze fed789, its verify, and the
// scoped review that ruled on the open findings and found breakage.
func fixRound(t *testing.T, addressed []bool, breakage ...engine.Finding) []store.Event {
	t.Helper()

	return []store.Event{autoFixReview, fixedFed, verifying, verifyPassed, reviewingFix, reviewEnd(t, engine.ReviewEndedPayload{
		Outcome: engine.ReviewReviewed, Head: "fed789", Scoped: true, Addressed: addressed, Findings: breakage,
	})}
}

func TestReduceReview(t *testing.T) {
	resets := time.Date(2030, 1, 1, 13, 0, 0, 0, time.UTC)
	asked := engine.Verdict{Reason: "Logging out is not handled."}
	breakage := engine.Finding{Severity: engine.SeverityImportant, Where: "login.go:31", What: "The fix broke the lockout.", Why: "Accounts never lock."}
	minorBreak := engine.Finding{Severity: engine.SeverityMinor, Where: "login.go:40", What: "A name is unclear.", Why: "It reads badly."}
	halted := reviewEnd(t, engine.ReviewEndedPayload{Outcome: engine.OutcomeUsageLimit, Head: "def456", ResetsAt: resets})
	limited := &engine.SessionFact{Outcome: engine.OutcomeUsageLimit, ResetsAt: resets}
	twoOpen := reviewedRun(reviewing, fullReview(t, critical, important))
	limitStop := ev(engine.Stopped, `{"reason":"usage_limit","explanation":"Usage limit.","action":"none","resets_at":"2030-01-01T13:00:00Z"}`)

	tests := []struct {
		name   string
		events []store.Event
		want   engine.ReviewFact
	}{
		{
			name:   "a full review opens its critical and important findings and records the minor ones",
			events: reviewedRun(reviewing, fullReview(t, critical, important, minor)),
			want:   engine.ReviewFact{Head: "def456", Open: []engine.Finding{critical, important}, Minor: []engine.Finding{minor}},
		},
		{
			name: "a failed first verdict with only a minor finding opens one important finding holding its reason",
			events: reviewedRun(reviewing, reviewEnd(t, engine.ReviewEndedPayload{
				Outcome: engine.ReviewReviewed, Head: "def456", Asked: asked, Built: pass, Findings: []engine.Finding{minor},
			})),
			want: engine.ReviewFact{
				Head: "def456", Open: []engine.Finding{{Severity: engine.SeverityImportant, What: asked.Reason}}, Minor: []engine.Finding{minor},
			},
		},
		{
			name:   "a scoped review keeps the findings it did not rule addressed",
			events: slices.Concat(twoOpen, fixRound(t, []bool{true, false})),
			want:   engine.ReviewFact{Head: "fed789", Open: []engine.Finding{important}},
		},
		{
			name:   "a finding the verdicts do not name is not addressed",
			events: slices.Concat(twoOpen, fixRound(t, []bool{false})),
			want:   engine.ReviewFact{Head: "fed789", Open: []engine.Finding{critical, important}},
		},
		{
			name:   "breakage in a fix joins the open findings",
			events: slices.Concat(twoOpen, fixRound(t, []bool{true, true}, breakage, minorBreak)),
			want:   engine.ReviewFact{Head: "fed789", Open: []engine.Finding{breakage}, Minor: []engine.Finding{minorBreak}},
		},
		{
			name: "a review without a verdict records why and the head it saw",
			events: reviewedRun(reviewing, reviewEnd(t, engine.ReviewEndedPayload{
				Outcome: engine.ReviewFailed, Head: "def456", Detail: "The reviewer ended without a structured result.",
			})),
			want: engine.ReviewFact{Head: "def456", Failed: "The reviewer ended without a structured result."},
		},
		{
			name: "a usage limit halts the review and leaves the head",
			events: slices.Concat(twoOpen, []store.Event{autoFixReview, fixedFed, verifying, verifyPassed, reviewingFix, reviewEnd(t, engine.ReviewEndedPayload{
				Outcome: engine.OutcomeUsageLimit, Head: "fed789", Scoped: true, ResetsAt: resets,
			})}),
			want: engine.ReviewFact{Head: "def456", Open: []engine.Finding{critical, important}, Halt: limited},
		},
		{
			name:   "the stop a halt gives keeps the halt",
			events: reviewedRun(reviewing, halted, limitStop),
			want:   engine.ReviewFact{Halt: limited},
		},
		{
			name:   "a person's retry clears the halt",
			events: reviewedRun(reviewing, halted, limitStop, retried),
			want:   engine.ReviewFact{},
		},
		{
			name:   "a person's cancel clears the halt",
			events: reviewedRun(reviewing, halted, limitStop, ev(engine.HumanCancelled, "")),
			want:   engine.ReviewFact{},
		},
		{
			name: "the export's run end keeps the review",
			events: slices.Concat(twoOpen, []store.Event{
				ev(engine.ExportStarted, ""), ev(engine.Exported, `{"head_commit":"def456"}`), ev(engine.RunEnded, `{"outcome":"exported"}`),
			}),
			want: engine.ReviewFact{Head: "def456", Open: []engine.Finding{critical, important}},
		},
		{
			name:   "a new run clears the review",
			events: slices.Concat(twoOpen, []store.Event{ev(engine.RunEnded, `{"outcome":"environment"}`), ev(engine.RunStarted, `{"reason":"retry"}`)}),
			want:   engine.ReviewFact{},
		},
		{
			name:   "a review that ends after its run is ignored",
			events: reviewedRun(reviewing, ev(engine.RunEnded, `{"outcome":"cancelled"}`), fullReview(t, critical)),
			want:   engine.ReviewFact{},
		},
	}

	for _, claim := range []store.Event{reviewing, resumeSession, verifying, ev(engine.ExportStarted, ""), ev(engine.PrepareStarted, "")} {
		tests = append(tests, struct {
			name   string
			events []store.Event
			want   engine.ReviewFact
		}{name: "the claim " + claim.Type + " clears the halt", events: reviewedRun(reviewing, halted, claim), want: engine.ReviewFact{}})
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, engine.Reduce(tc.events).Review)
		})
	}

	t.Run("a review without a verdict or a detail still records that it gave none", func(t *testing.T) {
		f := engine.Reduce(reviewedRun(reviewing, reviewEnd(t, engine.ReviewEndedPayload{Outcome: engine.ReviewFailed, Head: "def456"})))
		assert.NotEmpty(t, f.Review.Failed)
	})

	t.Run("automatic fix sessions count until a person acts", func(t *testing.T) {
		events := slices.Concat(twoOpen, fixRound(t, []bool{false, false}), fixRound(t, []bool{false, false}), []store.Event{autoFixReview})
		assert.Equal(t, 3, engine.Reduce(events).ReviewFixes)

		f := engine.Reduce(slices.Concat(events, []store.Event{
			ev(engine.SessionEnded, `{"outcome":"max_turns"}`), ev(engine.Stopped, `{"reason":"budget","explanation":"Turns used.","action":"retry"}`), retried,
		}))
		assert.Zero(t, f.ReviewFixes)
		assert.Equal(t, engine.ReviewFact{Head: "fed789", Open: []engine.Finding{critical, important}}, f.Review)
	})

	t.Run("a fresh fix session sets the role, and a new run clears it", func(t *testing.T) {
		fresh := ev(engine.SessionStarted, `{"mode":"fix_review","session_id":"s2","role":"strong","automatic":true}`)

		f := engine.Reduce(slices.Concat(twoOpen, []store.Event{fresh}))
		assert.Equal(t, engine.RoleStrong, f.Role)
		assert.Equal(t, "s2", f.SessionID)
		assert.Equal(t, 1, f.ReviewFixes)

		f = engine.Reduce(slices.Concat(twoOpen, []store.Event{fresh, ev(engine.RunEnded, `{"outcome":"environment"}`), ev(engine.RunStarted, `{"reason":"retry"}`)}))
		assert.Empty(t, f.Role)
	})

	t.Run("the answers are the task's and the assumptions the open run's", func(t *testing.T) {
		answering := ev(engine.SessionStarted, `{"mode":"answer","session_id":"s1","resume":true}`)
		events := slices.Concat(questionAnswered(), []store.Event{
			answering,
			ev(engine.SessionEnded, `{"outcome":"blocked","questions":["Which host?"],"assumptions":["Kept the old API."]}`),
			ev(engine.Stopped, `{"reason":"question","explanation":"Which host?","action":"answer"}`),
			ev(engine.HumanAnswered, `{"text":"localhost"}`),
			answering,
			ev(engine.SessionEnded, `{"outcome":"done","changed":true,"head_commit":"def456","assumptions":["Times are UTC."]}`),
		})

		f := engine.Reduce(events)
		assert.Equal(t, []string{"Use 8080.", "localhost"}, f.Answers)
		assert.Equal(t, []string{"Kept the old API.", "Times are UTC."}, f.Assumptions)

		f = engine.Reduce(slices.Concat(events, []store.Event{ev(engine.RunEnded, `{"outcome":"environment"}`), ev(engine.RunStarted, `{"reason":"retry"}`)}))
		assert.Equal(t, []string{"Use 8080.", "localhost"}, f.Answers)
		assert.Empty(t, f.Assumptions)
	})
}

// passedAt is a session that froze head and passed verify.
func passedAt(head string) engine.Facts {
	f := ended(engine.SessionFact{Outcome: engine.OutcomeDone, Changed: true, HeadCommit: head})
	f.Verify = engine.ResultPassed

	return f
}

var reviewOn = engine.Switches{Continuations: true, VerifyFix: true, Review: true}

// reviewCases follow the review's rules: the change at head h1 passed verify.
func reviewCases(t *testing.T) []decideCase {
	t.Helper()

	later := now.Add(2 * time.Hour)
	resets := now.Add(30 * time.Minute)
	reset := now.Add(-30 * time.Minute)
	export := engine.Action{Kind: engine.KindExport}
	full := engine.Action{Kind: engine.KindReview}
	scoped := engine.Action{Kind: engine.KindReview, Scoped: true}
	wait := func(until time.Time) engine.Action { return engine.Action{Kind: engine.KindWait, Until: until} }
	open := []engine.Finding{important}

	fix := func(fresh bool) engine.Action {
		return engine.Action{Kind: engine.KindWork, WorkMode: engine.ModeFixReview, Automatic: true, Fresh: fresh}
	}

	// with is the change at h1 with the review r and fixes automatic fix rounds done.
	with := func(r engine.ReviewFact, fixes int) engine.Facts {
		f := passedAt("h1")
		f.Review, f.ReviewFixes = r, fixes

		return f
	}

	halted := func(outcome string, resetsAt time.Time) engine.Facts {
		return with(engine.ReviewFact{Halt: &engine.SessionFact{Outcome: outcome, ResetsAt: resetsAt}}, 0)
	}

	// limitStop is the stop a review's usage limit gave, still open.
	limitStop := func(resetsAt time.Time) engine.Facts {
		f := halted(engine.OutcomeUsageLimit, resetsAt)
		f.OpenStop = &engine.StopFact{Reason: engine.StopUsageLimit, ResetsAt: resetsAt}

		return f
	}

	waived := passedAt("h1")
	waived.Verify = engine.ResultWaived

	on := func(name string, f engine.Facts, want engine.Action) decideCase {
		return decideCase{name: name, snap: snapWith(f, reviewOn), want: want}
	}

	pausedOn := func(p engine.Pause) engine.Snapshot {
		s := snapWith(passedAt("h1"), reviewOn)
		s.Pause = p

		return s
	}

	// reduced decides the facts events add up to.
	reduced := func(name string, events []store.Event, want engine.Action, clock bool) decideCase {
		return decideCase{name: name, snap: snapWith(engine.Reduce(events), reviewOn), want: want, clock: clock}
	}

	// haltRetried is a review that ended as outcome, the stop it gave, and a person's retry.
	haltRetried := func(outcome, reason string, scoped bool) []store.Event {
		head, start, base := "def456", reviewing, reviewedRun()
		if scoped {
			head, start = "fed789", reviewingFix
			base = slices.Concat(reviewedRun(reviewing, fullReview(t, important)), []store.Event{autoFixReview, fixedFed, verifying, verifyPassed})
		}

		return slices.Concat(base, []store.Event{
			start,
			reviewEnd(t, engine.ReviewEndedPayload{Outcome: outcome, Head: head, Scoped: scoped, Detail: "503 overloaded"}),
			stopEv(t, engine.StoppedPayload{Reason: reason, Explanation: "The review stopped.", Action: engine.ClearRetry}),
			retried,
		})
	}

	cases := []decideCase{
		on("verify passed, no review yet", passedAt("h1"), full),
		on("verify waived, no review yet", waived, full),
		on("the head reviewed, nothing open", with(engine.ReviewFact{Head: "h1"}, 0), export),
		on("the head reviewed, only minor findings", with(engine.ReviewFact{Head: "h1", Minor: []engine.Finding{minor}}, 0), export),
		on("findings open on a new head", with(engine.ReviewFact{Head: "h0", Open: open}, 1), scoped),
		on("no verdict on an older head", with(engine.ReviewFact{Head: "h0", Failed: "No structured result."}, 0), full),
		on("findings open, no fix round yet", with(engine.ReviewFact{Head: "h1", Open: open}, 0), fix(false)),
		on("findings open after one fix round", with(engine.ReviewFact{Head: "h1", Open: open}, 1), fix(false)),
		on("findings open after two fix rounds", with(engine.ReviewFact{Head: "h1", Open: open}, 2), fix(false)),
		on("findings open after three fix rounds", with(engine.ReviewFact{Head: "h1", Open: open}, 3), fix(true)),
		on("findings open after four fix rounds", with(engine.ReviewFact{Head: "h1", Open: open}, 4), fix(true)),
		on("findings open after five fix rounds", with(engine.ReviewFact{Head: "h1", Open: open}, 5), export),
		on("no verdict on this head", with(engine.ReviewFact{Head: "h1", Open: open, Failed: "No structured result."}, 1), export),
		on("a review halted by a usage limit", halted(engine.OutcomeUsageLimit, later),
			engine.Action{Kind: engine.KindStop, Stop: engine.StopUsageLimit, Clear: engine.ClearNone, ResetsAt: later}),
		{
			name: "a review halted by a usage limit with no reset time", snap: snapWith(halted(engine.OutcomeUsageLimit, time.Time{}), reviewOn),
			want:  engine.Action{Kind: engine.KindStop, Stop: engine.StopUsageLimit, Clear: engine.ClearNone, ResetsAt: now.Add(engine.UsageLimitFallback)},
			clock: true,
		},
		on("a review halted by the endpoint", halted(engine.OutcomeEndpoint, time.Time{}), halt(engine.StopEndpoint, engine.ClearRetry)),
		on("a review that timed out", halted(engine.OutcomeTimeout, time.Time{}), halt(engine.StopTimeout, engine.ClearRetry)),
		on("a review that used all its turns", halted(engine.OutcomeMaxTurns, time.Time{}), halt(engine.StopBudget, engine.ClearRetry)),
		{name: "the usage limit that ended a review, before its reset", snap: snapWith(limitStop(resets), reviewOn), want: wait(resets), clock: true},
		{name: "the usage limit that ended a review, after its reset", snap: snapWith(limitStop(reset), reviewOn), want: full, clock: true},
		{name: "an endpoint pause holds a review", snap: pausedOn(engine.Pause{EndpointTask: 7}), want: wait(time.Time{}), whyHas: "7"},
		{
			name: "a usage limit pause holds a review", snap: pausedOn(engine.Pause{LimitedUntil: now.Add(time.Hour), LimitedBy: 9}),
			want: wait(now.Add(time.Hour)), whyHas: "9", clock: true,
		},
		reduced("a review cut by a runner fault is redone after the backoff", reviewedRun(reviewing, sandboxStop), full, true),
		reduced("a retry after a review's endpoint stop redoes the review", haltRetried(engine.OutcomeEndpoint, engine.StopEndpoint, false), full, false),
		reduced("a retry after a review's timeout redoes the review", haltRetried(engine.OutcomeTimeout, engine.StopTimeout, false), full, false),
		reduced("a retry after a scoped review's endpoint stop redoes it", haltRetried(engine.OutcomeEndpoint, engine.StopEndpoint, true), scoped, false),
		reduced("the usage limit that ended a review leads to the review once it resets", reviewedRun(reviewing,
			reviewEnd(t, engine.ReviewEndedPayload{Outcome: engine.OutcomeUsageLimit, Head: "def456", ResetsAt: reset}),
			stopEv(t, engine.StoppedPayload{Reason: engine.StopUsageLimit, Explanation: "Usage limit.", Action: engine.ClearNone, ResetsAt: reset}),
		), full, true),
	}

	// With the review off, every change that passed verify is exported.
	for _, c := range []struct {
		name string
		f    engine.Facts
	}{
		{"no review yet", passedAt("h1")},
		{"findings open on a new head", with(engine.ReviewFact{Head: "h0", Open: open}, 1)},
		{"findings open", with(engine.ReviewFact{Head: "h1", Open: open}, 3)},
		{"no verdict on this head", with(engine.ReviewFact{Head: "h1", Failed: "No structured result."}, 0)},
		{"a review halted by a usage limit", halted(engine.OutcomeUsageLimit, later)},
		{"a review halted by the endpoint", halted(engine.OutcomeEndpoint, time.Time{})},
		{"the usage limit that ended a review, after its reset", limitStop(reset)},
	} {
		cases = append(cases, decideCase{name: "review off: " + c.name, snap: snap(c.f), want: export, clock: c.f.OpenStop != nil})
	}

	return cases
}

func TestDecideReview(t *testing.T) {
	for _, tc := range reviewCases(t) {
		t.Run(tc.name, func(t *testing.T) { assertDecides(t, tc) })
	}
}

// withReview turns the review on for the harness's project.
func (h *harness) withReview() {
	h.t.Helper()

	p := h.project()
	p.Switches["review"] = true
	require.NoError(h.t, h.store.UpdateProject(h.t.Context(), p))
}

// frozeAt is the work result workDone with its session's work frozen into head.
func frozeAt(head string) step[engine.WorkResult] {
	res := workDone
	res.HeadCommit = head

	return step[engine.WorkResult]{res: res}
}

func reviewed(addressed ...bool) step[engine.ReviewResult] {
	return step[engine.ReviewResult]{res: engine.ReviewResult{Outcome: engine.ReviewReviewed, Addressed: addressed}}
}

func TestLoopReviewRound(t *testing.T) {
	h := newHarness(t)
	h.withReview()

	first := found
	first.Usage = []store.Usage{{Source: "agent", Model: "model-r", Input: 7, Output: 3}}
	h.runner.works = []step[engine.WorkResult]{frozeAt("head456"), frozeAt("head789")}
	h.runner.reviews = []step[engine.ReviewResult]{{res: first}, reviewed(true)}

	status, err := h.run(t.Context())
	require.NoError(t, err)
	assert.Equal(t, engine.StatusInReview, status)
	assert.Equal(t, []string{
		"prepare", "work/start", "verify", "review", "work/fix_review", "verify", "review/scoped", "export", "release/remove",
	}, h.runner.trace())

	works := h.runner.callsOf("work")
	require.Len(t, works, 2)

	fix := works[1].work
	assert.Equal(t, []engine.Finding{important}, fix.Findings)
	assert.True(t, fix.Resume)
	assert.Equal(t, works[0].work.SessionID, fix.SessionID)
	assert.Equal(t, engine.RoleWork, fix.Role)
	assert.Equal(t, engine.RoleWork, works[0].work.Role)

	reviews := h.runner.callsOf("review")
	require.Len(t, reviews, 2)

	full, scoped := reviews[0].review, reviews[1].review
	assert.Equal(t, "head456", full.Head)
	assert.Empty(t, full.FixBase)
	assert.Empty(t, full.Findings)
	assert.Regexp(t, uuidV4, full.SessionID)
	assert.NotEqual(t, works[0].work.SessionID, full.SessionID)
	assert.Equal(t, "head789", scoped.Head)
	assert.Equal(t, "head456", scoped.FixBase)
	assert.Equal(t, []engine.Finding{important}, scoped.Findings)
	assert.Regexp(t, uuidV4, scoped.SessionID)
	assert.NotEqual(t, full.SessionID, scoped.SessionID)

	assert.Equal(t, "head789", h.runner.callsOf("export")[0].commit)

	starts, ends := h.eventsOf(engine.ReviewStarted), h.eventsOf(engine.ReviewEnded)
	require.Len(t, starts, 2)
	require.Len(t, ends, 2)

	runID := starts[0].RunID
	assert.NotZero(t, runID)

	start := payloadOf[engine.ReviewStartedPayload](t, starts[1])
	assert.Equal(t, scoped.SessionID, start.SessionID)
	assert.True(t, start.Scoped)
	assert.Equal(t, "head789", start.Head)
	assert.NotEmpty(t, start.Why)

	end := payloadOf[engine.ReviewEndedPayload](t, ends[0])
	assert.Equal(t, engine.ReviewReviewed, end.Outcome)
	assert.Equal(t, "head456", end.Head)
	assert.Equal(t, []engine.Finding{important}, end.Findings)
	assert.Equal(t, "runner", ends[0].Actor)
	assert.Equal(t, runID, ends[0].RunID)
	assert.Equal(t, []bool{true}, payloadOf[engine.ReviewEndedPayload](t, ends[1]).Addressed)

	usage, err := h.store.UsageTotals(t.Context(), h.task.ID)
	require.NoError(t, err)
	assert.Contains(t, usage, store.Usage{Source: "agent", Model: "model-r", Input: 7, Output: 3})
}

// The review is built from the record: the person's answers, the sessions'
// assumptions and the working session's summary.
func TestLoopReviewReadsTheRecord(t *testing.T) {
	h := newHarness(t)
	h.withReview()

	done := workDone
	done.Assumptions = []string{"Kept the old API."}
	h.runner.works = []step[engine.WorkResult]{{res: engine.WorkResult{Outcome: engine.OutcomeBlocked, Questions: []string{"Which port?"}}}, {res: done}}

	status, err := h.run(t.Context())
	require.NoError(t, err)
	require.Equal(t, engine.StatusBlocked, status)
	require.NoError(t, h.loop.Answer(t.Context(), h.task.ID, "Use port 8080."))

	status, err = h.run(t.Context())
	require.NoError(t, err)
	assert.Equal(t, engine.StatusInReview, status)

	reviews := h.runner.callsOf("review")
	require.Len(t, reviews, 1)
	assert.Equal(t, []string{"Use port 8080."}, reviews[0].review.Answers)
	assert.Equal(t, []string{"Kept the old API."}, reviews[0].review.Assumptions)
	assert.Equal(t, workDone.Summary, reviews[0].review.Summary)
}

func TestLoopReviewNeverBlocks(t *testing.T) {
	h := newHarness(t)
	h.withReview()

	h.runner.works = []step[engine.WorkResult]{frozeAt("head0")}
	h.runner.reviews = []step[engine.ReviewResult]{{res: found}}

	for i := range 5 {
		h.runner.works = append(h.runner.works, frozeAt(fmt.Sprintf("head%d", i+1)))
		h.runner.reviews = append(h.runner.reviews, reviewed(false))
	}

	status, err := h.run(t.Context())
	require.NoError(t, err)
	assert.Equal(t, engine.StatusInReview, status)

	works := h.runner.callsOf("work")
	require.Len(t, works, 6)

	opening := works[0].work.SessionID

	var fixes []engine.WorkRequest

	for _, c := range works[1:] {
		require.Equal(t, engine.ModeFixReview, c.work.Mode)
		fixes = append(fixes, c.work)
	}

	for i, w := range fixes[:3] {
		assert.True(t, w.Resume, "round %d", i+1)
		assert.Equal(t, opening, w.SessionID, "round %d", i+1)
		assert.Equal(t, engine.RoleWork, w.Role, "round %d", i+1)
	}

	for i, w := range fixes[3:] {
		assert.False(t, w.Resume, "round %d", i+4)
		assert.Regexp(t, uuidV4, w.SessionID, "round %d", i+4)
		assert.NotEqual(t, opening, w.SessionID, "round %d", i+4)
		assert.Equal(t, engine.RoleStrong, w.Role, "round %d", i+4)
		assert.Equal(t, []engine.Finding{important}, w.Findings, "round %d", i+4)
	}

	assert.NotEqual(t, fixes[3].SessionID, fixes[4].SessionID)

	starts := h.eventsOf(engine.SessionStarted)
	require.Len(t, starts, 6)

	for _, e := range starts[4:] {
		p := payloadOf[engine.SessionStartedPayload](t, e)
		assert.Equal(t, engine.RoleStrong, p.Role)
		assert.True(t, p.Automatic)
		assert.False(t, p.Resume)
	}

	exports := h.runner.callsOf("export")
	require.Len(t, exports, 1)
	assert.Equal(t, "head5", exports[0].commit)
	assert.Len(t, h.runner.callsOf("review"), 6, "the last fix is reviewed, and nothing follows")

	snap, err := h.loop.Snapshot(t.Context(), h.task.ID)
	require.NoError(t, err)
	assert.Equal(t, []engine.Finding{important}, snap.Facts.Review.Open)
}

func TestLoopReviewWithoutAVerdict(t *testing.T) {
	h := newHarness(t)
	h.withReview()

	const detail = "The reviewer ended without a structured result."

	h.runner.reviews = []step[engine.ReviewResult]{{res: engine.ReviewResult{Outcome: engine.ReviewFailed, Detail: detail}}}

	status, err := h.run(t.Context())
	require.NoError(t, err)
	assert.Equal(t, engine.StatusInReview, status)
	assert.Equal(t, []string{"prepare", "work/start", "verify", "review", "export", "release/remove"}, h.runner.trace())

	snap, err := h.loop.Snapshot(t.Context(), h.task.ID)
	require.NoError(t, err)
	assert.Equal(t, detail, snap.Facts.Review.Failed)
}

// A person's retry of the stop a review's halt gave redoes the review, and
// sends the finished work session nothing.
func TestLoopRetryAfterAReviewHalt(t *testing.T) {
	h := newHarness(t)
	h.withReview()

	h.runner.reviews = []step[engine.ReviewResult]{{res: engine.ReviewResult{Outcome: engine.OutcomeEndpoint, Detail: "503 overloaded"}}}

	status, err := h.run(t.Context())
	require.NoError(t, err)
	require.Equal(t, engine.StatusBlocked, status)
	assert.Equal(t, engine.StopEndpoint, h.lastStop().Reason)

	before := len(h.runner.calls)

	require.NoError(t, h.loop.Retry(t.Context(), h.task.ID))

	status, err = h.run(t.Context())
	require.NoError(t, err)
	assert.Equal(t, engine.StatusInReview, status)
	assert.Equal(t, []string{"review", "export", "release/remove"}, h.runner.trace()[before:])
}

func TestLoopVerifyFixInsideAReviewRound(t *testing.T) {
	failed := step[engine.VerifyResult]{res: engine.VerifyResult{Result: engine.ResultFailed, Command: "make test", Output: "FAIL TestLogin"}}
	passed := step[engine.VerifyResult]{res: verifyOK}

	// check finds the fix_verify session in the fix_review session's role,
	// resuming it, then the scoped review of what it froze.
	check := func(t *testing.T, h *harness, fixBase string) {
		t.Helper()

		works := h.runner.callsOf("work")
		require.GreaterOrEqual(t, len(works), 2)

		fix, again := works[len(works)-2].work, works[len(works)-1].work
		assert.Equal(t, engine.ModeFixReview, fix.Mode)
		assert.Equal(t, engine.ModeFixVerify, again.Mode)
		assert.Equal(t, "FAIL TestLogin", again.Detail)
		assert.True(t, again.Resume)
		assert.Equal(t, fix.SessionID, again.SessionID)
		assert.Equal(t, fix.Role, again.Role)

		reviews := h.runner.callsOf("review")
		require.NotEmpty(t, reviews)

		scoped := reviews[len(reviews)-1].review
		assert.Equal(t, fixBase, scoped.FixBase)
		assert.Equal(t, "fixed2", scoped.Head)
		assert.Equal(t, "fixed2", h.runner.callsOf("export")[0].commit)
	}

	t.Run("in a round that resumes the session", func(t *testing.T) {
		h := newHarness(t)
		h.withReview()

		h.runner.works = []step[engine.WorkResult]{frozeAt("head456"), frozeAt("fixed1"), frozeAt("fixed2")}
		h.runner.verifies = []step[engine.VerifyResult]{passed, failed, passed}
		h.runner.reviews = []step[engine.ReviewResult]{{res: found}, reviewed(true)}

		status, err := h.run(t.Context())
		require.NoError(t, err)
		assert.Equal(t, engine.StatusInReview, status)
		assert.Equal(t, []string{
			"prepare", "work/start", "verify", "review", "work/fix_review", "verify", "work/fix_verify", "verify", "review/scoped",
			"export", "release/remove",
		}, h.runner.trace())
		check(t, h, "head456")
		assert.Equal(t, engine.RoleWork, h.runner.callsOf("work")[2].work.Role)
	})

	t.Run("in a fresh round on the stronger model", func(t *testing.T) {
		h := newHarness(t)
		h.withReview()

		// Three rounds left the finding open.
		events := []store.Event{
			ev(engine.RunStarted, `{"reason":"initial"}`), ev(engine.PrepareStarted, ""), ev(engine.Prepared, `{"base_commit":"base123"}`),
			ev(engine.SessionStarted, `{"mode":"start","session_id":"`+seededSession+`","role":"work"}`),
			ev(engine.SessionEnded, `{"outcome":"done","changed":true,"head_commit":"head1"}`),
			ev(engine.VerifyStarted, ""), ev(engine.VerifyEnded, `{"result":"passed"}`),
			ev(engine.ReviewStarted, `{"session_id":"r1","head":"head1"}`),
			reviewEnd(t, engine.ReviewEndedPayload{Outcome: engine.ReviewReviewed, Head: "head1", Asked: pass, Built: pass, Findings: []engine.Finding{important}}),
		}

		for i := 2; i <= 4; i++ {
			head := fmt.Sprintf("head%d", i)
			events = append(events,
				ev(engine.SessionStarted, `{"mode":"fix_review","session_id":"`+seededSession+`","resume":true,"automatic":true,"role":"work"}`),
				ev(engine.SessionEnded, `{"outcome":"done","changed":true,"head_commit":"`+head+`"}`),
				ev(engine.VerifyStarted, ""), ev(engine.VerifyEnded, `{"result":"passed"}`),
				ev(engine.ReviewStarted, `{"scoped":true,"head":"`+head+`"}`),
				reviewEnd(t, engine.ReviewEndedPayload{Outcome: engine.ReviewReviewed, Head: head, Scoped: true, Addressed: []bool{false}}),
			)
		}

		h.seed(events...)

		h.runner.works = []step[engine.WorkResult]{frozeAt("fixed1"), frozeAt("fixed2")}
		h.runner.verifies = []step[engine.VerifyResult]{failed, passed}
		h.runner.reviews = []step[engine.ReviewResult]{reviewed(true)}

		status, err := h.run(t.Context())
		require.NoError(t, err)
		assert.Equal(t, engine.StatusInReview, status)
		assert.Equal(t, []string{
			"work/fix_review", "verify", "work/fix_verify", "verify", "review/scoped", "export", "release/remove",
		}, h.runner.trace())
		check(t, h, "head4")

		fix := h.runner.callsOf("work")[0].work
		assert.Equal(t, engine.RoleStrong, fix.Role)
		assert.False(t, fix.Resume)
		assert.NotEqual(t, seededSession, fix.SessionID)
	})
}
