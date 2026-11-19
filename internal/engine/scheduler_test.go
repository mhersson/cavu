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
	"testing/synctest"
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
	b, err := h.loop.AddTask(t.Context(), store.Task{ProjectID: h.task.ProjectID, Title: "B"}, []int64{h.task.ID}, false)
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

	h.eventually(func() bool { return h.inReview(b.ID) && slices.Contains(h.runner.callsFor(b.ID), "release/remove") }, "B rests with its home removed")
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
	h.eventually(func() bool { return len(h.recordedOf(h.task.ID, engine.HomeRemoved)) == 1 }, "the cancelled task's home is recorded removed")
	assert.False(t, h.runOpen())

	evs := h.events()
	require.GreaterOrEqual(t, len(evs), 3)
	assert.Equal(t, []string{engine.Stopped, engine.RunEnded, engine.HomeRemoved}, types(evs[len(evs)-3:]))
	assert.Equal(t, engine.StopCancelled, h.lastStop().Reason)

	work := h.runner.callsOf("work")[0]
	assert.Equal(t, []string{"prepare", "work/start", "release/run", "release/remove"}, h.runner.trace())
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
		assert.Equal(t, engine.KeepVolumes, releases[id].remove, "task %d keeps its volumes", id)
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

	h.eventually(func() bool { return h.inReview(h.task.ID) && slices.Contains(h.runner.trace(), "release/remove") }, "the task rests with its home removed")
	assert.Equal(t, []string{
		"prepare", "work/start", "release/keep", "work/continue", "verify", "export", "release/run", "release/remove",
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
	assert.Equal(t, store.CheckResult{
		Tools: preparedOK.Tools, BaseCommit: preparedOK.BaseCommit, VerifySuggestion: "make test",
	}, got.CheckResult)
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

// A failed drive is not driven again at every pass; an event drives it before
// its clock comes due and starts the clock again, and each failure is reported.
// A task the clock finds resting is read again only after an event.
func TestSchedulerReportsEachFailedDrive(t *testing.T) {
	h := newHarness(t)
	gone := errors.New("the container daemon is unreachable")
	h.runner.works = []step[engine.WorkResult]{{err: gone}}
	looks := h.logLooks()

	var releases atomic.Int64

	h.runner.gate = func(_ context.Context, c call) error {
		if c.method == "release" && releases.Add(1) <= 2 {
			return gone
		}

		return nil
	}

	s := h.schedule(nil)

	h.eventually(func() bool { return len(s.errs()) == 1 })
	s.passes(t, 3)
	assert.Equal(t, []int64{h.task.ID}, s.errs(), "an error is reported once while the clock stands")
	assert.Equal(t, []string{"prepare", "work/start", "release/keep"}, h.runner.trace(), "the task is not driven again before its clock")

	require.NoError(t, h.loop.Retry(t.Context(), h.task.ID))
	s.Nudge()

	h.eventually(func() bool { return len(s.errs()) == 2 }, "the retry drives the task, whose run's release fails")
	s.passes(t, 2)
	assert.Equal(t, []int64{h.task.ID, h.task.ID}, s.errs())
	assert.True(t, h.inReview(h.task.ID))

	reads := looks.readsOf(h.task.ID)
	h.clock.Advance(time.Minute)
	s.passes(t, 2)
	require.Greater(t, looks.readsOf(h.task.ID), reads, "the task is read a minute after its newest failure")

	reads = looks.readsOf(h.task.ID)
	h.clock.Advance(time.Hour)
	s.passes(t, 2)
	assert.Equal(t, reads, looks.readsOf(h.task.ID), "the task rests, read again only after an event")
	assert.Len(t, s.errs(), 2)
}

// A run's release that fails as its task enters a timed wait ends the drive in
// an error; the task is read again a minute later, and the wait's action runs.
func TestSchedulerWaitEndsAfterAFailedRelease(t *testing.T) {
	h := newHarness(t)
	gone := errors.New("the container daemon is unreachable")
	h.runner.works = []step[engine.WorkResult]{{err: gone}}

	var releases atomic.Int64

	h.runner.gate = func(_ context.Context, c call) error {
		if c.method == "release" && releases.Add(1) == 1 {
			return gone
		}

		return nil
	}

	s := h.schedule(nil)

	h.eventually(func() bool { return len(s.errs()) == 1 }, "the release of the stopped run fails")
	s.passes(t, 2)
	require.Equal(t, engine.StopSandbox, h.lastStop().Reason)

	before := h.runner.trace()
	h.clock.Advance(time.Minute - time.Millisecond) // past the stop's wait
	s.passes(t, 2)
	assert.Equal(t, before, h.runner.trace(), "the task is not read before a minute")

	h.clock.Advance(time.Millisecond)
	s.Nudge()

	h.eventually(func() bool { return h.inReview(h.task.ID) && slices.Contains(h.runner.trace(), "release/remove") }, "the cut action runs again")
	assert.Equal(t, []string{
		"prepare", "work/start", "release/keep", "work/continue", "verify", "export", "release/run", "release/remove",
	}, h.runner.trace())
	assert.Equal(t, []int64{h.task.ID}, s.errs())
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
	assert.Equal(t, []string{"prepare", "work/start", "release/run", "release/remove"}, h.runner.callsFor(second.ID))
	assert.Equal(t, []int64{h.task.ID}, g.held("work"), "the answered task still holds the slot")
	assert.Equal(t, engine.StatusCancelled, h.statusOf(second.ID))

	s.passes(t, 2)
	assert.Zero(t, slotWaits.Load(), "the cancelled task never waits for a slot")
	assert.Empty(t, s.Waiting(second.ID))

	g.free("work")
	h.eventually(func() bool { return h.inReview(h.task.ID) })
	assert.Empty(t, s.errs())
}

// A check asked for while that project's check runs is not lost: the running
// check leaves the later request, so the next pass runs the check again.
func TestCheckRequestedDuringACheckRunsAgain(t *testing.T) {
	h := newHarness(t)
	web := h.task.ProjectID
	require.NoError(t, h.loop.RequestCheck(t.Context(), web))
	g := h.gate("check")

	s := h.schedule(func(s *engine.Scheduler) {
		s.Checks = true
		s.Only = func(store.Task) bool { return false }
	})

	h.eventually(func() bool { return slices.Equal(g.held("check"), []int64{web}) })
	h.clock.Advance(time.Second)
	require.NoError(t, h.loop.RequestCheck(t.Context(), web))
	g.let(t, "check", web)

	h.eventually(func() bool {
		return len(h.runner.callsOf("check")) == 2 && slices.Equal(g.held("check"), []int64{web})
	}, "the later request runs the check again")
	assert.False(t, h.checkCleared(web), "the request stays until the check that started after it ends")

	g.let(t, "check", web)
	h.eventually(func() bool { return h.checkCleared(web) })
	s.passes(t, 2)
	assert.Len(t, h.runner.callsOf("check"), 2)
	assert.Empty(t, s.errs())
}

// editVerify changes the project's verify command the way a person's edit does.
func (h *harness) editVerify(command string) {
	h.t.Helper()

	p := h.project()
	p.VerifyCommand = command
	require.NoError(h.t, engine.EditProject(h.t.Context(), h.store, p))
}

// A check whose project changed under it stored nothing: its baseline comes
// from a verify command the person has replaced. The request stays, so the
// next check runs on the new ground and stores its own result.
func TestCheckOfAnEditedProjectStoresNothing(t *testing.T) {
	h := newHarness(t)
	web := h.task.ProjectID
	require.NoError(t, h.loop.RequestCheck(t.Context(), web))
	g := h.gate("check")

	s := h.schedule(func(s *engine.Scheduler) {
		s.Checks = true
		s.Only = func(store.Task) bool { return false }
	})

	h.eventually(func() bool { return slices.Equal(g.held("check"), []int64{web}) })
	h.editVerify("make check")
	g.let(t, "check", web)

	h.eventually(func() bool {
		return len(h.runner.callsOf("check")) == 2 && slices.Equal(g.held("check"), []int64{web})
	}, "the request stays, so the check runs again on the new command")

	got := h.project()
	assert.Nil(t, got.Baseline)
	assert.False(t, engine.Runnable(got))
	assert.NotNil(t, got.CheckRequestedAt)
	assert.Equal(t, store.CheckResult{}, got.CheckResult)

	g.let(t, "check", web)
	h.eventually(func() bool { return h.checkCleared(web) })

	got = h.project()
	require.NotNil(t, got.Baseline)
	assert.True(t, engine.Runnable(got))
	assert.Equal(t, preparedOK.BaseCommit, got.CheckResult.BaseCommit)
	assert.Empty(t, s.errs())
}

func TestCheckByAPersonOfAnEditedProjectStoresNothing(t *testing.T) {
	h := newHarness(t)
	web := h.task.ProjectID
	g := h.gate("check")

	done := make(chan error, 1)

	go func() {
		_, _, err := h.loop.CheckProject(t.Context(), web)
		done <- err
	}()

	h.eventually(func() bool { return slices.Equal(g.held("check"), []int64{web}) })
	h.editVerify("make check")
	g.let(t, "check", web)

	require.Error(t, <-done, "the person is told to run it again")

	got := h.project()
	assert.Nil(t, got.Baseline)
	assert.False(t, engine.Runnable(got))
	assert.Equal(t, store.CheckResult{}, got.CheckResult)
}

func TestSchedulerDrivingCoversARelease(t *testing.T) {
	h := newHarness(t)
	g := h.gate("release")

	s := h.schedule(nil)

	h.eventually(func() bool { return slices.Equal(g.held("release"), []int64{h.task.ID}) })
	assert.Equal(t, engine.StatusInReview, h.statusOf(h.task.ID), "the task rests while its run is released")
	assert.True(t, s.Driving())

	g.free("release")
	h.eventually(func() bool { return !s.Driving() })
	assert.True(t, h.inReview(h.task.ID))
	assert.Empty(t, s.errs())
}

// lookLog records the clock's time at each look at a pull request the runner
// is asked for, as an offset from when it began, and counts each task's reads:
// every snapshot of a task begins with its newest event id.
type lookLog struct {
	engine.Store

	h      *harness
	start  time.Time
	mu     sync.Mutex
	at     []time.Duration
	reads  map[int64]int
	looked int // the reads of the task looked at, when the newest look began
}

// logLooks makes the runner record each look and the loop's store count each
// read. Call it before the scheduler runs.
func (h *harness) logLooks() *lookLog {
	l := &lookLog{Store: h.loop.Store, h: h, start: h.clock.Now(), reads: map[int64]int{}}
	h.loop.Store = l

	h.runner.onCall = func(c call) {
		if c.method == "observe" {
			l.mu.Lock()
			defer l.mu.Unlock()

			l.at = append(l.at, h.clock.Now().Sub(l.start))
			l.looked = l.reads[c.task]
		}
	}

	return l
}

func (l *lookLog) LastEventID(ctx context.Context, taskID int64) (int64, error) {
	l.mu.Lock()
	l.reads[taskID]++
	l.mu.Unlock()

	return l.Store.LastEventID(ctx, taskID)
}

func (l *lookLog) offsets() []time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()

	return slices.Clone(l.at)
}

func (l *lookLog) readsOf(id int64) int {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.reads[id]
}

// after waits for the newest look's end, then moves the clock to a millisecond
// short of gap past that look, where the task is not read, and to gap past it,
// where the task is looked at again.
func (l *lookLog) after(t *testing.T, s *running, gap time.Duration) {
	t.Helper()

	id := l.h.task.ID

	l.h.eventually(func() bool {
		l.mu.Lock()
		defer l.mu.Unlock()

		return l.reads[id] > l.looked
	}, "the look's end has the task read again")

	seen, reads := l.offsets(), l.readsOf(id)
	l.h.clock.Advance(l.start.Add(seen[len(seen)-1] + gap - time.Millisecond).Sub(l.h.clock.Now()))
	s.passes(t, 2)
	require.Equal(t, reads, l.readsOf(id), "the task is not read before %s", gap)

	l.h.clock.Advance(time.Millisecond)
	s.Nudge()
	l.h.eventually(func() bool { return len(l.offsets()) > len(seen) }, "a look %s after the last", gap)
}

func seconds(ss ...int) []time.Duration {
	out := make([]time.Duration, len(ss))
	for i, s := range ss {
		out[i] = time.Duration(s) * time.Second
	}

	return out
}

// why is the reason of the action Decide gives the task now.
func (h *harness) why(id int64) string {
	snap, err := h.loop.Snapshot(context.Background(), id)
	if err != nil {
		return ""
	}

	return engine.Decide(snap).Why
}

func TestSchedulerPollsWhileChecksArePending(t *testing.T) {
	h := newHarness(t)
	h.watching()

	passed := checksPassed
	passed.Draft = false
	h.runner.observes = []step[engine.Observation]{{res: checksPending}, {res: checksPending}, {res: passed}, {res: passed}, {res: passed}}
	looks := h.logLooks()

	s := h.schedule(nil)

	h.eventually(func() bool { return len(looks.offsets()) == 1 }, "the first pass looks")

	for _, gap := range []time.Duration{10 * time.Second, 10 * time.Second, time.Minute, time.Minute} {
		looks.after(t, s, gap)
	}

	assert.Equal(t, seconds(0, 10, 20, 80, 140), looks.offsets())
	h.eventually(func() bool { return s.Waiting(h.task.ID) == h.why(h.task.ID) }, "the task waits for what the watch waits for")
	assert.NotEmpty(t, s.Waiting(h.task.ID))
	assert.Empty(t, s.errs())
}

func TestSchedulerBacksOffOnForgeErrors(t *testing.T) {
	t.Run("GitHub fails", func(t *testing.T) {
		h := newHarness(t)
		h.watching()

		down := errors.New("GitHub answered 502 Bad Gateway")
		failed := step[engine.Observation]{err: down}
		h.runner.observes = []step[engine.Observation]{failed, failed, failed, failed, failed, failed, failed, {res: checksPending}, failed}
		looks := h.logLooks()

		s := h.schedule(nil)

		h.eventually(func() bool { return len(looks.offsets()) == 1 }, "the first pass looks")

		for _, gap := range []time.Duration{10 * time.Second, 20 * time.Second, 40 * time.Second, 80 * time.Second, 160 * time.Second, 5 * time.Minute} {
			looks.after(t, s, gap)
		}

		next := looks.start.Add(910 * time.Second).UTC().Format(time.RFC3339)
		h.eventually(func() bool {
			why := s.Waiting(h.task.ID)

			return strings.Contains(why, down.Error()) && strings.Contains(why, next)
		}, "the task names the error and when the next look comes, %s", next)
		assert.Equal(t, []int64{h.task.ID}, s.errs(), "a run of failures is reported once")

		looks.after(t, s, 5*time.Minute) // never longer; this look succeeds
		h.eventually(func() bool { return s.Waiting(h.task.ID) == h.why(h.task.ID) }, "a success ends the run of failures")

		looks.after(t, s, 10*time.Second) // the cadence again; this look fails
		h.eventually(func() bool { return len(s.errs()) == 2 }, "a new run of failures is reported")

		assert.Equal(t, seconds(0, 10, 30, 70, 150, 310, 610, 910, 920), looks.offsets())
	})

	t.Run("the look's config cannot be built", func(t *testing.T) {
		h := newHarness(t)
		h.watching()

		p := h.project()
		p.SecretVars = []string{"NPM_TOKEN"}
		require.NoError(t, h.store.UpdateProject(t.Context(), p))

		before := len(h.events())

		s := h.schedule(nil)

		h.eventually(func() bool { return strings.Contains(s.Waiting(h.task.ID), "project.web.NPM_TOKEN") }, "the task names the secret it misses")
		s.passes(t, 2)
		assert.Empty(t, h.runner.callsOf("observe"))
		assert.Len(t, h.events(), before, "nothing is recorded")
		assert.Equal(t, []int64{h.task.ID}, s.errs())
	})
}

func TestSchedulerWaitsOutARateLimit(t *testing.T) {
	tests := []struct {
		name         string
		resets, wait time.Duration // from the look that met the limit
	}{
		{name: "the limit lifts within the hour", resets: 20 * time.Minute, wait: 20 * time.Minute},
		{name: "the limit lifts later", resets: 3 * time.Hour, wait: time.Hour},
		{name: "the limit has lifted already", wait: 10 * time.Second}, // a Retry-After of 0, or a clock ahead of GitHub's: the backoff
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			h.watching()

			limited := &engine.RateLimited{Until: h.clock.Now().Add(tt.resets)}
			h.runner.observes = []step[engine.Observation]{{err: limited}, {res: checksPending}}
			looks := h.logLooks()

			s := h.schedule(nil)

			h.eventually(func() bool { return len(looks.offsets()) == 1 }, "the first pass looks")

			next := looks.start.Add(tt.wait).UTC().Format(time.RFC3339)
			h.eventually(func() bool { return strings.Contains(s.Waiting(h.task.ID), next) }, "the task says when the next look comes, %s", next)

			looks.after(t, s, tt.wait)
			assert.Equal(t, []time.Duration{0, tt.wait}, looks.offsets())
			assert.Equal(t, []int64{h.task.ID}, s.errs())
		})
	}

	// The checks' timeout is Decide's clock: the task is read and stops when it
	// comes, and the stop brings no look before the limit's wait ends.
	t.Run("the checks' timeout comes during the wait", func(t *testing.T) {
		h := newHarness(t)
		h.set("ci_timeout_minutes", "30")
		h.watching()

		limited := &engine.RateLimited{Until: h.clock.Now().Add(3 * time.Hour)}
		h.runner.observes = []step[engine.Observation]{{res: checksPending}, {err: limited}, {res: checksPending}}
		looks := h.logLooks()

		s := h.schedule(nil)

		h.eventually(func() bool { return len(looks.offsets()) == 1 }, "the first pass looks")
		looks.after(t, s, 10*time.Second) // meets the limit

		next := looks.start.Add(time.Hour + 10*time.Second).UTC().Format(time.RFC3339)
		h.eventually(func() bool { return strings.Contains(s.Waiting(h.task.ID), next) }, "the task says when the next look comes, %s", next)

		h.clock.Advance(30*time.Minute - 10*time.Second - time.Millisecond)
		s.passes(t, 2)
		assert.Empty(t, h.stopsOf())

		h.clock.Advance(time.Millisecond)
		s.Nudge()

		h.eventually(func() bool {
			return slices.Equal(h.stopsOf(), []string{engine.StopCITimeout}) && !s.Driving()
		}, "the task stops when its checks' time is up")
		s.passes(t, 2) // the pass after the stop's drive reads the task again
		assert.Contains(t, s.Waiting(h.task.ID), next, "the stopped task is watched on the same clock")

		looks.after(t, s, time.Hour)
		assert.Equal(t, seconds(0, 10, 3610), looks.offsets())
	})
}

