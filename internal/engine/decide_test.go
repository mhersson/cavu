package engine_test

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/mhersson/cavu/internal/engine"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// now is far from the real clock, so a Decide that read the clock would fail.
var now = time.Date(2030, 1, 1, 12, 0, 0, 0, time.UTC)

type decideCase struct {
	name       string
	snap       engine.Snapshot
	want       engine.Action
	explainHas string // a value the stop's explanation must carry
	whyHas     string // a value the action's why must carry
	clock      bool   // the action changes when Now moves an hour
}

func snap(f engine.Facts) engine.Snapshot {
	return snapWith(f, engine.Switches{Continuations: true, VerifyFix: true})
}

func snapWith(f engine.Facts, sw engine.Switches) engine.Snapshot {
	return engine.Snapshot{
		Facts: f, Now: now, Switches: sw, Caps: engine.Caps{Continuations: 2, VerifyFixes: 2, ReviewFixes: 5, FaultRetries: 2, CIFixes: 2, CommentRuns: 3},
		Limits: engine.Limits{Turns: 1000, SessionTime: 300 * time.Minute},
	}
}

func withDeps(f engine.Facts, deps ...engine.Dep) engine.Snapshot {
	s := snap(f)
	s.Deps = deps

	return s
}

func paused(f engine.Facts, p engine.Pause) engine.Snapshot {
	s := snap(f)
	s.Pause = p

	return s
}

func prepared() engine.Facts {
	return engine.Facts{Started: true, RunOpen: true, Prepared: true}
}

func openStop(reason string, resetsAt time.Time) engine.Facts {
	f := prepared()
	f.SessionID = "s1"
	f.OpenStop = &engine.StopFact{Reason: reason, ResetsAt: resetsAt}

	return f
}

func resumed(from, answer string) engine.Facts {
	f := prepared()
	f.SessionID = "s1"
	f.Resumed = &engine.Resume{From: from, Answer: answer}

	return f
}

// faulted is a session claimed and cut by its nth runner fault, recorded at at.
func faulted(n int, at time.Time) engine.Facts {
	f := prepared()
	f.SessionID = "s1"
	f.OpenStop = &engine.StopFact{Reason: engine.StopSandbox, At: at}
	f.Faults = n

	return f
}

func ended(s engine.SessionFact) engine.Facts {
	f := prepared()
	f.SessionID = "s1"
	f.Session = &s

	return f
}

func cut(outcome string, continuations int) engine.Facts {
	f := ended(engine.SessionFact{Outcome: outcome})
	f.Continuations = continuations

	return f
}

func verified(result string, fixes int) engine.Facts {
	f := ended(engine.SessionFact{Outcome: engine.OutcomeDone, Changed: true})
	f.Verify = result
	f.VerifyFixes = fixes

	return f
}

func work(mode string, automatic bool) engine.Action {
	return engine.Action{Kind: engine.KindWork, WorkMode: mode, Automatic: automatic}
}

func halt(reason, clears string) engine.Action {
	return engine.Action{Kind: engine.KindStop, Stop: reason, Clear: clears}
}

