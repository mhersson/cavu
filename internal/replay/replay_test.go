package replay_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mhersson/cavu/internal/auth"
	"github.com/mhersson/cavu/internal/engine"
	"github.com/mhersson/cavu/internal/replay"
	"github.com/mhersson/cavu/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeRunner answers from queues, then with a success, and records what it saw.
type fakeRunner struct {
	mu         sync.Mutex
	configs    []engine.RunConfig
	modes      []string
	works      []engine.WorkResult
	verifies   []engine.VerifyResult
	releases   []release
	duringWork func(n int) // runs inside the nth Work call, counted from 1
}

// release is one Release call.
type release struct {
	runID  int64
	remove engine.Remove
}

func (r *fakeRunner) Prepare(_ context.Context, c engine.RunConfig) (engine.PrepareResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.configs = append(r.configs, c)

	return engine.PrepareResult{BaseCommit: c.Repo.BaseCommit, Baseline: "passed"}, nil
}

func (r *fakeRunner) Work(ctx context.Context, _ engine.RunConfig, w engine.WorkRequest, _ func(engine.Progress)) (engine.WorkResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.modes = append(r.modes, w.Mode)

	if r.duringWork != nil {
		r.duringWork(len(r.modes))
	}

	if err := ctx.Err(); err != nil {
		return engine.WorkResult{}, err
	}

	if len(r.works) == 0 {
		return engine.WorkResult{Outcome: engine.OutcomeDone, Changed: true, Title: "Done", Turns: 1}, nil
	}

	res := r.works[0]
	r.works = r.works[1:]

	return res, nil
}

func (r *fakeRunner) Verify(context.Context, engine.RunConfig) (engine.VerifyResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if len(r.verifies) == 0 {
		return engine.VerifyResult{Result: engine.ResultPassed, Command: "make test"}, nil
	}

	res := r.verifies[0]
	r.verifies = r.verifies[1:]

	return res, nil
}

func (r *fakeRunner) Review(context.Context, engine.RunConfig, engine.ReviewRequest, func(engine.Progress)) (engine.ReviewResult, error) {
	return engine.ReviewResult{Outcome: engine.ReviewReviewed, Asked: engine.Verdict{Pass: true}, Built: engine.Verdict{Pass: true}}, nil
}

func (r *fakeRunner) Export(_ context.Context, c engine.RunConfig, _ string) (engine.ExportResult, error) {
	return engine.ExportResult{HeadCommit: "head", Diff: fmt.Appendf(nil, "diff of task %d", c.TaskID), FilesChanged: 1}, nil
}

// Publish is not scripted: the fake does not publish.
func (r *fakeRunner) Publish(context.Context, engine.RunConfig, engine.PublishRequest) (engine.PublishResult, error) {
	return engine.PublishResult{}, errors.New("fake: the runner does not publish")
}

var errNoPullRequest = errors.New("fake: no pull request")

// Observe, Ready and Reply are not scripted: a shadow never publishes, so it has no pull request.
func (r *fakeRunner) Observe(context.Context, engine.RunConfig, int) (engine.Observation, error) {
	return engine.Observation{}, errNoPullRequest
}

func (r *fakeRunner) Ready(context.Context, engine.RunConfig, string) error { return errNoPullRequest }

func (r *fakeRunner) Reply(context.Context, engine.RunConfig, int, string) error {
	return errNoPullRequest
}

func (r *fakeRunner) RemoveProject(context.Context, engine.RunRef) error { return nil }

func (r *fakeRunner) Release(_ context.Context, ref engine.RunRef, remove engine.Remove) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.releases = append(r.releases, release{ref.RunID, remove})

	return nil
}

func (r *fakeRunner) CheckEnvironment(context.Context, engine.RunConfig) (engine.PrepareResult, string, error) {
	return engine.PrepareResult{}, "", nil
}