// A look, a ready-marking and a clean start no container, so a watched task's
// merge is recorded and its home removed while a session holds the only slot.
func TestSchedulerWatchNeedsNoSlot(t *testing.T) {
	h := newHarness(t)
	h.watching()
	h.set("concurrency", "1")

	merged := checksPassed
	merged.State = engine.PRMerged
	h.runner.observes = []step[engine.Observation]{{res: checksPending}, {res: checksPassed}, {res: merged}}
	looks := h.logLooks()
	second := h.started()
	g := h.gate("work")

	s := h.schedule(nil)

	var slotWaits atomic.Int64

	s.watchEach(func() {
		if s.Waiting(h.task.ID) == engine.WaitingForSlot {
			slotWaits.Add(1)
		}
	})

	h.eventually(func() bool { return slices.Equal(g.held("work"), []int64{second.ID}) && len(looks.offsets()) == 1 })
	looks.after(t, s, 10*time.Second) // the checks passed on a draft
	h.eventually(func() bool { return len(h.recordedOf(h.task.ID, engine.PRReady)) == 1 }, "the pull request is marked ready")

	h.clock.Advance(time.Minute)
	s.Nudge()

	h.eventually(func() bool {
		return h.statusOf(h.task.ID) == engine.StatusDone && len(h.recordedOf(h.task.ID, engine.HomeRemoved)) == 1
	}, "the merge is recorded and the task's home removed")
	assert.Equal(t, []int64{second.ID}, g.held("work"), "the session still holds the only slot")
	assert.Zero(t, slotWaits.Load(), "the watched task never waits for a slot")

	trace := h.runner.callsFor(h.task.ID)
	assert.Equal(t, []string{"observe", "observe", "ready", "observe", "release/remove"}, trace[len(trace)-5:])
	assert.Empty(t, s.errs())
}

