package engine_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mhersson/cavu/internal/engine"
	"github.com/mhersson/cavu/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// wait bounds how long a test waits for the scheduler to reach a state.
const wait = 10 * time.Second

// gates holds the runner's calls of the held methods until the test lets each
// go, or its context ends.
type gates struct {
	mu   sync.Mutex
	hold map[string]bool
	open []gated
	cut  []call // held calls whose context ended
}

type gated struct {
	c   call
	let chan struct{}
}

// gate makes the harness's runner hold the calls of methods.
func (h *harness) gate(methods ...string) *gates {
	g := &gates{hold: map[string]bool{}}
	for _, m := range methods {
		g.hold[m] = true
	}

	h.runner.gate = g.wait

	return g
}

// subject is the task a call is for, or the project of a check.
func subject(c call) int64 {
	if c.method == "check" {
		return c.cfg.ProjectID
	}

	return c.task
}

func (g *gates) wait(ctx context.Context, c call) error {
	g.mu.Lock()

	if !g.hold[c.method] {
		g.mu.Unlock()

		return nil
	}

	w := gated{c: c, let: make(chan struct{})}
	g.open = append(g.open, w)
	g.mu.Unlock()

	select {
	case <-w.let:
		return nil
	case <-ctx.Done():
		g.mu.Lock()
		defer g.mu.Unlock()

		g.open = slices.DeleteFunc(g.open, func(o gated) bool { return o.let == w.let })
		g.cut = append(g.cut, c)

		return ctx.Err()
	}
}

// held returns the subjects of the held calls of method, ascending.
func (g *gates) held(method string) []int64 {
	g.mu.Lock()
	defer g.mu.Unlock()

	var out []int64

	for _, w := range g.open {
		if w.c.method == method {
			out = append(out, subject(w.c))
		}
	}

	slices.Sort(out)

	return out
}

// cutOf returns the subjects of the held calls of method whose context ended, ascending.
func (g *gates) cutOf(method string) []int64 {
	g.mu.Lock()
	defer g.mu.Unlock()

	var out []int64

	for _, c := range g.cut {
		if c.method == method {
			out = append(out, subject(c))
		}
	}

	slices.Sort(out)

	return out
}

// let lets the held call of method for id go.
func (g *gates) let(t *testing.T, method string, id int64) {
	t.Helper()
	g.mu.Lock()
	defer g.mu.Unlock()

	i := slices.IndexFunc(g.open, func(w gated) bool { return w.c.method == method && subject(w.c) == id })
	require.NotEqual(t, -1, i, "no %s call of %d is held", method, id)

	close(g.open[i].let)
	g.open = slices.Delete(g.open, i, i+1)
}

// holdOn makes the gate hold the calls of method from now on.
func (g *gates) holdOn(method string) {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.hold[method] = true
}

// free stops holding method and lets its held calls go.
func (g *gates) free(method string) {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.hold[method] = false
	g.open = slices.DeleteFunc(g.open, func(w gated) bool {
		if w.c.method == method {
			close(w.let)

			return true
		}

		return false
	})
}

// running is a scheduler that runs until the test ends or stop is called.
type running struct {
	*engine.Scheduler

	ended  atomic.Int64 // passes ended
	mu     sync.Mutex
	failed []int64 // the task id of each OnError call
	watch  func()  // runs at the end of each pass, before it counts
	stop   func() error
}

// schedule runs a scheduler on the harness's loop. Its tick is far beyond any
// test, so only a nudge or a drive's end makes it pass.
func (h *harness) schedule(edit func(s *engine.Scheduler)) *running {
	h.t.Helper()

	r := &running{}
	r.Scheduler = &engine.Scheduler{Loop: h.loop, Tick: time.Hour, OnError: func(id int64, err error) {
		h.t.Logf("OnError(%d): %v", id, err)
		r.mu.Lock()
		defer r.mu.Unlock()

		r.failed = append(r.failed, id)
	}}

	if edit != nil {
		edit(r.Scheduler)
	}

	engine.SetPassHook(r.Scheduler, func() {
		r.mu.Lock()
		watch := r.watch
		r.mu.Unlock()

		if watch != nil {
			watch()
		}

		r.ended.Add(1)
	})

	ctx, cancel := context.WithCancel(h.t.Context())
	done := make(chan error, 1)

	go func() { done <- r.Run(ctx) }()

	var (
		once sync.Once
		err  error
	)

	r.stop = func() error {
		once.Do(func() {
			cancel()

			err = <-done
		})

		return err
	}

	h.t.Cleanup(func() { assert.NoError(h.t, r.stop()) })

	return r
}

