package engine_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/mhersson/cavu/internal/auth"
	"github.com/mhersson/cavu/internal/engine"
	"github.com/mhersson/cavu/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// clock is a fake clock moved by hand.
type clock struct {
	mu sync.Mutex
	at time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.at
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.at = c.at.Add(d)
}

type step[T any] struct {
	res T
	err error
}

func pop[T any](q *[]step[T], def T) (T, error) {
	if len(*q) == 0 {
		return def, nil
	}

	s := (*q)[0]
	*q = (*q)[1:]

	return s.res, s.err
}

type call struct {
	method string
	task   int64
	runID  int64
	work   engine.WorkRequest
	review engine.ReviewRequest
	remove engine.Remove
	commit string // the commit an export was given
	pub    engine.PublishRequest
	number int    // the pull request an observe or a reply was given
	nodeID string // the pull request a ready was given
	body   string // a reply's
	ref    engine.RunRef
	eco    []string         // the config's ecosystems
	cfg    engine.RunConfig // the config of every call but a release
}

func (c call) String() string {
	switch c.method {
	case "work":
		return "work/" + c.work.Mode
	case "review":
		if c.review.FixBase != "" {
			return "review/scoped"
		}
	case "release":
		return "release/" + [...]string{engine.KeepVolumes: "keep", engine.RemoveRun: "run", engine.RemoveAll: "remove"}[c.remove]
	}

	return c.method
}

// fakeRunner returns its queued results, then a success, and records every call.
type fakeRunner struct {
	mu         sync.Mutex
	calls      []call
	prepares   []step[engine.PrepareResult]
	works      []step[engine.WorkResult]
	verifies   []step[engine.VerifyResult]
	exports    []step[engine.ExportResult]
	reviews    []step[engine.ReviewResult]
	publishes  []step[engine.PublishResult]
	observes   []step[engine.Observation]
	readies    []step[struct{}]
	replies    []step[struct{}]
	releaseErr error
	suggestion string                                  // every check's verify suggestion
	progress   []engine.Progress                       // sent during every Work, each from its own goroutine
	onCall     func(c call)                            // runs as a call starts
	gate       func(ctx context.Context, c call) error // runs after onCall; an error is the call's
}

var (
	preparedOK = engine.PrepareResult{BaseCommit: "base123", Tools: []string{"go"}, Baseline: "passed"}
	workDone   = engine.WorkResult{
		Outcome: engine.OutcomeDone, Changed: true, Title: "Fix login", Summary: "Fixed it.", Turns: 3, HeadCommit: "head456",
		Usage: []store.Usage{{Source: "agent", Model: "model-a", Input: 10, Output: 5}},
	}
	verifyOK   = engine.VerifyResult{Result: engine.ResultPassed, Command: "make test", Output: "ok"}
	reviewedOK = engine.ReviewResult{Outcome: engine.ReviewReviewed, Asked: engine.Verdict{Pass: true}, Built: engine.Verdict{Pass: true}}
	exportOK   = engine.ExportResult{
		HeadCommit: "head456", Diff: []byte("diff --git a/AGENTS.md b/AGENTS.md\n"), Bundle: []byte("# v2 git bundle\n"), FilesChanged: 1,
		DiffStat: []string{"AGENTS.md | 1 +", "1 file changed, 1 insertion(+)"}, Files: []engine.ChangedFile{{Path: "AGENTS.md", Size: 12}},
	}
	publishedOK = engine.PublishResult{
		Branch: "cavu/fix-the-login", Head: "head456", Number: 7, URL: "https://example.com/web/pull/7", NodeID: "PR_7",
	}
	observedOK = engine.Observation{State: engine.PROpen, Draft: true, Head: "head456", Checks: engine.ChecksPassed}
)

func (r *fakeRunner) begin(ctx context.Context, c call) error {
	r.mu.Lock()
	r.calls = append(r.calls, c)
	hook, gate := r.onCall, r.gate
	r.mu.Unlock()

	if hook != nil {
		hook(c)
	}

	if gate != nil {
		return gate(ctx, c)
	}

	return nil
}