type env struct {
	store   *store.Store
	loop    *engine.Loop
	runner  *fakeRunner
	project store.Project
}

func newEnv(t *testing.T) *env {
	t.Helper()

	ctx := t.Context()

	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "cavu.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	v, err := auth.NewVault(s, bytes.Repeat([]byte{7}, auth.MasterKeyLen))
	require.NoError(t, err)

	for k, val := range map[string]string{
		"instance_id": "inst-1", "endpoint_mode": "subscription", "model_work": "model-a",
		"commit_name": "CAVU", "commit_email": "cavu@example.com",
	} {
		require.NoError(t, s.SetSetting(ctx, k, val))
	}

	// The readiness turn is off: only the tests that turn it on are about it.
	p, err := s.CreateProject(ctx, store.Project{
		Name: "web", RepoURL: "https://example.com/web.git", BaseBranch: "main",
		SetupCommand: "make setup", VerifyCommand: "make test", Baseline: &store.Baseline{Commit: "tip", Passed: true},
		Switches: map[string]bool{"readiness": false},
	})
	require.NoError(t, err)

	r := &fakeRunner{}

	// A clock that moves one second per reading makes durations exact.
	var (
		mu sync.Mutex
		at = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	)

	tick := func() time.Time {
		mu.Lock()
		defer mu.Unlock()

		at = at.Add(time.Second)

		return at
	}

	return &env{
		store: s, runner: r, project: p,
		loop: &engine.Loop{Store: s, Vault: v, Runner: r, Now: tick, DataDir: t.TempDir(), Image: "cavu-sandbox:test"},
	}
}

func (e *env) task(t *testing.T, title string, reference bool, base string) store.Task {
	t.Helper()

	task, err := e.store.CreateTask(t.Context(), store.Task{
		ProjectID: e.project.ID, Title: title, Body: "body of " + title, Requester: "ann",
		BaseCommit: base, Reference: reference,
	})
	require.NoError(t, err)

	return task
}

func (e *env) shadows(t *testing.T) []store.Task {
	t.Helper()

	all, err := e.store.Tasks(t.Context(), true)
	require.NoError(t, err)

	var out []store.Task

	for _, task := range all {
		if task.ReplayID != 0 {
			out = append(out, task)
		}
	}

	return out
}

// drive drives the shadow until it rests, as a scheduler would.
func (e *env) drive(ctx context.Context, taskID int64) (engine.Status, error) {
	return e.loop.RunTask(ctx, taskID)
}

func (e *env) run(t *testing.T, c replay.Config) (store.Replay, []store.ReplayItem, string) {
	t.Helper()

	var out bytes.Buffer

	r, err := replay.Run(t.Context(), e.store, e.loop, c, e.drive, &out)
	require.NoError(t, err)

	items, err := e.store.ReplayItems(t.Context(), r.ID)
	require.NoError(t, err)

	return r, items, out.String()
}

func TestReplayRunsEveryReferenceTask(t *testing.T) {
	t.Parallel()

	e := newEnv(t)
	a := e.task(t, "first", true, "aaa")
	b := e.task(t, "second", true, "bbb")
	e.task(t, "plain", false, "ccc")
	e.task(t, "never prepared", true, "")

	r, items, out := e.run(t, replay.Config{Name: "baseline"})

	shadows := e.shadows(t)
	require.Len(t, shadows, 2)
	assert.Equal(t, 2, strings.Count(out, "\n"), "one progress line per item")
	require.NotNil(t, r.EndedAt)

	prev, ok, err := e.store.PreviousReplay(t.Context(), r.ID+1)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, r.ID, prev.ID)
	assert.Equal(t, e.loop.Image, prev.Image)

	for i, src := range []store.Task{a, b} {
		sh := shadows[i]
		assert.Equal(t, r.ID, sh.ReplayID)
		assert.False(t, sh.Reference)
		assert.Equal(t, src.BaseCommit, sh.BaseCommit)
		assert.Equal(t, src.Title, sh.Title)
		assert.Equal(t, src.Body, sh.Body)
		assert.Equal(t, src.Requester, sh.Requester)
		assert.Equal(t, src.ProjectID, sh.ProjectID)
		assert.Equal(t, src.ID, items[i].SourceTaskID)
		assert.Equal(t, sh.ID, items[i].TaskID)
	}
}