// decideCases follow the decision rules in order, one or more cases per rule.
func decideCases() []decideCase {
	resets := now.Add(30 * time.Minute)
	reset := now.Add(-30 * time.Minute)
	later := now.Add(2 * time.Hour)
	fallback := now.Add(engine.UsageLimitFallback)
	none := engine.Action{Kind: engine.KindNone}
	clean := engine.Action{Kind: engine.KindClean}
	contOff := engine.Switches{VerifyFix: true}
	fixOff := engine.Switches{Continuations: true}
	readiness := engine.Switches{Continuations: true, VerifyFix: true, Readiness: true}

	cancelledStop := openStop(engine.StopCancelled, time.Time{})
	cancelledStop.Cancelled = true

	cancelledQuestion := openStop(engine.StopQuestion, time.Time{})
	cancelledQuestion.Cancelled = true

	exportedOpen := prepared()
	exportedOpen.Exported = true

	startedOnly := engine.Facts{Started: true}

	exported := startedOnly
	exported.Exported = true

	unprepared := engine.Facts{Started: true, RunOpen: true}

	prepareFailed := unprepared
	prepareFailed.PrepareFailure = &engine.PrepareFailure{Step: "setup", Output: "npm ERR! missing script"}

	withSession := prepared()
	withSession.SessionID = "s1"

	crashedWithDetail := cut(engine.OutcomeCrashed, 2)
	crashedWithDetail.Session.Detail = "stream closed: 502 Bad Gateway"

	longSummary := strings.Repeat("日本語", 200)

	merged := exported
	merged.Merged = true

	mergedOpen := merged
	mergedOpen.RunOpen = true

	mergedStop := openStop(engine.StopUsageLimit, resets)
	mergedStop.Exported, mergedStop.Merged = true, true

	// A run ended and kept the task's home.
	cancelledHome := engine.Facts{Started: true, Cancelled: true, Home: true}

	cancelledStopHome := cancelledHome
	cancelledStopHome.OpenStop = &engine.StopFact{Reason: engine.StopCancelled}

	exportedHome := exported
	exportedHome.Home = true

	publishedHome := exportedHome
	publishedHome.PR = &engine.PRFact{Number: 7, Head: "head1", Heads: []string{"head1"}}

	mergedHome := publishedHome
	mergedHome.Merged = true

	environmentHome := engine.Facts{Started: true, Home: true, OpenStop: &engine.StopFact{Reason: engine.StopEnvironment}}

	continueWork := work(engine.ModeContinue, false)
	wait := func(until time.Time) engine.Action { return engine.Action{Kind: engine.KindWait, Until: until} }
	prepare := engine.Action{Kind: engine.KindPrepare, NewRun: true}
	endpointPause := engine.Pause{EndpointTask: 7}
	limitPause := engine.Pause{LimitedUntil: now.Add(time.Hour), LimitedBy: 9}

	return []decideCase{
		{name: "not started", snap: snap(engine.Facts{}), want: none},
		{name: "cancelled", snap: snap(engine.Facts{Started: true, Cancelled: true, RunOpen: true}), want: halt(engine.StopCancelled, engine.ClearNone)},
		{name: "cancelled with another stop open", snap: snap(cancelledQuestion), want: halt(engine.StopCancelled, engine.ClearNone)},
		{name: "merged", snap: snap(merged), want: none},
		{name: "merged with the run open", snap: snap(mergedOpen), want: engine.Action{Kind: engine.KindFinish}},
		{name: "merged with a stale stop and the run open", snap: snap(mergedStop), want: engine.Action{Kind: engine.KindFinish}},
		{name: "cancelled with the home kept", snap: snap(cancelledHome), want: halt(engine.StopCancelled, engine.ClearNone)},
		{name: "cancelled with its stop recorded and the home kept", snap: snap(cancelledStopHome), want: clean},
		{name: "merged through its pull request with the home kept", snap: snap(mergedHome), want: clean},
		{name: "exported with no pull request and the home kept", snap: snap(exportedHome), want: clean},
		{name: "exported with a pull request and the home kept", snap: snap(publishedHome), want: engine.Action{Kind: engine.KindWatch}},
		{name: "stopped with the home kept", snap: snap(environmentHome), want: none},
		{name: "usage limit before its reset", snap: snap(openStop(engine.StopUsageLimit, resets)), want: engine.Action{Kind: engine.KindWait, Until: resets}, clock: true},
		{name: "usage limit reset reached", snap: snap(openStop(engine.StopUsageLimit, reset)), want: work(engine.ModeContinue, false), clock: true},
		{name: "usage limit with no reset time", snap: snap(openStop(engine.StopUsageLimit, time.Time{})), want: work(engine.ModeContinue, false)},
		{name: "a first fault before its backoff", snap: snap(faulted(1, now)), want: wait(now.Add(30 * time.Second)), clock: true},
		{name: "a first fault after its backoff", snap: snap(faulted(1, now.Add(-30*time.Second-time.Nanosecond))), want: continueWork, clock: true},
		{name: "a second fault before its backoff", snap: snap(faulted(2, now)), want: wait(now.Add(2 * time.Minute)), clock: true},
		{name: "a second fault after its backoff", snap: snap(faulted(2, now.Add(-2*time.Minute-time.Nanosecond))), want: continueWork, clock: true},
		{name: "a third fault", snap: snap(faulted(3, now)), want: none},
		{name: "another open stop", snap: snap(openStop(engine.StopQuestion, time.Time{})), want: none},
		{name: "cancelled with its stop recorded", snap: snap(cancelledStop), want: none},
		{name: "exported with the run open", snap: snap(exportedOpen), want: engine.Action{Kind: engine.KindFinish}},
		{name: "exported", snap: snap(exported), want: none},
		{
			name: "a cancelled dependency", explainHas: "42",
			snap: withDeps(startedOnly, engine.Dep{TaskID: 41, Status: engine.StatusQueued}, engine.Dep{TaskID: 42, Status: engine.StatusCancelled}),
			want: halt(engine.StopDependency, engine.ClearRetry),
		},
		{
			name: "a dependency in review", whyHas: "41",
			snap: withDeps(startedOnly, engine.Dep{TaskID: 40, Status: engine.StatusDone}, engine.Dep{TaskID: 41, Status: engine.StatusInReview}),
			want: wait(time.Time{}),
		},
		{name: "dependencies done", snap: withDeps(startedOnly, engine.Dep{TaskID: 40, Status: engine.StatusDone}), want: prepare},
		{
			name: "a cancelled dependency with a run open", snap: withDeps(prepared(), engine.Dep{TaskID: 42, Status: engine.StatusCancelled}),
			want: work(engine.ModeStart, false),
		},
		{
			name: "a dependency not done with a run open", snap: withDeps(prepared(), engine.Dep{TaskID: 41, Status: engine.StatusQueued}),
			want: work(engine.ModeStart, false),
		},
		{name: "no open run", snap: snap(startedOnly), want: prepare},
		{name: "resumed from environment", snap: snap(resumed(engine.StopEnvironment, "")), want: engine.Action{Kind: engine.KindPrepare, NewRun: true}},
		{name: "run not prepared", snap: snap(unprepared), want: engine.Action{Kind: engine.KindPrepare}},
		{name: "prepare failed", snap: snap(prepareFailed), want: halt(engine.StopEnvironment, engine.ClearRetry), explainHas: "setup"},
		{
			name: "resumed from a question with an answer", snap: snap(resumed(engine.StopQuestion, "Use port 8080.")),
			want: engine.Action{Kind: engine.KindWork, WorkMode: engine.ModeAnswer, Detail: "Use port 8080."},
		},
		{name: "resumed from verify failed", snap: snap(resumed(engine.StopVerifyFailed, "")), want: work(engine.ModeFixVerify, false)},
		{name: "resumed from an agent error", snap: snap(resumed(engine.StopAgentError, "")), want: work(engine.ModeContinue, false)},
		{name: "resumed from a question without an answer", snap: snap(resumed(engine.StopQuestion, "")), want: work(engine.ModeContinue, false)},
		{name: "no ended session, readiness on", snap: snapWith(prepared(), readiness), want: work(engine.ModeReadiness, false)},
		{name: "no ended session with a session id, readiness on", snap: snapWith(withSession, readiness), want: work(engine.ModeContinue, false)},
		{name: "no ended session", snap: snap(prepared()), want: work(engine.ModeStart, false)},
		{name: "no ended session with a session id", snap: snap(withSession), want: work(engine.ModeContinue, false)},
		{
			name: "session usage limit with a future reset", snap: snap(ended(engine.SessionFact{Outcome: engine.OutcomeUsageLimit, ResetsAt: later})),
			want: engine.Action{Kind: engine.KindStop, Stop: engine.StopUsageLimit, Clear: engine.ClearNone, ResetsAt: later},
		},
		{
			name: "session usage limit with no reset time", snap: snap(ended(engine.SessionFact{Outcome: engine.OutcomeUsageLimit})),
			want: engine.Action{Kind: engine.KindStop, Stop: engine.StopUsageLimit, Clear: engine.ClearNone, ResetsAt: fallback}, clock: true,
		},
		{
			name: "session usage limit with a past reset", snap: snap(ended(engine.SessionFact{Outcome: engine.OutcomeUsageLimit, ResetsAt: reset})),
			want: engine.Action{Kind: engine.KindStop, Stop: engine.StopUsageLimit, Clear: engine.ClearNone, ResetsAt: fallback}, clock: true,
		},
		{
			name: "session usage limit with a status", snap: snap(ended(engine.SessionFact{Outcome: engine.OutcomeUsageLimit, ResetsAt: later, Detail: "429 rate_limit_error"})),
			want: engine.Action{Kind: engine.KindStop, Stop: engine.StopUsageLimit, Clear: engine.ClearNone, ResetsAt: later}, explainHas: "429",
		},
		{name: "session endpoint", snap: snap(ended(engine.SessionFact{Outcome: engine.OutcomeEndpoint})), want: halt(engine.StopEndpoint, engine.ClearRetry)},
		{
			name: "session endpoint with a status", snap: snap(ended(engine.SessionFact{Outcome: engine.OutcomeEndpoint, Detail: "401 invalid x-api-key"})),
			want: halt(engine.StopEndpoint, engine.ClearRetry), explainHas: "401",
		},
		{
			name: "session endpoint with a long detail", snap: snap(ended(engine.SessionFact{Outcome: engine.OutcomeEndpoint, Detail: "503 " + longSummary})),
			want: halt(engine.StopEndpoint, engine.ClearRetry), explainHas: "503",
		},
		{name: "session timeout", snap: snap(ended(engine.SessionFact{Outcome: engine.OutcomeTimeout})), want: halt(engine.StopTimeout, engine.ClearRetry)},
		{name: "session max turns", snap: snap(ended(engine.SessionFact{Outcome: engine.OutcomeMaxTurns})), want: halt(engine.StopBudget, engine.ClearRetry)},
		{
			name: "session blocked", snap: snap(ended(engine.SessionFact{Outcome: engine.OutcomeBlocked, Questions: []string{"Which port should it listen on?", "Is IPv6 needed?"}})),
			want: halt(engine.StopQuestion, engine.ClearAnswer), explainHas: "Which port should it listen on?",
		},
		{
			name: "session failed", snap: snap(ended(engine.SessionFact{Outcome: engine.OutcomeFailed, Summary: "The tests need a database.\nNone is running."})),
			want: halt(engine.StopAgentError, engine.ClearRetry), explainHas: "The tests need a database.",
		},
		{
			name: "session failed with a long summary", snap: snap(ended(engine.SessionFact{Outcome: engine.OutcomeFailed, Summary: longSummary})),
			want: halt(engine.StopAgentError, engine.ClearRetry), explainHas: "日本語",
		},
		{name: "session ready", snap: snap(ended(engine.SessionFact{Outcome: engine.OutcomeReady})), want: work(engine.ModeStart, false)},
		{name: "session ready, readiness on", snap: snapWith(ended(engine.SessionFact{Outcome: engine.OutcomeReady}), readiness), want: work(engine.ModeStart, false)},
		{name: "unfinished with no continuation yet", snap: snap(cut(engine.OutcomeUnfinished, 0)), want: work(engine.ModeContinue, true)},
		{name: "unfinished with one continuation", snap: snap(cut(engine.OutcomeUnfinished, 1)), want: work(engine.ModeContinue, true)},
		{name: "unfinished at the continuation cap", snap: snap(cut(engine.OutcomeUnfinished, 2)), want: halt(engine.StopAgentError, engine.ClearRetry)},
		{name: "unfinished with continuations off", snap: snapWith(cut(engine.OutcomeUnfinished, 0), contOff), want: halt(engine.StopAgentError, engine.ClearRetry)},
		{name: "crashed with no continuation yet", snap: snap(cut(engine.OutcomeCrashed, 0)), want: work(engine.ModeContinue, true)},
		{name: "crashed at the continuation cap", snap: snap(cut(engine.OutcomeCrashed, 2)), want: halt(engine.StopAgentError, engine.ClearRetry)},
		{name: "crashed at the continuation cap with a detail", snap: snap(crashedWithDetail), want: halt(engine.StopAgentError, engine.ClearRetry), explainHas: "502"},
		{name: "done without a change", snap: snap(ended(engine.SessionFact{Outcome: engine.OutcomeDone})), want: halt(engine.StopNoChange, engine.ClearRetry)},
		{name: "done and not verified", snap: snap(verified("", 0)), want: engine.Action{Kind: engine.KindVerify}},
		{name: "verify failed with no fix yet", snap: snap(verified(engine.ResultFailed, 0)), want: work(engine.ModeFixVerify, true)},
		{name: "verify timed out with one fix", snap: snap(verified(engine.ResultTimeout, 1)), want: work(engine.ModeFixVerify, true)},
		{name: "verify failed at the fix cap", snap: snap(verified(engine.ResultFailed, 2)), want: halt(engine.StopVerifyFailed, engine.ClearRetry)},
		{name: "verify failed with verify fix off", snap: snapWith(verified(engine.ResultFailed, 0), fixOff), want: halt(engine.StopVerifyFailed, engine.ClearRetry)},
		{name: "verify timed out at the fix cap", snap: snap(verified(engine.ResultTimeout, 2)), want: halt(engine.StopVerifyFailed, engine.ClearRetry)},
		{name: "verify passed", snap: snap(verified(engine.ResultPassed, 0)), want: engine.Action{Kind: engine.KindExport}},
		{name: "verify waived", snap: snap(verified(engine.ResultWaived, 0)), want: engine.Action{Kind: engine.KindExport}},
		{name: "an endpoint pause holds a session", snap: paused(prepared(), endpointPause), want: wait(time.Time{}), whyHas: "7"},
		{name: "an endpoint pause holds a prepare", snap: paused(startedOnly, endpointPause), want: wait(time.Time{}), whyHas: "7"},
		{name: "an endpoint pause leaves a verify alone", snap: paused(verified("", 0), endpointPause), want: engine.Action{Kind: engine.KindVerify}},
		{name: "an endpoint pause leaves an export alone", snap: paused(verified(engine.ResultPassed, 0), endpointPause), want: engine.Action{Kind: engine.KindExport}},
		{name: "a usage limit pause holds a session", snap: paused(prepared(), limitPause), want: wait(limitPause.LimitedUntil), whyHas: "9", clock: true},
		{name: "a usage limit pause holds a prepare", snap: paused(startedOnly, limitPause), want: wait(limitPause.LimitedUntil), whyHas: "9", clock: true},
		{name: "a usage limit pause leaves a verify alone", snap: paused(verified("", 0), limitPause), want: engine.Action{Kind: engine.KindVerify}},
		{
			name: "a usage limit pause already past", snap: paused(prepared(), engine.Pause{LimitedUntil: now.Add(-time.Minute)}),
			want: work(engine.ModeStart, false), clock: true,
		},
	}
}