func (r *fakeRunner) Prepare(ctx context.Context, c engine.RunConfig) (engine.PrepareResult, error) {
	if err := r.begin(ctx, call{method: "prepare", task: c.TaskID, runID: c.RunID, eco: c.Ecosystems, cfg: c}); err != nil {
		return engine.PrepareResult{}, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	return pop(&r.prepares, preparedOK)
}

// CheckEnvironment answers from the prepare queue: a check is a prepare.
func (r *fakeRunner) CheckEnvironment(ctx context.Context, c engine.RunConfig) (engine.PrepareResult, string, error) {
	if err := r.begin(ctx, call{method: "check", runID: c.RunID, cfg: c}); err != nil {
		return engine.PrepareResult{}, "", err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	res, err := pop(&r.prepares, preparedOK)

	return res, r.suggestion, err
}

func (r *fakeRunner) Work(ctx context.Context, c engine.RunConfig, w engine.WorkRequest, progress func(engine.Progress)) (engine.WorkResult, error) {
	if err := r.begin(ctx, call{method: "work", task: c.TaskID, runID: c.RunID, work: w, eco: c.Ecosystems, cfg: c}); err != nil {
		return engine.WorkResult{}, err
	}

	var wg sync.WaitGroup
	for _, p := range r.progress {
		wg.Go(func() { progress(p) })
	}

	wg.Wait()

	r.mu.Lock()
	defer r.mu.Unlock()

	return pop(&r.works, workDone)
}

func (r *fakeRunner) Verify(ctx context.Context, c engine.RunConfig) (engine.VerifyResult, error) {
	if err := r.begin(ctx, call{method: "verify", task: c.TaskID, runID: c.RunID, eco: c.Ecosystems, cfg: c}); err != nil {
		return engine.VerifyResult{}, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	return pop(&r.verifies, verifyOK)
}

func (r *fakeRunner) Export(ctx context.Context, c engine.RunConfig, commit string) (engine.ExportResult, error) {
	if err := r.begin(ctx, call{method: "export", task: c.TaskID, runID: c.RunID, commit: commit, eco: c.Ecosystems, cfg: c}); err != nil {
		return engine.ExportResult{}, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	return pop(&r.exports, exportOK)
}

func (r *fakeRunner) Publish(ctx context.Context, c engine.RunConfig, p engine.PublishRequest) (engine.PublishResult, error) {
	if err := r.begin(ctx, call{method: "publish", task: c.TaskID, runID: c.RunID, pub: p, eco: c.Ecosystems, cfg: c}); err != nil {
		return engine.PublishResult{}, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	return pop(&r.publishes, publishedOK)
}

func (r *fakeRunner) Observe(ctx context.Context, c engine.RunConfig, number int) (engine.Observation, error) {
	if err := r.begin(ctx, call{method: "observe", task: c.TaskID, runID: c.RunID, number: number, cfg: c}); err != nil {
		return engine.Observation{}, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	return pop(&r.observes, observedOK)
}

func (r *fakeRunner) Ready(ctx context.Context, c engine.RunConfig, nodeID string) error {
	if err := r.begin(ctx, call{method: "ready", task: c.TaskID, runID: c.RunID, nodeID: nodeID, cfg: c}); err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	_, err := pop(&r.readies, struct{}{})

	return err
}

func (r *fakeRunner) Reply(ctx context.Context, c engine.RunConfig, number int, body string) error {
	if err := r.begin(ctx, call{method: "reply", task: c.TaskID, runID: c.RunID, number: number, body: body, cfg: c}); err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	_, err := pop(&r.replies, struct{}{})

	return err
}

func (r *fakeRunner) Review(ctx context.Context, c engine.RunConfig, rv engine.ReviewRequest, _ func(engine.Progress)) (engine.ReviewResult, error) {
	if err := r.begin(ctx, call{method: "review", task: c.TaskID, runID: c.RunID, review: rv, eco: c.Ecosystems, cfg: c}); err != nil {
		return engine.ReviewResult{}, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	return pop(&r.reviews, reviewedOK)
}

func (r *fakeRunner) Release(ctx context.Context, ref engine.RunRef, remove engine.Remove) error {
	if err := r.begin(ctx, call{method: "release", task: ref.TaskID, runID: ref.RunID, remove: remove, ref: ref}); err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	return r.releaseErr
}

func (r *fakeRunner) trace() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]string, len(r.calls))
	for i, c := range r.calls {
		out[i] = c.String()
	}

	return out
}

func (r *fakeRunner) callsOf(method string) []call {
	r.mu.Lock()
	defer r.mu.Unlock()

	var out []call

	for _, c := range r.calls {
		if c.method == method {
			out = append(out, c)
		}
	}

	return out
}

// spyStore wraps the store to put a rival event in front of chosen claims, to
// fail chosen appends and to see run rows written.
type spyStore struct {
	*store.Store

	mu         sync.Mutex
	claims     int
	rival      func(claim int) (store.Event, bool)
	failAppend func(events []store.Event) error
	failUpdate func(r store.Run) error
	createRuns int
	runWrites  []store.Run
	usageTies  []string // per append with usage rows, the type of its last event, which the rows are tied to
}

func (s *spyStore) Append(ctx context.Context, taskID, expectLast int64, events []store.Event, usage []store.Usage) (int64, error) {
	s.mu.Lock()
	rival, fail := s.rival, s.failAppend

	if expectLast >= 0 {
		s.claims++
	}

	n := s.claims

	if len(usage) > 0 && len(events) > 0 {
		s.usageTies = append(s.usageTies, events[len(events)-1].Type)
	}

	s.mu.Unlock()

	if fail != nil {
		if err := fail(events); err != nil {
			return 0, err
		}
	}

	if expectLast >= 0 && rival != nil {
		if e, ok := rival(n); ok {
			if _, err := s.Store.Append(ctx, taskID, -1, []store.Event{e}, nil); err != nil {
				return 0, err
			}
		}
	}

	return s.Store.Append(ctx, taskID, expectLast, events, usage)
}

func (s *spyStore) CreateRun(ctx context.Context, r store.Run) (store.Run, error) {
	s.mu.Lock()
	s.createRuns++
	s.mu.Unlock()

	return s.Store.CreateRun(ctx, r)
}

func (s *spyStore) UpdateRun(ctx context.Context, r store.Run) error {
	s.mu.Lock()
	fail := s.failUpdate
	s.mu.Unlock()

	if fail != nil {
		if err := fail(r); err != nil {
			return err
		}
	}

	err := s.Store.UpdateRun(ctx, r)
	if err == nil {
		s.mu.Lock()
		s.runWrites = append(s.runWrites, r)
		s.mu.Unlock()
	}

	return err
}

// ended returns the outcome each run row was ended with.
func (s *spyStore) ended() map[int64]string {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := map[int64]string{}

	for _, r := range s.runWrites {
		if r.EndedAt != nil {
			out[r.ID] = r.Outcome
		}
	}

	return out
}

// row returns the last write of the run row with id.
func (s *spyStore) row(id int64) store.Run {
	s.mu.Lock()
	defer s.mu.Unlock()

	var out store.Run

	for _, r := range s.runWrites {
		if r.ID == id {
			out = r
		}
	}

	return out
}

func (s *spyStore) created() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.createRuns
}

type harness struct {
	t      *testing.T
	store  *store.Store
	spy    *spyStore
	vault  *auth.Vault
	runner *fakeRunner
	clock  *clock
	loop   *engine.Loop
	task   store.Task
	start  int64 // id of the task_started event
}

var loopSettings = map[string]string{
	"instance_id":   "inst-1",
	"endpoint_mode": "subscription",
	"model_work":    "model-a",
	"commit_name":   "CAVU",
	"commit_email":  "cavu@example.com",
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	ctx := t.Context()

	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "cavu.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	v, err := auth.NewVault(s, bytes.Repeat([]byte{7}, auth.MasterKeyLen))
	require.NoError(t, err)

	for k, val := range loopSettings {
		require.NoError(t, s.SetSetting(ctx, k, val))
	}

	// The readiness turn, the review and the publish are off: only the tests that turn them on are about them.
	p, err := s.CreateProject(ctx, store.Project{
		Name: "web", RepoURL: "https://example.com/web.git", BaseBranch: "main", SetupCommand: "make setup", VerifyCommand: "make test",
		Switches: map[string]bool{"readiness": false, "review": false, "publish": false},
	})
	require.NoError(t, err)

	task, err := s.CreateTask(ctx, store.Task{ProjectID: p.ID, Title: "Fix the login", Body: "It fails."})
	require.NoError(t, err)

	h := &harness{t: t, store: s, spy: &spyStore{Store: s}, vault: v, runner: &fakeRunner{}, clock: &clock{at: now}, task: task}
	h.loop = &engine.Loop{Store: h.spy, Vault: v, Runner: h.runner, Now: h.clock.Now, DataDir: t.TempDir(), Image: "cavu-sandbox:test"}
	h.start = h.add(0, engine.TaskStarted, "user", "")

	return h
}

// add appends one event outside the loop and returns its id.
func (h *harness) add(runID int64, typ, actor, payload string) int64 {
	h.t.Helper()

	if payload == "" {
		payload = "{}"
	}

	id, err := h.store.Append(h.t.Context(), h.task.ID, -1,
		[]store.Event{{RunID: runID, Type: typ, Actor: actor, Payload: json.RawMessage(payload)}}, nil)
	require.NoError(h.t, err)

	return id
}

// seed opens a run row the way a dead process left it: the row, then events.
func (h *harness) seed(events ...store.Event) store.Run {
	h.t.Helper()

	r, err := h.store.CreateRun(h.t.Context(), store.Run{TaskID: h.task.ID, Reason: "initial", StartedAt: now})
	require.NoError(h.t, err)

	for _, e := range events {
		h.add(r.ID, e.Type, "engine", string(e.Payload))
	}

	return r
}

func (h *harness) run(ctx context.Context) (engine.Status, error) {
	return h.loop.RunTask(ctx, h.task.ID)
}

// events returns the events the loop wrote after task_started.
func (h *harness) events() []store.Event {
	h.t.Helper()

	evs, err := h.store.Events(h.t.Context(), h.task.ID, h.start, 0)
	require.NoError(h.t, err)

	return evs
}

func (h *harness) eventsOf(typ string) []store.Event {
	var out []store.Event

	for _, e := range h.events() {
		if e.Type == typ {
			out = append(out, e)
		}
	}

	return out
}

func (h *harness) lastStop() engine.StoppedPayload {
	h.t.Helper()

	return h.lastStopOf(h.task.ID)
}

// lastStopOf returns the newest stop of the task.
func (h *harness) lastStopOf(taskID int64) engine.StoppedPayload {
	h.t.Helper()

	var stops []store.Event

	for _, e := range h.recorded(taskID) {
		if e.Type == engine.Stopped {
			stops = append(stops, e)
		}
	}

	require.NotEmpty(h.t, stops)

	return payloadOf[engine.StoppedPayload](h.t, stops[len(stops)-1])
}

func types(evs []store.Event) []string {
	out := make([]string, len(evs))
	for i, e := range evs {
		out[i] = e.Type
	}

	return out
}

func payloadOf[T any](t *testing.T, e store.Event) T {
	t.Helper()

	var p T
	require.NoError(t, json.Unmarshal(e.Payload, &p), "payload of %s", e.Type)

	return p
}

var uuidV4 = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

var happyTrace = []string{"prepare", "work/start", "verify", "export", "release/run", "release/remove"}

func TestLoopHappyPath(t *testing.T) {
	h := newHarness(t)

	status, err := h.run(t.Context())
	require.NoError(t, err)
	assert.Equal(t, engine.StatusInReview, status)
	assert.Equal(t, happyTrace, h.runner.trace())

	evs := h.events()
	assert.Equal(t, []string{
		engine.RunStarted, engine.PrepareStarted, engine.Prepared, engine.SessionStarted, engine.SessionEnded,
		engine.VerifyStarted, engine.VerifyEnded, engine.ExportStarted, engine.Exported, engine.RunEnded, engine.HomeRemoved,
	}, types(evs))

	actors := make([]string, len(evs))
	for i, e := range evs {
		actors[i] = e.Actor
	}

	assert.Equal(t, []string{
		"engine", "engine", "runner", "engine", "runner", "engine", "runner", "engine", "runner", "engine", "engine",
	}, actors)

	runID := evs[0].RunID
	for _, e := range evs {
		assert.Equal(t, runID, e.RunID, "%s carries the run id", e.Type)
	}

	for _, i := range []int{1, 3, 5, 7, 9, 10} {
		assert.NotEmpty(t, payloadOf[struct{ Why string }](t, evs[i]).Why, "%s records why", evs[i].Type)
	}

	assert.Equal(t, engine.RunStartedPayload{Reason: "initial"}, payloadOf[engine.RunStartedPayload](t, evs[0]))
	assert.Equal(t, engine.RunExported, payloadOf[engine.RunEndedPayload](t, evs[9]).Outcome)

	work := h.runner.callsOf("work")[0].work
	assert.Regexp(t, uuidV4, work.SessionID)
	assert.False(t, work.Resume)
	assert.Equal(t, work.SessionID, payloadOf[engine.SessionStartedPayload](t, evs[3]).SessionID)

	for _, c := range h.runner.calls {
		assert.Equal(t, runID, c.runID, "%s runs on the run", c)
	}

	assert.Equal(t, map[int64]string{runID: engine.RunExported}, h.spy.ended())
	assert.Equal(t, engine.RunRef{
		InstanceID: "inst-1", Image: "cavu-sandbox:test", TaskID: h.task.ID, RunID: runID, ProjectID: h.task.ProjectID,
	}, h.runner.callsOf("release")[0].ref)

	assert.Equal(t, work.SessionID, h.spy.row(runID).SessionID, "the run row keeps the session id")

	task, err := h.store.Task(t.Context(), h.task.ID)
	require.NoError(t, err)
	assert.Equal(t, "base123", task.BaseCommit)

	usage, err := h.store.UsageTotals(t.Context(), h.task.ID)
	require.NoError(t, err)
	require.Len(t, usage, 1)
	assert.Equal(t, int64(10), usage[0].Input)
}

func TestLoopVerifyFixRounds(t *testing.T) {
	h := newHarness(t)

	failed := step[engine.VerifyResult]{res: engine.VerifyResult{
		Result: engine.ResultFailed, Command: "make test", ExitCode: 2, Output: "FAIL TestLogin",
	}}
	h.runner.verifies = []step[engine.VerifyResult]{failed, failed, failed}

	status, err := h.run(t.Context())
	require.NoError(t, err)
	assert.Equal(t, engine.StatusBlocked, status)
	assert.Equal(t, []string{
		"prepare", "work/start", "verify", "work/fix_verify", "verify", "work/fix_verify", "verify", "release/keep",
	}, h.runner.trace())
	assert.Equal(t, engine.StopVerifyFailed, h.lastStop().Reason)

	works := h.runner.callsOf("work")
	require.Len(t, works, 3)

	for _, w := range works[1:] {
		assert.Equal(t, "FAIL TestLogin", w.work.Detail)
		assert.True(t, w.work.Resume)
		assert.Equal(t, works[0].work.SessionID, w.work.SessionID)
	}
}

func TestLoopContinuations(t *testing.T) {
	h := newHarness(t)

	cutShort := step[engine.WorkResult]{res: engine.WorkResult{Outcome: engine.OutcomeUnfinished}}
	h.runner.works = []step[engine.WorkResult]{cutShort, cutShort, cutShort}

	status, err := h.run(t.Context())
	require.NoError(t, err)
	assert.Equal(t, engine.StatusBlocked, status)
	assert.Equal(t, []string{"prepare", "work/start", "work/continue", "work/continue", "release/keep"}, h.runner.trace())
	assert.Equal(t, engine.StopAgentError, h.lastStop().Reason)
}

const seededSession = "0b6c4f7e-2d1a-4c3b-9a8e-5f6d7c8b9a01"

func TestLoopRedoesInterruptedAction(t *testing.T) {
	tests := []struct {
		name   string
		events []store.Event
		starts []string // the first runner calls
	}{
		{
			name: "a session started and not ended",
			events: []store.Event{
				ev(engine.RunStarted, `{"reason":"initial"}`), ev(engine.PrepareStarted, ""), ev(engine.Prepared, `{"base_commit":"base123"}`),
				ev(engine.SessionStarted, `{"mode":"start","session_id":"`+seededSession+`"}`),
			},
			starts: []string{"work/continue"},
		},
		{
			name:   "a prepare started and not prepared",
			events: []store.Event{ev(engine.RunStarted, `{"reason":"initial"}`), ev(engine.PrepareStarted, "")},
			starts: []string{"prepare"},
		},
		{
			name: "a verify started and not ended",
			events: []store.Event{
				ev(engine.RunStarted, `{"reason":"initial"}`), ev(engine.PrepareStarted, ""), ev(engine.Prepared, `{"base_commit":"base123"}`),
				ev(engine.SessionStarted, `{"mode":"start","session_id":"`+seededSession+`"}`),
				ev(engine.SessionEnded, `{"outcome":"done","changed":true}`), ev(engine.VerifyStarted, ""),
			},
			starts: []string{"verify"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			r := h.seed(tt.events...)

			status, err := h.run(t.Context())
			require.NoError(t, err)
			assert.Equal(t, engine.StatusInReview, status)

			trace := h.runner.trace()
			require.GreaterOrEqual(t, len(trace), len(tt.starts))
			assert.Equal(t, tt.starts, trace[:len(tt.starts)])
			assert.Zero(t, h.spy.created(), "no second run row")

			for _, c := range h.runner.calls {
				assert.Equal(t, r.ID, c.runID, "%s runs on the seeded run", c)
			}

			if works := h.runner.callsOf("work"); tt.starts[0] == "work/continue" {
				require.NotEmpty(t, works)
				assert.True(t, works[0].work.Resume)
				assert.Equal(t, seededSession, works[0].work.SessionID)
				assert.Equal(t, seededSession, h.spy.row(r.ID).SessionID, "the redo writes the session id onto the row")
			}
		})
	}

	t.Run("a runner call cut short by ctx", func(t *testing.T) {
		h := newHarness(t)
		ctx, cancel := context.WithCancel(t.Context())

		h.runner.works = []step[engine.WorkResult]{{err: context.Canceled}}
		h.runner.onCall = func(c call) {
			if c.method == "work" {
				cancel()
			}
		}

		_, err := h.run(ctx)
		require.ErrorIs(t, err, context.Canceled)
		assert.Empty(t, h.eventsOf(engine.Stopped), "a cancelled call is not a fault")

		h.runner.onCall = nil

		status, err := h.run(t.Context())
		require.NoError(t, err)
		assert.Equal(t, engine.StatusInReview, status)

		works := h.runner.callsOf("work")
		require.Len(t, works, 2)
		assert.True(t, works[1].work.Resume)
		assert.Equal(t, works[0].work.SessionID, works[1].work.SessionID)
		assert.Equal(t, 1, h.spy.created())
	})
}

// A result that comes back as ctx ends is recorded, so the next call does not redo its action.
func TestLoopRecordsResultAfterCtxEnds(t *testing.T) {
	tests := []struct {
		name   string
		method string
		script func(r *fakeRunner)
		result string // the event that records the call's result
		status engine.Status
		next   []string // the next RunTask's runner calls
	}{
		{name: "prepare", method: "prepare", result: engine.Prepared, status: engine.StatusInReview, next: []string{"work/start", "verify", "export", "release/run", "release/remove"}},
		{name: "work", method: "work", result: engine.SessionEnded, status: engine.StatusInReview, next: []string{"verify", "export", "release/run", "release/remove"}},
		{name: "verify", method: "verify", result: engine.VerifyEnded, status: engine.StatusInReview, next: []string{"export", "release/run", "release/remove"}},
		{name: "export", method: "export", result: engine.Exported, status: engine.StatusInReview, next: []string{"release/run", "release/remove"}},
		{
			name: "an export problem", method: "export", result: engine.Stopped, status: engine.StatusBlocked, next: []string{},
			script: func(r *fakeRunner) {
				r.exports = []step[engine.ExportResult]{{res: engine.ExportResult{Problem: "the push was rejected"}}}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			ctx, cancel := context.WithCancel(t.Context())

			if tt.script != nil {
				tt.script(h.runner)
			}

			h.runner.onCall = func(c call) {
				if c.method == tt.method {
					cancel()
				}
			}

			_, err := h.run(ctx)
			require.ErrorIs(t, err, context.Canceled)
			require.Len(t, h.eventsOf(tt.result), 1, "the result is recorded")

			h.runner.onCall = nil
			before := len(h.runner.calls)

			status, err := h.run(t.Context())
			require.NoError(t, err)
			assert.Equal(t, tt.status, status)
			assert.Equal(t, tt.next, h.runner.trace()[before:])
			assert.Len(t, h.runner.callsOf(tt.method), 1, "the action is not redone")
		})
	}

	t.Run("a session's usage", func(t *testing.T) {
		h := newHarness(t)
		ctx, cancel := context.WithCancel(t.Context())

		h.runner.onCall = func(c call) {
			if c.method == "work" {
				cancel()
			}
		}

		_, err := h.run(ctx)
		require.ErrorIs(t, err, context.Canceled)

		usage, err := h.store.UsageTotals(t.Context(), h.task.ID)
		require.NoError(t, err)
		require.Len(t, usage, 1)
		assert.Equal(t, int64(10), usage[0].Input)
	})
}

// A baseline the prepare verified is kept on the project for the next task at that commit.
func TestLoopStoresPassedBaseline(t *testing.T) {
	tests := []struct {
		baseline string
		stored   bool
	}{
		{engine.ResultPassed, true},
		{"reused", false},
		{"waived", false},
	}

	for _, tt := range tests {
		t.Run(tt.baseline, func(t *testing.T) {
			h := newHarness(t)

			res := preparedOK
			res.Baseline = tt.baseline
			h.runner.prepares = []step[engine.PrepareResult]{{res: res}}

			_, err := h.run(t.Context())
			require.NoError(t, err)

			p, err := h.store.Project(t.Context(), h.task.ProjectID)
			require.NoError(t, err)

			if !tt.stored {
				assert.Nil(t, p.Baseline)

				return
			}

			require.NotNil(t, p.Baseline)
			assert.Equal(t, "base123", p.Baseline.Commit)
			assert.True(t, p.Baseline.Passed)
			assert.False(t, p.Baseline.At.IsZero())
		})
	}
}

// A replay's shadow prepares an old commit, whose baseline is no longer the project's.
func TestShadowBaselineIsNotStored(t *testing.T) {
	h := newHarness(t)
	h.withBaseline()
	before := h.project().Baseline

	h.task.ReplayID = 1
	require.NoError(t, h.store.UpdateTask(t.Context(), h.task))

	res := preparedOK
	res.BaseCommit = "old789"
	h.runner.prepares = []step[engine.PrepareResult]{{res: res}}

	_, err := h.run(t.Context())
	require.NoError(t, err)
	require.Len(t, h.runner.callsOf("prepare"), 1)
	assert.Equal(t, before, h.project().Baseline)
}

func TestLoopRecordsIgnoredTools(t *testing.T) {
	h := newHarness(t)

	res := engine.PrepareResult{
		BaseCommit: "base123", Tools: []string{"go@1.25.0"}, Baseline: engine.ResultPassed,
		IgnoredTools: []string{".tool-versions: ubi:owner/tool 1.0"},
	}
	h.runner.prepares = []step[engine.PrepareResult]{{res: res}}

	_, err := h.run(t.Context())
	require.NoError(t, err)

	prepared := h.eventsOf(engine.Prepared)
	require.Len(t, prepared, 1)
	assert.Equal(t, engine.PreparedPayload{
		BaseCommit: res.BaseCommit, Tools: res.Tools, Baseline: res.Baseline, IgnoredTools: res.IgnoredTools,
	}, payloadOf[engine.PreparedPayload](t, prepared[0]))
}

// The ecosystems a prepare detected reach every later action of its run, also
// after a restart; the prepare itself gets none.
func TestLoopCarriesEcosystems(t *testing.T) {
	t.Run("from the prepare", func(t *testing.T) {
		h := newHarness(t)

		res := preparedOK
		res.Ecosystems = []string{"go", "node"}
		h.runner.prepares = []step[engine.PrepareResult]{{res: res}}

		_, err := h.run(t.Context())
		require.NoError(t, err)
		assert.Equal(t, happyTrace, h.runner.trace())

		prepared := h.eventsOf(engine.Prepared)
		require.Len(t, prepared, 1)
		assert.Equal(t, res.Ecosystems, payloadOf[engine.PreparedPayload](t, prepared[0]).Ecosystems)

		for _, c := range h.runner.calls {
			switch c.method {
			case "prepare":
				assert.Nil(t, c.eco)
			case "work", "verify", "export":
				assert.Equal(t, res.Ecosystems, c.eco, c.String())
			}
		}
	})

	t.Run("after a restart", func(t *testing.T) {
		h := newHarness(t)
		h.seed(
			ev(engine.RunStarted, `{"reason":"initial"}`), ev(engine.PrepareStarted, ""),
			ev(engine.Prepared, `{"base_commit":"base123","ecosystems":["rust"]}`),
			ev(engine.SessionStarted, `{"mode":"start","session_id":"`+seededSession+`"}`),
		)

		_, err := h.run(t.Context())
		require.NoError(t, err)
		assert.Equal(t, []string{"work/continue", "verify", "export", "release/run", "release/remove"}, h.runner.trace())

		for _, c := range h.runner.calls {
			if c.method != "release" {
				assert.Equal(t, []string{"rust"}, c.eco, c.String())
			}
		}
	})
}

// The export takes the commit the newest ended session froze, also after a restart.
func TestLoopExportsFrozenCommit(t *testing.T) {
	t.Run("the newest session's", func(t *testing.T) {
		h := newHarness(t)

		first, fix := workDone, workDone
		first.HeadCommit, fix.HeadCommit = "frozen1", "frozen2"
		h.runner.works = []step[engine.WorkResult]{{res: first}, {res: fix}}
		h.runner.verifies = []step[engine.VerifyResult]{{res: engine.VerifyResult{Result: engine.ResultFailed, Command: "make test"}}}

		_, err := h.run(t.Context())
		require.NoError(t, err)
		assert.Equal(t, []string{"prepare", "work/start", "verify", "work/fix_verify", "verify", "export", "release/run", "release/remove"},
			h.runner.trace())

		ended := h.eventsOf(engine.SessionEnded)
		require.Len(t, ended, 2)
		assert.Equal(t, "frozen1", payloadOf[engine.SessionEndedPayload](t, ended[0]).HeadCommit)
		assert.Equal(t, "frozen2", payloadOf[engine.SessionEndedPayload](t, ended[1]).HeadCommit)
		assert.Equal(t, "frozen2", h.runner.callsOf("export")[0].commit)
	})

	t.Run("after a restart", func(t *testing.T) {
		h := newHarness(t)
		h.seed(
			ev(engine.RunStarted, `{"reason":"initial"}`), ev(engine.PrepareStarted, ""), ev(engine.Prepared, `{"base_commit":"base123"}`),
			ev(engine.SessionStarted, `{"mode":"start","session_id":"`+seededSession+`"}`),
			ev(engine.SessionEnded, `{"outcome":"done","changed":true,"head_commit":"frozen3"}`),
			ev(engine.VerifyStarted, ""), ev(engine.VerifyEnded, `{"result":"passed"}`),
		)

		_, err := h.run(t.Context())
		require.NoError(t, err)
		assert.Equal(t, []string{"export", "release/run", "release/remove"}, h.runner.trace())
		assert.Equal(t, "frozen3", h.runner.callsOf("export")[0].commit)
	})
}

func artifactDir(h *harness, runID int64) string {
	return filepath.Join(h.loop.DataDir, "artifacts", strconv.FormatInt(h.task.ID, 10), strconv.FormatInt(runID, 10))
}

// assertArtifacts checks that dir holds exactly the export's two files, private, with want's content.
func assertArtifacts(t *testing.T, dir string, want engine.ExportResult) {
	t.Helper()

	info, err := os.Stat(dir)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), info.Mode().Perm())

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)

	got := map[string][]byte{}

	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		require.NoError(t, err)

		got[e.Name()] = data

		info, err := e.Info()
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), e.Name())
	}

	assert.Equal(t, map[string][]byte{"diff.patch": want.Diff, "task.bundle": want.Bundle}, got)
}

