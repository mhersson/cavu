package engine_test

import (
	"encoding/json"
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

	t.Run("the newest human action is where the spend counts from", func(t *testing.T) {
		// numbered gives each event its id, in order.
		numbered := func(events ...store.Event) []store.Event {
			for i := range events {
				events[i].ID = int64(i + 1)
			}

			return events
		}

		answered := ev(engine.HumanAnswered, `{"text":"Use 8080."}`)
		cancelled := ev(engine.HumanCancelled, "")
		question := ev(engine.Stopped, `{"reason":"question","explanation":"Which port?","action":"answer"}`)

		for _, tc := range []struct {
			name   string
			events []store.Event
			want   int64
		}{
			{"the start", numbered(preparedRun(firstSession, unfinished)...), 1},
			{"an answer", numbered(preparedRun(firstSession, question, answered, resumeSession)...), 7},
			{"a retry", numbered(preparedRun(firstSession, budgetStop, retried, resumeSession)...), 7},
			{"a retry after a fault", numbered(preparedRun(firstSession, sandboxStop, retried, resumeSession)...), 7},
			{"a cancel", numbered(preparedRun(firstSession, cancelled)...), 6},
		} {
			t.Run(tc.name, func(t *testing.T) {
				assert.Equal(t, tc.want, engine.Reduce(tc.events).LastHumanEventID)
			})
		}
	})

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

	t.Run("an empty cap is not stored and leaves the setting", func(t *testing.T) {
		h := newHarness(t)
		h.set("task_max_turns", "400")

		add := validProject()
		add.Caps = map[string]string{"task_max_turns": ""}

		added, err := engine.AddProject(t.Context(), h.store, add)
		require.NoError(t, err)
		assert.NotContains(t, added.Caps, "task_max_turns")

		edit := h.project()
		edit.Caps = map[string]string{"task_max_turns": ""}
		require.NoError(t, engine.EditProject(t.Context(), h.store, edit))
		assert.NotContains(t, h.project().Caps, "task_max_turns")
		assert.Equal(t, map[string]string{"task_max_turns": ""}, edit.Caps, "the caller's map is left alone")

		s, err := h.loop.Snapshot(t.Context(), h.task.ID)
		require.NoError(t, err)
		assert.Equal(t, 400, s.Limits.Turns)
	})

	t.Run("a cap the project may not hold is refused", func(t *testing.T) {
		for _, caps := range []map[string]string{{"concurrency": "2"}, {"task_max_turns": "-1"}, {"daily_budget_usd": "5"}, {"task_budget_usd": "-1"}} {
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

func TestPrices(t *testing.T) {
	got, err := engine.Prices(map[string]string{"model_prices": `{
		"claude-sonnet-4-5-20250929": {"input": 3, "output": 15, "cache_read": 0.3, "cache_write": 3.75},
		"*": {"input": 15, "output": 75, "cache_read": 1.5, "cache_write": 18.75}}`})
	require.NoError(t, err)
	assert.Equal(t, map[string]engine.Price{
		"claude-sonnet-4-5-20250929": {Input: 3, Output: 15, CacheRead: 0.3, CacheWrite: 3.75},
		"*":                          {Input: 15, Output: 75, CacheRead: 1.5, CacheWrite: 18.75},
	}, got)

	none, err := engine.Prices(map[string]string{})
	require.NoError(t, err)
	assert.Empty(t, none, "unset gives none")

	for _, bad := range []string{
		`{"m": {"input": -1, "output": 1, "cache_read": 1, "cache_write": 1}}`,
		`{"m": {"input": 1, "output": 1, "cache_read": 1, "cache_write": 1, "cache_raed": 1}}`,
		`{"m": null}`,
		`[{"input": 1}]`,
		`"cheap"`,
	} {
		_, err := engine.Prices(map[string]string{"model_prices": bad})
		require.Error(t, err, bad)
		require.Error(t, engine.CheckSetting("model_prices", bad), bad)
	}

	// A kind of token left out would count as free.
	for _, field := range []string{"input", "output", "cache_read", "cache_write"} {
		price := map[string]float64{"input": 3, "output": 15, "cache_read": 0.3, "cache_write": 3.75}
		delete(price, field)

		raw, err := json.Marshal(map[string]any{"claude-x": price})
		require.NoError(t, err)

		_, err = engine.Prices(map[string]string{"model_prices": string(raw)})
		require.ErrorContains(t, err, "claude-x", field)
		require.ErrorContains(t, err, field)
		require.Error(t, engine.CheckSetting("model_prices", string(raw)), field)
	}
}

// sonnet is a price list's row, in dollars per million tokens.
var sonnet = engine.Price{Input: 3, Output: 15, CacheRead: 0.3, CacheWrite: 3.75}

// sonnetUse costs 51,000 micro-dollars at sonnet: 3,000 + 30,000 + 3,000 + 15,000.
func sonnetUse(source, model string) store.Usage {
	return store.Usage{Source: source, Model: model, Input: 1000, Output: 2000, CacheRead: 10000, CacheWrite: 4000}
}

func TestCost(t *testing.T) {
	tests := []struct {
		name     string
		usage    []store.Usage
		prices   map[string]engine.Price
		want     int64
		unpriced []string
	}{
		{name: "a model at its price", usage: []store.Usage{sonnetUse("proxy", "s")}, prices: map[string]engine.Price{"s": sonnet}, want: 51000},
		{
			name:   "rows add up",
			usage:  []store.Usage{sonnetUse("proxy", "s"), {Source: "proxy", Model: "o", Input: 100, Output: 200, CacheRead: 1000, CacheWrite: 40}},
			prices: map[string]engine.Price{"s": sonnet, "o": {Input: 15, Output: 75, CacheRead: 1.5, CacheWrite: 18.75}}, want: 51000 + 18750,
		},
		{
			// 0.4 + 7.5 + 0.3 + 0.45 costs 8, not 9.
			name: "each bucket rounds on its own", usage: []store.Usage{{Source: "proxy", Model: "m", Input: 1, Output: 3, CacheRead: 1, CacheWrite: 1}},
			prices: map[string]engine.Price{"m": {Input: 0.4, Output: 2.5, CacheRead: 0.3, CacheWrite: 0.45}}, want: 8,
		},
		{
			name:   "a model without a price costs nothing and is named",
			usage:  []store.Usage{sonnetUse("proxy", "b"), sonnetUse("proxy", "s"), sonnetUse("proxy", "a")},
			prices: map[string]engine.Price{"s": sonnet}, want: 51000, unpriced: []string{"a", "b"},
		},
		{name: "the default prices a model without its own", usage: []store.Usage{sonnetUse("proxy", "s")}, prices: map[string]engine.Price{"*": sonnet}, want: 51000},
		{
			name: "a model's own price wins over the default", usage: []store.Usage{sonnetUse("proxy", "s")},
			prices: map[string]engine.Price{"s": sonnet, "*": {}}, want: 51000,
		},
		{
			name: "without the default a model without its own price is named", usage: []store.Usage{sonnetUse("proxy", "s")},
			prices: map[string]engine.Price{"t": sonnet}, unpriced: []string{"s"},
		},
		{
			name: "rows the agent reported are left out", usage: []store.Usage{sonnetUse("agent", "s"), sonnetUse("agent", "x")},
			prices: map[string]engine.Price{"s": sonnet},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, unpriced := engine.Cost(tc.usage, tc.prices)
			assert.Equal(t, tc.want, got)
			assert.Equal(t, tc.unpriced, unpriced)
		})
	}
}

func TestDecideMoney(t *testing.T) {
	const taskCap, dayCap = 5_000_000, 20_000_000

	zone := time.FixedZone("UTC+5", 5*60*60)
	evening := time.Date(2030, 1, 1, 22, 30, 0, 0, zone)
	midnight := engine.Action{Kind: engine.KindWait, Until: time.Date(2030, 1, 2, 0, 0, 0, 0, zone)}
	budgetStop := halt(engine.StopBudget, engine.ClearRetry)

	// spent is f, with the review on and both caps set, after spending task and day micro-dollars.
	spent := func(f engine.Facts, task, day int64, unpriced ...string) engine.Snapshot {
		s := snapWith(f, reviewOn)
		s.Now = evening
		s.Limits.TaskMicroUSD, s.Limits.DayMicroUSD = taskCap, dayCap
		s.Spent = engine.Spent{TaskMicroUSD: task, DayMicroUSD: day, Unpriced: unpriced}

		return s
	}

	dayOnly := spent(prepared(), 0, 0, "claude-haiku-x")
	dayOnly.Limits.TaskMicroUSD = 0

	// priced decides a task that used sonnetUse of claude-opus-x at prices.
	priced := func(prices map[string]engine.Price) engine.Snapshot {
		cost, unpriced := engine.Cost([]store.Usage{sonnetUse("proxy", "claude-opus-x")}, prices)

		return spent(prepared(), cost, cost, unpriced...)
	}

	sessionEnded := func(outcome, detail string) engine.Facts {
		return ended(engine.SessionFact{Outcome: outcome, Detail: detail})
	}

	reviewHalted := func(outcome, detail string) engine.Facts {
		f := passedAt("h1")
		f.Review.Halt = &engine.SessionFact{Outcome: outcome, Detail: detail}

		return f
	}

	unmetered := `Model "claude-haiku-x" has no price, so the run's spend cannot be counted.`
	exported := spent(passedAt("h1"), taskCap, dayCap)
	exported.Switches.Review = false

	cases := []decideCase{
		{name: "the task's cap reached", snap: spent(prepared(), taskCap, 0), want: budgetStop, explainHas: "5.00"},
		{name: "the task's cap reached before a review", snap: spent(passedAt("h1"), taskCap+1, 0), want: budgetStop},
		{name: "a micro-dollar below the task's cap", snap: spent(prepared(), taskCap-1, dayCap-1), want: work(engine.ModeStart, false)},
		{name: "a model without a price", snap: spent(prepared(), 0, 0, "claude-haiku-x"), want: budgetStop, explainHas: "claude-haiku-x"},
		{name: "a model without a price under the daily cap alone", snap: dayOnly, want: budgetStop, explainHas: "claude-haiku-x"},
		{name: "a model priced by the default", snap: priced(map[string]engine.Price{"*": sonnet}), want: work(engine.ModeStart, false)},
		{name: "a model without its own price and no default", snap: priced(map[string]engine.Price{"s": sonnet}), want: budgetStop, explainHas: "claude-opus-x"},
		{name: "the day's cap reached", snap: spent(prepared(), 0, dayCap), want: midnight},
		{name: "the day's cap holds a prepare", snap: spent(engine.Facts{Started: true}, 0, dayCap), want: midnight},
		{name: "the day's cap holds a review", snap: spent(passedAt("h1"), 0, dayCap), want: midnight},
		{name: "the day's cap leaves a verify alone", snap: spent(verified("", 0), taskCap, dayCap), want: engine.Action{Kind: engine.KindVerify}},
		{name: "the caps leave an export alone", snap: exported, want: engine.Action{Kind: engine.KindExport}},
		{name: "a session the proxy could not meter", snap: spent(sessionEnded(engine.OutcomeUnmetered, unmetered), 0, 0), want: budgetStop, explainHas: "claude-haiku-x"},
		{name: "a review the proxy could not meter", snap: spent(reviewHalted(engine.OutcomeUnmetered, unmetered), 0, 0), want: budgetStop, explainHas: "claude-haiku-x"},
		{name: "a session that spent its budget with the task's cap reached", snap: spent(sessionEnded(engine.OutcomeBudget, ""), taskCap, 0), want: budgetStop},
		{name: "a session that spent its budget with the day's cap reached", snap: spent(sessionEnded(engine.OutcomeBudget, ""), 0, dayCap), want: midnight},
		{name: "a session that spent its budget with neither reached", snap: spent(sessionEnded(engine.OutcomeBudget, ""), 0, 0), want: work(engine.ModeContinue, false)},
		{name: "a review that spent its budget is reviewed again", snap: spent(reviewHalted(engine.OutcomeBudget, ""), 0, 0), want: engine.Action{Kind: engine.KindReview}},
		{name: "a review that spent its budget with the task's cap reached", snap: spent(reviewHalted(engine.OutcomeBudget, ""), taskCap, 0), want: budgetStop},
		{name: "a review that spent its budget with the day's cap reached", snap: spent(reviewHalted(engine.OutcomeBudget, ""), 0, dayCap), want: midnight},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { assertDecides(t, tc) })
	}
}

// keyMode sets the harness to key mode, every model at one dollar per million tokens of each kind.
func (h *harness) keyMode() {
	h.t.Helper()

	h.set("endpoint_mode", "key")
	h.set("endpoint_url", "https://gateway.example")
	h.set("endpoint_auth_header", "x-api-key")
	h.set("model_prices", `{"*": {"input": 1, "output": 1, "cache_read": 1, "cache_write": 1}}`)
}

// spend records usage of model by task as the proxy counted it, at at.
func (h *harness) spend(taskID int64, source, model string, tokens int64, at time.Time) {
	h.t.Helper()

	_, err := h.store.Append(h.t.Context(), taskID, -1,
		[]store.Event{{At: at, Type: engine.AgentText, Actor: "agent", Payload: json.RawMessage(`{"text":"..."}`)}},
		[]store.Usage{{Source: source, Model: model, Input: tokens}})
	require.NoError(h.t, err)
}

// setCaps gives the harness's project caps over the settings.
func (h *harness) setCaps(caps map[string]string) {
	h.t.Helper()

	p := h.project()
	p.Caps = caps
	require.NoError(h.t, engine.EditProject(h.t.Context(), h.store, p))
}

func TestSnapshotReadsTheSpend(t *testing.T) {
	h := newHarness(t)
	h.keyMode()
	h.set("model_prices", `{"claude-x": {"input": 1, "output": 1, "cache_read": 1, "cache_write": 1}}`)
	h.set("task_budget_usd", "5")
	h.set("daily_budget_usd", "20")

	other := h.draft()

	h.spend(h.task.ID, "proxy", "claude-x", 1_000_000, now)
	h.add(0, engine.HumanRetried, "user", "")
	h.spend(h.task.ID, "proxy", "claude-x", 2_000_000, now)
	h.spend(h.task.ID, "agent", "claude-x", 7_000_000, now)
	h.spend(h.task.ID, "proxy", "claude-new", 10, now)
	h.spend(other.ID, "proxy", "claude-x", 4_000_000, now)
	h.spend(other.ID, "proxy", "claude-x", 8_000_000, now.Add(-24*time.Hour))

	s, err := h.loop.Snapshot(t.Context(), h.task.ID)
	require.NoError(t, err)
	assert.Equal(t, engine.Limits{Turns: 1000, SessionTime: 300 * time.Minute, TaskMicroUSD: 5_000_000, DayMicroUSD: 20_000_000}, s.Limits)
	assert.Equal(t, engine.Spent{TaskMicroUSD: 2_000_000, DayMicroUSD: 7_000_000, Unpriced: []string{"claude-new"}}, s.Spent,
		"the task's since the retry, every task's today, the proxy's rows only")

	h.set("model_prices", `{"claude-x": {"input": 1, "output": 1, "cache_read": 1, "cache_write": 1}, "*": {"input": 2, "output": 2, "cache_read": 2, "cache_write": 2}}`)

	s, err = h.loop.Snapshot(t.Context(), h.task.ID)
	require.NoError(t, err)
	assert.Equal(t, engine.Spent{TaskMicroUSD: 2_000_020, DayMicroUSD: 7_000_020}, s.Spent, "the default prices the new model")
}

// budgets is the budget each of the runner's calls but the releases was given.
func budgets(calls []call) []int64 {
	var out []int64

	for _, c := range calls {
		if c.method != "release" {
			out = append(out, c.cfg.Budget.MicroUSD)
		}
	}

	return out
}

func TestLoopPassesWhatIsLeft(t *testing.T) {
	tests := []struct {
		name     string
		settings map[string]string
		caps     map[string]string
		capped   bool
		want     int64
	}{
		{name: "the task's cap", settings: map[string]string{"task_budget_usd": "5"}, capped: true, want: 1_800_000},
		// The day spent the task's 3.20 and another task's 0.30 of its 4.
		{name: "the day's cap leaves less", settings: map[string]string{"task_budget_usd": "5", "daily_budget_usd": "4"}, capped: true, want: 500_000},
		{name: "no cap"},
		{name: "caps of 0", settings: map[string]string{"task_budget_usd": "0", "daily_budget_usd": "0"}},
		{name: "a project's 0 under the instance's cap", settings: map[string]string{"task_budget_usd": "5"}, caps: map[string]string{"task_budget_usd": "0"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.keyMode()

			for k, v := range tc.settings {
				h.set(k, v)
			}

			if tc.caps != nil {
				h.setCaps(tc.caps)
			}

			other := h.draft()
			h.spend(h.task.ID, "proxy", "claude-x", 3_200_000, now)
			h.spend(other.ID, "proxy", "claude-x", 300_000, now)

			_, err := h.run(t.Context())
			require.NoError(t, err)

			works := h.runner.callsOf("work")
			require.Len(t, works, 1)

			b := works[0].cfg.Budget
			assert.Equal(t, tc.capped, b.Capped)
			assert.Equal(t, tc.want, b.MicroUSD)

			if !tc.capped {
				assert.Nil(t, b.Prices)

				return
			}

			assert.Equal(t, map[string]engine.Price{"*": {Input: 1, Output: 1, CacheRead: 1, CacheWrite: 1}}, b.Prices)
			assert.Equal(t, []int64{tc.want, tc.want, tc.want, tc.want}, budgets(h.runner.calls), "prepare, work, verify and export")
		})
	}
}

// Without a money cap nothing is priced and nothing spent stops a task: under
// a subscription, the caps are not read at all.
func TestNoMoneyCapChangesNothing(t *testing.T) {
	tests := []struct {
		name     string
		key      bool
		settings map[string]string
		caps     map[string]string
	}{
		{name: "a subscription with caps set", settings: map[string]string{"task_budget_usd": "1", "daily_budget_usd": "1"}},
		{name: "key mode without a cap", key: true},
		{name: "key mode with caps of 0", key: true, settings: map[string]string{"task_budget_usd": "0", "daily_budget_usd": "0"}},
		{name: "key mode with a project's 0", key: true, settings: map[string]string{"task_budget_usd": "1"}, caps: map[string]string{"task_budget_usd": "0"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			if tc.key {
				h.keyMode()
			}

			for k, v := range tc.settings {
				h.set(k, v)
			}

			if tc.caps != nil {
				h.setCaps(tc.caps)
			}

			// Prices that do not parse show that nothing reads them.
			h.set("model_prices", "not prices")
			h.spend(h.task.ID, "proxy", "claude-unpriced", 1_000_000_000, now)

			s, err := h.loop.Snapshot(t.Context(), h.task.ID)
			require.NoError(t, err)
			assert.Zero(t, s.Limits.TaskMicroUSD)
			assert.Zero(t, s.Limits.DayMicroUSD)
			assert.Zero(t, s.Spent)

			status, err := h.run(t.Context())
			require.NoError(t, err)
			assert.Equal(t, engine.StatusInReview, status)
			assert.False(t, h.runner.callsOf("work")[0].cfg.Budget.Capped)
		})
	}
}

func TestLoopBudgetStopAndRetry(t *testing.T) {
	h := newHarness(t)
	h.keyMode()
	h.set("task_budget_usd", "5")

	h.runner.works = []step[engine.WorkResult]{{res: engine.WorkResult{
		Outcome: engine.OutcomeBudget, Turns: 4, Usage: []store.Usage{{Source: "proxy", Model: "claude-x", Input: 5_000_100}},
	}}}

	status, err := h.run(t.Context())
	require.NoError(t, err)
	assert.Equal(t, engine.StatusBlocked, status)

	stop := h.lastStop()
	assert.Equal(t, engine.StopBudget, stop.Reason)
	assert.Equal(t, engine.ClearRetry, stop.Action)

	works := h.runner.callsOf("work")
	require.Len(t, works, 1)
	assert.Equal(t, int64(5_000_000), works[0].cfg.Budget.MicroUSD)

	h.add(0, engine.HumanRetried, "user", "")

	_, err = h.run(t.Context())
	require.NoError(t, err)

	works = h.runner.callsOf("work")
	require.Len(t, works, 2)
	assert.Equal(t, engine.ModeContinue, works[1].work.Mode)
	assert.Equal(t, int64(5_000_000), works[1].cfg.Budget.MicroUSD, "a retry gives the whole budget again")
}

func TestLoopDailyCapWaits(t *testing.T) {
	h := newHarness(t)
	h.keyMode()
	h.set("task_budget_usd", "5")
	h.set("daily_budget_usd", "1")

	// An hour before midnight where the server runs, and the afternoon in UTC.
	zone := time.FixedZone("UTC+5", 5*60*60)
	h.clock.at = time.Date(2030, 1, 1, 23, 0, 0, 0, zone)

	h.runner.works = []step[engine.WorkResult]{{res: engine.WorkResult{
		Outcome: engine.OutcomeBudget, Usage: []store.Usage{{Source: "proxy", Model: "claude-x", Input: 1_000_000}},
	}}}

	_, err := h.run(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []string{"prepare", "work/start", "release/keep"}, h.runner.trace(), "a waiting task holds no container")

	s, err := h.loop.Snapshot(t.Context(), h.task.ID)
	require.NoError(t, err)

	wait := engine.Decide(s)
	assert.Equal(t, engine.KindWait, wait.Kind)
	assert.Equal(t, time.Date(2030, 1, 2, 0, 0, 0, 0, zone), wait.Until)

	h.clock.Advance(time.Hour + time.Second)

	_, err = h.run(t.Context())
	require.NoError(t, err)

	works := h.runner.callsOf("work")
	require.Len(t, works, 2)
	assert.Equal(t, engine.ModeContinue, works[1].work.Mode)
	assert.Equal(t, int64(1_000_000), works[1].cfg.Budget.MicroUSD, "a new day's budget")
}