func TestDecide(t *testing.T) {
	for _, tc := range decideCases() {
		t.Run(tc.name, func(t *testing.T) { assertDecides(t, tc) })
	}
}

func assertDecides(t *testing.T, tc decideCase) {
	t.Helper()

	got := engine.Decide(tc.snap)

	assert.NotEmpty(t, got.Why)
	assert.NotContains(t, got.Why, "\n")

	if got.Kind == engine.KindStop {
		assert.NotEmpty(t, got.Explain)
		assert.NotContains(t, got.Explain, "\n")
		assert.LessOrEqual(t, len(got.Explain), 300)
		assert.True(t, utf8.ValidString(got.Explain))
	}

	if tc.explainHas != "" {
		assert.Contains(t, got.Explain, tc.explainHas)
	}

	if tc.whyHas != "" {
		assert.Contains(t, got.Why, tc.whyHas)
	}

	got.Why, got.Explain = "", ""
	assert.Equal(t, tc.want, got)
}

func TestDecideFallback(t *testing.T) {
	tests := []struct {
		name  string
		facts engine.Facts
	}{
		{"unknown session outcome", ended(engine.SessionFact{Outcome: "exploded"})},
		{"unknown verify result", verified("skipped", 0)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := engine.Decide(snap(tc.facts))

			assert.NotEmpty(t, got.Explain)

			got.Why, got.Explain = "", ""
			assert.Equal(t, halt(engine.StopAgentError, engine.ClearRetry), got)
		})
	}
}