// A new scheduler keeps no clock of the old one's: it looks at a watched task
// in its first pass.
func TestSchedulerRestartResumesTheWatch(t *testing.T) {
	h := newHarness(t)
	h.watching()
	h.runner.observes = []step[engine.Observation]{{res: checksPending}, {res: checksPending}}
	g := h.gate("observe")

	first := h.schedule(nil)

	h.eventually(func() bool { return slices.Equal(g.held("observe"), []int64{h.task.ID}) })
	g.let(t, "observe", h.task.ID)
	h.eventually(func() bool { return len(h.eventsOf(engine.PRObserved)) == 1 })
	h.clock.Advance(5 * time.Second) // short of the old scheduler's next look
	require.NoError(t, first.stop())
	assert.Empty(t, first.errs())

	again := h.schedule(nil)

	h.eventually(func() bool { return slices.Equal(g.held("observe"), []int64{h.task.ID}) && again.ended.Load() == 1 })
	assert.Equal(t, int64(1), again.ended.Load(), "the look starts in the first pass")
	assert.Len(t, h.runner.callsOf("observe"), 2)

	require.NoError(t, again.stop())
	assert.Equal(t, []int64{h.task.ID}, g.cutOf("observe"))
	assert.Empty(t, again.errs(), "a look cut by Run's end is not reported")
}

