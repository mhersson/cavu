package engine_test

import (
	"strings"
	"testing"

	"github.com/mhersson/cavu/internal/engine"
	"github.com/mhersson/cavu/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAddTaskRules(t *testing.T) {
	tests := []struct {
		name  string
		title string
		ok    bool
	}{
		{"a title", "Fix the login", true},
		{"the longest title", strings.Repeat("x", 200), true},
		{"a title with multibyte characters at the limit", strings.Repeat("å", 200), true},
		{"no title", "", false},
		{"a blank title", "  \t", false},
		{"a title over the limit", strings.Repeat("x", 201), false},
		{"a title with a line break", "Fix\nthe login", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)

			got, err := engine.AddTask(t.Context(), h.store, store.Task{ProjectID: h.task.ProjectID, Title: tc.title})

			tasks, listErr := h.store.Tasks(t.Context(), false)
			require.NoError(t, listErr)

			if tc.ok {
				require.NoError(t, err)
				assert.NotZero(t, got.ID)
				assert.Len(t, tasks, 2)

				return
			}

			require.ErrorIs(t, err, engine.ErrRefused)
			assert.Contains(t, err.Error(), "title")
			assert.Len(t, tasks, 1, "nothing is stored")
		})
	}
}

// draft returns a task that was never started, on the harness's project.
func (h *harness) draft() store.Task {
	h.t.Helper()

	task, err := h.store.CreateTask(h.t.Context(), store.Task{ProjectID: h.task.ProjectID, Title: "Draft"})
	require.NoError(h.t, err)

	return task
}

// recorded returns every event of the task.
func (h *harness) recorded(taskID int64) []store.Event {
	h.t.Helper()

	es, err := h.store.Events(h.t.Context(), taskID, 0, 0)
	require.NoError(h.t, err)

	return es
}

func TestStart(t *testing.T) {
	t.Run("a draft is started by the user", func(t *testing.T) {
		h := newHarness(t)
		h.withBaseline()
		d := h.draft()

		require.NoError(t, h.loop.Start(t.Context(), d.ID))

		es := h.recorded(d.ID)
		require.Len(t, es, 1)
		assert.Equal(t, engine.TaskStarted, es[0].Type)
		assert.Equal(t, "user", es[0].Actor)
		assert.True(t, now.Equal(es[0].At))
	})

	t.Run("a waived verify needs no baseline", func(t *testing.T) {
		h := newHarness(t)
		p := h.project()
		p.VerifyCommand, p.VerifyWaived = "", true
		require.NoError(t, h.store.UpdateProject(t.Context(), p))
		d := h.draft()

		require.NoError(t, h.loop.Start(t.Context(), d.ID))
		assert.Len(t, h.recorded(d.ID), 1)
	})

	t.Run("a started task gets nothing appended", func(t *testing.T) {
		h := newHarness(t)
		before := h.recorded(h.task.ID)

		require.NoError(t, h.loop.Start(t.Context(), h.task.ID))
		assert.Equal(t, before, h.recorded(h.task.ID))
	})

	t.Run("a cancelled task is refused", func(t *testing.T) {
		h := newHarness(t)
		h.withBaseline()
		d := h.draft()
		require.NoError(t, h.loop.Cancel(t.Context(), d.ID))

		err := h.loop.Start(t.Context(), d.ID)
		require.ErrorIs(t, err, engine.ErrRefused)
		require.NotErrorIs(t, err, engine.ErrNoBaseline)
		assert.Len(t, h.recorded(d.ID), 1)
	})

	t.Run("a project with neither a passed baseline nor a waiver is refused", func(t *testing.T) {
		h := newHarness(t)
		d := h.draft()

		err := h.loop.Start(t.Context(), d.ID)
		require.ErrorIs(t, err, engine.ErrRefused)
		require.ErrorIs(t, err, engine.ErrNoBaseline)
		assert.Empty(t, h.recorded(d.ID))

		p := h.project()
		p.Baseline = &store.Baseline{Commit: "c", Passed: false}
		require.NoError(t, h.store.UpdateProject(t.Context(), p))

		require.ErrorIs(t, h.loop.Start(t.Context(), d.ID), engine.ErrNoBaseline, "a failed baseline is no baseline")
		assert.Empty(t, h.recorded(d.ID))
	})

	t.Run("a task that does not exist", func(t *testing.T) {
		h := newHarness(t)

		require.ErrorIs(t, h.loop.Start(t.Context(), 999), store.ErrNotFound)
	})
}

// stopped puts the harness's started task in an open stop for reason.
func (h *harness) stopped(reason string) {
	h.t.Helper()

	h.add(0, engine.Stopped, "engine", `{"reason":"`+reason+`"}`)
}