func TestReplayAppliesOverrides(t *testing.T) {
	t.Parallel()

	t.Run("model, effort and web tools reach the run", func(t *testing.T) {
		t.Parallel()

		e := newEnv(t)
		e.task(t, "one", true, "aaa")

		r, _, _ := e.run(t, replay.Config{Name: "x", Model: "model-b", Effort: "high", WebTools: new(true)})

		require.Len(t, e.runner.configs, 1)
		assert.Equal(t, "model-b", e.runner.configs[0].Agent.Model)
		assert.Equal(t, "high", e.runner.configs[0].Agent.Effort)
		assert.True(t, e.runner.configs[0].Agent.WebTools)
		assert.Equal(t, map[string]string{"model_work": "model-b", "effort_work": "high", "web_tools": "true"}, r.Config)
	})

	t.Run("an empty config overrides nothing", func(t *testing.T) {
		t.Parallel()

		e := newEnv(t)
		e.task(t, "one", true, "aaa")

		r, _, _ := e.run(t, replay.Config{Name: "x"})

		assert.Equal(t, "model-a", e.runner.configs[0].Agent.Model)
		assert.False(t, e.runner.configs[0].Agent.WebTools)
		assert.Empty(t, r.Config)
	})

	t.Run("verify_fix off stops a failed verify", func(t *testing.T) {
		t.Parallel()

		e := newEnv(t)
		e.task(t, "one", true, "aaa")
		e.runner.verifies = []engine.VerifyResult{{Result: engine.ResultFailed, Command: "make test", Output: "red"}}

		_, items, _ := e.run(t, replay.Config{Name: "x", VerifyFix: new(false)})

		assert.NotContains(t, e.runner.modes, engine.ModeFixVerify)
		assert.Equal(t, "verify_failed", items[0].Stop)
		assert.Equal(t, map[string]string{"verify_fix": "false"}, e.shadows(t)[0].Overrides)
	})

	t.Run("verify_fix on tries a fix", func(t *testing.T) {
		t.Parallel()

		e := newEnv(t)
		e.task(t, "one", true, "aaa")
		e.runner.verifies = []engine.VerifyResult{{Result: engine.ResultFailed, Command: "make test", Output: "red"}}

		_, items, _ := e.run(t, replay.Config{Name: "x", VerifyFix: new(true)})

		assert.Contains(t, e.runner.modes, engine.ModeFixVerify)
		assert.Empty(t, items[0].Stop)
	})

	t.Run("continuations off stops an unfinished session", func(t *testing.T) {
		t.Parallel()

		e := newEnv(t)
		e.task(t, "one", true, "aaa")
		e.runner.works = []engine.WorkResult{{Outcome: engine.OutcomeUnfinished, Turns: 2}}

		_, items, _ := e.run(t, replay.Config{Name: "x", Continuations: new(false)})

		assert.NotContains(t, e.runner.modes, engine.ModeContinue)
		assert.NotEmpty(t, items[0].Stop)
		assert.Equal(t, map[string]string{"continuations": "false"}, e.shadows(t)[0].Overrides)
	})
}

