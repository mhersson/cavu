package engine_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/mhersson/cavu/internal/engine"
	"github.com/mhersson/cavu/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// addTo appends one event of typ to the task at at, with usage rows when given.
func (h *harness) addTo(taskID int64, at time.Time, typ string, payload any, usage ...store.Usage) {
	h.t.Helper()

	raw, err := json.Marshal(payload)
	require.NoError(h.t, err)

	_, err = h.store.Append(h.t.Context(), taskID, -1, []store.Event{{At: at, Type: typ, Actor: "engine", Payload: raw}}, usage)
	require.NoError(h.t, err)
}

func (h *harness) viewOf(taskID int64, waiting func(int64) string) engine.View {
	h.t.Helper()

	v, err := h.loop.View(h.t.Context(), taskID, waiting)
	require.NoError(h.t, err)

	return v
}

func TestViewOfAQuestion(t *testing.T) {
	h := newHarness(t)
	id := h.task.ID

	h.addTo(id, now, engine.RunStarted, engine.RunStartedPayload{Reason: "initial"})
	h.addTo(id, now, engine.SessionEnded, engine.SessionEndedPayload{
		Outcome: engine.OutcomeBlocked, Questions: []string{"Which colour?", "Which shade?"}, SuggestedAnswers: []string{"Blue."},
	})
	h.addTo(id, now, engine.Stopped, engine.StoppedPayload{Reason: engine.StopQuestion, Explanation: "Which colour?", Action: engine.ClearAnswer})

	v := h.viewOf(id, nil)
	assert.Equal(t, id, v.ID)
	assert.Equal(t, "web", v.Project)
	assert.Equal(t, h.task.Title, v.Title)
	assert.Equal(t, h.task.Body, v.Body)
	assert.Equal(t, engine.StatusBlocked, v.Status)
	assert.True(t, v.NeedsYou)
	assert.Equal(t, &engine.Stop{Reason: engine.StopQuestion, Explanation: "Which colour?", Action: engine.ClearAnswer}, v.Stop)
	assert.Equal(t, []engine.Question{{Question: "Which colour?", SuggestedAnswer: "Blue."}, {Question: "Which shade?"}}, v.Questions)
	assert.Empty(t, v.Waiting)
	assert.Nil(t, v.WaitingUntil)
}

func TestViewWaitsOutAUsageLimit(t *testing.T) {
	h := newHarness(t)
	id := h.task.ID
	resets := now.Add(time.Hour).In(time.FixedZone("CET", 3600))

	h.addTo(id, now, engine.RunStarted, engine.RunStartedPayload{Reason: "initial"})
	h.addTo(id, now, engine.SessionEnded, engine.SessionEndedPayload{Outcome: engine.OutcomeUsageLimit, ResetsAt: resets})
	h.addTo(id, now, engine.Stopped, engine.StoppedPayload{
		Reason: engine.StopUsageLimit, Explanation: "The usage limit was reached.", Action: engine.ClearNone, ResetsAt: resets,
	})

	v := h.viewOf(id, nil)
	assert.Equal(t, engine.StatusBlocked, v.Status)
	assert.False(t, v.NeedsYou, "the limit resets by itself")
	require.NotNil(t, v.Stop)
	assert.Equal(t, new(resets.UTC()), v.Stop.ResetsAt)
	assert.NotEmpty(t, v.Waiting)
	assert.Equal(t, new(resets.UTC()), v.WaitingUntil)

	decided := v.Waiting
	tells := func(why string) func(int64) string {
		return func(taskID int64) string {
			if taskID == id {
				return why
			}

			return ""
		}
	}

	v = h.viewOf(id, tells(engine.WaitingForSlot))
	assert.Equal(t, engine.WaitingForSlot, v.Waiting, "the scheduler's reason comes first")
	assert.Nil(t, v.WaitingUntil, "and has no end")

	v = h.viewOf(id, tells(decided))
	assert.Equal(t, decided, v.Waiting)
	assert.Equal(t, new(resets.UTC()), v.WaitingUntil, "the scheduler tells Decide's own wait, whose end is known")
}