func TestAnswer(t *testing.T) {
	t.Run("an open question is answered", func(t *testing.T) {
		h := newHarness(t)
		h.stopped(engine.StopQuestion)

		require.NoError(t, h.loop.Answer(t.Context(), h.task.ID, "Use the v2 API.\n"))

		es := h.recorded(h.task.ID)
		last := es[len(es)-1]
		assert.Equal(t, engine.HumanAnswered, last.Type)
		assert.Equal(t, "user", last.Actor)
		assert.Equal(t, "Use the v2 API.", payloadOf[engine.HumanAnsweredPayload](t, last).Text)
	})

	t.Run("a secret in an answer is masked", func(t *testing.T) {
		h := newHarness(t)
		require.NoError(t, h.vault.Set(t.Context(), "forge_token", "s3cret-token-value"))
		h.stopped(engine.StopQuestion)

		require.NoError(t, h.loop.Answer(t.Context(), h.task.ID, "the token is s3cret-token-value"))

		es := h.recorded(h.task.ID)
		assert.NotContains(t, string(es[len(es)-1].Payload), "s3cret-token-value")
	})

	refused := map[string]func(h *harness){
		"no stop":                func(*harness) {},
		"a stop of another kind": func(h *harness) { h.stopped(engine.StopVerifyFailed) },
		"a stop already answered": func(h *harness) {
			h.stopped(engine.StopQuestion)
			h.add(0, engine.HumanAnswered, "user", `{"text":"yes"}`)
		},
	}

	for name, setup := range refused {
		t.Run(name+" is refused", func(t *testing.T) {
			h := newHarness(t)
			setup(h)
			before := h.recorded(h.task.ID)

			require.ErrorIs(t, h.loop.Answer(t.Context(), h.task.ID, "yes"), engine.ErrRefused)
			assert.Equal(t, before, h.recorded(h.task.ID))
		})
	}

	t.Run("a blank answer is refused", func(t *testing.T) {
		h := newHarness(t)
		h.stopped(engine.StopQuestion)
		before := h.recorded(h.task.ID)

		require.ErrorIs(t, h.loop.Answer(t.Context(), h.task.ID, " \n"), engine.ErrRefused)
		assert.Equal(t, before, h.recorded(h.task.ID))
	})
}

func TestRetry(t *testing.T) {
	t.Run("an open stop is retried", func(t *testing.T) {
		h := newHarness(t)
		h.stopped(engine.StopVerifyFailed)

		require.NoError(t, h.loop.Retry(t.Context(), h.task.ID))

		es := h.recorded(h.task.ID)
		assert.Equal(t, engine.HumanRetried, es[len(es)-1].Type)
		assert.Equal(t, "user", es[len(es)-1].Actor)
	})

	refused := map[string]func(h *harness){
		"no stop": func(*harness) {},
		"a cancelled task": func(h *harness) {
			h.stopped(engine.StopVerifyFailed)
			h.add(0, engine.HumanCancelled, "user", "")
		},
		"a draft": func(h *harness) { h.task = h.draft() },
	}

	for name, setup := range refused {
		t.Run(name+" is refused", func(t *testing.T) {
			h := newHarness(t)
			setup(h)
			before := h.recorded(h.task.ID)

			require.ErrorIs(t, h.loop.Retry(t.Context(), h.task.ID), engine.ErrRefused)
			assert.Equal(t, before, h.recorded(h.task.ID))
		})
	}
}

func TestCancel(t *testing.T) {
	for name, setup := range map[string]func(h *harness){
		"a draft":        func(h *harness) { h.task = h.draft() },
		"a running task": func(*harness) {},
		"a blocked task": func(h *harness) { h.stopped(engine.StopQuestion) },
	} {
		t.Run(name+" is cancelled", func(t *testing.T) {
			h := newHarness(t)
			setup(h)

			require.NoError(t, h.loop.Cancel(t.Context(), h.task.ID))

			es := h.recorded(h.task.ID)
			assert.Equal(t, engine.HumanCancelled, es[len(es)-1].Type)
			assert.Equal(t, "user", es[len(es)-1].Actor)
		})
	}

	refused := map[string]func(h *harness){
		"a cancelled task": func(h *harness) { h.add(0, engine.HumanCancelled, "user", "") },
		"a task in review": func(h *harness) { h.add(0, engine.Exported, "runner", "") },
	}

	for name, setup := range refused {
		t.Run(name+" is refused", func(t *testing.T) {
			h := newHarness(t)
			setup(h)
			before := h.recorded(h.task.ID)

			require.ErrorIs(t, h.loop.Cancel(t.Context(), h.task.ID), engine.ErrRefused)
			assert.Equal(t, before, h.recorded(h.task.ID))
		})
	}
}

func TestHumanActionLosesToANewerEvent(t *testing.T) {
	h := newHarness(t)
	h.spy.rival = func(int) (store.Event, bool) {
		return store.Event{Type: engine.AgentText, Actor: "agent"}, true
	}

	require.ErrorIs(t, h.loop.Cancel(t.Context(), h.task.ID), store.ErrConflict)

	for _, e := range h.recorded(h.task.ID) {
		assert.NotEqual(t, engine.HumanCancelled, e.Type)
	}
}