func TestReplayRecordsItems(t *testing.T) {
	t.Parallel()

	e := newEnv(t)
	src := e.task(t, "one", true, "aaa")
	e.runner.works = []engine.WorkResult{{
		Outcome: engine.OutcomeDone, Changed: true, Title: "Done", Turns: 4, CostEstimateUSD: 0.5,
		Usage: []store.Usage{
			{Source: "agent", Model: "model-a", Input: 10, Output: 5, CacheRead: 100, CacheWrite: 20, CostEstimateUSD: 0.25},
			{Source: "proxy", Model: "model-a", Input: 1, Output: 2, CacheRead: 3, CacheWrite: 4, CostEstimateUSD: 0.5},
		},
	}}

	_, items, _ := e.run(t, replay.Config{Name: "x"})

	require.Len(t, items, 1)
	sh := e.shadows(t)[0]
	got := items[0]

	assert.Equal(t, src.ID, got.SourceTaskID)
	assert.Equal(t, engine.ResultPassed, got.Verify)
	assert.Empty(t, got.Stop)
	assert.Zero(t, got.Asked)
	assert.Equal(t, 4, got.Turns)
	assert.Equal(t, int64(145), got.Tokens)
	assert.InDelta(t, 0.75, got.CostEstimateUSD, 1e-9)

	diff, err := os.ReadFile(got.DiffPath)
	require.NoError(t, err)
	assert.Equal(t, fmt.Sprintf("diff of task %d", sh.ID), string(diff), "the item points at the shadow task's diff")

	evs, err := e.store.Events(t.Context(), sh.ID, 0, 0)
	require.NoError(t, err)
	assert.Equal(t, evs[len(evs)-1].At.Sub(evs[0].At), got.Duration)
	assert.Positive(t, got.Duration)
}

func TestReplayQuestionCountsAsAsked(t *testing.T) {
	t.Parallel()

	e := newEnv(t)
	e.task(t, "one", true, "aaa")
	e.runner.works = []engine.WorkResult{{Outcome: engine.OutcomeBlocked, Questions: []string{"which?"}, Turns: 2}}

	_, items, _ := e.run(t, replay.Config{Name: "x"})

	assert.Equal(t, 1, items[0].Asked)
	assert.Equal(t, "question", items[0].Stop)
	assert.Empty(t, items[0].Verify)
	assert.Empty(t, items[0].DiffPath)
}

func TestReplayAnswersReadinessQuestions(t *testing.T) {
	t.Parallel()

	questions := []string{"Which port?", "Is IPv6 needed?"}

	t.Run("every question with a suggested answer", func(t *testing.T) {
		t.Parallel()

		e := newEnv(t)
		e.task(t, "one", true, "aaa")
		e.runner.works = []engine.WorkResult{{Outcome: engine.OutcomeBlocked, Questions: questions, SuggestedAnswers: []string{"8080", "No"}}}

		_, items, _ := e.run(t, replay.Config{Name: "x", Readiness: new(true)})

		var answers []engine.HumanAnsweredPayload

		for _, ev := range e.events(t, e.shadows(t)[0].ID) {
			if ev.Type == engine.HumanAnswered {
				var p engine.HumanAnsweredPayload
				require.NoError(t, json.Unmarshal(ev.Payload, &p))
				answers = append(answers, p)
			}
		}

		require.Len(t, answers, 1, "one answer holds every pair")

		for _, v := range []string{"Which port?", "8080", "Is IPv6 needed?", "No"} {
			assert.Contains(t, answers[0].Text, v)
		}

		assert.Less(t, strings.Index(answers[0].Text, "8080"), strings.Index(answers[0].Text, "Is IPv6 needed?"), "each answer follows its question")
		assert.Equal(t, []string{engine.ModeReadiness, engine.ModeAnswer}, e.runner.modes)
		assert.Equal(t, 2, items[0].Asked)
		assert.Equal(t, engine.ResultPassed, items[0].Verify, "the shadow runs on to verify")
		assert.Empty(t, items[0].Stop)
	})

	t.Run("a question without one", func(t *testing.T) {
		t.Parallel()

		e := newEnv(t)
		e.task(t, "one", true, "aaa")
		e.runner.works = []engine.WorkResult{{Outcome: engine.OutcomeBlocked, Questions: questions, SuggestedAnswers: []string{"8080", ""}}}

		_, items, _ := e.run(t, replay.Config{Name: "x", Readiness: new(true)})

		evs := e.events(t, e.shadows(t)[0].ID)
		assert.False(t, slices.ContainsFunc(evs, func(ev store.Event) bool { return ev.Type == engine.HumanAnswered }))
		assert.Equal(t, engine.StatusCancelled, engine.StatusOf(engine.Reduce(evs)))
		assert.Equal(t, []string{engine.ModeReadiness}, e.runner.modes)
		assert.Equal(t, engine.StopQuestion, items[0].Stop)
		assert.Equal(t, 2, items[0].Asked)
		assert.Empty(t, items[0].Verify)
	})
}