// watchEach makes f run at the end of every pass from now on.
func (r *running) watchEach(f func()) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.watch = f
}

func (r *running) errs() []int64 {
	r.mu.Lock()
	defer r.mu.Unlock()

	return slices.Clone(r.failed)
}

// passes forces n passes, waiting for each to end. Two in a row see a whole
// pass that began after the first was forced.
func (r *running) passes(t *testing.T, n int) {
	t.Helper()

	for range n {
		before := r.ended.Load()
		r.Nudge()
		require.Eventually(t, func() bool { return r.ended.Load() > before }, wait, time.Millisecond)
	}
}

func (h *harness) eventually(cond func() bool, msgAndArgs ...any) {
	h.t.Helper()
	require.Eventually(h.t, cond, wait, time.Millisecond, msgAndArgs...)
}

// started adds a task a person started.
func (h *harness) started() store.Task {
	h.t.Helper()

	d := h.draft()
	h.startTask(d.ID)

	return d
}

// startTask records that a person started the task.
func (h *harness) startTask(id int64) {
	h.t.Helper()

	_, err := h.store.Append(h.t.Context(), id, -1,
		[]store.Event{{Type: engine.TaskStarted, Actor: "user", Payload: json.RawMessage("{}")}}, nil)
	require.NoError(h.t, err)
}

func (h *harness) set(key, value string) {
	h.t.Helper()
	require.NoError(h.t, h.store.SetSetting(h.t.Context(), key, value))
}

// statusOf reads the task's status; it may run outside the test's goroutine.
func (h *harness) statusOf(id int64) engine.Status {
	es, err := h.store.Events(context.Background(), id, 0, 0)
	if err != nil {
		return ""
	}

	return engine.StatusOf(engine.Reduce(es))
}

// inReview reports whether the task is in review with no run open.
func (h *harness) inReview(ids ...int64) bool {
	for _, id := range ids {
		_, open, err := h.store.OpenRun(context.Background(), id)
		if err != nil || open || h.statusOf(id) != engine.StatusInReview {
			return false
		}
	}

	return true
}

// checkCleared reports whether the project's check request is cleared.
func (h *harness) checkCleared(projectID int64) bool {
	p, err := h.store.Project(context.Background(), projectID)

	return err == nil && p.CheckRequestedAt == nil
}

// callsFor returns the runner calls made for the task.
func (r *fakeRunner) callsFor(task int64) []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	var out []string

	for _, c := range r.calls {
		if c.task == task {
			out = append(out, c.String())
		}
	}

	return out
}

// tasks returns the task of every runner call, in call order.
func (r *fakeRunner) tasks() []int64 {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]int64, len(r.calls))
	for i, c := range r.calls {
		out[i] = c.task
	}

	return out
}

func TestSchedulerRespectsConcurrency(t *testing.T) {
	h := newHarness(t)
	second, third := h.started(), h.started()
	h.set("concurrency", "2")
	g := h.gate("work")

	s := h.schedule(nil)

	h.eventually(func() bool { return slices.Equal(g.held("work"), []int64{h.task.ID, second.ID}) })
	h.eventually(func() bool { return s.Waiting(third.ID) == engine.WaitingForSlot })
	assert.Empty(t, h.runner.callsFor(third.ID))

	g.let(t, "work", h.task.ID)
	h.eventually(func() bool { return slices.Equal(g.held("work"), []int64{second.ID, third.ID}) }, "the third starts when one ends")
	assert.True(t, h.inReview(h.task.ID))
	assert.Empty(t, s.Waiting(third.ID))

	h.set("concurrency", "1")
	fourth := h.started()

	g.let(t, "work", second.ID)
	h.eventually(func() bool { return h.inReview(second.ID) })
	s.passes(t, 2)
	assert.Equal(t, engine.WaitingForSlot, s.Waiting(fourth.ID))
	assert.Empty(t, h.runner.callsFor(fourth.ID), "one drive is in flight and the setting allows one")

	g.let(t, "work", third.ID)
	h.eventually(func() bool { return slices.Equal(g.held("work"), []int64{fourth.ID}) })

	tasks := h.runner.tasks()
	assert.NotContains(t, tasks[slices.Index(tasks, fourth.ID):], third.ID, "the fourth starts after the third's drive ended")
	assert.Empty(t, s.errs())
}

