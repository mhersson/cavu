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

			got, err := h.loop.AddTask(t.Context(), store.Task{ProjectID: h.task.ProjectID, Title: tc.title}, nil, false)

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

func TestAddTaskDependencies(t *testing.T) {
	t.Run("the dependencies are stored", func(t *testing.T) {
		h := newHarness(t)
		d := h.draft()

		got, err := h.loop.AddTask(t.Context(), store.Task{ProjectID: h.task.ProjectID, Title: "B"}, []int64{d.ID, h.task.ID}, false)
		require.NoError(t, err)
		assert.Equal(t, []int64{h.task.ID, d.ID}, h.deps(got.ID))
	})

	t.Run("a dependency on a missing task is refused", func(t *testing.T) {
		h := newHarness(t)

		_, err := h.loop.AddTask(t.Context(), store.Task{ProjectID: h.task.ProjectID, Title: "B"}, []int64{h.task.ID, 999}, false)
		require.ErrorIs(t, err, engine.ErrRefused)

		tasks, err := h.store.Tasks(t.Context(), false)
		require.NoError(t, err)
		assert.Len(t, tasks, 1, "nothing is stored")
	})
}

// deps returns the ids the task depends on.
func (h *harness) deps(taskID int64) []int64 {
	h.t.Helper()

	ids, err := h.store.TaskDeps(h.t.Context(), taskID)
	require.NoError(h.t, err)

	return ids
}