func TestDecideIsPure(t *testing.T) {
	for _, tc := range slices.Concat(decideCases(), reviewCases(t), watchCases()) {
		t.Run(tc.name, func(t *testing.T) {
			first := engine.Decide(tc.snap)
			for range 100 {
				require.Equal(t, first, engine.Decide(tc.snap))
			}

			later, earlier := tc.snap, tc.snap
			later.Now = now.Add(time.Hour)
			earlier.Now = now.Add(-time.Hour)

			moved := !reflect.DeepEqual(engine.Decide(later), first) || !reflect.DeepEqual(engine.Decide(earlier), first)
			assert.Equal(t, tc.clock, moved)
		})
	}
}

// A usage limit with no usable reset time still waits real time before each
// resume, so the cycle of limit, stop and resume cannot spin.
func TestUsageLimitCycleWaits(t *testing.T) {
	limited := ev(engine.SessionEnded, `{"outcome":"usage_limit"}`)
	events := preparedRun(firstSession, limited)
	at := now

	for round := range 2 {
		s := snap(engine.Reduce(events))
		s.Now = at

		stop := engine.Decide(s)
		require.Equal(t, engine.KindStop, stop.Kind, "round %d", round)
		require.True(t, stop.ResetsAt.After(at), "round %d", round)

		payload, err := json.Marshal(engine.StoppedPayload{Reason: stop.Stop, Explanation: stop.Explain, Action: stop.Clear, ResetsAt: stop.ResetsAt})
		require.NoError(t, err)

		events = append(events, ev(engine.Stopped, string(payload)))
		s = snap(engine.Reduce(events))
		s.Now = at

		wait := engine.Decide(s)
		assert.Equal(t, engine.KindWait, wait.Kind, "round %d", round)
		assert.Equal(t, stop.ResetsAt, wait.Until, "round %d", round)

		at = stop.ResetsAt
		s.Now = at
		resume := engine.Decide(s)
		assert.Equal(t, engine.KindWork, resume.Kind, "round %d", round)
		assert.Equal(t, engine.ModeContinue, resume.WorkMode, "round %d", round)

		events = append(events, ev(engine.SessionStarted, `{"mode":"continue","session_id":"s1","resume":true}`), limited)
	}
}