func TestViewGivesEachDependencyWithItsStatus(t *testing.T) {
	h := newHarness(t)
	done, draft := h.draft(), h.draft()

	h.addTo(done.ID, now, engine.TaskStarted, struct{}{})
	h.addTo(done.ID, now, engine.RunStarted, engine.RunStartedPayload{Reason: "initial"})
	h.addTo(done.ID, now, engine.Exported, engine.ExportedPayload{HeadCommit: "c1"})
	h.addTo(done.ID, now, engine.Merged, struct{}{})
	require.NoError(t, h.store.SetTaskDeps(t.Context(), h.task.ID, []int64{done.ID, draft.ID}))

	v := h.viewOf(h.task.ID, nil)
	assert.Equal(t, []engine.Dep{{TaskID: done.ID, Status: engine.StatusDone}, {TaskID: draft.ID, Status: engine.StatusDraft}}, v.Dependencies)
	assert.NotEmpty(t, v.Waiting, "the draft dependency holds the task")
	assert.False(t, v.NeedsYou)
}

func TestViewShowsMoneyUnderACapInForce(t *testing.T) {
	h := newHarness(t)
	h.keyMode()

	h.spend(h.task.ID, "proxy", "model-a", 2_000_000, now)
	h.spend(h.draft().ID, "proxy", "model-a", 1_000_000, now)

	b := h.viewOf(h.task.ID, nil).Budget
	assert.Nil(t, b.USD)
	assert.Nil(t, b.MaxUSD)
	assert.Nil(t, b.DayUSD, "no daily cap is set")
	assert.Nil(t, b.MaxDayUSD)

	h.set("daily_budget_usd", "20")

	b = h.viewOf(h.task.ID, nil).Budget
	assert.Nil(t, b.USD, "no task cap is set")
	assert.Equal(t, new(3.0), b.DayUSD, "every task's spend today")
	assert.Equal(t, new(20.0), b.MaxDayUSD)

	h.set("task_budget_usd", "5")

	b = h.viewOf(h.task.ID, nil).Budget
	assert.Equal(t, new(2.0), b.USD)
	assert.Equal(t, new(5.0), b.MaxUSD)
	assert.Equal(t, new(3.0), b.DayUSD)
}

func TestViewSumsUsageTurnsAndDuration(t *testing.T) {
	h := newHarness(t)
	id := h.draft().ID

	h.addTo(id, now, engine.TaskStarted, struct{}{})
	h.addTo(id, now.Add(time.Minute), engine.SessionEnded, engine.SessionEndedPayload{Outcome: engine.OutcomeDone, Turns: 7},
		store.Usage{Source: "proxy", Model: "model-a", Input: 10, Output: 20, CacheRead: 30, CacheWrite: 40, CostEstimateUSD: 0.25},
		store.Usage{Source: "agent", Model: "model-b", Input: 1, Output: 2, CostEstimateUSD: 0.5})
	h.addTo(id, now.Add(90*time.Second), engine.ReviewEnded, engine.ReviewEndedPayload{Outcome: engine.ReviewFailed, Turns: 2},
		store.Usage{Source: "proxy", Model: "model-a", Input: 5, CostEstimateUSD: 0.25})

	v := h.viewOf(id, nil)
	assert.Equal(t, []engine.UsageRow{
		{Source: "agent", Model: "model-b", Input: 1, Output: 2, CostEstimateUSD: 0.5},
		{Source: "proxy", Model: "model-a", Input: 15, Output: 20, CacheRead: 30, CacheWrite: 40, CostEstimateUSD: 0.5},
	}, v.Usage)
	assert.InDelta(t, 1.0, v.CostEstimateUSD, 1e-9)
	assert.Equal(t, 9, v.Turns)
	assert.EqualValues(t, 90_000, v.DurationMS)
}

