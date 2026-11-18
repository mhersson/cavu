package main

import (
	"context"
	"errors"
	"io"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mhersson/cavu/internal/engine"
	"github.com/mhersson/cavu/internal/sandbox"
	"github.com/mhersson/cavu/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// markWithBase marks the task for replay and gives it a base commit.
func markWithBase(t *testing.T, dir, id string) {
	t.Helper()

	cavuIn(t, dir, "task", "mark", id)

	a, err := openApp(t.Context(), dir)
	require.NoError(t, err)

	defer a.close()

	n, err := strconv.ParseInt(id, 10, 64)
	require.NoError(t, err)

	task, err := a.store.Task(t.Context(), n)
	require.NoError(t, err)

	task.BaseCommit = "base123"
	require.NoError(t, a.store.UpdateTask(t.Context(), task))
}

// storedReplays returns every replay with its items, oldest first.
func storedReplays(t *testing.T, dir string) (replays []store.Replay, items map[int64][]store.ReplayItem) {
	t.Helper()

	a, err := openApp(t.Context(), dir)
	require.NoError(t, err)

	defer a.close()

	items = map[int64][]store.ReplayItem{}

	for id := int64(1); ; id++ {
		r, ok, err := a.store.PreviousReplay(t.Context(), id+1)
		require.NoError(t, err)

		if !ok || r.ID != id {
			return replays, items
		}

		replays = append(replays, r)
		items[id], err = a.store.ReplayItems(t.Context(), id)
		require.NoError(t, err)
	}
}

func TestReplayVerbEmptySet(t *testing.T) {
	dir, _, fake := runSetup(t)

	code, stdout, stderr := runCavu("replay", "--name", "first", "--data-dir", dir)

	assert.Equal(t, 0, code, stderr)
	assert.Contains(t, stdout, "cavu task mark")
	assert.Empty(t, fake.runner.actions, "no container starts")

	replays, _ := storedReplays(t, dir)
	assert.Empty(t, replays)
}

func TestReplayVerbRequiresName(t *testing.T) {
	dir, _, fake := runSetup(t)

	code, _, _ := runCavu("replay", "--data-dir", dir)

	assert.Equal(t, 2, code)
	assert.Empty(t, fake.removals)
}

func TestReplayVerbPrintsOneRowPerSource(t *testing.T) {
	dir, a, fake := runSetup(t)
	b := addTask(t, dir, "Fix the logout")
	markWithBase(t, dir, a)
	markWithBase(t, dir, b)

	// Header, then one line per source task; the previous columns are the
	// ones that hold the first replay's values.
	table := func(stdout string) [][]string {
		var rows [][]string

		for l := range strings.SplitSeq(strings.TrimSpace(stdout), "\n") {
			if f := strings.Fields(l); len(f) > 0 && (f[0] == a || f[0] == b) {
				rows = append(rows, f)
			}
		}

		return rows
	}

	code, stdout, stderr := runCavu("replay", "--name", "first", "--data-dir", dir)
	require.Equal(t, 0, code, stderr)
	assert.Len(t, fake.removals, 1)

	_, items := storedReplays(t, dir)
	require.Len(t, items[1], 2)

	first := table(stdout)
	require.Len(t, first, 2)

	code, stdout, stderr = runCavu("replay", "--name", "second", "--data-dir", dir)
	require.Equal(t, 0, code, stderr)

	second := table(stdout)
	require.Len(t, second, 2)
	assert.Greater(t, len(second[0]), len(first[0]), "the previous replay's values fill the columns beside this one's")

	_, items = storedReplays(t, dir)
	assert.Equal(t, items[2][0].DiffPath, second[0][len(second[0])-1], "the diff path is this replay's")
}

func TestReplayTablePrintsTheQuestionsAsked(t *testing.T) {
	dir, id, _ := runSetup(t)

	a, err := openApp(t.Context(), dir)
	require.NoError(t, err)

	defer a.close()

	task, err := strconv.ParseInt(id, 10, 64)
	require.NoError(t, err)

	var r store.Replay

	for _, asked := range []int{2, 3} {
		r, err = a.store.CreateReplay(t.Context(), store.Replay{Name: "r", Config: map[string]string{}})
		require.NoError(t, err)
		require.NoError(t, a.store.PutReplayItem(t.Context(), store.ReplayItem{ReplayID: r.ID, SourceTaskID: task, TaskID: task, Asked: asked}))
		require.NoError(t, a.store.EndReplay(t.Context(), r.ID, time.Now()))
	}

	var out strings.Builder
	require.NoError(t, printReplay(t.Context(), a.store, r, &out))

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	require.Len(t, lines, 2, out.String())

	// The columns are aligned, so a cell starts where its heading does.
	col := strings.Index(lines[0], "ASKED")
	require.GreaterOrEqual(t, col, 0, out.String())
	assert.Equal(t, []string{"3", "2"}, strings.Fields(lines[1][col:])[:2], "this replay's value, then the previous one's")
}

func TestReplayVerbFlagsReachConfig(t *testing.T) {
	dir, id, fake := runSetup(t)
	markWithBase(t, dir, id)

	code, _, stderr := runCavu("replay", "--name", "tuned", "--model", "model-b", "--effort", "high",
		"--no-continuations", "--no-verify-fix", "--no-readiness", "--web-tools", "--image", "cavu-sandbox:other", "--data-dir", dir)
	require.Equal(t, 0, code, stderr)

	replays, _ := storedReplays(t, dir)
	require.Len(t, replays, 1)
	assert.Equal(t, "tuned", replays[0].Name)
	assert.Equal(t, "cavu-sandbox:other", replays[0].Image)
	assert.Equal(t, map[string]string{
		"model_work": "model-b", "effort_work": "high", "continuations": "false", "verify_fix": "false", "web_tools": "true",
		"readiness": "false",
	}, replays[0].Config)
	assert.Empty(t, fake.image, "an explicit image is not checked")
	require.NotEmpty(t, fake.runner.releases)

	for _, r := range fake.runner.releases {
		assert.Equal(t, "cavu-sandbox:other", r.ref.Image, "the runs drive on the image named")
	}
}

func TestReplayNoReview(t *testing.T) {
	for _, tt := range []struct {
		name    string
		args    []string
		config  map[string]string
		reviews int
	}{
		{name: "the default", reviews: 1, config: map[string]string{}},
		{name: "--no-review", args: []string{"--no-review"}, config: map[string]string{"review": "false"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir, id, fake := runSetup(t)
			markWithBase(t, dir, id)

			code, _, stderr := runCavu(append([]string{"replay", "--name", "x", "--data-dir", dir}, tt.args...)...)
			require.Equal(t, 0, code, stderr)

			replays, _ := storedReplays(t, dir)
			require.Len(t, replays, 1)
			assert.Equal(t, tt.config, replays[0].Config)
			assert.Equal(t, tt.reviews, countOf(fake.runner.actions, engine.KindReview))
		})
	}
}

func countOf[T comparable](all []T, v T) (n int) {
	for _, x := range all {
		if x == v {
			n++
		}
	}

	return n
}

func TestReplayVerbChecksTheDefaultImage(t *testing.T) {
	dir, id, fake := runSetup(t)
	markWithBase(t, dir, id)

	fake.labels.CavuVersion = "0.0.1"

	code, _, stderr := runCavu("replay", "--name", "first", "--data-dir", dir)

	assert.Equal(t, 1, code)
	assert.Contains(t, stderr, "cavu image build")
	assert.Equal(t, sandbox.ImageName(version), fake.image)
	assert.Empty(t, fake.runner.actions)

	replays, _ := storedReplays(t, dir)
	assert.Empty(t, replays)
}

func TestReplayVerbInterruptReleasesTheShadowAndExits130(t *testing.T) {
	dir, id, fake := runSetup(t)
	markWithBase(t, dir, id)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	fake.runner.duringWork = func(context.Context) { cancel() }

	err := runReplay(ctx, []string{"--name", "cut", "--data-dir", dir}, io.Discard, io.Discard)

	ee, ok := errors.AsType[exitError](err)
	require.True(t, ok, err)
	assert.Equal(t, 130, ee.code)

	replays, _ := storedReplays(t, dir)
	assert.Empty(t, replays, "the cut replay is never ended")

	a, err := openApp(t.Context(), dir)
	require.NoError(t, err)

	defer a.close()

	all, err := a.store.Tasks(t.Context(), true)
	require.NoError(t, err)

	var shadow store.Task

	for _, task := range all {
		if task.ReplayID != 0 {
			shadow = task
		}
	}

	require.NotZero(t, shadow.ID)

	run, ok, err := a.store.OpenRun(t.Context(), shadow.ID)
	require.NoError(t, err)
	require.True(t, ok)

	assert.Equal(t, []release{{
		ref: engine.RunRef{
			InstanceID: storedSettings(t, dir)["instance_id"], Image: sandbox.ImageName(version),
			TaskID: shadow.ID, RunID: run.ID, ProjectID: shadow.ProjectID,
		},
		remove: engine.KeepVolumes,
	}}, fake.runner.releases)
}

func TestReplayRefusesImageUnderAServer(t *testing.T) {
	dir, id, fake := runSetup(t)
	markWithBase(t, dir, id)
	holdLock(t, dir, "serve")
	refuseRuntime(t)

	// Bounded, so a verb that waits for the server instead fails the test.
	ctx, cancel := context.WithTimeout(t.Context(), within)
	defer cancel()

	var stdout strings.Builder

	err := runReplay(ctx, []string{"--name", "other", "--image", "cavu-sandbox:other", "--data-dir", dir}, &stdout, io.Discard)

	require.Error(t, err)
	require.NoError(t, ctx.Err(), "refused, not cut short")
	assert.Empty(t, stdout.String())
	assert.Empty(t, fake.runner.actions)

	replays, _ := storedReplays(t, dir)
	assert.Empty(t, replays)

	a, err := openApp(t.Context(), dir)
	require.NoError(t, err)

	defer a.close()

	all, err := a.store.Tasks(t.Context(), true)
	require.NoError(t, err)
	assert.Len(t, all, 1, "no shadow is added")
}

// shadowOf adds a shadow of the task to an earlier replay, started and run on
// fake's runner until it rests.
func shadowOf(t *testing.T, dir string, fake *fakeRuntime, replayID int64, id string) store.Task {
	t.Helper()

	a, err := openApp(t.Context(), dir)
	require.NoError(t, err)

	defer a.close()

	src := storedTask(t, dir, id)
	sh, err := a.store.CreateTask(t.Context(), store.Task{
		ProjectID: src.ProjectID, Title: src.Title, BaseCommit: src.BaseCommit, ReplayID: replayID,
	})
	require.NoError(t, err)

	l := a.loop(fake)
	require.NoError(t, l.Start(t.Context(), sh.ID))
	_, err = l.RunTask(t.Context(), sh.ID)
	require.NoError(t, err)

	return sh
}

func TestNewReplayCancelsOldUnfinishedShadows(t *testing.T) {
	dir, id, fake := runSetup(t)
	markWithBase(t, dir, id)

	a, err := openApp(t.Context(), dir)
	require.NoError(t, err)

	old, err := a.store.CreateReplay(t.Context(), store.Replay{Name: "cut", Image: sandbox.ImageName(version)})
	require.NoError(t, err)
	a.close()

	finished := shadowOf(t, dir, fake, old.ID, id)

	fake.runner.work = engine.WorkResult{Outcome: engine.OutcomeBlocked, Questions: []string{"Which API version?"}}
	stopped := shadowOf(t, dir, fake, old.ID, id)
	fake.runner.work = engine.WorkResult{Outcome: engine.OutcomeDone, Changed: true, HeadCommit: "head456"}

	a, err = openApp(t.Context(), dir)
	require.NoError(t, err)

	run, ok, err := a.store.OpenRun(t.Context(), stopped.ID)
	require.NoError(t, err)
	require.True(t, ok, "the stopped shadow keeps its run")
	a.close()

	fake.runner.releases = nil

	cavuIn(t, dir, "replay", "--name", "new")

	statusOf := func(task store.Task) engine.Status {
		return engine.StatusOf(engine.Reduce(storedEvents(t, dir, strconv.FormatInt(task.ID, 10))))
	}

	assert.Equal(t, engine.StatusInReview, statusOf(finished), "a finished shadow is left as it is")
	assert.Equal(t, engine.StatusCancelled, statusOf(stopped))
	assert.Contains(t, fake.runner.releases, release{
		ref: engine.RunRef{
			InstanceID: storedSettings(t, dir)["instance_id"], Image: sandbox.ImageName(version),
			TaskID: stopped.ID, RunID: run.ID, ProjectID: stopped.ProjectID,
		},
		remove: engine.RemoveAll,
	}, "the replay's own scheduler ends the cancelled shadow's run")

	a, err = openApp(t.Context(), dir)
	require.NoError(t, err)

	defer a.close()

	r, ok, err := a.store.PreviousReplay(t.Context(), 1<<40)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "new", r.Name, "the new replay ends")
}