func TestDecidePublish(t *testing.T) {
	on := engine.Switches{Continuations: true, VerifyFix: true, Publish: true}
	off := engine.Switches{Continuations: true, VerifyFix: true}
	pr := &engine.PRFact{Number: 7, Branch: "cavu/fix-the-login", Head: "head1", Heads: []string{"head1"}}

	// exported is head exported in the open run of a task with pull request pr, or none.
	exported := func(head string, pr *engine.PRFact) engine.Facts {
		f := verified(engine.ResultPassed, 0)
		f.Session.HeadCommit = head
		f.Exported, f.Export, f.PR = true, engine.ExportedPayload{HeadCommit: head}, pr

		return f
	}

	// fixRun is head exported in a run started from the pull request's head.
	fixRun := func(head string) engine.Facts {
		f := exported(head, pr)
		f.RunReason, f.RunHead = engine.ReasonCIFix, pr.Head

		return f
	}

	shadow := snapWith(exported("head1", nil), on)
	shadow.Task.ReplayID = 3

	rejected := exported("head1", nil)
	rejected.OpenStop = &engine.StopFact{Reason: engine.StopPushRejected, Action: engine.ClearRetry}

	publish := engine.Action{Kind: engine.KindPublish}
	finish := engine.Action{Kind: engine.KindFinish}

	for _, tc := range []decideCase{
		{name: "exported with the run open", snap: snapWith(exported("head1", nil), on), want: publish},
		{name: "publish off", snap: snapWith(exported("head1", nil), off), want: finish},
		{name: "a replay's shadow", snap: shadow, want: finish},
		{name: "after the head was published", snap: snapWith(exported("head1", pr), on), want: finish},
		{name: "a fix run's new head", snap: snapWith(fixRun("head2"), on), want: publish},
		{name: "a fix run whose exported head is the pull request's head", snap: snapWith(fixRun("head1"), on), want: finish},
		{name: "a rejected push waits for a person", snap: snapWith(rejected, on), want: engine.Action{Kind: engine.KindNone}},
	} {
		t.Run(tc.name, func(t *testing.T) { assertDecides(t, tc) })
	}
}