func TestViewJSONHoldsNoHostPath(t *testing.T) {
	h := newHarness(t)
	id := h.task.ID

	h.addTo(id, now, engine.RunStarted, engine.RunStartedPayload{Reason: "initial"})
	h.addTo(id, now, engine.SessionEnded, engine.SessionEndedPayload{Outcome: engine.OutcomeDone, Changed: true, HeadCommit: "c1"})
	h.addTo(id, now, engine.VerifyEnded, engine.VerifyEndedPayload{Result: engine.ResultPassed})
	h.addTo(id, now, engine.Exported, engine.ExportedPayload{
		HeadCommit: "c1", DiffPath: "/data/runs/1/change.diff", BundlePath: "/data/runs/1/change.bundle",
		FilesChanged: 1, DiffStat: []string{"a.go | 1 +"},
	})
	h.addTo(id, now, engine.RunEnded, engine.RunEndedPayload{Outcome: engine.RunExported})

	v := h.viewOf(id, nil)
	assert.Equal(t, engine.StatusInReview, v.Status)
	assert.True(t, v.NeedsYou, "with no pull request, the change waits for a person")
	require.NotNil(t, v.Change)
	assert.Equal(t, "/data/runs/1/change.diff", v.Change.DiffPath)
	assert.Equal(t, "/data/runs/1/change.bundle", v.Change.BundlePath)

	b, err := json.Marshal(v)
	require.NoError(t, err)

	var got map[string]any
	require.NoError(t, json.Unmarshal(b, &got))

	assert.Equal(t, map[string]any{
		"head_commit": "c1", "files_changed": 1.0, "diff_stat": []any{"a.go | 1 +"},
		"flagged": map[string]any{"workflows": []any{}, "instructions": []any{}, "large": []any{}},
	}, got["change"])

	for _, k := range []string{"dependencies", "questions", "usage"} {
		assert.Equal(t, []any{}, got[k], k)
	}

	assert.Contains(t, got, "waiting_until")
	assert.Nil(t, got["waiting_until"])
}

func TestList(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()

	asked := h.draft()
	h.addTo(asked.ID, now, engine.TaskStarted, struct{}{})
	h.addTo(asked.ID, now, engine.Stopped, engine.StoppedPayload{Reason: engine.StopQuestion, Explanation: "Which colour?", Action: engine.ClearAnswer})

	_, err := h.store.CreateTask(ctx, store.Task{ProjectID: h.task.ProjectID, Title: "A replay's shadow", ReplayID: 1})
	require.NoError(t, err)

	gone, err := h.store.CreateProject(ctx, store.Project{Name: "api", RepoURL: "https://example.com/api.git", BaseBranch: "main"})
	require.NoError(t, err)

	elsewhere, err := h.store.CreateTask(ctx, store.Task{ProjectID: gone.ID, Title: "Elsewhere"})
	require.NoError(t, err)
	require.NoError(t, h.store.RemoveProject(ctx, gone.ID, now, "project.api."))

	type row struct {
		ID                  int64
		Project             string
		Status              engine.Status
		NeedsYou            bool
		StopReason, Waiting string
	}

	list := func(projectID int64) []row {
		t.Helper()

		scheduler := func(taskID int64) string {
			if taskID == h.task.ID {
				return engine.WaitingForSlot
			}

			return ""
		}

		rows, err := h.loop.List(ctx, engine.ListQuery{ProjectID: projectID}, scheduler)
		require.NoError(t, err)

		out := []row{}
		for _, r := range rows {
			out = append(out, row{r.ID, r.Project, r.Status, r.NeedsYou, r.StopReason, r.Waiting})
		}

		return out
	}

	web := []row{
		{h.task.ID, "web", engine.StatusQueued, false, "", engine.WaitingForSlot},
		{asked.ID, "web", engine.StatusBlocked, true, engine.StopQuestion, ""},
	}

	assert.Equal(t, append(web, row{elsewhere.ID, "api", engine.StatusDraft, false, "", ""}), list(0),
		"every task but the shadow, a removed project's with its name")
	assert.Equal(t, web, list(h.task.ProjectID))
}