func TestSchedulerOrder(t *testing.T) {
	h := newHarness(t)
	second, third := h.draft(), h.draft()
	h.startTask(third.ID)
	h.startTask(second.ID)
	h.set("concurrency", "1")
	g := h.gate("work")

	s := h.schedule(nil)

	for _, id := range []int64{h.task.ID, second.ID, third.ID} {
		h.eventually(func() bool { return slices.Equal(g.held("work"), []int64{id}) }, "task %d is driven next", id)
		g.let(t, "work", id)
	}

	h.eventually(func() bool { return h.inReview(h.task.ID, second.ID, third.ID) })
	assert.Empty(t, s.errs())
}

func TestSchedulerDependencyChain(t *testing.T) {
	h := newHarness(t)
	b, err := engine.AddTask(t.Context(), h.store, store.Task{ProjectID: h.task.ProjectID, Title: "B"}, []int64{h.task.ID})
	require.NoError(t, err)
	h.startTask(b.ID)
	g := h.gate("work")

	s := h.schedule(nil)

	h.eventually(func() bool { return slices.Equal(g.held("work"), []int64{h.task.ID}) })
	h.eventually(func() bool { return s.Waiting(b.ID) != "" })

	g.let(t, "work", h.task.ID)
	h.eventually(func() bool { return h.inReview(h.task.ID) })
	s.passes(t, 2)
	assert.NotEmpty(t, s.Waiting(b.ID))
	assert.Empty(t, h.runner.callsFor(b.ID), "a dependency in review is not done")

	g.free("work")
	require.NoError(t, h.loop.Done(t.Context(), h.task.ID))
	s.Nudge()

	h.eventually(func() bool { return h.inReview(b.ID) })
	assert.Equal(t, happyTrace, h.runner.callsFor(b.ID))
	assert.Empty(t, s.Waiting(b.ID))
	assert.Empty(t, s.errs())
}

func TestSchedulerCancelInterruptsASession(t *testing.T) {
	h := newHarness(t)
	g := h.gate("work")

	s := h.schedule(nil)

	h.eventually(func() bool { return slices.Equal(g.held("work"), []int64{h.task.ID}) })
	require.NoError(t, h.loop.Cancel(t.Context(), h.task.ID))
	s.Nudge()

	h.eventually(func() bool { return slices.Equal(g.cutOf("work"), []int64{h.task.ID}) }, "the session's context ends")
	h.eventually(func() bool { return !h.runOpen() })

	evs := h.events()
	require.GreaterOrEqual(t, len(evs), 2)
	assert.Equal(t, []string{engine.Stopped, engine.RunEnded}, types(evs[len(evs)-2:]))
	assert.Equal(t, engine.StopCancelled, h.lastStop().Reason)

	work := h.runner.callsOf("work")[0]
	assert.Equal(t, []string{"prepare", "work/start", "release/remove"}, h.runner.trace())
	assert.Equal(t, work.runID, h.runner.callsOf("release")[0].runID)
	assert.Empty(t, s.errs())
}

// runOpen reports whether the harness's task has an open run row.
func (h *harness) runOpen() bool {
	_, open, err := h.store.OpenRun(context.Background(), h.task.ID)

	return err != nil || open
}

func TestSchedulerRestartContinuesEveryTask(t *testing.T) {
	h := newHarness(t)
	second := h.started()
	ids := []int64{h.task.ID, second.ID}
	g := h.gate("work")

	s := h.schedule(nil)

	h.eventually(func() bool { return slices.Equal(g.held("work"), ids) })
	require.NoError(t, s.stop())
	assert.Equal(t, ids, g.cutOf("work"), "every session in flight is interrupted")
	assert.Empty(t, s.errs())

	first := map[int64]call{}
	for _, c := range h.runner.callsOf("work") {
		first[c.task] = c
	}

	releases := map[int64]call{}
	for _, c := range h.runner.callsOf("release") {
		releases[c.task] = c
	}

	for _, id := range ids {
		require.Contains(t, releases, id)
		assert.False(t, releases[id].remove, "task %d keeps its volumes", id)
		assert.Equal(t, first[id].runID, releases[id].runID)
	}

	g.free("work")

	again := h.schedule(nil)

	h.eventually(func() bool { return h.inReview(ids...) })

	for _, id := range ids {
		var works []engine.WorkRequest

		for _, c := range h.runner.callsOf("work") {
			if c.task == id {
				works = append(works, c.work)
			}
		}

		require.Len(t, works, 2)
		assert.True(t, works[1].Resume, "task %d resumes", id)
		assert.Equal(t, first[id].work.SessionID, works[1].SessionID)
		assert.Len(t, h.recordedOf(id, engine.RunStarted), 1, "task %d has one run", id)
	}

	assert.Equal(t, 2, h.spy.created(), "one run row per task")
	assert.Empty(t, again.errs())
}