// prURL is the address of the task's pull request in the watch's cases.
const prURL = "https://github.com/acme/web/pull/7"

// pull is the task's pull request: CAVU published head1, then head2 at since,
// and o is what was seen of it since.
func pull(since time.Time, o *engine.Observation) *engine.PRFact {
	return &engine.PRFact{
		Number: 7, URL: prURL, NodeID: "PR_7", Branch: "cavu/fix-the-login", Head: "head2", Heads: []string{"head1", "head2"},
		Since: since, Seen: o,
	}
}

// open is an open draft pull request seen at head with checks.
func open(head, checks string, failed ...string) *engine.Observation {
	return &engine.Observation{State: engine.PROpen, Draft: true, Head: head, Checks: checks, Failed: failed}
}

// watched is a task with pull request pr and no run open.
func watched(pr *engine.PRFact) engine.Facts {
	return engine.Facts{Started: true, SessionID: "s1", RunReason: "initial", PR: pr}
}

// fixRun is a prepared fix run of reason on the pull request's head.
func fixRun(reason string, pr *engine.PRFact) engine.Facts {
	f := prepared()
	f.SessionID, f.RunReason, f.RunHead, f.PR = "s1", reason, pr.Head, pr

	return f
}

// watchSnap is s with ci_timeout_minutes at its default.
func watchSnap(f engine.Facts) engine.Snapshot {
	s := snapWith(f, engine.Switches{Continuations: true, VerifyFix: true, Review: true, Publish: true})
	s.Watch.CITimeout = 120 * time.Minute

	return s
}