// Run's end waits for a look in flight, as for a drive. In a bubble, Wait
// returns once every other goroutine is durably blocked, so a Run that has not
// returned is seen, not raced; a look left behind fails the bubble.
func TestSchedulerEndWaitsForALook(t *testing.T) {
	h := newHarness(t)
	h.watching()

	synctest.Test(t, func(t *testing.T) {
		g := h.gate("observe")

		// A look cut by Run's end returns only once the test lets it.
		release := make(chan struct{})
		h.runner.gate = func(ctx context.Context, c call) error {
			err := g.wait(ctx, c)
			if err != nil {
				<-release
			}

			return err
		}

		ctx, cancel := context.WithCancel(t.Context())
		s := &engine.Scheduler{Loop: h.loop, Tick: time.Hour}
		ended := make(chan error, 1)

		go func() { ended <- s.Run(ctx) }()

		synctest.Wait()
		require.Equal(t, []int64{h.task.ID}, g.held("observe"), "the first pass looks")

		cancel()
		synctest.Wait()
		require.Equal(t, []int64{h.task.ID}, g.cutOf("observe"))
		assert.Empty(t, ended, "Run has not returned while the look is in flight")

		close(release)
		synctest.Wait()
		require.Len(t, ended, 1, "Run returns once the look has")
		assert.NoError(t, <-ended)
	})
}

