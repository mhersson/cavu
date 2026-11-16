package replay_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
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
	mu       sync.Mutex
	configs  []engine.RunConfig
	modes    []string
	works    []engine.WorkResult
	verifies []engine.VerifyResult
}

func (r *fakeRunner) Prepare(_ context.Context, c engine.RunConfig) (engine.PrepareResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.configs = append(r.configs, c)

	return engine.PrepareResult{BaseCommit: c.Repo.BaseCommit, Baseline: "passed"}, nil
}

func (r *fakeRunner) Work(_ context.Context, _ engine.RunConfig, w engine.WorkRequest, _ func(engine.Progress)) (engine.WorkResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.modes = append(r.modes, w.Mode)

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

func (r *fakeRunner) Export(_ context.Context, c engine.RunConfig, _ string) (engine.ExportResult, error) {
	return engine.ExportResult{HeadCommit: "head", DiffPath: fmt.Sprintf("diff-%d.patch", c.TaskID), FilesChanged: 1}, nil
}

func (r *fakeRunner) Release(context.Context, engine.RunRef, bool) error { return nil }

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

	p, err := s.CreateProject(ctx, store.Project{
		Name: "web", RepoURL: "https://example.com/web.git", BaseBranch: "main",
		SetupCommand: "make setup", VerifyCommand: "make test",
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

func (e *env) run(t *testing.T, c replay.Config) (store.Replay, []store.ReplayItem, string) {
	t.Helper()

	var out bytes.Buffer

	r, err := replay.Run(t.Context(), e.store, e.loop, c, &out)
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
	assert.False(t, got.Asked)
	assert.Equal(t, 4, got.Turns)
	assert.Equal(t, int64(145), got.Tokens)
	assert.InDelta(t, 0.75, got.CostEstimateUSD, 1e-9)
	assert.Equal(t, fmt.Sprintf("diff-%d.patch", sh.ID), got.DiffPath)

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

	assert.True(t, items[0].Asked)
	assert.Equal(t, "question", items[0].Stop)
	assert.Empty(t, items[0].Verify)
	assert.Empty(t, items[0].DiffPath)
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

func TestReplayEmptySet(t *testing.T) {
	t.Parallel()

	e := newEnv(t)
	e.task(t, "plain", false, "aaa")
	e.task(t, "never prepared", true, "")

	var out bytes.Buffer

	_, err := replay.Run(t.Context(), e.store, e.loop, replay.Config{Name: "x"}, &out)
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

	r, err := replay.Run(t.Context(), e.store, e.loop, replay.Config{Name: "x"}, &out)
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