// The loop writes the export's diff and bundle under the data directory and
// records their paths; a write that fails leaves the export to be redone.
func TestLoopWritesArtifacts(t *testing.T) {
	t.Run("written and recorded", func(t *testing.T) {
		h := newHarness(t)

		_, err := h.run(t.Context())
		require.NoError(t, err)

		exported := h.eventsOf(engine.Exported)
		require.Len(t, exported, 1)

		dir := artifactDir(h, exported[0].RunID)
		assert.Equal(t, engine.ExportedPayload{
			HeadCommit: "head456", DiffPath: filepath.Join(dir, "diff.patch"), BundlePath: filepath.Join(dir, "task.bundle"),
			FilesChanged: 1, DiffStat: exportOK.DiffStat, Flagged: engine.Flagged{Instructions: []string{"AGENTS.md"}},
		}, payloadOf[engine.ExportedPayload](t, exported[0]))
		assertArtifacts(t, dir, exportOK)
	})

	t.Run("a failed write is redone and replaces the files", func(t *testing.T) {
		h := newHarness(t)

		long := exportOK
		long.Diff = bytes.Repeat([]byte("x"), 4096)
		h.runner.exports = []step[engine.ExportResult]{{res: long}}

		// A directory where the bundle goes fails its rename.
		var blocker string

		h.runner.onCall = func(c call) {
			if c.method == "export" && blocker == "" {
				blocker = filepath.Join(artifactDir(h, c.runID), "task.bundle")
				require.NoError(t, os.MkdirAll(blocker, 0o700))
			}
		}

		_, err := h.run(t.Context())
		require.Error(t, err)
		assert.Empty(t, h.eventsOf(engine.Exported))
		assert.Empty(t, h.eventsOf(engine.Stopped))

		require.NoError(t, os.Remove(blocker))

		status, err := h.run(t.Context())
		require.NoError(t, err)
		assert.Equal(t, engine.StatusInReview, status)

		assert.Equal(t, []string{"prepare", "work/start", "verify", "export", "export", "release/run", "release/remove"}, h.runner.trace())
		assertArtifacts(t, artifactDir(h, h.runner.callsOf("export")[1].runID), exportOK)
	})
}

