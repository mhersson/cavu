package engine_test

import (
	"encoding/json"
	"testing"
	"time"

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
	clock      bool   // the action changes when Now moves an hour
}

func snap(f engine.Facts) engine.Snapshot {
	return snapWith(f, engine.Switches{Continuations: true, VerifyFix: true})
}

func snapWith(f engine.Facts, sw engine.Switches) engine.Snapshot {
	return engine.Snapshot{Facts: f, Now: now, Switches: sw, Caps: engine.Caps{Continuations: 2, VerifyFixes: 2}}
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
	contOff := engine.Switches{VerifyFix: true}
	fixOff := engine.Switches{Continuations: true}

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

	return []decideCase{
		{name: "not started", snap: snap(engine.Facts{}), want: none},
		{name: "cancelled", snap: snap(engine.Facts{Started: true, Cancelled: true, RunOpen: true}), want: halt(engine.StopCancelled, engine.ClearNone)},
		{name: "cancelled with another stop open", snap: snap(cancelledQuestion), want: halt(engine.StopCancelled, engine.ClearNone)},
		{name: "usage limit before its reset", snap: snap(openStop(engine.StopUsageLimit, resets)), want: engine.Action{Kind: engine.KindWait, Until: resets}, clock: true},
		{name: "usage limit reset reached", snap: snap(openStop(engine.StopUsageLimit, reset)), want: work(engine.ModeContinue, false), clock: true},
		{name: "usage limit with no reset time", snap: snap(openStop(engine.StopUsageLimit, time.Time{})), want: work(engine.ModeContinue, false)},
		{name: "another open stop", snap: snap(openStop(engine.StopQuestion, time.Time{})), want: none},
		{name: "cancelled with its stop recorded", snap: snap(cancelledStop), want: none},
		{name: "exported with the run open", snap: snap(exportedOpen), want: engine.Action{Kind: engine.KindFinish}},
		{name: "exported", snap: snap(exported), want: none},
		{name: "no open run", snap: snap(startedOnly), want: engine.Action{Kind: engine.KindPrepare, NewRun: true}},
		{name: "resumed from environment", snap: snap(resumed(engine.StopEnvironment, "")), want: engine.Action{Kind: engine.KindPrepare, NewRun: true}},
		{name: "resumed from sandbox", snap: snap(resumed(engine.StopSandbox, "")), want: engine.Action{Kind: engine.KindPrepare, NewRun: true}},
		{name: "run not prepared", snap: snap(unprepared), want: engine.Action{Kind: engine.KindPrepare}},
		{name: "prepare failed", snap: snap(prepareFailed), want: halt(engine.StopEnvironment, engine.ClearRetry), explainHas: "setup"},
		{
			name: "resumed from a question with an answer", snap: snap(resumed(engine.StopQuestion, "Use port 8080.")),
			want: engine.Action{Kind: engine.KindWork, WorkMode: engine.ModeAnswer, Detail: "Use port 8080."},
		},
		{name: "resumed from verify failed", snap: snap(resumed(engine.StopVerifyFailed, "")), want: work(engine.ModeFixVerify, false)},
		{name: "resumed from an agent error", snap: snap(resumed(engine.StopAgentError, "")), want: work(engine.ModeContinue, false)},
		{name: "resumed from a question without an answer", snap: snap(resumed(engine.StopQuestion, "")), want: work(engine.ModeContinue, false)},
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
		{name: "session endpoint", snap: snap(ended(engine.SessionFact{Outcome: engine.OutcomeEndpoint})), want: halt(engine.StopEndpoint, engine.ClearRetry)},
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
		{name: "unfinished with no continuation yet", snap: snap(cut(engine.OutcomeUnfinished, 0)), want: work(engine.ModeContinue, true)},
		{name: "unfinished with one continuation", snap: snap(cut(engine.OutcomeUnfinished, 1)), want: work(engine.ModeContinue, true)},
		{name: "unfinished at the continuation cap", snap: snap(cut(engine.OutcomeUnfinished, 2)), want: halt(engine.StopAgentError, engine.ClearRetry)},
		{name: "unfinished with continuations off", snap: snapWith(cut(engine.OutcomeUnfinished, 0), contOff), want: halt(engine.StopAgentError, engine.ClearRetry)},
		{name: "crashed with no continuation yet", snap: snap(cut(engine.OutcomeCrashed, 0)), want: work(engine.ModeContinue, true)},
		{name: "crashed at the continuation cap", snap: snap(cut(engine.OutcomeCrashed, 2)), want: halt(engine.StopAgentError, engine.ClearRetry)},
		{name: "done without a change", snap: snap(ended(engine.SessionFact{Outcome: engine.OutcomeDone})), want: halt(engine.StopNoChange, engine.ClearRetry)},
		{name: "done and not verified", snap: snap(verified("", 0)), want: engine.Action{Kind: engine.KindVerify}},
		{name: "verify failed with no fix yet", snap: snap(verified(engine.ResultFailed, 0)), want: work(engine.ModeFixVerify, true)},
		{name: "verify timed out with one fix", snap: snap(verified(engine.ResultTimeout, 1)), want: work(engine.ModeFixVerify, true)},
		{name: "verify failed at the fix cap", snap: snap(verified(engine.ResultFailed, 2)), want: halt(engine.StopVerifyFailed, engine.ClearRetry)},
		{name: "verify failed with verify fix off", snap: snapWith(verified(engine.ResultFailed, 0), fixOff), want: halt(engine.StopVerifyFailed, engine.ClearRetry)},
		{name: "verify timed out at the fix cap", snap: snap(verified(engine.ResultTimeout, 2)), want: halt(engine.StopVerifyFailed, engine.ClearRetry)},
		{name: "verify passed", snap: snap(verified(engine.ResultPassed, 0)), want: engine.Action{Kind: engine.KindExport}},
		{name: "verify waived", snap: snap(verified(engine.ResultWaived, 0)), want: engine.Action{Kind: engine.KindExport}},
	}
}

func TestDecide(t *testing.T) {
	for _, tc := range decideCases() {
		t.Run(tc.name, func(t *testing.T) {
			got := engine.Decide(tc.snap)

			assert.NotEmpty(t, got.Why)
			assert.NotContains(t, got.Why, "\n")

			if got.Kind == engine.KindStop {
				assert.NotEmpty(t, got.Explain)
				assert.NotContains(t, got.Explain, "\n")
			}

			if tc.explainHas != "" {
				assert.Contains(t, got.Explain, tc.explainHas)
			}

			got.Why, got.Explain = "", ""
			assert.Equal(t, tc.want, got)
		})
	}
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
	for _, tc := range decideCases() {
		t.Run(tc.name, func(t *testing.T) {
			first := engine.Decide(tc.snap)
			for range 100 {
				require.Equal(t, first, engine.Decide(tc.snap))
			}

			later, earlier := tc.snap, tc.snap
			later.Now = now.Add(time.Hour)
			earlier.Now = now.Add(-time.Hour)

			moved := engine.Decide(later) != first || engine.Decide(earlier) != first
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