func TestEditTask(t *testing.T) {
	t.Run("the title, the body, the dependencies and the removals are written", func(t *testing.T) {
		h := newHarness(t)
		a, b := h.draft(), h.draft()
		gone, kept := h.putImage(b.ID, "gone.png"), h.putImage(b.ID, "kept.png")

		require.NoError(t, engine.EditTask(t.Context(), h.store, b.ID, store.TaskEdit{
			Title: new("Fix the logout"), Body: new("It hangs."), DependsOn: &[]int64{a.ID}, RemoveImages: []int64{gone.ID},
		}))

		got, err := h.store.Task(t.Context(), b.ID)
		require.NoError(t, err)
		assert.Equal(t, "Fix the logout", got.Title)
		assert.Equal(t, "It hangs.", got.Body)
		assert.Equal(t, h.task.ProjectID, got.ProjectID)
		assert.Equal(t, []int64{a.ID}, h.deps(b.ID))
		assert.Equal(t, []store.Image{kept}, h.images(b.ID))
		assert.Equal(t, []string{kept.SHA256}, h.files(), "the removed image's contents are gone")

		require.NoError(t, engine.EditTask(t.Context(), h.store, b.ID, store.TaskEdit{Title: new("Log out")}))

		got, err = h.store.Task(t.Context(), b.ID)
		require.NoError(t, err)
		assert.Equal(t, "Log out", got.Title)
		assert.Equal(t, "It hangs.", got.Body, "a field left out keeps its value")
		assert.Equal(t, []int64{a.ID}, h.deps(b.ID))

		require.NoError(t, engine.EditTask(t.Context(), h.store, b.ID, store.TaskEdit{Body: new(""), DependsOn: &[]int64{}}))

		got, err = h.store.Task(t.Context(), b.ID)
		require.NoError(t, err)
		assert.Equal(t, "Log out", got.Title)
		assert.Empty(t, got.Body)
		assert.Empty(t, h.deps(b.ID))
	})

	t.Run("an empty title is refused", func(t *testing.T) {
		h := newHarness(t)
		d := h.draft()

		err := engine.EditTask(t.Context(), h.store, d.ID, store.TaskEdit{Title: new(""), Body: new("It hangs.")})
		require.ErrorIs(t, err, engine.ErrRefused)

		got, err := h.store.Task(t.Context(), d.ID)
		require.NoError(t, err)
		assert.Equal(t, [2]string{d.Title, d.Body}, [2]string{got.Title, got.Body}, "nothing is written")
	})

	t.Run("an image the task lacks fails the whole edit", func(t *testing.T) {
		h := newHarness(t)
		a, b, c := h.draft(), h.draft(), h.draft()
		require.NoError(t, engine.EditTask(t.Context(), h.store, c.ID, store.TaskEdit{Title: new("Fix the logout"), Body: new("It hangs."), DependsOn: &[]int64{a.ID}}))
		img := h.putImage(c.ID, "shot.png")

		was, err := h.store.Task(t.Context(), c.ID)
		require.NoError(t, err)

		err = engine.EditTask(t.Context(), h.store, c.ID, store.TaskEdit{
			Title: new("Renamed"), Body: new("Another body."), DependsOn: &[]int64{b.ID}, RemoveImages: []int64{img.ID, img.ID + 1},
		})
		require.ErrorIs(t, err, store.ErrNotFound)

		got, err := h.store.Task(t.Context(), c.ID)
		require.NoError(t, err)
		assert.Equal(t, was, got, "nothing is written")
		assert.Equal(t, []int64{a.ID}, h.deps(c.ID))
		assert.Equal(t, []store.Image{img}, h.images(c.ID))
		assert.Equal(t, []byte("shot.png"), h.contents(c.ID, img.ID))
	})

	t.Run("a started task with no run is refused", func(t *testing.T) {
		h := newHarness(t)
		d := h.draft()
		img := h.putImage(h.task.ID, "shot.png")

		err := engine.EditTask(t.Context(), h.store, h.task.ID, store.TaskEdit{Title: new("Something else"), DependsOn: &[]int64{d.ID}, RemoveImages: []int64{img.ID}})
		require.ErrorIs(t, err, engine.ErrRefused)

		got, err := h.store.Task(t.Context(), h.task.ID)
		require.NoError(t, err)
		assert.Equal(t, h.task.Title, got.Title)
		assert.Empty(t, h.deps(h.task.ID))
		assert.Equal(t, []store.Image{img}, h.images(h.task.ID))
	})

	t.Run("a task whose run started is refused", func(t *testing.T) {
		h := newHarness(t)
		h.add(0, engine.RunStarted, "engine", `{"reason":"initial"}`)

		err := engine.EditTask(t.Context(), h.store, h.task.ID, store.TaskEdit{Title: new("Something else")})
		require.ErrorIs(t, err, engine.ErrRefused)

		got, err := h.store.Task(t.Context(), h.task.ID)
		require.NoError(t, err)
		assert.Equal(t, h.task.Title, got.Title)
	})

	t.Run("a cancelled draft is refused", func(t *testing.T) {
		h := newHarness(t)
		d := h.draft()
		require.NoError(t, h.loop.Cancel(t.Context(), d.ID))

		err := engine.EditTask(t.Context(), h.store, d.ID, store.TaskEdit{Title: new("Something else")})
		require.ErrorIs(t, err, engine.ErrRefused)

		got, err := h.store.Task(t.Context(), d.ID)
		require.NoError(t, err)
		assert.Equal(t, d.Title, got.Title)
	})

	refused := map[string]func(h *harness, a, b store.Task) []int64{
		"a dependency on itself":         func(_ *harness, _, b store.Task) []int64 { return []int64{b.ID} },
		"a dependency on a missing task": func(*harness, store.Task, store.Task) []int64 { return []int64{999} },
		"a dependency that closes a cycle": func(h *harness, a, b store.Task) []int64 {
			require.NoError(h.t, engine.EditTask(h.t.Context(), h.store, a.ID, store.TaskEdit{DependsOn: &[]int64{b.ID}}))

			return []int64{a.ID}
		},
		"a dependency that closes a longer cycle": func(h *harness, a, b store.Task) []int64 {
			c := h.draft()
			require.NoError(h.t, engine.EditTask(h.t.Context(), h.store, a.ID, store.TaskEdit{DependsOn: &[]int64{c.ID}}))
			require.NoError(h.t, engine.EditTask(h.t.Context(), h.store, c.ID, store.TaskEdit{DependsOn: &[]int64{b.ID}}))

			return []int64{h.task.ID, a.ID}
		},
	}

	for name, deps := range refused {
		t.Run(name+" is refused", func(t *testing.T) {
			h := newHarness(t)
			a, b := h.draft(), h.draft()
			dependsOn := deps(h, a, b)

			err := engine.EditTask(t.Context(), h.store, b.ID, store.TaskEdit{Title: new("Renamed"), DependsOn: &dependsOn})
			require.ErrorIs(t, err, engine.ErrRefused)

			got, err := h.store.Task(t.Context(), b.ID)
			require.NoError(t, err)
			assert.Equal(t, b.Title, got.Title, "nothing is written")
			assert.Empty(t, h.deps(b.ID))
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
		"a done task": func(h *harness) {
			h.add(0, engine.Exported, "runner", "")
			h.add(0, engine.Merged, "user", "")
		},
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

func TestRestoreAfterRun(t *testing.T) {
	h := newHarness(t)
	ctx, id := t.Context(), h.task.ID
	h.withBaseline()
	h.withPublish()
	img := h.putImage(id, "screen.png")

	status, err := h.run(ctx)
	require.NoError(t, err)
	require.Equal(t, engine.StatusInReview, status)

	previous := h.viewOf(id, nil)
	require.NotNil(t, previous.PullRequest)

	require.NoError(t, h.loop.Cancel(ctx, id))
	status, err = h.run(ctx)
	require.NoError(t, err)
	require.Equal(t, engine.StatusCancelled, status)
	cancelled, err := h.loop.List(ctx, engine.ListQuery{Statuses: []engine.Status{engine.StatusCancelled}}, nil)
	require.NoError(t, err)
	require.Len(t, cancelled, 1)

	history := h.recorded(id)

	require.NoError(t, h.loop.Restore(ctx, id))
	v := h.viewOf(id, nil)
	assert.Equal(t, engine.StatusDraft, v.Status)
	assert.Nil(t, v.PullRequest)
	assert.Nil(t, v.Change)
	assert.Nil(t, v.Stop)
	assert.False(t, v.NeedsYou)
	assert.Equal(t, previous.Turns, v.Turns)
	assert.Equal(t, previous.Usage, v.Usage)
	assert.Equal(t, previous.Images, v.Images)

	events := h.recorded(id)
	require.Len(t, events, len(history)+1)
	assert.Equal(t, history, events[:len(history)])
	assert.Equal(t, engine.HumanRestored, events[len(history)].Type)

	drafts, err := h.loop.List(ctx, engine.ListQuery{Statuses: []engine.Status{engine.StatusDraft}}, nil)
	require.NoError(t, err)
	require.Len(t, drafts, 1)
	assert.Equal(t, id, drafts[0].ID)

	require.NoError(t, engine.EditTask(ctx, h.store, id, store.TaskEdit{Title: new("Fix the logout"), RemoveImages: []int64{img.ID}}))
	status, err = h.run(ctx)
	require.NoError(t, err)
	require.Equal(t, engine.StatusDraft, status)
	require.NoError(t, h.loop.Start(ctx, id))
	require.ErrorIs(t, engine.EditTask(ctx, h.store, id, store.TaskEdit{Title: new("Too late")}), engine.ErrRefused,
		"a restored draft is editable until it is started again")
	status, err = h.run(ctx)
	require.NoError(t, err)
	assert.Equal(t, engine.StatusInReview, status)
	assert.Len(t, h.eventsOf(engine.RunStarted), 2)
}

func TestRestoreWhileCancellationReleasesRun(t *testing.T) {
	h := newHarness(t)
	ctx, id := t.Context(), h.task.ID
	run := h.seed(ev(engine.RunStarted, `{"reason":"initial"}`))
	require.NoError(t, h.loop.Cancel(ctx, id))
	require.NoError(t, h.loop.Restore(ctx, id))
	assert.Equal(t, "pending", h.viewOf(id, nil).Restore)
	require.NoError(t, h.loop.Restore(ctx, id))
	require.Len(t, h.eventsOf(engine.HumanRestored), 1)
	require.ErrorIs(t, h.loop.RemoveProject(ctx, h.task.ProjectID), engine.ErrRefused)

	status, err := h.run(ctx)
	require.NoError(t, err)
	assert.Equal(t, engine.StatusDraft, status)
	assert.Empty(t, h.viewOf(id, nil).Restore)
	assert.Equal(t, engine.StopCancelled, h.spy.ended()[run.ID])
	assert.Len(t, h.eventsOf(engine.HomeRemoved), 1)
}

func TestRestoreWithRemovedProject(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	require.NoError(t, h.loop.Cancel(ctx, h.task.ID))
	h.runner.onCall = func(c call) {
		if c.method == "remove-project" {
			require.ErrorIs(t, h.loop.Restore(ctx, h.task.ID), store.ErrConflict)
		}
	}
	require.NoError(t, h.loop.RemoveProject(ctx, h.task.ProjectID))
	assert.Empty(t, h.viewOf(h.task.ID, nil).Restore)
	require.ErrorIs(t, h.loop.Restore(ctx, h.task.ID), engine.ErrRefused)
	assert.Empty(t, h.eventsOf(engine.HumanRestored))
}

func TestTaskGuards(t *testing.T) {
	status := func(h *harness) engine.Status { return engine.StatusOf(engine.Reduce(h.recorded(h.task.ID))) }

	t.Run("done on a task that is not in review is refused", func(t *testing.T) {
		h := newHarness(t)
		before := h.recorded(h.task.ID)

		require.ErrorIs(t, h.loop.Done(t.Context(), h.task.ID), engine.ErrRefused)
		assert.Equal(t, before, h.recorded(h.task.ID))
	})

	t.Run("done on a task in review records the merge", func(t *testing.T) {
		h := newHarness(t)
		h.add(0, engine.Exported, "runner", "")

		require.NoError(t, h.loop.Done(t.Context(), h.task.ID))

		es := h.recorded(h.task.ID)
		assert.Equal(t, engine.Merged, es[len(es)-1].Type)
		assert.Equal(t, "user", es[len(es)-1].Actor)
		assert.Equal(t, engine.StatusDone, status(h))
	})

	t.Run("cancel on a task in review is recorded", func(t *testing.T) {
		h := newHarness(t)
		h.add(0, engine.Exported, "runner", "")

		require.NoError(t, h.loop.Cancel(t.Context(), h.task.ID))

		es := h.recorded(h.task.ID)
		assert.Equal(t, engine.HumanCancelled, es[len(es)-1].Type)
		assert.Equal(t, engine.StatusCancelled, status(h))
	})

	t.Run("cancel on a done task is refused", func(t *testing.T) {
		h := newHarness(t)
		h.add(0, engine.Exported, "runner", "")
		require.NoError(t, h.loop.Done(t.Context(), h.task.ID))
		before := h.recorded(h.task.ID)

		require.ErrorIs(t, h.loop.Cancel(t.Context(), h.task.ID), engine.ErrRefused)
		assert.Equal(t, before, h.recorded(h.task.ID))
	})
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

func TestHumanActionRetriesAfterANewerEvent(t *testing.T) {
	h := newHarness(t)
	h.spy.rival = func(claim int) (store.Event, bool) {
		return store.Event{Type: engine.AgentText, Actor: "agent"}, claim == 1
	}

	require.NoError(t, h.loop.Cancel(t.Context(), h.task.ID))

	var cancelled int

	for _, e := range h.recorded(h.task.ID) {
		if e.Type == engine.HumanCancelled {
			cancelled++
		}
	}

	assert.Equal(t, 1, cancelled)
}

// A task with a pull request is not marked done by hand.
func TestDoneIsRefusedWithAPullRequest(t *testing.T) {
	h := newHarness(t)
	h.add(0, engine.Exported, "runner", `{"head_commit":"head456"}`)
	h.add(0, engine.Published, "runner", `{"branch":"cavu/fix-the-login","head":"head456","number":7,"url":"https://example.com/web/pull/7"}`)
	before := h.recorded(h.task.ID)
	require.Equal(t, engine.StatusInReview, engine.StatusOf(engine.Reduce(before)))

	require.ErrorIs(t, h.loop.Done(t.Context(), h.task.ID), engine.ErrRefused)
	assert.Equal(t, before, h.recorded(h.task.ID))
}

func TestAddTaskStart(t *testing.T) {
	t.Run("a project that cannot run refuses a start and stores nothing", func(t *testing.T) {
		h := newHarness(t)

		_, err := h.loop.AddTask(t.Context(), store.Task{ProjectID: h.task.ProjectID, Title: "B"}, nil, true)
		require.ErrorIs(t, err, engine.ErrRefused)
		require.ErrorIs(t, err, engine.ErrNoBaseline)

		tasks, err := h.store.Tasks(t.Context(), false)
		require.NoError(t, err)
		assert.Len(t, tasks, 1, "nothing is stored")
	})

	t.Run("a project that cannot run still takes a draft", func(t *testing.T) {
		h := newHarness(t)

		got, err := h.loop.AddTask(t.Context(), store.Task{ProjectID: h.task.ProjectID, Title: "B"}, nil, false)
		require.NoError(t, err)
		assert.Empty(t, h.recorded(got.ID))
	})

	t.Run("a runnable project stores the task and starts it", func(t *testing.T) {
		h := newHarness(t)
		h.withBaseline()

		got, err := h.loop.AddTask(t.Context(), store.Task{ProjectID: h.task.ProjectID, Title: "B"}, nil, true)
		require.NoError(t, err)

		events := h.recorded(got.ID)
		require.Len(t, events, 1)
		assert.Equal(t, engine.TaskStarted, events[0].Type)
	})
}