func TestReplayPrintsAWaitOnce(t *testing.T) {
	for _, own := range []bool{true, false} {
		t.Run(map[bool]string{true: "its own usage limit", false: "another task's usage limit"}[own], func(t *testing.T) {
			dir, id, fake := runSetup(t)
			markWithBase(t, dir, id)

			reset := time.Now().Add(time.Hour)
			other := addTask(t, dir, "Other")

			if own {
				fake.runner.work = engine.WorkResult{Outcome: engine.OutcomeUsageLimit, ResetsAt: reset}
			} else {
				seedEvents(t, dir, other, nil, event(engine.TaskStarted, "user", struct{}{}), event(engine.Stopped, "engine",
					engine.StoppedPayload{Reason: engine.StopUsageLimit, Explanation: "limit", Action: engine.ClearNone, ResetsAt: reset}))
			}

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			out, done := &syncBuffer{}, make(chan error, 1)

			go func() { done <- runReplay(ctx, []string{"--name", "limited", "--data-dir", dir}, out, io.Discard) }()

			require.Eventually(t, func() bool { return len(waitReasons(out.String())) > 0 }, within, time.Millisecond)
			assert.Equal(t, !own, names(waitReasons(out.String())[0], other), "a wait another task holds names it")

			cancel()

			ee, ok := errors.AsType[exitError](<-done)
			require.True(t, ok)
			assert.Equal(t, 130, ee.code)
			assert.Len(t, waitReasons(out.String()), 1, out.String())
		})
	}
}