func TestReplayShadowTaskStopsAreRecorded(t *testing.T) {
	t.Parallel()

	e := newEnv(t)
	e.task(t, "one", true, "aaa")
	b := e.task(t, "two", true, "bbb")
	e.runner.verifies = []engine.VerifyResult{{Result: engine.ResultFailed, Command: "make test"}}

	_, items, _ := e.run(t, replay.Config{Name: "x", VerifyFix: new(false)})

	require.Len(t, items, 2)
	assert.Equal(t, "verify_failed", items[0].Stop)
	assert.Equal(t, engine.ResultFailed, items[0].Verify)
	assert.Equal(t, b.ID, items[1].SourceTaskID)
	assert.Empty(t, items[1].Stop)
}

func TestReplayEndsTheRunOfAStoppedShadow(t *testing.T) {
	t.Parallel()

	e := newEnv(t)
	e.task(t, "one", true, "aaa")
	e.task(t, "two", true, "bbb")
	e.runner.verifies = []engine.VerifyResult{{Result: engine.ResultFailed, Command: "make test"}}

	_, items, _ := e.run(t, replay.Config{Name: "x", VerifyFix: new(false)})

	require.Len(t, items, 2)
	assert.Equal(t, engine.StopVerifyFailed, items[0].Stop, "the item keeps the stop the shadow ended on")

	var runs []int64

	for i, want := range []engine.Status{engine.StatusCancelled, engine.StatusInReview} {
		evs, err := e.store.Events(t.Context(), e.shadows(t)[i].ID, 0, 0)
		require.NoError(t, err)

		facts := engine.Reduce(evs)
		assert.Equal(t, want, engine.StatusOf(facts))
		assert.False(t, facts.RunOpen)

		for _, ev := range evs {
			if ev.Type == engine.RunStarted {
				runs = append(runs, ev.RunID)
			}
		}
	}

	require.Len(t, runs, 2)
	assert.Equal(t, []release{
		{runs[0], engine.KeepVolumes}, {runs[0], engine.RemoveRun}, {runs[0], engine.RemoveAll}, {runs[1], engine.RemoveRun}, {runs[1], engine.RemoveAll},
	}, e.runner.releases, "the stopped shadow's run ends once it is recorded, and each shadow's home goes once it can run no more")
}

func TestReplayExcludedFromTaskList(t *testing.T) {
	t.Parallel()

	e := newEnv(t)
	a := e.task(t, "one", true, "aaa")
	b := e.task(t, "two", false, "bbb")

	e.run(t, replay.Config{Name: "x"})

	got, err := e.store.Tasks(t.Context(), false)
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, []int64{a.ID, b.ID}, []int64{got[0].ID, got[1].ID})
	assert.Len(t, e.shadows(t), 1)
}