func TestLoopFailedBaselineStops(t *testing.T) {
	h := newHarness(t)
	h.runner.prepares = []step[engine.PrepareResult]{{res: engine.PrepareResult{
		BaseCommit: "base123", Baseline: engine.ResultFailed, Output: "FAIL TestLogin",
	}}}

	status, err := h.run(t.Context())
	require.NoError(t, err)
	assert.Equal(t, engine.StatusBlocked, status)
	assert.Equal(t, []string{"prepare", "release/run"}, h.runner.trace())
	assert.Empty(t, h.eventsOf(engine.Prepared))

	failed := h.eventsOf(engine.PrepareFailed)
	require.Len(t, failed, 1)
	assert.Equal(t, engine.PrepareFailedPayload{Step: "baseline", Output: "FAIL TestLogin"},
		payloadOf[engine.PrepareFailedPayload](t, failed[0]))
	assert.Equal(t, engine.StopEnvironment, h.lastStop().Reason)
}

func TestLoopExportProblemStops(t *testing.T) {
	h := newHarness(t)

	const problem = "the push was rejected by a protected branch rule"

	h.runner.exports = []step[engine.ExportResult]{{res: engine.ExportResult{Problem: problem}}}

	status, err := h.run(t.Context())
	require.NoError(t, err)
	assert.Equal(t, engine.StatusBlocked, status)
	assert.Equal(t, []string{"prepare", "work/start", "verify", "export", "release/keep"}, h.runner.trace())

	stop := h.lastStop()
	assert.Equal(t, engine.StopAgentError, stop.Reason)
	assert.Equal(t, problem, stop.Explanation)
	assert.Equal(t, engine.ClearRetry, stop.Action)
	assert.Empty(t, h.eventsOf(engine.Exported))
	assert.Empty(t, h.eventsOf(engine.RunEnded))
	assert.Empty(t, h.spy.ended(), "the run keeps its volumes and stays open")
}

// A stop's explanation is one bounded line; the full agent text stays in its result.
func TestLoopBoundsStopExplanations(t *testing.T) {
	long := strings.Repeat("日本語", 200)

	tests := []struct {
		name    string
		script  func(r *fakeRunner)
		head    string // text the explanation starts from
		reason  string
		summary bool // the session's result keeps the full text
	}{
		{
			name: "a long summary",
			script: func(r *fakeRunner) {
				r.works = []step[engine.WorkResult]{{res: engine.WorkResult{Outcome: engine.OutcomeFailed, Summary: long}}}
			},
			head: "日本語", reason: engine.StopAgentError, summary: true,
		},
		{
			name: "a long runner error",
			script: func(r *fakeRunner) {
				r.works = []step[engine.WorkResult]{{err: errors.New("the container exited\n" + long)}}
			},
			head: "the container exited", reason: engine.StopSandbox,
		},
		{
			name: "a long export problem",
			script: func(r *fakeRunner) {
				r.exports = []step[engine.ExportResult]{{res: engine.ExportResult{Problem: "the push was rejected\n" + long}}}
			},
			head: "the push was rejected", reason: engine.StopAgentError,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			tt.script(h.runner)

			status, err := h.run(t.Context())
			require.NoError(t, err)
			assert.Equal(t, engine.StatusBlocked, status)

			stop := h.lastStop()
			assert.Equal(t, tt.reason, stop.Reason)
			assert.LessOrEqual(t, len(stop.Explanation), 300)
			assert.True(t, utf8.ValidString(stop.Explanation))
			assert.NotContains(t, stop.Explanation, "\n")
			assert.Contains(t, stop.Explanation, tt.head)

			if tt.summary {
				ended := h.eventsOf(engine.SessionEnded)
				require.Len(t, ended, 1)
				assert.Equal(t, long, payloadOf[engine.SessionEndedPayload](t, ended[0]).Summary)
			}
		})
	}
}

// Runner text is masked before its explanation is cut, so the cut leaves no head of a secret.
func TestLoopMasksBeforeCutting(t *testing.T) {
	const planted, mask = "sk-ant-api03-plainsecretvalue", "[redacted]"

	copies := strings.Repeat(planted, 40)

	tests := []struct {
		name   string
		script func(r *fakeRunner)
	}{
		{"a runner error", func(r *fakeRunner) { r.works = []step[engine.WorkResult]{{err: errors.New(copies)}} }},
		{"an export problem", func(r *fakeRunner) {
			r.exports = []step[engine.ExportResult]{{res: engine.ExportResult{Problem: copies}}}
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			require.NoError(t, h.vault.Set(t.Context(), "endpoint_credential", planted))
			tt.script(h.runner)

			_, err := h.run(t.Context())
			require.NoError(t, err)

			explanation := h.lastStop().Explanation
			last := strings.LastIndex(explanation, mask)
			require.GreaterOrEqual(t, last, 0)

			tail := explanation[last+len(mask):]
			assert.True(t, strings.HasPrefix(mask, tail), "the cut falls inside a mask, not a secret: %q", tail)
		})
	}
}

// A runner fault or export problem has no result event, so its stop keeps the whole redacted text.
func TestLoopStopKeepsRunnerText(t *testing.T) {
	const planted = "sk-ant-api03-plainsecretvalue"

	text := "the container exited with " + planted + "\n" + strings.Repeat("日本語", 200)

	tests := []struct {
		name   string
		script func(r *fakeRunner)
	}{
		{"a runner error", func(r *fakeRunner) { r.works = []step[engine.WorkResult]{{err: errors.New(text)}} }},
		{"an export problem", func(r *fakeRunner) {
			r.exports = []step[engine.ExportResult]{{res: engine.ExportResult{Problem: text}}}
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			require.NoError(t, h.vault.Set(t.Context(), "endpoint_credential", planted))
			tt.script(h.runner)

			_, err := h.run(t.Context())
			require.NoError(t, err)

			stop := h.lastStop()
			assert.LessOrEqual(t, len(stop.Explanation), 300)
			assert.NotContains(t, stop.Explanation, "\n")
			assert.Equal(t, strings.ReplaceAll(text, planted, "[redacted]"), stop.Detail)
		})
	}
}

func TestLoopRepairsRunRows(t *testing.T) {
	exportedRun := []store.Event{
		ev(engine.RunStarted, `{"reason":"initial"}`), ev(engine.PrepareStarted, ""), ev(engine.Prepared, `{"base_commit":"base123"}`),
		ev(engine.SessionStarted, `{"mode":"start","session_id":"`+seededSession+`"}`),
		ev(engine.SessionEnded, `{"outcome":"done","changed":true}`), ev(engine.VerifyStarted, ""),
		ev(engine.VerifyEnded, `{"result":"passed"}`), ev(engine.ExportStarted, ""), ev(engine.Exported, `{"head_commit":"head456"}`),
		ev(engine.RunEnded, `{"outcome":"exported"}`),
	}

	t.Run("a row whose run ended is released and ended", func(t *testing.T) {
		h := newHarness(t)
		r := h.seed(exportedRun...)

		status, err := h.run(t.Context())
		require.NoError(t, err)
		assert.Equal(t, engine.StatusInReview, status)
		assert.Equal(t, []string{"release/remove"}, h.runner.trace())
		assert.Equal(t, r.ID, h.runner.calls[0].runID)
		assert.Equal(t, map[int64]string{r.ID: engine.RunExported}, h.spy.ended())
	})

	t.Run("a row whose run ended and kept the home keeps it until the clean", func(t *testing.T) {
		h := newHarness(t)

		kept := slices.Clone(exportedRun)
		kept[len(kept)-1] = ev(engine.RunEnded, `{"outcome":"exported","kept_home":true}`)
		r := h.seed(kept...)

		status, err := h.run(t.Context())
		require.NoError(t, err)
		assert.Equal(t, engine.StatusInReview, status)
		assert.Equal(t, []string{"release/run", "release/remove"}, h.runner.trace())
		assert.Equal(t, map[int64]string{r.ID: engine.RunExported}, h.spy.ended())
	})

	t.Run("a release error is returned after the row is ended", func(t *testing.T) {
		h := newHarness(t)
		r := h.seed(exportedRun...)

		injected := errors.New("volume busy")
		h.runner.releaseErr = injected

		status, err := h.run(t.Context())
		require.ErrorIs(t, err, injected)
		assert.Equal(t, engine.StatusInReview, status)
		assert.Equal(t, map[int64]string{r.ID: engine.RunExported}, h.spy.ended())

		h.runner.releaseErr = nil

		status, err = h.run(t.Context())
		require.NoError(t, err)
		assert.Equal(t, engine.StatusInReview, status)
		assert.Len(t, h.runner.calls, 1, "the next call proceeds without a second release")
	})

	t.Run("a row with no run_started is abandoned", func(t *testing.T) {
		h := newHarness(t)
		r := h.seed()

		status, err := h.run(t.Context())
		require.NoError(t, err)
		assert.Equal(t, engine.StatusInReview, status)
		assert.Equal(t, happyTrace, h.runner.trace(), "an abandoned row needs no release")

		ended := h.spy.ended()
		assert.Equal(t, engine.RunAbandoned, ended[r.ID])

		prepare := h.runner.callsOf("prepare")[0]
		assert.NotEqual(t, r.ID, prepare.runID, "a new run replaces the abandoned row")
		assert.Equal(t, engine.RunExported, ended[prepare.runID])
	})

	t.Run("the walk ends every stale row", func(t *testing.T) {
		h := newHarness(t)
		failed := h.seed(ev(engine.RunStarted, `{"reason":"initial"}`), ev(engine.PrepareStarted, ""),
			ev(engine.PrepareFailed, `{"step":"setup","output":"exit 1"}`),
			ev(engine.Stopped, `{"reason":"environment","explanation":"Setup failed.","action":"retry"}`),
			ev(engine.RunEnded, `{"outcome":"environment"}`))
		h.add(0, engine.HumanRetried, "user", "")

		stray := h.seed()

		status, err := h.run(t.Context())
		require.NoError(t, err)
		assert.Equal(t, engine.StatusInReview, status)
		assert.Equal(t, append([]string{"release/remove"}, happyTrace...), h.runner.trace())
		assert.Equal(t, failed.ID, h.runner.calls[0].runID)

		ended := h.spy.ended()
		assert.Equal(t, engine.StopEnvironment, ended[failed.ID])
		assert.Equal(t, engine.RunAbandoned, ended[stray.ID])

		var reasons []string
		for _, e := range h.eventsOf(engine.RunStarted) {
			reasons = append(reasons, payloadOf[engine.RunStartedPayload](t, e).Reason)
		}

		assert.Equal(t, []string{"initial", "retry"}, reasons)
	})
}

func TestReleaseOpenRunKeepsVolumes(t *testing.T) {
	h := newHarness(t)

	require.NoError(t, h.loop.ReleaseOpenRun(t.Context(), h.task.ID))
	assert.Empty(t, h.runner.calls, "a task with no open run has nothing to release")

	r := h.seed(ev(engine.RunStarted, `{"reason":"initial"}`))

	require.NoError(t, h.loop.ReleaseOpenRun(t.Context(), h.task.ID))
	assert.Equal(t, []call{{method: "release", task: h.task.ID, runID: r.ID, ref: engine.RunRef{
		InstanceID: "inst-1", Image: "cavu-sandbox:test", TaskID: h.task.ID, RunID: r.ID, ProjectID: h.task.ProjectID,
	}}}, h.runner.calls)
	assert.Empty(t, h.spy.ended(), "the run stays open")
}