// watchCases follow the rules that read a pull request, in their order.
func watchCases() []decideCase {
	watch := func(until time.Time) engine.Action { return engine.Action{Kind: engine.KindWatch, Until: until} }
	ready := engine.Action{Kind: engine.KindReady}
	finish := engine.Action{Kind: engine.KindFinish}
	grace := 3 * time.Minute

	byPerson := watched(pull(now, open("head2", engine.ChecksPassed)))
	byPerson.Cancelled = true

	closed := watched(pull(now, &engine.Observation{State: engine.PRClosed, Head: "head2", Checks: engine.ChecksPassed}))
	closed.Cancelled = true

	closedInRun := fixRun(engine.ReasonCIFix, pull(now, &engine.Observation{State: engine.PRClosed, Head: "head2"}))
	closedInRun.Cancelled = true

	mergedInRun := fixRun(engine.ReasonCIFix, pull(now, &engine.Observation{State: engine.PRMerged, Head: "head2"}))
	mergedInRun.Merged = true

	stopped := watched(pull(now, open("head2", engine.ChecksFailed, "lint")))
	stopped.OpenStop = &engine.StopFact{Reason: engine.StopCIFailed}

	askedInRun := fixRun(engine.ReasonComments, pull(now, nil))
	askedInRun.OpenStop = &engine.StopFact{Reason: engine.StopQuestion}

	cancelledStop := watched(pull(now, open("head2", engine.ChecksPassed)))
	cancelledStop.Cancelled, cancelledStop.OpenStop = true, &engine.StopFact{Reason: engine.StopCancelled}

	exportedInRun := fixRun(engine.ReasonCIFix, pull(now, open("theirs", engine.ChecksPending)))
	exportedInRun.Session = &engine.SessionFact{Outcome: engine.OutcomeDone, Changed: true, HeadCommit: "head3"}
	exportedInRun.Verify, exportedInRun.Exported, exportedInRun.Export = engine.ResultPassed, true, engine.ExportedPayload{HeadCommit: "head3"}

	unchanged := func(reason string) engine.Facts {
		f := fixRun(reason, pull(now, open("head2", engine.ChecksFailed, "lint")))
		f.Session = &engine.SessionFact{Outcome: engine.OutcomeDone, HeadCommit: "head2"}

		return f
	}

	fixedHead := func(head string, fixes int) engine.Facts {
		pr := pull(now, open("head2", engine.ChecksFailed, "lint", "test"))
		pr.FixedHead = head
		f := watched(pr)
		f.RunReason, f.CIFixes = engine.ReasonCIFix, fixes

		return f
	}

	asked := func(checks string, runs int, answered ...string) engine.Facts {
		o := open("head2", checks)
		o.Comments = []engine.Comment{{ID: "c1", Author: "ana", Body: "Rename it."}, {ID: "c2", Author: "ana"}, {ID: "c3", Author: "bo"}}
		pr := pull(now, o)
		pr.Answered = answered
		f := watched(pr)
		f.CommentRuns = runs

		return f
	}

	conflict := open("head2", engine.ChecksPassed)
	conflict.Conflict = true

	marked := func(since time.Time, checks string) engine.Facts {
		pr := pull(since, open("head2", checks))
		pr.MarkedReady = true

		return watched(pr)
	}

	notDraft := open("head2", engine.ChecksPassed)
	notDraft.Draft = false

	noCI := func(o *engine.Observation) engine.Snapshot {
		s := watchSnap(watched(pull(now, o)))
		s.Watch.NoCI = true

		return s
	}

	noTimeout := watchSnap(watched(pull(now.Add(-10*time.Hour), open("head2", engine.ChecksPending))))
	noTimeout.Watch.CITimeout = 0

	stillRunning := fixRun(engine.ReasonCIFix, pull(now, open("head2", engine.ChecksFailed, "lint")))
	stillRunning.Prepared = false

	retried := func(reason string, requests ...string) engine.Facts {
		o := open("head2", engine.ChecksFailed, "lint")
		o.Comments = []engine.Comment{{ID: "c1"}, {ID: "c4"}}
		pr := pull(now, o)
		pr.FixedHead, pr.Answered = "head2", requests
		f := watched(pr)
		f.RunReason, f.RunRequests, f.Resumed = reason, requests, &engine.Resume{From: engine.StopEnvironment}

		return f
	}

	commentsRun := fixRun(engine.ReasonComments, pull(now, open("head2", engine.ChecksPassed)))
	commentsRun.RunRequests = []string{"c2", "c3"}

	fixPassed := fixRun(engine.ReasonCIFix, pull(now, nil))
	fixPassed.Session = &engine.SessionFact{Outcome: engine.OutcomeDone, Changed: true, HeadCommit: "head3"}
	fixPassed.Verify = engine.ResultPassed

	endedUnexported := asked(engine.ChecksPassed, 1, "c1", "c2", "c3")
	endedUnexported.RunReason, endedUnexported.RunRequests = engine.ReasonComments, []string{"c1", "c2", "c3"}
	endedUnexported.PR.Seen.Draft = false

	return []decideCase{
		{name: "cancelled by a person, the pull request stays open", snap: watchSnap(byPerson), want: halt(engine.StopCancelled, engine.ClearNone)},
		{name: "the pull request closed without a merge", snap: watchSnap(closed), want: halt(engine.StopCancelled, engine.ClearNone)},
		{name: "the pull request closed while a fix run is open", snap: watchSnap(closedInRun), want: halt(engine.StopCancelled, engine.ClearNone)},
		{name: "merged while a fix run with no export is open", snap: watchSnap(mergedInRun), want: finish},
		{name: "a stop with a pull request is watched", snap: watchSnap(stopped), want: watch(time.Time{})},
		{name: "a stop in a fix run is watched", snap: watchSnap(askedInRun), want: watch(time.Time{})},
		{name: "a cancelled task's pull request is not watched", snap: watchSnap(cancelledStop), want: engine.Action{Kind: engine.KindNone}},
		{name: "a head CAVU never published, no run open", snap: watchSnap(watched(pull(now, open("theirs", engine.ChecksPassed)))), want: halt(engine.StopTakenOver, engine.ClearRetry)},
		{name: "a head CAVU never published, before the open run's publish", snap: watchSnap(exportedInRun), want: halt(engine.StopTakenOver, engine.ClearRetry)},
		{name: "a fix run on failed checks that changed nothing", snap: watchSnap(unchanged(engine.ReasonCIFix)), want: finish},
		{name: "a follow-up run that changed nothing", snap: watchSnap(unchanged(engine.ReasonComments)), want: finish},
		{name: "nothing seen since the head was published", snap: watchSnap(watched(pull(now, nil))), want: watch(time.Time{})},
		{name: "an earlier head of CAVU's own", snap: watchSnap(watched(pull(now, open("head1", engine.ChecksFailed, "lint")))), want: watch(time.Time{})},
		{name: "a conflict", snap: watchSnap(watched(pull(now, conflict))), want: halt(engine.StopConflict, engine.ClearRetry)},
		{name: "failed checks on a head a fix run had", snap: watchSnap(fixedHead("head2", 1)), want: halt(engine.StopCIFailed, engine.ClearRetry), explainHas: "lint"},
		{name: "failed checks after the fix runs are used up", snap: watchSnap(fixedHead("head1", 2)), want: halt(engine.StopCIFailed, engine.ClearRetry), explainHas: "test"},
		{name: "failed checks", snap: watchSnap(fixedHead("head1", 1)), want: engine.Action{Kind: engine.KindPrepare, NewRun: true, Reason: engine.ReasonCIFix}},
		{name: "a check failed while others still run", snap: watchSnap(watched(pull(now, open("head2", engine.ChecksPending, "lint")))), want: watch(now.Add(2 * time.Hour))},
		{name: "requests after the follow-up runs are used up", snap: watchSnap(asked(engine.ChecksPending, 3, "c1")), want: halt(engine.StopChangesRequested, engine.ClearRetry)},
		{
			name: "requests no follow-up run was given", snap: watchSnap(asked(engine.ChecksPending, 2, "c1")),
			want: engine.Action{Kind: engine.KindPrepare, NewRun: true, Reason: engine.ReasonComments, Requests: []string{"c2", "c3"}},
		},
		{name: "every request answered", snap: watchSnap(asked(engine.ChecksPassed, 3, "c1", "c2", "c3")), want: ready},
		{
			name: "checks running past ci_timeout_minutes", snap: watchSnap(watched(pull(now.Add(-2*time.Hour), open("head2", engine.ChecksPending)))),
			want: halt(engine.StopCITimeout, engine.ClearRetry), explainHas: "ci_timeout_minutes", clock: true,
		},
		{
			name: "checks running", snap: watchSnap(watched(pull(now.Add(-90*time.Minute), open("head2", engine.ChecksPending)))),
			want: watch(now.Add(30 * time.Minute)), clock: true,
		},
		{name: "checks running with no ci_timeout_minutes", snap: noTimeout, want: watch(time.Time{})},
		{name: "no check yet, within the grace", snap: watchSnap(watched(pull(now.Add(-time.Minute), open("head2", engine.ChecksNone)))), want: watch(now.Add(grace - time.Minute)), clock: true},
		{name: "no check after the grace", snap: watchSnap(watched(pull(now.Add(-grace), open("head2", engine.ChecksNone)))), want: ready, clock: true},
		{name: "no check since the ready-marking, within the grace", snap: watchSnap(marked(now.Add(-time.Minute), engine.ChecksNone)), want: watch(now.Add(grace - time.Minute)), clock: true},
		{
			name: "no check after the ready-marking's grace", snap: watchSnap(marked(now.Add(-grace), engine.ChecksNone)),
			want: halt(engine.StopNoChecks, engine.ClearRetry), explainHas: "no_ci", clock: true,
		},
		{name: "checks passed on a draft", snap: watchSnap(watched(pull(now, open("head2", engine.ChecksPassed)))), want: ready},
		{name: "no check on a draft of a project without CI", snap: noCI(open("head2", engine.ChecksNone)), want: ready},
		{name: "checks passed, not a draft", snap: watchSnap(watched(pull(now, notDraft))), want: watch(time.Time{})},
		{name: "a draft again after CAVU marked it ready", snap: watchSnap(marked(now, engine.ChecksPassed)), want: watch(time.Time{})},
		{name: "a fix run open is not watched", snap: watchSnap(stillRunning), want: engine.Action{Kind: engine.KindPrepare}},
		{
			name: "a retry after a fix run's failed prepare starts it again", snap: watchSnap(retried(engine.ReasonCIFix)),
			want: engine.Action{Kind: engine.KindPrepare, NewRun: true, Reason: engine.ReasonCIFix},
		},
		{
			name: "a retry after a follow-up run's failed prepare gives its requests and those since", snap: watchSnap(retried(engine.ReasonComments, "c1")),
			want: engine.Action{Kind: engine.KindPrepare, NewRun: true, Reason: engine.ReasonComments, Requests: []string{"c1", "c4"}},
		},
		{name: "a fix run on failed checks starts its session", snap: watchSnap(fixRun(engine.ReasonCIFix, pull(now, nil))), want: work(engine.ModeFixCI, false)},
		{
			name: "a follow-up run starts its session with its requests", snap: watchSnap(commentsRun),
			want: engine.Action{Kind: engine.KindWork, WorkMode: engine.ModeComments, Requests: []string{"c2", "c3"}},
		},
		{name: "a fix run is not reviewed", snap: watchSnap(fixPassed), want: engine.Action{Kind: engine.KindExport}},
		{name: "a follow-up run that ended without an export is watched", snap: watchSnap(endedUnexported), want: watch(time.Time{})},
	}
}

// Every stop of the watch names the pull request's address.
func TestDecideWatch(t *testing.T) {
	for _, tc := range watchCases() {
		t.Run(tc.name, func(t *testing.T) {
			assertDecides(t, tc)

			if got := engine.Decide(tc.snap); got.Kind == engine.KindStop {
				assert.Contains(t, got.Explain, prURL)
			}
		})
	}
}