// recordedOf returns the task's events of type typ.
func (h *harness) recordedOf(taskID int64, typ string) []store.Event {
	h.t.Helper()

	return slices.DeleteFunc(h.recorded(taskID), func(e store.Event) bool { return e.Type != typ })
}

func TestSchedulerWaitsThenActs(t *testing.T) {
	h := newHarness(t)
	h.runner.works = []step[engine.WorkResult]{{res: engine.WorkResult{Outcome: engine.OutcomeUsageLimit, ResetsAt: now.Add(time.Hour)}}}

	s := h.schedule(nil)

	h.eventually(func() bool { return s.Waiting(h.task.ID) != "" })

	before := h.runner.trace()

	s.passes(t, 3)
	h.clock.Advance(time.Hour - time.Millisecond)
	s.passes(t, 3)
	assert.Equal(t, before, h.runner.trace(), "no runner call before the reset")

	h.clock.Advance(time.Millisecond)
	s.Nudge()

	h.eventually(func() bool { return h.inReview(h.task.ID) })
	assert.Equal(t, []string{
		"prepare", "work/start", "release/keep", "work/continue", "verify", "export", "release/remove",
	}, h.runner.trace())
	assert.Empty(t, s.errs())
}

func TestSchedulerEndpointPause(t *testing.T) {
	h := newHarness(t)
	second, third := h.started(), h.started()
	h.set("concurrency", "2")
	h.runner.works = []step[engine.WorkResult]{{res: engine.WorkResult{Outcome: engine.OutcomeEndpoint, Detail: "502 Bad Gateway"}}}
	g := h.gate("prepare", "work")

	s := h.schedule(nil)

	h.eventually(func() bool { return slices.Equal(g.held("prepare"), []int64{h.task.ID, second.ID}) })
	g.let(t, "prepare", h.task.ID)
	h.eventually(func() bool { return slices.Equal(g.held("work"), []int64{h.task.ID}) })
	g.let(t, "work", h.task.ID)

	first := strconv.FormatInt(h.task.ID, 10)
	h.eventually(func() bool { return strings.Contains(s.Waiting(third.ID), first) }, "the queue waits on task %s", first)

	g.let(t, "prepare", second.ID)
	h.eventually(func() bool { return strings.Contains(s.Waiting(second.ID), first) }, "the queue waits on task %s", first)
	s.passes(t, 2)

	assert.Equal(t, []string{"prepare", "release/keep"}, h.runner.callsFor(second.ID), "the prepared run waits with no container")
	assert.Empty(t, h.runner.callsFor(third.ID))

	require.NoError(t, h.loop.Retry(t.Context(), h.task.ID))
	g.free("prepare")
	g.free("work")
	s.Nudge()

	h.eventually(func() bool { return h.inReview(h.task.ID, second.ID, third.ID) })
	assert.Empty(t, s.errs())
}

func TestSchedulerUsageLimitPause(t *testing.T) {
	h := newHarness(t)
	h.set("concurrency", "1")
	h.runner.works = []step[engine.WorkResult]{{res: engine.WorkResult{Outcome: engine.OutcomeUsageLimit, ResetsAt: now.Add(time.Hour)}}}
	g := h.gate()

	s := h.schedule(nil)

	h.eventually(func() bool { return s.Waiting(h.task.ID) != "" })

	second := h.started()

	s.Nudge()
	h.eventually(func() bool {
		why := s.Waiting(second.ID)

		return why != "" && why != engine.WaitingForSlot
	})
	s.passes(t, 3)
	assert.Empty(t, h.runner.callsFor(second.ID), "no task starts before another's limit resets")

	g.holdOn("work")
	h.clock.Advance(time.Hour)
	s.Nudge()

	h.eventually(func() bool { return slices.Equal(g.held("work"), []int64{h.task.ID}) })
	h.eventually(func() bool { return s.Waiting(second.ID) == engine.WaitingForSlot })
	assert.Empty(t, h.runner.callsFor(second.ID))

	g.free("work")
	h.eventually(func() bool { return h.inReview(h.task.ID, second.ID) })
	assert.Empty(t, s.errs())
}