func TestLoopRunnerFaultStops(t *testing.T) {
	daemon := errors.New("the container daemon is unreachable")

	tests := []struct {
		name       string
		script     func(r *fakeRunner)
		trace      []string
		explainHas string
		releaseErr error
	}{
		{
			name:       "prepare",
			script:     func(r *fakeRunner) { r.prepares = []step[engine.PrepareResult]{{err: daemon}} },
			trace:      []string{"prepare", "release/keep"},
			explainHas: daemon.Error(),
		},
		{
			name:       "work",
			script:     func(r *fakeRunner) { r.works = []step[engine.WorkResult]{{err: daemon}} },
			trace:      []string{"prepare", "work/start", "release/keep"},
			explainHas: daemon.Error(),
		},
		{
			name:       "verify",
			script:     func(r *fakeRunner) { r.verifies = []step[engine.VerifyResult]{{err: daemon}} },
			trace:      []string{"prepare", "work/start", "verify", "release/keep"},
			explainHas: daemon.Error(),
		},
		{
			name:       "export",
			script:     func(r *fakeRunner) { r.exports = []step[engine.ExportResult]{{err: daemon}} },
			trace:      []string{"prepare", "work/start", "verify", "export", "release/keep"},
			explainHas: daemon.Error(),
		},
		{
			name: "an empty verify result",
			script: func(r *fakeRunner) {
				r.verifies = []step[engine.VerifyResult]{{res: engine.VerifyResult{Command: "make test"}}}
			},
			trace: []string{"prepare", "work/start", "verify", "release/keep"},
		},
		{
			name:       "prepare, then a failed release",
			script:     func(r *fakeRunner) { r.prepares = []step[engine.PrepareResult]{{err: daemon}} },
			trace:      []string{"prepare", "release/keep"},
			explainHas: daemon.Error(),
			releaseErr: errors.New("volume busy"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			tt.script(h.runner)
			h.runner.releaseErr = tt.releaseErr

			status, err := h.run(t.Context())
			if tt.releaseErr != nil {
				require.ErrorIs(t, err, tt.releaseErr)
			} else {
				require.NoError(t, err)
			}

			assert.Equal(t, engine.StatusBlocked, status)
			assert.Equal(t, tt.trace, h.runner.trace())

			stop := h.lastStop()
			assert.Equal(t, engine.StopSandbox, stop.Reason)
			assert.Equal(t, engine.ClearRetry, stop.Action)

			if tt.explainHas != "" {
				assert.Contains(t, stop.Explanation, tt.explainHas)
			}

			assert.Empty(t, h.eventsOf(engine.RunEnded))
			assert.Empty(t, h.spy.ended(), "the run keeps its volumes and stays open")
		})
	}
}

// What a session a runner fault cut used is recorded with its stop.
func TestUsageOfAFaultedSessionIsRecorded(t *testing.T) {
	daemon := errors.New("the sidecar ended")
	used := []store.Usage{
		{Source: "proxy", Model: "model-a", Input: 10, Output: 5},
		{Source: "proxy", Model: "model-b", Input: 3, Output: 1, CacheRead: 2},
	}

	tests := map[string]func(h *harness){
		"a session": func(h *harness) {
			h.runner.works = []step[engine.WorkResult]{{res: engine.WorkResult{Turns: 4, Usage: used}, err: daemon}}
		},
		"a review": func(h *harness) {
			h.withReview()
			h.runner.works = []step[engine.WorkResult]{{res: engine.WorkResult{Outcome: engine.OutcomeDone, Changed: true, HeadCommit: "head456"}}}
			h.runner.reviews = []step[engine.ReviewResult]{{res: engine.ReviewResult{Turns: 4, Usage: used}, err: daemon}}
		},
	}

	for name, script := range tests {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			script(h)

			_, err := h.run(t.Context())
			require.NoError(t, err)

			assert.Equal(t, engine.StopSandbox, h.lastStop().Reason)
			assert.Equal(t, []string{engine.Stopped}, h.spy.usageTies, "the rows are tied to the stop")

			totals, err := h.store.UsageTotals(t.Context(), h.task.ID)
			require.NoError(t, err)
			assert.Equal(t, used, totals)
		})
	}
}

// A session or review its caller cut keeps its usage on a session_cut event, which leaves the facts alone, so the next RunTask redoes the action.
func TestCutSessionKeepsItsUsage(t *testing.T) {
	used := []store.Usage{
		{Source: "proxy", Model: "model-a", Input: 10, Output: 5},
		{Source: "proxy", Model: "model-b", Input: 3, Output: 1, CacheRead: 2},
	}

	tests := map[string]struct {
		method string
		script func(h *harness)
	}{
		"a session": {"work", func(h *harness) {
			h.runner.works = []step[engine.WorkResult]{{res: engine.WorkResult{Turns: 4, Usage: used}, err: context.Canceled}}
		}},
		"a review": {"review", func(h *harness) {
			h.withReview()
			h.runner.works = []step[engine.WorkResult]{{res: engine.WorkResult{Outcome: engine.OutcomeDone, Changed: true, HeadCommit: "head456"}}}
			h.runner.reviews = []step[engine.ReviewResult]{{res: engine.ReviewResult{Turns: 4, Usage: used}, err: context.Canceled}}
		}},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			tc.script(h)

			ctx, cancel := context.WithCancel(t.Context())
			h.runner.onCall = func(c call) {
				if c.method == tc.method {
					cancel()
				}
			}

			_, err := h.run(ctx)
			require.ErrorIs(t, err, context.Canceled)
			assert.Empty(t, h.eventsOf(engine.Stopped), "a cancelled call is not a fault")

			cuts := h.eventsOf(engine.SessionCut)
			require.Len(t, cuts, 1)
			assert.Equal(t, "runner", cuts[0].Actor)
			assert.Equal(t, h.runner.callsOf(tc.method)[0].runID, cuts[0].RunID)
			assert.Equal(t, 4, payloadOf[engine.SessionCutPayload](t, cuts[0]).Turns)
			assert.Equal(t, []string{engine.SessionCut}, h.spy.usageTies, "the rows are tied to the cut")

			counted, err := h.store.UsageSince(t.Context(), h.task.ID, 0)
			require.NoError(t, err)
			assert.Equal(t, used, counted)

			all := h.events()
			before := all[:len(all)-1]
			require.Equal(t, engine.SessionCut, all[len(all)-1].Type)
			assert.Equal(t, engine.Reduce(before), engine.Reduce(all), "the cut leaves the facts alone")

			h.runner.onCall = nil

			status, err := h.run(t.Context())
			require.NoError(t, err)
			assert.Equal(t, engine.StatusInReview, status)

			calls := h.runner.callsOf(tc.method)
			require.Len(t, calls, 2, "the cut action starts again")

			if tc.method == "work" {
				assert.True(t, calls[1].work.Resume)
				assert.Equal(t, calls[0].work.SessionID, calls[1].work.SessionID)
			}
		})
	}
}

// A runner fault keeps its run: the action it cut is redone on that run and its
// volumes, by itself after the backoff or at once on a person's retry. A
// cancel ends that run and removes them.
func TestLoopRetryAfterRunnerFault(t *testing.T) {
	daemon := errors.New("the container daemon is unreachable")

	tests := []struct {
		name   string
		script func(r *fakeRunner)
		next   []string // the runner calls after the retry
	}{
		{
			name:   "prepare",
			script: func(r *fakeRunner) { r.prepares = []step[engine.PrepareResult]{{err: daemon}} },
			next:   []string{"prepare", "work/start", "verify", "export", "release/run", "release/remove"},
		},
		{
			name:   "session",
			script: func(r *fakeRunner) { r.works = []step[engine.WorkResult]{{err: daemon}} },
			next:   []string{"work/continue", "verify", "export", "release/run", "release/remove"},
		},
		{
			name:   "verify",
			script: func(r *fakeRunner) { r.verifies = []step[engine.VerifyResult]{{err: daemon}} },
			next:   []string{"verify", "export", "release/run", "release/remove"},
		},
		{
			name:   "export",
			script: func(r *fakeRunner) { r.exports = []step[engine.ExportResult]{{err: daemon}} },
			next:   []string{"export", "release/run", "release/remove"},
		},
	}

	resumes := map[string]func(h *harness){
		"by itself after the backoff": func(h *harness) { h.clock.Advance(30 * time.Second) },
		"on a person's retry":         func(h *harness) { h.add(0, engine.HumanRetried, "user", "") },
	}

	for _, tt := range tests {
		for how, resume := range resumes {
			t.Run(tt.name+" "+how, func(t *testing.T) {
				h := newHarness(t)
				tt.script(h.runner)

				_, err := h.run(t.Context())
				require.NoError(t, err)
				require.Equal(t, engine.StopSandbox, h.lastStop().Reason)

				before := len(h.runner.calls)
				resume(h)

				status, err := h.run(t.Context())
				require.NoError(t, err)
				assert.Equal(t, engine.StatusInReview, status)
				assert.Equal(t, tt.next, h.runner.trace()[before:])

				runID := h.runner.calls[0].runID
				for _, c := range h.runner.calls {
					assert.Equal(t, runID, c.runID, "%s runs on the first run", c)
				}

				assert.Equal(t, 1, h.spy.created(), "no second run row")
				assert.Equal(t, map[int64]string{runID: engine.RunExported}, h.spy.ended())

				works := h.runner.callsOf("work")
				for _, w := range works[1:] {
					assert.True(t, w.work.Resume)
					assert.Equal(t, works[0].work.SessionID, w.work.SessionID)
				}

				for _, e := range h.runner.callsOf("export") {
					assert.Equal(t, workDone.HeadCommit, e.commit)
				}
			})
		}
	}

	t.Run("a cancel ends the run and removes its volumes", func(t *testing.T) {
		h := newHarness(t)
		h.runner.works = []step[engine.WorkResult]{{err: daemon}}

		_, err := h.run(t.Context())
		require.NoError(t, err)
		require.Equal(t, engine.StopSandbox, h.lastStop().Reason)

		before := len(h.runner.calls)
		h.add(0, engine.HumanCancelled, "user", "")

		status, err := h.run(t.Context())
		require.NoError(t, err)
		assert.Equal(t, engine.StatusCancelled, status)
		require.Equal(t, []string{"release/run", "release/remove"}, h.runner.trace()[before:])

		runID := h.runner.calls[0].runID
		assert.Equal(t, runID, h.runner.calls[before].runID)
		assert.Equal(t, map[int64]string{runID: engine.StopCancelled}, h.spy.ended())

		ends := h.eventsOf(engine.RunEnded)
		require.Len(t, ends, 1)
		assert.Equal(t, engine.StopCancelled, payloadOf[engine.RunEndedPayload](t, ends[0]).Outcome)
	})
}

// A cancel that lands as the session ends is the drive's own to stop on, and a
// scheduler cuts the drive for the same cancel, here during the stop's release.
func TestLoopEndsARunWhoseContextIsCutDuringItsRelease(t *testing.T) {
	h := newHarness(t)

	ctx, cut := context.WithCancel(t.Context())
	defer cut()

	var released error

	h.runner.onCall = func(c call) {
		if c.method == "work" {
			h.add(0, engine.HumanCancelled, "user", "")
		}
	}
	h.runner.gate = func(rctx context.Context, c call) error {
		if c.method != "release" {
			return nil
		}

		cut()

		released = rctx.Err()

		return released
	}

	_, _ = h.run(ctx) // ends with ctx's error, after the run is ended

	assert.Equal(t, []string{"prepare", "work/start", "release/run"}, h.runner.trace())
	require.NoError(t, released, "the release runs on past the cut")
	assert.Equal(t, map[int64]string{h.runner.calls[0].runID: engine.StopCancelled}, h.spy.ended(), "the run row is ended")
}