func TestComparisonAfterRestoringReference(t *testing.T) {
	t.Parallel()

	e := newEnv(t)
	ctx := t.Context()
	src := e.task(t, "one", true, "aaa")
	first, _, _ := e.run(t, replay.Config{Name: "first"})
	require.NoError(t, e.loop.Cancel(ctx, src.ID))
	require.NoError(t, e.loop.Restore(ctx, src.ID))
	src.Body = "A different assignment."
	require.NoError(t, engine.EditTask(ctx, e.store, src, nil))

	second, revised, _ := e.run(t, replay.Config{Name: "second"})
	original, err := e.store.ReplayItems(ctx, first.ID)
	require.NoError(t, err)
	assert.Nil(t, replay.Compare(revised, original)[0].Before)

	_, err = engine.AddImage(ctx, e.store, store.Image{TaskID: src.ID, Name: "screen.png", Type: "image/png"}, strings.NewReader("screen"))
	require.NoError(t, err)
	_, withImage, _ := e.run(t, replay.Config{Name: "third"})
	beforeImage, err := e.store.ReplayItems(ctx, second.ID)
	require.NoError(t, err)
	assert.Nil(t, replay.Compare(withImage, beforeImage)[0].Before)

	_, same, _ := e.run(t, replay.Config{Name: "fourth"})
	assert.NotNil(t, replay.Compare(same, withImage)[0].Before)

	shadow := withImage[0].TaskID
	require.NoError(t, e.loop.Cancel(ctx, shadow))
	require.ErrorIs(t, e.loop.Restore(ctx, shadow), engine.ErrRefused)
}

func TestReplayCarriesImages(t *testing.T) {
	t.Parallel()

	e := newEnv(t)
	src := e.task(t, "one", true, "aaa")

	for _, name := range []string{"first.png", "second.png"} {
		_, err := e.store.PutImage(t.Context(), store.Image{TaskID: src.ID, Name: name, Type: "image/png"}, strings.NewReader(name))
		require.NoError(t, err)
	}

	var atWork []store.Image

	e.runner.duringWork = func(int) {
		var err error

		atWork, err = e.store.TaskImages(t.Context(), e.shadows(t)[0].ID)
		require.NoError(t, err)
	}

	e.run(t, replay.Config{Name: "x"})

	shadows := e.shadows(t)
	require.Len(t, shadows, 1)

	images := func(id int64) []engine.Image {
		v, err := e.loop.View(t.Context(), id, nil)
		require.NoError(t, err)

		return v.Images
	}

	want, got := images(src.ID), images(shadows[0].ID)
	require.Len(t, got, 2)

	for i := range got {
		assert.NotEqual(t, want[i].ID, got[i].ID, "rows of the shadow's own")
		got[i].ID = want[i].ID
	}

	assert.Equal(t, want, got, "the source's images, in their order")
	assert.Len(t, atWork, 2, "the shadow has them while its agent works")
}

func TestReplayDoesNotTouchSourceTask(t *testing.T) {
	t.Parallel()

	e := newEnv(t)
	src := e.task(t, "one", true, "aaa")

	_, err := e.store.Append(t.Context(), src.ID, -1, []store.Event{{Type: engine.TaskStarted, Actor: "user"}}, nil)
	require.NoError(t, err)

	before, err := e.store.Events(t.Context(), src.ID, 0, 0)
	require.NoError(t, err)

	src, err = e.store.Task(t.Context(), src.ID)
	require.NoError(t, err)

	e.run(t, replay.Config{Name: "x", Model: "model-b"})

	after, err := e.store.Events(t.Context(), src.ID, 0, 0)
	require.NoError(t, err)
	assert.Equal(t, before, after)

	got, err := e.store.Task(t.Context(), src.ID)
	require.NoError(t, err)
	assert.Equal(t, src, got)
}