func TestSchedulerRunsRequestedChecks(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	web := h.project()

	api, err := h.store.CreateProject(ctx, store.Project{
		Name: "api", RepoURL: "https://example.com/api.git", BaseBranch: "main", VerifyCommand: "make test",
	})
	require.NoError(t, err)

	fault := errors.New("the container runtime is gone")
	h.runner.prepares = []step[engine.PrepareResult]{{res: preparedOK}, {err: fault}}
	h.runner.suggestion = "make test"

	require.NoError(t, h.loop.RequestCheck(ctx, web.ID))
	h.clock.Advance(time.Second)
	require.NoError(t, h.loop.RequestCheck(ctx, api.ID))
	h.clock.Advance(time.Second)
	require.NoError(t, h.loop.RequestCheck(ctx, web.ID))
	assert.True(t, now.Equal(*h.project().CheckRequestedAt), "a waiting request keeps its time")

	g := h.gate("check")
	s := h.schedule(func(s *engine.Scheduler) {
		s.Checks = true
		s.Only = func(store.Task) bool { return false }
	})

	h.eventually(func() bool { return slices.Equal(g.held("check"), []int64{web.ID}) }, "the oldest request runs first")
	s.passes(t, 2)
	assert.Len(t, h.runner.callsOf("check"), 1, "one check at a time")

	g.let(t, "check", web.ID)
	h.eventually(func() bool { return slices.Equal(g.held("check"), []int64{api.ID}) })

	got := h.project()
	assert.Nil(t, got.CheckRequestedAt)
	assert.Equal(t, store.CheckResult{Tools: preparedOK.Tools, VerifySuggestion: "make test"}, got.CheckResult)
	require.NotNil(t, got.Baseline)
	assert.True(t, got.Baseline.Passed)

	g.let(t, "check", api.ID)
	h.eventually(func() bool { return h.checkCleared(api.ID) })

	got, err = h.store.Project(ctx, api.ID)
	require.NoError(t, err)
	assert.Equal(t, fault.Error(), got.CheckResult.Fault)
	assert.Nil(t, got.Baseline)
	assert.Empty(t, s.errs())
}

func TestSchedulerOnly(t *testing.T) {
	h := newHarness(t)
	second := h.started()
	require.NoError(t, h.loop.RequestCheck(t.Context(), h.task.ProjectID))

	s := h.schedule(func(s *engine.Scheduler) { s.Only = func(t store.Task) bool { return t.ID == second.ID } })

	h.eventually(func() bool { return h.inReview(second.ID) })
	s.passes(t, 2)
	assert.Empty(t, h.runner.callsFor(h.task.ID), "a task Only does not admit is never driven")
	assert.Empty(t, h.runner.callsOf("check"), "no check runs without Checks")
	assert.NotNil(t, h.project().CheckRequestedAt)
	assert.Empty(t, s.errs())
}

func TestSchedulerReportsAnErrorOnce(t *testing.T) {
	h := newHarness(t)
	gone := errors.New("the container daemon is unreachable")
	h.runner.works = []step[engine.WorkResult]{{err: gone}}
	h.runner.releaseErr = gone

	s := h.schedule(nil)

	h.eventually(func() bool { return len(s.errs()) == 1 })
	s.passes(t, 3)
	assert.Equal(t, []int64{h.task.ID}, s.errs(), "an error is reported once while nothing changes")
	assert.Equal(t, []string{"prepare", "work/start", "release/keep"}, h.runner.trace(), "the task is not driven again")

	require.NoError(t, h.loop.Retry(t.Context(), h.task.ID))
	s.Nudge()

	h.eventually(func() bool { return len(s.errs()) == 2 })
	s.passes(t, 2)
	assert.Equal(t, []int64{h.task.ID, h.task.ID}, s.errs())
	assert.Equal(t, engine.StatusInReview, h.statusOf(h.task.ID))
}

