package engine_test

import (
	"slices"
	"testing"
	"time"

	"github.com/mhersson/cavu/internal/engine"
	"github.com/mhersson/cavu/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReduceBudget(t *testing.T) {
	t0 := time.Date(2030, 1, 1, 9, 0, 0, 0, time.UTC)

	// at is e recorded d after t0.
	at := func(e store.Event, d time.Duration) store.Event {
		e.At = t0.Add(d)

		return e
	}

	session := []store.Event{
		at(firstSession, 0),
		at(ev(engine.SessionEnded, `{"outcome":"done","changed":true,"head_commit":"def456","turns":20}`), 10*time.Minute),
	}
	review := []store.Event{
		at(ev(engine.ReviewStarted, `{"session_id":"r1","head":"def456"}`), 11*time.Minute),
		at(ev(engine.ReviewEnded, `{"outcome":"reviewed","head":"def456","turns":7}`), 16*time.Minute),
	}
	budgetStop := ev(engine.Stopped, `{"reason":"budget","explanation":"Turns used.","action":"retry"}`)

	tests := []struct {
		name   string
		events []store.Event
		turns  int
		time   time.Duration
	}{
		{name: "a session and a review", events: preparedRun(slices.Concat(session, review)...), turns: 27, time: 15 * time.Minute},
		{name: "a person's retry zeroes both", events: preparedRun(slices.Concat(session, review, []store.Event{budgetStop, retried})...)},
		{name: "a retry after a fault zeroes both", events: preparedRun(slices.Concat(session, review, []store.Event{sandboxStop, retried})...)},
		{name: "a session started and not ended adds nothing", events: preparedRun(at(firstSession, 0))},
		{
			name: "a session a fault cut adds nothing, and the one redone counts from its own start",
			events: preparedRun(at(firstSession, 0), sandboxStop, at(resumeSession, 20*time.Minute),
				at(ev(engine.SessionEnded, `{"outcome":"done","turns":3}`), 24*time.Minute)),
			turns: 3, time: 4 * time.Minute,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := engine.Reduce(tc.events)
			assert.Equal(t, tc.turns, f.Turns)
			assert.Equal(t, tc.time, f.SessionTime)
		})
	}

	t.Run("sessions without a cost count over the whole task", func(t *testing.T) {
		noCost := ev(engine.SessionEnded, `{"outcome":"crashed","cost_missing":true}`)
		f := engine.Reduce(preparedRun(firstSession, noCost, budgetStop, retried, resumeSession, noCost, resumeSession, doneChanged))
		assert.Equal(t, 2, f.CostMissing)
	})
}

// A session's and a review's missing cost reach the record and the facts.
func TestLoopRecordsAMissingCost(t *testing.T) {
	h := newHarness(t)
	h.withReview()
	h.runner.works = []step[engine.WorkResult]{{res: engine.WorkResult{
		Outcome: engine.OutcomeDone, Changed: true, HeadCommit: "head456", CostMissing: true,
	}}}
	h.runner.reviews = []step[engine.ReviewResult]{{res: engine.ReviewResult{Outcome: engine.ReviewFailed, CostMissing: true}}}

	_, err := h.run(t.Context())
	require.NoError(t, err)

	assert.True(t, payloadOf[engine.SessionEndedPayload](t, h.eventsOf(engine.SessionEnded)[0]).CostMissing)
	assert.True(t, payloadOf[engine.ReviewEndedPayload](t, h.eventsOf(engine.ReviewEnded)[0]).CostMissing)

	s, err := h.loop.Snapshot(t.Context(), h.task.ID)
	require.NoError(t, err)
	assert.Equal(t, 2, s.Facts.CostMissing)
}

func TestDecideBudget(t *testing.T) {
	limits := engine.Limits{Turns: 400, SessionTime: 120 * time.Minute}

	// used is f, with the review on, and the turns and session time the task used since a person last acted.
	used := func(f engine.Facts, turns int, d time.Duration) engine.Snapshot {
		f.Turns, f.SessionTime = turns, d
		s := snapWith(f, reviewOn)
		s.Limits = limits

		return s
	}

	turnsUsed, timeUsed := halt(engine.StopBudget, engine.ClearRetry), halt(engine.StopTimeout, engine.ClearRetry)

	exported := used(passedAt("h1"), 400, 120*time.Minute)
	exported.Switches.Review = false

	reviewHalted := passedAt("h1")
	reviewHalted.Review.Halt = &engine.SessionFact{Outcome: engine.OutcomeMaxTurns}

	cases := []decideCase{
		{name: "a work action with the turns used up", snap: used(prepared(), 400, 0), want: turnsUsed, explainHas: "400"},
		{name: "a work action with the time used up", snap: used(prepared(), 0, 120*time.Minute), want: timeUsed, explainHas: "120"},
		{name: "a review action with the turns used up", snap: used(passedAt("h1"), 401, 0), want: turnsUsed, explainHas: "401"},
		{name: "a review action with the time used up", snap: used(passedAt("h1"), 0, 121*time.Minute), want: timeUsed, explainHas: "121"},
		{name: "a prepare is left alone", snap: used(engine.Facts{Started: true}, 400, 120*time.Minute), want: engine.Action{Kind: engine.KindPrepare, NewRun: true}},
		{name: "a verify is left alone", snap: used(verified("", 0), 400, 120*time.Minute), want: engine.Action{Kind: engine.KindVerify}},
		{name: "an export is left alone", snap: exported, want: engine.Action{Kind: engine.KindExport}},
		{name: "one turn below the cap", snap: used(prepared(), 399, 119*time.Minute), want: work(engine.ModeStart, false)},
		{name: "a review one turn below the cap", snap: used(passedAt("h1"), 399, 0), want: engine.Action{Kind: engine.KindReview}},
		{
			name: "a session that used its turns names the task's", snap: used(ended(engine.SessionFact{Outcome: engine.OutcomeMaxTurns}), 400, 0),
			want: turnsUsed, explainHas: "400",
		},
		{
			name: "a session that timed out names the task's time", snap: used(ended(engine.SessionFact{Outcome: engine.OutcomeTimeout}), 0, 120*time.Minute),
			want: timeUsed, explainHas: "120",
		},
		{name: "a review that used its turns names the task's", snap: used(reviewHalted, 400, 0), want: turnsUsed, explainHas: "400"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { assertDecides(t, tc) })
	}
}