func TestReplayLeavesOutMarkedTasksOfARemovedProject(t *testing.T) {
	t.Parallel()

	e := newEnv(t)
	live := e.task(t, "live", true, "aaa")

	gone, err := e.store.CreateProject(t.Context(), store.Project{
		Name: "old", RepoURL: "https://example.com/old.git", BaseBranch: "main",
		Baseline: &store.Baseline{Commit: "tip", Passed: true},
	})
	require.NoError(t, err)

	orphan, err := e.store.CreateTask(t.Context(), store.Task{ProjectID: gone.ID, Title: "orphan", BaseCommit: "bbb", Reference: true})
	require.NoError(t, err)
	require.NoError(t, e.loop.Cancel(t.Context(), orphan.ID))
	require.NoError(t, e.loop.RemoveProject(t.Context(), gone.ID))

	_, items, _ := e.run(t, replay.Config{Name: "baseline"})
	require.Len(t, items, 1)
	assert.Equal(t, live.ID, items[0].SourceTaskID)

	t.Run("none of a live project", func(t *testing.T) {
		task, err := e.store.Task(t.Context(), live.ID)
		require.NoError(t, err)

		task.Reference = false
		require.NoError(t, e.store.UpdateTask(t.Context(), task))

		_, err = replay.Run(t.Context(), e.store, e.loop, replay.Config{Name: "x"}, e.drive, &bytes.Buffer{})
		require.ErrorIs(t, err, replay.ErrNoReferenceTasks)
	})
}

func TestReplayEmptySet(t *testing.T) {
	t.Parallel()

	e := newEnv(t)
	e.task(t, "plain", false, "aaa")
	e.task(t, "never prepared", true, "")

	var out bytes.Buffer

	_, err := replay.Run(t.Context(), e.store, e.loop, replay.Config{Name: "x"}, e.drive, &out)
	require.ErrorIs(t, err, replay.ErrNoReferenceTasks)

	_, ok, err := e.store.PreviousReplay(t.Context(), 1<<40)
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Empty(t, e.shadows(t))
}

// failingStore makes reading one task's events fail, which RunTask returns as an error.
type failingStore struct {
	*store.Store

	taskID int64
}

func (f failingStore) Events(ctx context.Context, taskID, afterID int64, limit int) ([]store.Event, error) {
	if taskID == f.taskID {
		return nil, errors.New("disk gone")
	}

	return f.Store.Events(ctx, taskID, afterID, limit)
}

func TestReplayRunErrorEndsTheReplay(t *testing.T) {
	t.Parallel()

	e := newEnv(t)
	first := e.task(t, "one", true, "aaa")
	e.task(t, "two", true, "bbb")
	e.task(t, "three", true, "ccc")

	// Shadow ids follow the three sources in order.
	e.loop.Store = failingStore{Store: e.store, taskID: 5}

	var out bytes.Buffer

	r, err := replay.Run(t.Context(), e.store, e.loop, replay.Config{Name: "x"}, e.drive, &out)
	require.Error(t, err)
	assert.NotZero(t, r.ID)

	items, err := e.store.ReplayItems(t.Context(), r.ID)
	require.NoError(t, err)
	require.Len(t, items, 1)
	assert.Equal(t, first.ID, items[0].SourceTaskID)
	assert.Len(t, e.shadows(t), 2, "the failed shadow stays, the third source never starts")
	assert.Equal(t, 1, strings.Count(out.String(), "\n"))
	assert.Nil(t, r.EndedAt)

	_, ok, err := e.store.PreviousReplay(t.Context(), r.ID+1)
	require.NoError(t, err)
	assert.False(t, ok, "a cut-off replay is not a comparison base")
}

// events reads the task's events.
func (e *env) events(t *testing.T, taskID int64) []store.Event {
	t.Helper()

	evs, err := e.store.Events(t.Context(), taskID, 0, 0)
	require.NoError(t, err)

	return evs
}