// A pass reads every task before it starts a drive, so a higher task's open
// endpoint stop holds a lower task from the first pass on, even when Only
// leaves the stopped task out.
func TestSchedulerFirstPassSeesEveryStop(t *testing.T) {
	for _, tc := range []struct {
		name string
		only bool
	}{{"every task admitted", false}, {"the stopped task left out", true}} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			second := h.started()
			h.runner.works = []step[engine.WorkResult]{{res: engine.WorkResult{Outcome: engine.OutcomeEndpoint}}}

			_, err := h.loop.RunTask(t.Context(), second.ID)
			require.NoError(t, err)
			require.Equal(t, engine.StopEndpoint, h.lastStopOf(second.ID).Reason)

			before := h.runner.trace()
			s := h.schedule(func(s *engine.Scheduler) {
				if tc.only {
					s.Only = func(t store.Task) bool { return t.ID == h.task.ID }
				}
			})

			stopped := strconv.FormatInt(second.ID, 10)
			h.eventually(func() bool { return strings.Contains(s.Waiting(h.task.ID), stopped) }, "the queue waits on task %s", stopped)
			s.passes(t, 2)
			assert.Equal(t, before, h.runner.trace(), "no runner call while the pause holds")

			require.NoError(t, h.loop.Retry(t.Context(), second.ID))
			s.Nudge()

			h.eventually(func() bool { return h.inReview(h.task.ID) })
			assert.Empty(t, s.errs())
		})
	}
}

// A requested check takes the first free slot, before any task, and holds it
// while it runs; while every slot is taken, it waits.
func TestSchedulerCheckTakesASlot(t *testing.T) {
	h := newHarness(t)
	h.set("concurrency", "1")
	require.NoError(t, h.loop.RequestCheck(t.Context(), h.task.ProjectID))
	g := h.gate("work", "check")

	s := h.schedule(func(s *engine.Scheduler) { s.Checks = true })

	h.eventually(func() bool { return slices.Equal(g.held("check"), []int64{h.task.ProjectID}) }, "the check starts before the task")
	h.eventually(func() bool { return s.Waiting(h.task.ID) == engine.WaitingForSlot })
	s.passes(t, 2)
	assert.Empty(t, h.runner.callsFor(h.task.ID), "the check holds the slot")

	g.let(t, "check", h.task.ProjectID)
	h.eventually(func() bool { return slices.Equal(g.held("work"), []int64{h.task.ID}) })

	require.NoError(t, h.loop.RequestCheck(t.Context(), h.task.ProjectID))
	s.passes(t, 2)
	assert.Len(t, h.runner.callsOf("check"), 1, "the check waits for the slot")

	g.free("work")
	h.eventually(func() bool { return slices.Equal(g.held("check"), []int64{h.task.ProjectID}) })
	g.let(t, "check", h.task.ProjectID)
	h.eventually(func() bool { return h.inReview(h.task.ID) && h.checkCleared(h.task.ProjectID) })
	assert.Empty(t, s.errs())
}

// A stop takes no slot, so a cancelled task's interrupted run is ended and
// released at once, even while a task that became ready holds the only slot.
func TestSchedulerStopTakesNoSlot(t *testing.T) {
	h := newHarness(t)
	second := h.started()
	h.set("concurrency", "1")
	h.runner.works = []step[engine.WorkResult]{{res: engine.WorkResult{Outcome: engine.OutcomeBlocked, Questions: []string{"Which port?"}}}}
	g := h.gate("work")

	s := h.schedule(nil)

	h.eventually(func() bool { return slices.Equal(g.held("work"), []int64{h.task.ID}) })
	g.let(t, "work", h.task.ID)
	h.eventually(func() bool { return slices.Equal(g.held("work"), []int64{second.ID}) })

	var slotWaits atomic.Int64

	s.watchEach(func() {
		if s.Waiting(second.ID) == engine.WaitingForSlot {
			slotWaits.Add(1)
		}
	})

	require.NoError(t, h.loop.Answer(t.Context(), h.task.ID, "Use port 8080."))
	s.Nudge()
	h.eventually(func() bool { return s.Waiting(h.task.ID) == engine.WaitingForSlot })

	require.NoError(t, h.loop.Cancel(t.Context(), second.ID))
	s.Nudge()

	h.eventually(func() bool { return slices.Equal(g.held("work"), []int64{h.task.ID}) }, "the answered task takes the slot")
	h.eventually(func() bool { return slices.Contains(h.runner.callsFor(second.ID), "release/remove") }, "the cancelled run is released")
	assert.Equal(t, []string{"prepare", "work/start", "release/remove"}, h.runner.callsFor(second.ID))
	assert.Equal(t, []int64{h.task.ID}, g.held("work"), "the answered task still holds the slot")
	assert.Equal(t, engine.StatusCancelled, h.statusOf(second.ID))

	s.passes(t, 2)
	assert.Zero(t, slotWaits.Load(), "the cancelled task never waits for a slot")
	assert.Empty(t, s.Waiting(second.ID))

	g.free("work")
	h.eventually(func() bool { return h.inReview(h.task.ID) })
	assert.Empty(t, s.errs())
}