func TestLoopClaimConflict(t *testing.T) {
	rivalText := store.Event{Type: engine.AgentText, Actor: "agent", Payload: json.RawMessage(`{"text":"rival"}`)}
	cancelled := store.Event{Type: engine.HumanCancelled, Actor: "user", Payload: json.RawMessage(`{}`)}

	at := func(claim int, e store.Event) func(int) (store.Event, bool) {
		return func(n int) (store.Event, bool) { return e, n == claim }
	}

	tests := []struct {
		name    string
		rival   func(int) (store.Event, bool)
		status  engine.Status
		trace   []string
		created int
	}{
		{name: "the new-run claim", rival: at(1, rivalText), status: engine.StatusInReview, trace: happyTrace, created: 2},
		{name: "the work claim", rival: at(2, rivalText), status: engine.StatusInReview, trace: happyTrace, created: 1},
		{name: "the finish claim", rival: at(5, rivalText), status: engine.StatusInReview, trace: happyTrace, created: 1},
		{
			name: "a cancel before the work claim", rival: at(2, cancelled), status: engine.StatusCancelled,
			trace: []string{"prepare", "release/run", "release/remove"}, created: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			h.spy.rival = tt.rival

			status, err := h.run(t.Context())
			require.NoError(t, err)
			assert.Equal(t, tt.status, status)
			assert.Equal(t, tt.trace, h.runner.trace())
			assert.Equal(t, tt.created, h.spy.created())

			ended := h.spy.ended()
			if tt.created == 2 {
				assert.Equal(t, engine.RunAbandoned, ended[1], "the row whose claim lost is abandoned")
			}
		})
	}

	t.Run("a failed abandon is returned, not retried", func(t *testing.T) {
		h := newHarness(t)
		h.spy.rival = at(1, rivalText)

		injected := errors.New("database is locked")
		h.spy.failUpdate = func(r store.Run) error {
			if r.Outcome == engine.RunAbandoned {
				return injected
			}

			return nil
		}

		_, err := h.run(t.Context())
		require.ErrorIs(t, err, injected)
		require.NotErrorIs(t, err, store.ErrConflict)
		assert.Equal(t, 1, h.spy.created())
		assert.Empty(t, h.runner.trace())
	})

	t.Run("a rival before every claim hits the conflict cap", func(t *testing.T) {
		h := newHarness(t)
		h.spy.rival = func(int) (store.Event, bool) { return rivalText, true }

		_, err := h.run(t.Context())
		require.ErrorIs(t, err, engine.ErrConflictCap)
		assert.Empty(t, h.runner.trace())
		assert.Empty(t, h.eventsOf(engine.Stopped))
		assert.Empty(t, h.eventsOf(engine.RunStarted))
	})
}

func TestLoopRedactsPayloads(t *testing.T) {
	for _, secret := range []string{"sk-ant-api03-plainsecretvalue", `sk-ant<api>&"quoted"secret`} {
		t.Run(secret, func(t *testing.T) {
			h := newHarness(t)
			require.NoError(t, h.vault.Set(t.Context(), "endpoint_credential", secret))

			res := workDone
			res.Summary = "Logged in with " + secret + " and fixed it."
			h.runner.works = []step[engine.WorkResult]{{res: res}}
			h.runner.progress = []engine.Progress{{Type: engine.AgentText, Payload: engine.AgentTextPayload{Text: "using " + secret}}}

			_, err := h.run(t.Context())
			require.NoError(t, err)

			ended := h.eventsOf(engine.SessionEnded)
			require.Len(t, ended, 1)

			summary := payloadOf[engine.SessionEndedPayload](t, ended[0]).Summary
			assert.Contains(t, summary, "[redacted]")
			assert.NotContains(t, summary, secret)

			texts := h.eventsOf(engine.AgentText)
			require.Len(t, texts, 1)
			assert.NotContains(t, payloadOf[engine.AgentTextPayload](t, texts[0]).Text, secret)
		})
	}
}

func TestLoopRecordsProgress(t *testing.T) {
	long := strings.Repeat("日", 1000) // 3000 bytes, a rune every 3, so 2 KiB falls inside one
	resets := now.Add(time.Hour)

	progress := []engine.Progress{
		{Type: engine.AgentText, Payload: engine.AgentTextPayload{Text: long}},
		{Type: engine.AgentTool, Payload: map[string]any{"name": "Bash", "summary": long, "extra": "dropped"}},
		{Type: engine.RateLimit, Payload: engine.RateLimitPayload{Window: "5h", Status: "allowed", ResetsAt: resets}},
		{Type: engine.EgressDenied, Payload: &engine.EgressDeniedPayload{Host: "evil.example"}},
		{Type: engine.SessionEnded, Payload: engine.SessionEndedPayload{Outcome: engine.OutcomeDone}},
		{Type: engine.Stopped, Payload: engine.StoppedPayload{Reason: engine.StopQuestion}},
	}

	t.Run("allowed types are recorded, cut and attributed", func(t *testing.T) {
		h := newHarness(t)
		h.runner.progress = progress

		status, err := h.run(t.Context())
		require.NoError(t, err)
		assert.Equal(t, engine.StatusInReview, status)
		assert.Len(t, h.eventsOf(engine.SessionEnded), 1, "progress cannot forge a result")
		assert.Empty(t, h.eventsOf(engine.Stopped), "progress cannot forge a stop")

		endedID := h.eventsOf(engine.SessionEnded)[0].ID
		actors := map[string]string{}

		for _, e := range h.events() {
			switch e.Type {
			case engine.AgentText, engine.AgentTool, engine.RateLimit, engine.EgressDenied:
				actors[e.Type] = e.Actor
				assert.Less(t, e.ID, endedID, "%s comes before the session's result", e.Type)
			}
		}

		assert.Equal(t, map[string]string{
			engine.AgentText: "agent", engine.AgentTool: "agent", engine.RateLimit: "agent", engine.EgressDenied: "runner",
		}, actors)

		text := payloadOf[engine.AgentTextPayload](t, h.eventsOf(engine.AgentText)[0]).Text
		assert.LessOrEqual(t, len(text), 2048)
		assert.Greater(t, len(text), 2040)
		assert.True(t, utf8.ValidString(text))
		assert.True(t, strings.HasPrefix(long, text))

		tool := h.eventsOf(engine.AgentTool)[0]
		assert.Equal(t, "Bash", payloadOf[engine.AgentToolPayload](t, tool).Name)
		assert.LessOrEqual(t, len(payloadOf[engine.AgentToolPayload](t, tool).Summary), 2048)
		assert.NotContains(t, payloadOf[map[string]any](t, tool), "extra")

		assert.True(t, resets.Equal(payloadOf[engine.RateLimitPayload](t, h.eventsOf(engine.RateLimit)[0]).ResetsAt))
		assert.Equal(t, "evil.example", payloadOf[engine.EgressDeniedPayload](t, h.eventsOf(engine.EgressDenied)[0]).Host)
	})

	failedAppends := []struct {
		name    string
		works   []step[engine.WorkResult]
		status  engine.Status
		trace   []string
		resultT string // the event that records the session's result
	}{
		{name: "after a result", status: engine.StatusInReview, trace: happyTrace, resultT: engine.SessionEnded},
		{
			name: "after a runner fault", works: []step[engine.WorkResult]{{err: errors.New("the container exited")}},
			status: engine.StatusBlocked, trace: []string{"prepare", "work/start", "release/keep"}, resultT: engine.Stopped,
		},
	}

	for _, tt := range failedAppends {
		t.Run("a failed progress append is returned "+tt.name, func(t *testing.T) {
			h := newHarness(t)
			h.runner.progress = progress[:1]
			h.runner.works = tt.works

			injected := errors.New("disk full")
			h.spy.failAppend = func(events []store.Event) error {
				if events[0].Type == engine.AgentText {
					return injected
				}

				return nil
			}

			status, err := h.run(t.Context())
			require.ErrorIs(t, err, injected)
			assert.Equal(t, tt.status, status)
			assert.Equal(t, tt.trace, h.runner.trace())
			assert.Len(t, h.eventsOf(tt.resultT), 1)
			assert.Empty(t, h.eventsOf(engine.AgentText))
		})
	}
}

func TestRunTaskReturnsAtAWait(t *testing.T) {
	h := newHarness(t)
	h.runner.works = []step[engine.WorkResult]{{res: engine.WorkResult{Outcome: engine.OutcomeUsageLimit, ResetsAt: now.Add(2 * time.Hour)}}}

	status, err := h.run(t.Context())
	require.NoError(t, err)
	assert.Equal(t, engine.StatusBlocked, status)
	assert.Equal(t, []string{"prepare", "work/start", "release/keep"}, h.runner.trace())

	stop := h.lastStop()
	assert.Equal(t, engine.StopUsageLimit, stop.Reason)
	assert.True(t, now.Add(2*time.Hour).Equal(stop.ResetsAt))

	h.clock.Advance(2*time.Hour + time.Minute)

	status, err = h.run(t.Context())
	require.NoError(t, err)
	assert.Equal(t, engine.StatusInReview, status)
	assert.Equal(t, []string{
		"prepare", "work/start", "release/keep", "work/continue", "verify", "export", "release/run", "release/remove",
	}, h.runner.trace())

	works := h.runner.callsOf("work")
	assert.True(t, works[1].work.Resume)
	assert.Equal(t, works[0].work.SessionID, works[1].work.SessionID)
}

// A pause that begins while the task prepares leaves its run open with no container.
func TestRunTaskReleasesAtAWait(t *testing.T) {
	h := newHarness(t)

	limited := false
	h.loop.Paused = func(int64) engine.Pause {
		if limited {
			return engine.Pause{LimitedUntil: now.Add(time.Hour)}
		}

		return engine.Pause{}
	}
	h.runner.onCall = func(c call) {
		if c.method == "prepare" {
			limited = true
		}
	}

	status, err := h.run(t.Context())
	require.NoError(t, err)
	assert.Equal(t, engine.StatusRunning, status)
	assert.Equal(t, []string{"prepare", "release/keep"}, h.runner.trace())
	assert.Equal(t, h.runner.callsOf("prepare")[0].runID, h.runner.callsOf("release")[0].runID)
	assert.Empty(t, h.spy.ended(), "the run stays open")

	h.clock.Advance(time.Hour)

	status, err = h.run(t.Context())
	require.NoError(t, err)
	assert.Equal(t, engine.StatusInReview, status)
	assert.Equal(t, []string{"prepare", "release/keep", "work/start", "verify", "export", "release/run", "release/remove"}, h.runner.trace())
	assert.Equal(t, 1, h.spy.created())
}