func TestReplayDrivesItsShadows(t *testing.T) {
	t.Parallel()

	e := newEnv(t)
	e.task(t, "one", true, "aaa")
	e.task(t, "two", true, "bbb")
	e.runner.verifies = []engine.VerifyResult{{Result: engine.ResultFailed, Command: "make test"}}

	var driven []int64

	r, err := replay.Run(t.Context(), e.store, e.loop, replay.Config{Name: "x", VerifyFix: new(false)},
		func(ctx context.Context, id int64) (engine.Status, error) {
			driven = append(driven, id)

			return e.drive(ctx, id)
		}, io.Discard)
	require.NoError(t, err)
	require.NotNil(t, r.EndedAt)

	shadows := e.shadows(t)
	require.Len(t, shadows, 2)
	assert.Equal(t, []int64{shadows[0].ID, shadows[0].ID, shadows[1].ID}, driven,
		"each shadow is driven once it is started, and a stopped one again once it is cancelled")

	for _, sh := range shadows {
		assert.Equal(t, engine.TaskStarted, e.events(t, sh.ID)[0].Type, "a shadow starts as any task does")
	}
}

func TestInterruptedReplayCancelsItsShadows(t *testing.T) {
	t.Parallel()

	e := newEnv(t)
	e.task(t, "one", true, "aaa")
	e.task(t, "two", true, "bbb")

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	e.runner.duringWork = func(n int) {
		if n == 2 {
			cancel()
		}
	}

	r, err := replay.Run(ctx, e.store, e.loop, replay.Config{Name: "x"}, e.drive, io.Discard)
	require.ErrorIs(t, err, context.Canceled)
	assert.Nil(t, r.EndedAt)

	_, ok, err := e.store.PreviousReplay(t.Context(), r.ID+1)
	require.NoError(t, err)
	assert.False(t, ok, "an interrupted replay is not a comparison base")

	shadows := e.shadows(t)
	require.Len(t, shadows, 2)

	for i, want := range []engine.Status{engine.StatusInReview, engine.StatusCancelled} {
		evs := e.events(t, shadows[i].ID)
		assert.Equal(t, want, engine.StatusOf(engine.Reduce(evs)))
		assert.Equal(t, i == 1, slices.ContainsFunc(evs, func(ev store.Event) bool { return ev.Type == engine.HumanCancelled }),
			"only the shadow the interrupt cut is cancelled")
	}
}

func TestReplayRefusedWithoutBaseline(t *testing.T) {
	t.Parallel()

	e := newEnv(t)
	e.task(t, "one", true, "aaa")

	e.project.Baseline = nil
	require.NoError(t, e.store.UpdateProject(t.Context(), e.project))

	r, err := replay.Run(t.Context(), e.store, e.loop, replay.Config{Name: "x"}, e.drive, io.Discard)
	require.ErrorIs(t, err, engine.ErrNoBaseline)
	assert.Nil(t, r.EndedAt)
	assert.Empty(t, e.runner.configs, "no run starts")

	for _, sh := range e.shadows(t) {
		assert.Empty(t, e.events(t, sh.ID))
	}
}

func TestShadowCancelledFromOutsideEndsTheReplay(t *testing.T) {
	t.Parallel()

	e := newEnv(t)
	e.task(t, "one", true, "aaa")
	e.task(t, "two", true, "bbb")

	// As a person, or another replay, cancels the shadow while it is driven.
	drive := func(ctx context.Context, id int64) (engine.Status, error) {
		require.NoError(t, e.loop.Cancel(ctx, id))

		return e.drive(ctx, id)
	}

	r, err := replay.Run(t.Context(), e.store, e.loop, replay.Config{Name: "x"}, drive, io.Discard)
	require.Error(t, err)
	require.NotErrorIs(t, err, engine.ErrRefused, "the replay cancels nothing itself")
	assert.Nil(t, r.EndedAt)

	_, ok, err := e.store.PreviousReplay(t.Context(), r.ID+1)
	require.NoError(t, err)
	assert.False(t, ok, "a replay with a shadow cancelled from outside is not a comparison base")

	items, err := e.store.ReplayItems(t.Context(), r.ID)
	require.NoError(t, err)
	assert.Empty(t, items)
	assert.Len(t, e.shadows(t), 1, "the second source never starts")
}