// maxTurns is the turns each work call was given.
func maxTurns(calls []call) []int {
	out := make([]int, len(calls))
	for i, c := range calls {
		out[i] = c.cfg.Agent.MaxTurns
	}

	return out
}

func TestLoopGivesASessionWhatIsLeft(t *testing.T) {
	h := newHarness(t)
	h.set("task_max_turns", "400")
	h.set("task_timeout_minutes", "120")

	// The first session runs 30 minutes, uses 150 turns and stops early, so a continuation follows.
	h.runner.works = []step[engine.WorkResult]{{res: engine.WorkResult{Outcome: engine.OutcomeUnfinished, Turns: 150}}}
	h.runner.onCall = func(c call) {
		if c.method == "work" {
			h.clock.Advance(30 * time.Minute)
		}
	}

	_, err := h.run(t.Context())
	require.NoError(t, err)

	works := h.runner.callsOf("work")
	require.Len(t, works, 2)
	assert.Equal(t, []int{400, 250}, maxTurns(works))
	assert.Equal(t, 120*time.Minute, works[0].cfg.Agent.Timeout)
	assert.Equal(t, 90*time.Minute, works[1].cfg.Agent.Timeout)
}

func TestLoopBudgetAcrossSessions(t *testing.T) {
	h := newHarness(t)
	h.set("task_max_turns", "400")

	// Each session would use 150 turns; the third, given the 100 left, uses them up.
	done := step[engine.WorkResult]{res: engine.WorkResult{Outcome: engine.OutcomeDone, Changed: true, HeadCommit: "head456", Turns: 150}}
	failed := step[engine.VerifyResult]{res: engine.VerifyResult{Result: engine.ResultFailed, Command: "make test", Output: "FAIL"}}
	h.runner.works = []step[engine.WorkResult]{done, done, {res: engine.WorkResult{Outcome: engine.OutcomeMaxTurns, Turns: 100}}}
	h.runner.verifies = []step[engine.VerifyResult]{failed, failed}

	status, err := h.run(t.Context())
	require.NoError(t, err)
	assert.Equal(t, engine.StatusBlocked, status)
	assert.Equal(t, engine.StopBudget, h.lastStop().Reason)
	assert.Equal(t, []int{400, 250, 100}, maxTurns(h.runner.callsOf("work")), "no fourth session")

	h.add(0, engine.HumanRetried, "user", "")

	_, err = h.run(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 400, maxTurns(h.runner.callsOf("work"))[3], "a retry gives the whole budget again")
}

func TestProjectCaps(t *testing.T) {
	t.Run("a project's cap beats the setting", func(t *testing.T) {
		h := newHarness(t)
		h.set("task_max_turns", "400")
		h.set("task_timeout_minutes", "120")

		p := h.project()
		p.Caps = map[string]string{"task_max_turns": "50"}
		require.NoError(t, engine.EditProject(t.Context(), h.store, p))

		s, err := h.loop.Snapshot(t.Context(), h.task.ID)
		require.NoError(t, err)
		assert.Equal(t, engine.Limits{Turns: 50, SessionTime: 120 * time.Minute}, s.Limits)
	})

	t.Run("a cap the project may not hold is refused", func(t *testing.T) {
		for _, caps := range []map[string]string{{"concurrency": "2"}, {"task_max_turns": "-1"}} {
			h := newHarness(t)

			add := validProject()
			add.Caps = caps

			_, err := engine.AddProject(t.Context(), h.store, add)
			require.ErrorIs(t, err, engine.ErrRefused, caps)
			assert.Contains(t, err.Error(), "caps")

			before := h.project()
			edit := before
			edit.Caps = caps

			require.ErrorIs(t, engine.EditProject(t.Context(), h.store, edit), engine.ErrRefused, caps)
			assert.Equal(t, before, h.project(), "nothing is changed")
		}
	})

	t.Run("an edit of the caps keeps the baseline", func(t *testing.T) {
		h := newHarness(t)
		stored := h.withBaseline().Baseline

		edit := h.project()
		edit.Caps = map[string]string{"task_timeout_minutes": "60"}
		require.NoError(t, engine.EditProject(t.Context(), h.store, edit))

		got := h.project()
		require.NotNil(t, got.Baseline)
		assert.Equal(t, stored.Commit, got.Baseline.Commit)
		assert.Equal(t, edit.Caps, got.Caps)
	})
}