// A runner fault is retried by itself on the same run, 30 seconds after the
// first fault and 2 minutes after the second; a third waits for a person.
func TestLoopRetriesAFault(t *testing.T) {
	h := newHarness(t)

	fault := step[engine.WorkResult]{err: errors.New("the container daemon is unreachable")}
	h.runner.works = []step[engine.WorkResult]{fault, fault, fault, fault, fault}

	// after moves the clock by d, runs the task and returns the runner calls the run made.
	after := func(d time.Duration, want engine.Status) []string {
		t.Helper()
		h.clock.Advance(d)

		before := len(h.runner.calls)

		status, err := h.run(t.Context())
		require.NoError(t, err)
		assert.Equal(t, want, status)

		return h.runner.trace()[before:]
	}

	faulted := []string{"work/continue", "release/keep"}

	assert.Equal(t, []string{"prepare", "work/start", "release/keep"}, after(0, engine.StatusBlocked))
	assert.Empty(t, after(30*time.Second-time.Millisecond, engine.StatusBlocked), "no retry before 30 seconds")
	assert.Equal(t, faulted, after(time.Millisecond, engine.StatusBlocked))
	assert.Empty(t, after(2*time.Minute-time.Millisecond, engine.StatusBlocked), "no retry before 2 minutes")
	assert.Equal(t, faulted, after(time.Millisecond, engine.StatusBlocked))
	assert.Empty(t, after(time.Hour, engine.StatusBlocked), "a third fault waits for a person")

	require.NoError(t, h.loop.Retry(t.Context(), h.task.ID))
	assert.Equal(t, faulted, after(0, engine.StatusBlocked))
	assert.Equal(t, faulted, after(30*time.Second, engine.StatusBlocked))
	assert.Equal(t, []string{"work/continue", "verify", "export", "release/run", "release/remove"}, after(2*time.Minute, engine.StatusInReview))

	works := h.runner.callsOf("work")
	require.Len(t, works, 6)

	for _, w := range works[1:] {
		assert.True(t, w.work.Resume)
		assert.Equal(t, works[0].work.SessionID, w.work.SessionID)
	}

	assert.Equal(t, 1, h.spy.created(), "every retry runs on the first run")
}

// A failed model endpoint is retried by itself as a runner fault is, on the
// same waits and the same cap, counted with the runner's faults; a refused one
// waits for a person.
func TestLoopRetriesAFailedEndpoint(t *testing.T) {
	// after moves h's clock by d, runs the task and returns the runner calls the run made.
	after := func(h *harness, d time.Duration, want engine.Status) []string {
		t.Helper()
		h.clock.Advance(d)

		before := len(h.runner.calls)

		status, err := h.run(t.Context())
		require.NoError(t, err)
		assert.Equal(t, want, status)

		return h.runner.trace()[before:]
	}

	stopped := []string{"work/continue", "release/keep"}

	t.Run("failed", func(t *testing.T) {
		h := newHarness(t)

		failed := step[engine.WorkResult]{res: engine.WorkResult{Outcome: engine.OutcomeEndpointFailed, Detail: "upstream status 529: Overloaded"}}
		fault := step[engine.WorkResult]{err: errors.New("the container daemon is unreachable")}
		h.runner.works = []step[engine.WorkResult]{failed, fault, failed}

		assert.Equal(t, []string{"prepare", "work/start", "release/keep"}, after(h, 0, engine.StatusBlocked))
		assert.Equal(t, engine.StopEndpoint, h.lastStop().Reason)
		assert.True(t, h.lastStop().Fault)

		assert.Empty(t, after(h, 30*time.Second-time.Millisecond, engine.StatusBlocked), "no retry before 30 seconds")
		assert.Equal(t, stopped, after(h, time.Millisecond, engine.StatusBlocked))
		assert.Equal(t, engine.StopSandbox, h.lastStop().Reason)
		assert.Empty(t, after(h, 2*time.Minute-time.Millisecond, engine.StatusBlocked), "no retry before 2 minutes")
		assert.Equal(t, stopped, after(h, time.Millisecond, engine.StatusBlocked))
		assert.True(t, h.lastStop().Fault)
		assert.Empty(t, after(h, time.Hour, engine.StatusBlocked), "a third fault waits for a person")

		require.NoError(t, h.loop.Retry(t.Context(), h.task.ID))
		assert.Equal(t, []string{"work/continue", "verify", "export", "release/run", "release/remove"}, after(h, 0, engine.StatusInReview))

		works := h.runner.callsOf("work")
		require.Len(t, works, 4)

		for _, w := range works[1:] {
			assert.True(t, w.work.Resume)
			assert.Equal(t, works[0].work.SessionID, w.work.SessionID)
		}
	})

	t.Run("refused", func(t *testing.T) {
		h := newHarness(t)
		h.runner.works = []step[engine.WorkResult]{{res: engine.WorkResult{Outcome: engine.OutcomeEndpoint, Detail: "upstream status 403: Forbidden"}}}

		assert.Equal(t, []string{"prepare", "work/start", "release/keep"}, after(h, 0, engine.StatusBlocked))
		assert.Equal(t, engine.StopEndpoint, h.lastStop().Reason)
		assert.False(t, h.lastStop().Fault)
		assert.Empty(t, after(h, time.Hour, engine.StatusBlocked), "a refused endpoint waits for a person")
	})
}

func TestDependencies(t *testing.T) {
	// dependent adds and starts a task that depends on the harness's task.
	dependent := func(h *harness) store.Task {
		h.t.Helper()
		h.withBaseline()

		b, err := engine.AddTask(h.t.Context(), h.store, store.Task{ProjectID: h.task.ProjectID, Title: "B"}, []int64{h.task.ID})
		require.NoError(h.t, err)
		require.NoError(h.t, h.loop.Start(h.t.Context(), b.ID))

		return b
	}

	t.Run("a task is queued until its dependency is done", func(t *testing.T) {
		h := newHarness(t)
		b := dependent(h)

		status, err := h.loop.RunTask(t.Context(), b.ID)
		require.NoError(t, err)
		assert.Equal(t, engine.StatusQueued, status)
		assert.Empty(t, h.runner.calls)

		status, err = h.run(t.Context())
		require.NoError(t, err)
		require.Equal(t, engine.StatusInReview, status)

		status, err = h.loop.RunTask(t.Context(), b.ID)
		require.NoError(t, err)
		assert.Equal(t, engine.StatusQueued, status, "a dependency in review is not done")
		assert.Equal(t, happyTrace, h.runner.trace())

		require.NoError(t, h.loop.Done(t.Context(), h.task.ID))

		status, err = h.loop.RunTask(t.Context(), b.ID)
		require.NoError(t, err)
		assert.Equal(t, engine.StatusInReview, status)
		assert.Equal(t, append(slices.Clone(happyTrace), happyTrace...), h.runner.trace())
	})

	t.Run("a cancelled dependency stops the task until a person retries", func(t *testing.T) {
		h := newHarness(t)
		b := dependent(h)
		require.NoError(t, h.loop.Cancel(t.Context(), h.task.ID))

		status, err := h.loop.RunTask(t.Context(), b.ID)
		require.NoError(t, err)
		assert.Equal(t, engine.StatusBlocked, status)
		assert.Empty(t, h.runner.calls)

		stop := h.lastStopOf(b.ID)
		assert.Equal(t, engine.StopDependency, stop.Reason)
		assert.Equal(t, engine.ClearRetry, stop.Action)
		assert.Contains(t, stop.Explanation, strconv.FormatInt(h.task.ID, 10))

		require.NoError(t, engine.EditTask(t.Context(), h.store, b, nil))
		require.NoError(t, h.loop.Retry(t.Context(), b.ID))

		status, err = h.loop.RunTask(t.Context(), b.ID)
		require.NoError(t, err)
		assert.Equal(t, engine.StatusInReview, status)
		assert.Equal(t, happyTrace, h.runner.trace())
	})
}

func TestLoopActionCap(t *testing.T) {
	h := newHarness(t)

	// A usage limit that has reset by the time its stop is released never lets the task rest.
	h.runner.works = slices.Repeat([]step[engine.WorkResult]{{res: engine.WorkResult{Outcome: engine.OutcomeUsageLimit}}}, 100)
	h.runner.onCall = func(c call) {
		if c.method == "release" {
			h.clock.Advance(time.Hour)
		}
	}

	_, err := h.run(t.Context())
	require.ErrorIs(t, err, engine.ErrActionCap)
	assert.Len(t, h.eventsOf(engine.Stopped), len(h.eventsOf(engine.SessionEnded))-1, "hitting the cap writes no stop")
}

func TestRetryAfterVerifyFailed(t *testing.T) {
	h := newHarness(t)

	failed := step[engine.VerifyResult]{res: engine.VerifyResult{Result: engine.ResultFailed, Output: "FAIL TestLogin"}}
	for range 6 {
		h.runner.verifies = append(h.runner.verifies, failed)
	}

	status, err := h.run(t.Context())
	require.NoError(t, err)
	require.Equal(t, engine.StatusBlocked, status)
	require.Equal(t, engine.StopVerifyFailed, h.lastStop().Reason)

	before := len(h.runner.calls)
	startsBefore := len(h.eventsOf(engine.SessionStarted))

	h.add(0, engine.HumanRetried, "user", "")

	status, err = h.run(t.Context())
	require.NoError(t, err)
	assert.Equal(t, engine.StatusBlocked, status)
	assert.Equal(t, engine.StopVerifyFailed, h.lastStop().Reason)
	assert.Equal(t, []string{
		"work/fix_verify", "verify", "work/fix_verify", "verify", "work/fix_verify", "verify", "release/keep",
	}, h.runner.trace()[before:])

	var automatic []bool
	for _, e := range h.eventsOf(engine.SessionStarted)[startsBefore:] {
		automatic = append(automatic, payloadOf[engine.SessionStartedPayload](t, e).Automatic)
	}

	assert.Equal(t, []bool{false, true, true}, automatic)
}

func TestLoopAnswerReachesTheAgent(t *testing.T) {
	h := newHarness(t)
	h.runner.works = []step[engine.WorkResult]{{res: engine.WorkResult{Outcome: engine.OutcomeBlocked, Questions: []string{"Which port?"}}}}

	status, err := h.run(t.Context())
	require.NoError(t, err)
	require.Equal(t, engine.StatusBlocked, status)
	require.Equal(t, engine.StopQuestion, h.lastStop().Reason)

	h.add(0, engine.HumanAnswered, "user", `{"text":"Use port 8080."}`)

	status, err = h.run(t.Context())
	require.NoError(t, err)
	assert.Equal(t, engine.StatusInReview, status)

	works := h.runner.callsOf("work")
	require.Len(t, works, 2)
	assert.Equal(t, engine.ModeAnswer, works[1].work.Mode)
	assert.Equal(t, "Use port 8080.", works[1].work.Detail)
	assert.True(t, works[1].work.Resume)
	assert.Equal(t, works[0].work.SessionID, works[1].work.SessionID)
}

// withDefaultSwitches gives the harness's project the default switches but
// publish, which turn the readiness turn and the review on.
func (h *harness) withDefaultSwitches() {
	h.t.Helper()

	p := h.project()
	p.Switches = map[string]bool{"publish": false}
	require.NoError(h.t, h.store.UpdateProject(h.t.Context(), p))
}

func TestLoopReadinessThenWork(t *testing.T) {
	h := newHarness(t)
	h.withDefaultSwitches()
	h.runner.works = []step[engine.WorkResult]{{res: engine.WorkResult{Outcome: engine.OutcomeReady, Turns: 2}}}

	status, err := h.run(t.Context())
	require.NoError(t, err)
	assert.Equal(t, engine.StatusInReview, status)
	assert.Equal(t, []string{"prepare", "work/readiness", "work/start", "verify", "review", "export", "release/run", "release/remove"}, h.runner.trace())

	works := h.runner.callsOf("work")
	require.Len(t, works, 2)
	assert.Regexp(t, uuidV4, works[0].work.SessionID)
	assert.False(t, works[0].work.Resume, "the readiness turn opens the session")
	assert.True(t, works[1].work.Resume, "the work resumes it")
	assert.Equal(t, works[0].work.SessionID, works[1].work.SessionID)
}