// A drive that keeps failing, as the clean of a merged task whose release
// fails, is read again a minute after the first failure, then after twice the
// last wait up to 30 minutes, and never between: the clock of its watch is
// dropped. Each failure is reported; once the clean succeeds, the task rests.
func TestSchedulerFailedDriveAfterAWatchBacksOff(t *testing.T) {
	h := newHarness(t)
	h.watching()

	merged := checksPassed
	merged.State = engine.PRMerged
	h.runner.observes = []step[engine.Observation]{{res: merged}}
	h.runner.releaseErr = errors.New("the volume is in use")
	looks := h.logLooks()
	releases := len(h.runner.callsOf("release"))

	s := h.schedule(nil)

	h.eventually(func() bool { return len(s.errs()) == 1 }, "the clean of the merged task fails")

	gaps := []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 16 * time.Minute, 30 * time.Minute, 30 * time.Minute}
	for i, gap := range gaps {
		s.passes(t, 2)

		reads := looks.readsOf(h.task.ID)
		h.clock.Advance(gap - time.Millisecond)
		s.passes(t, 2)
		require.Equal(t, reads, looks.readsOf(h.task.ID), "the task is not read before %s", gap)

		h.clock.Advance(time.Millisecond)
		s.Nudge()
		h.eventually(func() bool { return len(s.errs()) == i+2 }, "the clean fails again %s after the last failure", gap)
	}

	assert.Len(t, h.runner.callsOf("release"), releases+1+len(gaps), "one clean at each read")

	h.runner.mu.Lock()
	h.runner.releaseErr = nil
	h.runner.mu.Unlock()
	s.passes(t, 2)
	h.clock.Advance(30 * time.Minute)
	s.Nudge()

	h.eventually(func() bool { return len(h.recordedOf(h.task.ID, engine.HomeRemoved)) == 1 && !s.Driving() }, "the clean succeeds")
	s.passes(t, 2)

	reads := looks.readsOf(h.task.ID)
	h.clock.Advance(time.Hour)
	s.passes(t, 2)
	assert.Equal(t, reads, looks.readsOf(h.task.ID), "the task rests, read again only after an event")
	assert.Len(t, s.errs(), 1+len(gaps))
}

// A failed check's fix run starts a container, so it waits for a free slot.
func TestWatchStartsAFixRunInASlot(t *testing.T) {
	h := newHarness(t)
	h.watching()
	h.set("concurrency", "1")
	h.runner.observes = []step[engine.Observation]{{res: checksFailed}}
	second := h.started()
	g := h.gate("work")

	s := h.schedule(nil)

	h.eventually(func() bool { return slices.Equal(g.held("work"), []int64{second.ID}) })
	h.eventually(func() bool { return s.Waiting(h.task.ID) == engine.WaitingForSlot }, "the fix run waits for the slot")
	s.passes(t, 2)
	assert.Equal(t, []engine.RunStartedPayload{initialRun}, h.runStarts(), "no fix run starts while the session holds the slot")
	assert.Len(t, h.runner.callsOf("observe"), 1, "a task that waits for a slot is not looked at")

	g.holdOn("prepare")
	g.free("work")

	h.eventually(func() bool { return slices.Equal(g.held("prepare"), []int64{h.task.ID}) }, "the fix run starts once the slot is free")
	assert.Equal(t, []engine.RunStartedPayload{initialRun, ciFixFrom("head456")}, h.runStarts())
	assert.Empty(t, s.errs())
}