// countedStore counts the snapshots a loop takes through it: one LastEventID each.
type countedStore struct {
	*store.Store

	snapshots atomic.Int64
}

func (s *countedStore) LastEventID(ctx context.Context, taskID int64) (int64, error) {
	s.snapshots.Add(1)

	return s.Store.LastEventID(ctx, taskID)
}

func TestRestSaysAWaitOnce(t *testing.T) {
	dir, id, _ := runSetup(t)
	seedEvents(t, dir, id, nil, event(engine.TaskStarted, "user", struct{}{}), event(engine.Stopped, "engine",
		engine.StoppedPayload{Reason: engine.StopUsageLimit, Explanation: "limit", Action: engine.ClearNone, ResetsAt: time.Now().Add(time.Hour)}))

	a, err := openApp(t.Context(), dir)
	require.NoError(t, err)

	defer a.close()

	counted := &countedStore{Store: a.store}
	l := a.actions()
	l.Store = counted

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	out, done, n := &syncBuffer{}, make(chan error, 1), taskID(t, id)

	go func() {
		_, err := rest(ctx, l, n, out)
		done <- err
	}()

	require.Eventually(t, func() bool { return counted.snapshots.Load() > 20 }, within, time.Millisecond, "many polls")
	cancel()

	require.ErrorIs(t, <-done, context.Canceled)
	assert.Len(t, waitReasons(out.String()), 1, out.String())
}