func TestLoopReadinessQuestion(t *testing.T) {
	h := newHarness(t)
	h.withDefaultSwitches()

	questions, suggested := []string{"Which port?", "Is IPv6 needed?"}, []string{"8080", "No"}
	h.runner.works = []step[engine.WorkResult]{{res: engine.WorkResult{
		Outcome: engine.OutcomeBlocked, Questions: questions, SuggestedAnswers: suggested,
	}}}

	status, err := h.run(t.Context())
	require.NoError(t, err)
	require.Equal(t, engine.StatusBlocked, status)
	assert.Equal(t, engine.StopQuestion, h.lastStop().Reason)

	snap, err := h.loop.Snapshot(t.Context(), h.task.ID)
	require.NoError(t, err)
	require.NotNil(t, snap.Facts.Session)
	assert.Equal(t, questions, snap.Facts.Session.Questions)
	assert.Equal(t, suggested, snap.Facts.Session.SuggestedAnswers)

	require.NoError(t, h.loop.Answer(t.Context(), h.task.ID, "Port 8080, and no IPv6."))

	status, err = h.run(t.Context())
	require.NoError(t, err)
	assert.Equal(t, engine.StatusInReview, status)

	works := h.runner.callsOf("work")
	require.Len(t, works, 2)
	assert.Equal(t, engine.ModeReadiness, works[0].work.Mode)
	assert.Equal(t, engine.ModeAnswer, works[1].work.Mode)
	assert.Equal(t, "Port 8080, and no IPv6.", works[1].work.Detail)
	assert.True(t, works[1].work.Resume)
	assert.Equal(t, works[0].work.SessionID, works[1].work.SessionID)
}

// A stop needs no run config: a broken setting neither keeps a person's cancel
// out of the record nor keeps the run from being released.
func TestLoopStopsWithoutConfig(t *testing.T) {
	h := newHarness(t)
	r := h.seed(
		ev(engine.RunStarted, `{"reason":"initial"}`), ev(engine.PrepareStarted, ""), ev(engine.Prepared, `{"base_commit":"base123"}`),
		ev(engine.SessionStarted, `{"mode":"start","session_id":"`+seededSession+`"}`),
		ev(engine.SessionEnded, `{"outcome":"blocked","questions":["Which port?"]}`),
		ev(engine.Stopped, `{"reason":"question","explanation":"Which port?","action":"answer"}`),
	)
	h.add(0, engine.HumanCancelled, "user", "")
	require.NoError(t, h.store.SetSetting(t.Context(), "model_work", ""))

	status, err := h.run(t.Context())
	require.NoError(t, err)
	assert.Equal(t, engine.StatusCancelled, status)
	assert.Equal(t, engine.StopCancelled, h.lastStop().Reason)

	ends := h.eventsOf(engine.RunEnded)
	require.Len(t, ends, 1)
	assert.Equal(t, engine.StopCancelled, payloadOf[engine.RunEndedPayload](t, ends[0]).Outcome)
	assert.Equal(t, []string{"release/run", "release/remove"}, h.runner.trace())
	assert.Equal(t, engine.RunRef{
		InstanceID: "inst-1", Image: "cavu-sandbox:test", TaskID: h.task.ID, RunID: r.ID, ProjectID: h.task.ProjectID,
	}, h.runner.calls[0].ref)
	assert.Equal(t, map[int64]string{r.ID: engine.StopCancelled}, h.spy.ended())
}

// A run config that cannot be built is a stop a person clears by fixing the
// cause and retrying, as a runner fault is; nothing of the runner runs.
func TestLoopStopsOnABrokenRunConfig(t *testing.T) {
	const missing = "project.web.NPM_TOKEN" // the vault name of the secret the project names

	// unset makes the project name a secret that is not set.
	unset := func(h *harness) {
		p := h.project()
		p.SecretVars = []string{"NPM_TOKEN"}
		require.NoError(h.t, h.store.UpdateProject(h.t.Context(), p))
	}

	// fix sets the secret and records a person's retry.
	fix := func(h *harness) {
		require.NoError(h.t, h.vault.Set(h.t.Context(), missing, "npm-secret-value"))
		require.NoError(h.t, h.loop.Retry(h.t.Context(), h.task.ID))
	}

	t.Run("before a run", func(t *testing.T) {
		h := newHarness(t)
		unset(h)

		status, err := h.run(t.Context())
		require.NoError(t, err)
		assert.Equal(t, engine.StatusBlocked, status)
		assert.Empty(t, h.runner.trace())

		stop := h.lastStop()
		assert.Equal(t, engine.StopSandbox, stop.Reason)
		assert.Equal(t, engine.ClearRetry, stop.Action)
		assert.Contains(t, stop.Explanation, missing)

		evs := h.events()
		assert.Equal(t, []string{engine.Stopped}, types(evs))
		assert.Zero(t, evs[0].RunID, "the stop carries no run")
		assert.Equal(t, 1, h.spy.created())
		assert.Equal(t, []string{engine.RunAbandoned}, slices.Collect(maps.Values(h.spy.ended())), "the new run's row is abandoned")

		fix(h)

		status, err = h.run(t.Context())
		require.NoError(t, err)
		assert.Equal(t, engine.StatusInReview, status)
		assert.Equal(t, happyTrace, h.runner.trace())
	})

	t.Run("with a run open", func(t *testing.T) {
		h := newHarness(t)
		r := h.seed(ev(engine.RunStarted, `{"reason":"initial"}`), ev(engine.PrepareStarted, ""), ev(engine.Prepared, `{"base_commit":"base123"}`))
		unset(h)

		status, err := h.run(t.Context())
		require.NoError(t, err)
		assert.Equal(t, engine.StatusBlocked, status)
		assert.Equal(t, []string{"release/keep"}, h.runner.trace(), "the run's containers stop and its volumes stay")
		assert.Equal(t, r.ID, h.runner.callsOf("release")[0].runID)
		assert.Empty(t, h.spy.ended(), "the run stays open")

		stops := h.eventsOf(engine.Stopped)
		require.Len(t, stops, 1)
		assert.Equal(t, r.ID, stops[0].RunID)
		assert.Contains(t, h.lastStop().Explanation, missing)
		assert.Empty(t, h.eventsOf(engine.RunEnded))

		fix(h)

		status, err = h.run(t.Context())
		require.NoError(t, err)
		assert.Equal(t, engine.StatusInReview, status)
		assert.Equal(t, []string{"release/keep", "work/start", "verify", "export", "release/run", "release/remove"}, h.runner.trace())
		assert.Zero(t, h.spy.created(), "the work goes on in the same run")
	})
}

// A run's end keeps the task's home, where a fix run on the pull request
// resumes the session; once the task can run no more, the release of its
// newest run removes the home, once, and the task rests.
func TestHomeIsRemovedWhenTheTaskIsOver(t *testing.T) {
	cancel := func(h *harness) { require.NoError(h.t, h.loop.Cancel(h.t.Context(), h.task.ID)) }
	merge := func(h *harness) {
		h.observe(engine.Observation{State: engine.PRMerged, Head: "head456", Checks: engine.ChecksPassed})
	}

	tests := []struct {
		name    string
		publish bool
		script  func(r *fakeRunner)
		end     func(h *harness) // what ends the task once it rests, nil when it is over then
		trace   []string
		status  engine.Status
	}{
		{
			name: "cancelled after a stop ended its run",
			script: func(r *fakeRunner) {
				r.prepares = []step[engine.PrepareResult]{{res: engine.PrepareResult{FailedStep: "setup", Output: "exit 1"}}}
			},
			end: cancel, trace: []string{"prepare", "release/run", "release/remove"}, status: engine.StatusCancelled,
		},
		{
			name: "cancelled with its run open",
			script: func(r *fakeRunner) {
				r.works = []step[engine.WorkResult]{{res: engine.WorkResult{Outcome: engine.OutcomeBlocked, Questions: []string{"Which port?"}}}}
			},
			end: cancel, trace: []string{"prepare", "work/start", "release/keep", "release/run", "release/remove"}, status: engine.StatusCancelled,
		},
		{name: "exported with publish off", script: func(*fakeRunner) {}, trace: happyTrace, status: engine.StatusInReview},
		{
			name: "merged", publish: true, script: func(*fakeRunner) {}, end: merge,
			trace:  []string{"prepare", "work/start", "verify", "export", "publish", "release/run", "observe", "release/remove"},
			status: engine.StatusDone,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			if tt.publish {
				h.withPublish()
			}

			tt.script(h.runner)

			_, err := h.run(t.Context())
			require.NoError(t, err)

			if tt.end != nil {
				assert.NotContains(t, h.runner.trace(), "release/remove", "the home stays while the task can run again")
				tt.end(h)

				_, err = h.run(t.Context())
				require.NoError(t, err)
			}

			status, err := h.run(t.Context())
			require.NoError(t, err)
			assert.Equal(t, tt.status, status)
			assert.Equal(t, tt.trace, h.runner.trace(), "the task rests once its home is removed")

			runs := h.eventsOf(engine.RunStarted)
			require.Len(t, runs, 1)

			ends := h.eventsOf(engine.RunEnded)
			require.Len(t, ends, 1)
			assert.True(t, payloadOf[engine.RunEndedPayload](t, ends[0]).KeptHome)

			removed := h.eventsOf(engine.HomeRemoved)
			require.Len(t, removed, 1)
			assert.Equal(t, runs[0].RunID, removed[0].RunID)

			releases := h.runner.callsOf("release")
			assert.Equal(t, engine.RemoveAll, releases[len(releases)-1].remove)
			assert.Equal(t, runs[0].RunID, releases[len(releases)-1].runID, "the newest run's release removes the home")

			if tt.status == engine.StatusCancelled {
				var cancels int

				for _, e := range h.eventsOf(engine.Stopped) {
					if payloadOf[engine.StoppedPayload](t, e).Reason == engine.StopCancelled {
						cancels++
					}
				}

				assert.Equal(t, 1, cancels, "the removal leaves the cancel's stop")
			}
		})
	}

	t.Run("a release that fails records nothing, and the next pass removes the home", func(t *testing.T) {
		h := newHarness(t)

		injected := errors.New("volume busy")
		h.runner.gate = func(_ context.Context, c call) error {
			if c.method == "release" && c.remove == engine.RemoveAll {
				return injected
			}

			return nil
		}

		_, err := h.run(t.Context())
		require.ErrorIs(t, err, injected)
		assert.Empty(t, h.eventsOf(engine.HomeRemoved))

		h.runner.gate = nil

		status, err := h.run(t.Context())
		require.NoError(t, err)
		assert.Equal(t, engine.StatusInReview, status)
		assert.Equal(t, append(slices.Clone(happyTrace), "release/remove"), h.runner.trace())
		assert.Len(t, h.eventsOf(engine.HomeRemoved), 1)
	})

	t.Run("kept while a pull request is open", func(t *testing.T) {
		h := newHarness(t)
		h.withPublish()

		status, err := h.run(t.Context())
		require.NoError(t, err)
		assert.Equal(t, engine.StatusInReview, status)
		assert.Equal(t, []string{"prepare", "work/start", "verify", "export", "publish", "release/run"}, h.runner.trace())
		assert.Empty(t, h.eventsOf(engine.HomeRemoved))

		snap, err := h.loop.Snapshot(t.Context(), h.task.ID)
		require.NoError(t, err)
		assert.True(t, snap.Facts.Home)
	})
}