func TestNeedsYouSince(t *testing.T) {
	at := func(m int) time.Time { return now.Add(time.Duration(m) * time.Minute) }
	stop := func(h *harness, action string) {
		h.addTo(h.task.ID, at(1), engine.RunStarted, engine.RunStartedPayload{Reason: "initial"})
		h.addTo(h.task.ID, at(2), engine.Stopped, engine.StoppedPayload{Reason: engine.StopVerifyFailed, Explanation: "x", Action: action})
	}
	published := func(h *harness) {
		h.addTo(h.task.ID, at(1), engine.RunStarted, engine.RunStartedPayload{Reason: "initial"})
		h.addTo(h.task.ID, at(2), engine.Exported, engine.ExportedPayload{})
		h.addTo(h.task.ID, at(3), engine.Published, engine.PublishedPayload{Number: 3, Head: "h1"})
		h.addTo(h.task.ID, at(4), engine.RunEnded, engine.RunEndedPayload{})
		h.addTo(h.task.ID, at(5), engine.PRObserved, engine.Observation{State: engine.PROpen, Head: "h1", Checks: engine.ChecksPassed})
	}

	tests := []struct {
		name  string
		setup func(*harness)
		way   string
		since *time.Time
	}{
		{"a question", func(h *harness) { stop(h, engine.ClearAnswer) }, engine.ClearAnswer, new(at(2))},
		{"a failure", func(h *harness) { stop(h, engine.ClearRetry) }, engine.ClearRetry, new(at(2))},
		{"a ready pull request", published, engine.NeedsReview, new(at(5))},
		{"a pull request made ready by a run that only replied", func(h *harness) {
			published(h)
			h.addTo(h.task.ID, at(6), engine.RunStarted, engine.RunStartedPayload{Reason: engine.ReasonComments, Head: "h1"})
			h.addTo(h.task.ID, at(7), engine.RunEnded, engine.RunEndedPayload{})
		}, engine.NeedsReview, new(at(7))},
		{"a change to take", func(h *harness) {
			h.addTo(h.task.ID, at(1), engine.RunStarted, engine.RunStartedPayload{Reason: "initial"})
			h.addTo(h.task.ID, at(2), engine.Exported, engine.ExportedPayload{})
			h.addTo(h.task.ID, at(3), engine.RunEnded, engine.RunEndedPayload{})
		}, engine.NeedsTake, new(at(3))},
		{"a running task", func(h *harness) {
			h.addTo(h.task.ID, at(1), engine.RunStarted, engine.RunStartedPayload{Reason: "initial"})
		}, "", nil},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			tc.setup(h)

			v := h.viewOf(h.task.ID, nil)
			assert.Equal(t, tc.way, v.Needs)
			assert.Equal(t, tc.since, v.NeedsYouSince)
			assert.Equal(t, tc.way != "", v.NeedsYou)

			rows, err := h.loop.List(t.Context(), engine.ListQuery{}, nil)
			require.NoError(t, err)
			require.Len(t, rows, 1)
			assert.Equal(t, tc.way, rows[0].Needs)
			assert.Equal(t, tc.since, rows[0].NeedsYouSince)
		})
	}
}

func TestRowCarriesTheWait(t *testing.T) {
	h := newHarness(t)
	id := h.task.ID
	resets := now.Add(time.Hour)

	h.addTo(id, now, engine.RunStarted, engine.RunStartedPayload{Reason: "initial"})
	h.addTo(id, now, engine.SessionEnded, engine.SessionEndedPayload{Outcome: engine.OutcomeUsageLimit, ResetsAt: resets})
	h.addTo(id, now, engine.Stopped, engine.StoppedPayload{
		Reason: engine.StopUsageLimit, Explanation: "The usage limit was reached.", Action: engine.ClearNone, ResetsAt: resets,
	})

	rows, err := h.loop.List(t.Context(), engine.ListQuery{}, nil)
	require.NoError(t, err)
	require.Len(t, rows, 1)

	v := h.viewOf(id, nil)
	require.NotNil(t, v.WaitingUntil)
	assert.Equal(t, v.WaitingUntil, rows[0].WaitingUntil)
}

func TestListReadsAFinishedTaskOnce(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()

	done, cancelled, told, halting := h.draft(), h.draft(), h.draft(), h.draft()

	for _, id := range []int64{done.ID, told.ID} {
		h.addTo(id, now, engine.TaskStarted, struct{}{})
		h.addTo(id, now, engine.Exported, engine.ExportedPayload{})
		h.addTo(id, now, engine.Merged, struct{}{})
	}

	require.NoError(t, h.loop.Cancel(ctx, cancelled.ID))
	h.addTo(halting.ID, now, engine.TaskStarted, struct{}{})
	h.addTo(halting.ID, now, engine.HumanCancelled, struct{}{}) // its cancelled stop is not recorded yet

	s := &readsStore{Store: h.store}
	l := &engine.Loop{Store: s, Now: h.clock.Now}

	list := func(waiting func(int64) string) []engine.Row {
		t.Helper()

		rows, err := l.List(ctx, engine.ListQuery{}, waiting)
		require.NoError(t, err)

		return rows
	}

	first := list(func(id int64) string {
		if id == told.ID {
			return engine.WaitingForSlot
		}

		return ""
	})

	assert.Equal(t, []int64{h.task.ID, done.ID, cancelled.ID, told.ID, halting.ID}, s.reads, "the first list reads every task")

	s.reads = nil
	second := list(nil)

	assert.Equal(t, []int64{h.task.ID, told.ID, halting.ID}, s.reads,
		"a finished task is read once; an open one, one with a reason to wait and one the engine has yet to stop, every time")
	assert.Equal(t, first[1:3], second[1:3], "a kept row is the row the task had")
}
