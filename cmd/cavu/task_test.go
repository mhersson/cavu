package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mhersson/cavu/internal/engine"
	"github.com/mhersson/cavu/internal/sandbox"
	"github.com/mhersson/cavu/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// withStdin runs cavu with text as standard input.
func withStdin(text string, args ...string) (code int, stdout, stderr string) {
	var out, errOut strings.Builder

	code = run(args, strings.NewReader(text), &out, &errOut)

	return code, out.String(), errOut.String()
}

// webData returns a data directory with the project web, which can run tasks.
func webData(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	cavuIn(t, dir, "project", "add", "web", "--repo", repo, "--verify", "make test")
	storeBaseline(t, dir, "web")

	return dir
}

// addTask adds a task titled title to web and returns its id.
func addTask(t *testing.T, dir, title string) string {
	t.Helper()

	return strings.TrimSpace(cavuIn(t, dir, "task", "add", "web", "--title", title))
}

func storedTask(t *testing.T, dir, id string) store.Task {
	t.Helper()

	a, err := openApp(t.Context(), dir)
	require.NoError(t, err)

	defer a.close()

	n, err := strconv.ParseInt(id, 10, 64)
	require.NoError(t, err)

	task, err := a.store.Task(t.Context(), n)
	require.NoError(t, err)

	return task
}

func storedEvents(t *testing.T, dir, id string) []store.Event {
	t.Helper()

	a, err := openApp(t.Context(), dir)
	require.NoError(t, err)

	defer a.close()

	n, err := strconv.ParseInt(id, 10, 64)
	require.NoError(t, err)

	es, err := a.store.Events(t.Context(), n, 0, 0)
	require.NoError(t, err)

	return es
}

// seedEvents appends events to the task as the engine's actors would.
func seedEvents(t *testing.T, dir, id string, usage []store.Usage, events ...store.Event) {
	t.Helper()

	a, err := openApp(t.Context(), dir)
	require.NoError(t, err)

	defer a.close()

	n, err := strconv.ParseInt(id, 10, 64)
	require.NoError(t, err)

	_, err = a.store.Append(t.Context(), n, -1, events, usage)
	require.NoError(t, err)
}

func event(typ, actor string, payload any) store.Event {
	raw, _ := json.Marshal(payload)

	return store.Event{Type: typ, Actor: actor, Payload: raw}
}

func question() store.Event {
	return event(engine.Stopped, "engine", engine.StoppedPayload{Reason: engine.StopQuestion, Explanation: "which API", Action: engine.ClearAnswer})
}

func TestTaskAddReadsBodyFromStdin(t *testing.T) {
	dir := webData(t)

	code, stdout, stderr := withStdin("It fails on Safari.\n", "task", "add", "web", "--title", "Fix the login", "--data-dir", dir)

	require.Equal(t, 0, code, stderr)

	task := storedTask(t, dir, strings.TrimSpace(stdout))
	assert.Equal(t, "Fix the login", task.Title)
	assert.Equal(t, "It fails on Safari.\n", task.Body)
	assert.Empty(t, storedEvents(t, dir, strings.TrimSpace(stdout)), "a draft has no events")

	file := filepath.Join(t.TempDir(), "body.md")
	require.NoError(t, os.WriteFile(file, []byte("from a file"), 0o600))

	code, stdout, stderr = withStdin("ignored", "task", "add", "web", "--title", "Second", "--body-file", file, "--data-dir", dir)
	require.Equal(t, 0, code, stderr)
	assert.Equal(t, "from a file", storedTask(t, dir, strings.TrimSpace(stdout)).Body, "the file wins over standard input")
}

func TestTaskAddHasNoBodyOnATerminal(t *testing.T) {
	dir := webData(t)

	tty, err := os.Open("/dev/null") // a character device, as a terminal is
	require.NoError(t, err)

	defer tty.Close()

	var out strings.Builder

	require.Equal(t, 0, run([]string{"task", "add", "web", "--title", "Fix", "--data-dir", dir}, tty, &out, io.Discard))
	assert.Empty(t, storedTask(t, dir, strings.TrimSpace(out.String())).Body)
}

func TestTaskAddRequiresTitle(t *testing.T) {
	dir := webData(t)

	for _, args := range [][]string{{"task", "add", "web"}, {"task", "add", "web", "--title", " "}} {
		code, stdout, _ := runCavu(append(args, "--data-dir", dir)...)

		assert.Equal(t, 1, code)
		assert.Empty(t, stdout)
	}

	code, _, stderr := runCavu("task", "add", "nope", "--title", "x", "--data-dir", dir)
	assert.Equal(t, 1, code)
	assert.Contains(t, stderr, strconv.Quote("nope"))

	a, err := openApp(t.Context(), dir)
	require.NoError(t, err)

	defer a.close()

	tasks, err := a.store.Tasks(t.Context(), true)
	require.NoError(t, err)
	assert.Empty(t, tasks)
}

func TestTaskTitleLengthLimit(t *testing.T) {
	dir := webData(t)

	code, stdout, stderr := runCavu("task", "add", "web", "--title", strings.Repeat("x", 201), "--data-dir", dir)

	assert.Equal(t, 1, code)
	assert.Empty(t, stdout)
	assert.Contains(t, stderr, "title")

	a, err := openApp(t.Context(), dir)
	require.NoError(t, err)

	defer a.close()

	tasks, err := a.store.Tasks(t.Context(), true)
	require.NoError(t, err)
	assert.Empty(t, tasks, "nothing is stored")

	cavuIn(t, dir, "task", "add", "web", "--title", strings.Repeat("x", 200))
}

func TestAnswerRetryCancelGuards(t *testing.T) {
	tests := []struct {
		name   string
		verb   string
		events []store.Event // before the verb
	}{
		{"answer with no stop", "answer", nil},
		{"answer to a stop that is no question", "answer", []store.Event{event(engine.Stopped, "engine", engine.StoppedPayload{Reason: engine.StopVerifyFailed})}},
		{"retry with no stop", "retry", nil},
		{"retry on a cancelled task", "retry", []store.Event{event(engine.HumanCancelled, "user", struct{}{})}},
		{"cancel when cancelled", "cancel", []store.Event{event(engine.HumanCancelled, "user", struct{}{})}},
		{"cancel in review", "cancel", []store.Event{event(engine.Exported, "runner", engine.ExportedPayload{})}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := webData(t)
			id := addTask(t, dir, "Fix")
			seedEvents(t, dir, id, nil, append([]store.Event{event(engine.TaskStarted, "user", struct{}{})}, tc.events...)...)

			before := storedEvents(t, dir, id)

			code, stdout, _ := withStdin("some text", "task", tc.verb, id, "--data-dir", dir)

			assert.Equal(t, 1, code)
			assert.Empty(t, stdout)
			assert.Equal(t, before, storedEvents(t, dir, id), "no event is appended")
		})
	}

	t.Run("a blank answer", func(t *testing.T) {
		dir := webData(t)
		id := addTask(t, dir, "Fix")
		seedEvents(t, dir, id, nil, event(engine.TaskStarted, "user", struct{}{}), question())
		before := storedEvents(t, dir, id)

		code, _, _ := withStdin(" \n", "task", "answer", id, "--data-dir", dir)

		assert.Equal(t, 1, code)
		assert.Equal(t, before, storedEvents(t, dir, id))
	})

	t.Run("an unknown task", func(t *testing.T) {
		code, _, _ := runCavu("task", "cancel", "99", "--data-dir", webData(t))

		assert.Equal(t, 1, code)
	})
}

func TestAnswerAndRetryAreRecorded(t *testing.T) {
	dir := webData(t)
	id := addTask(t, dir, "Fix")
	seedEvents(t, dir, id, nil, event(engine.TaskStarted, "user", struct{}{}), question())

	code, _, stderr := withStdin("Use the v2 API.\n", "task", "answer", id, "--data-dir", dir)
	require.Equal(t, 0, code, stderr)

	es := storedEvents(t, dir, id)
	last := es[len(es)-1]
	assert.Equal(t, engine.HumanAnswered, last.Type)
	assert.Equal(t, "user", last.Actor)
	assert.JSONEq(t, `{"text":"Use the v2 API."}`, string(last.Payload))

	seedEvents(t, dir, id, nil, event(engine.Stopped, "engine", engine.StoppedPayload{Reason: engine.StopVerifyFailed}))
	cavuIn(t, dir, "task", "retry", id)

	es = storedEvents(t, dir, id)
	assert.Equal(t, engine.HumanRetried, es[len(es)-1].Type)
}

func TestAnswerOnATerminalReadsToTheEnd(t *testing.T) {
	dir := webData(t)
	id := addTask(t, dir, "Fix")
	seedEvents(t, dir, id, nil, event(engine.TaskStarted, "user", struct{}{}), question())

	tty, err := os.Open("/dev/null")
	require.NoError(t, err)

	defer tty.Close()

	var errOut strings.Builder

	// /dev/null ends the input at once, so the answer is blank and refused after the hint.
	code := run([]string{"task", "answer", id, "--data-dir", dir}, tty, io.Discard, &errOut)

	assert.Equal(t, 1, code)
	assert.NotEmpty(t, errOut.String())
}

func TestHumanEventsAreRecorded(t *testing.T) {
	dir := webData(t)
	id := addTask(t, dir, "Fix")

	cavuIn(t, dir, "task", "cancel", id)

	out := cavuIn(t, dir, "task", "show", id, "--events")

	var found bool

	for line := range strings.SplitSeq(out, "\n") {
		f := strings.Fields(line)
		if len(f) > 3 && slices.Contains(f, engine.HumanCancelled) && slices.Contains(f, "user") {
			found = true
		}
	}

	assert.True(t, found, out)

	// The status reads from the record.
	assert.Contains(t, cavuIn(t, dir, "task", "show", id), string(engine.StatusCancelled))
}

func TestTaskShowLabelsCostAsEstimate(t *testing.T) {
	dir := webData(t)
	id := addTask(t, dir, "Fix the login")

	seedEvents(t, dir, id, []store.Usage{
		{Source: "proxy", Model: "model-a", Input: 10, Output: 5, CostEstimateUSD: 0.5},
		{Source: "agent", Model: "model-a", Input: 1, Output: 2, CostEstimateUSD: 0.25},
		{Source: "agent", Model: "model-b", Input: 3, Output: 4, CostEstimateUSD: 0.25},
	}, event(engine.TaskStarted, "user", struct{}{}),
		event(engine.Stopped, "engine", engine.StoppedPayload{Reason: engine.StopNoChange, Explanation: "nothing changed", Action: engine.ClearRetry}))

	out := cavuIn(t, dir, "task", "show", id)

	var usage, cost []string

	for line := range strings.SplitSeq(out, "\n") {
		switch {
		case strings.HasPrefix(line, "usage"):
			usage = append(usage, line)
		case strings.HasPrefix(line, "cost"):
			cost = append(cost, line)
		}
	}

	assert.Len(t, usage, 3, "a row per source and model")
	require.Len(t, cost, 1)
	assert.Contains(t, cost[0], "estimate")
	assert.Contains(t, cost[0], "1.00")

	for _, want := range []string{"Fix the login", string(engine.StatusBlocked), engine.StopNoChange, "nothing changed", engine.ClearRetry} {
		assert.Contains(t, out, want)
	}
}

func TestTaskShowPrintsTheNewestExport(t *testing.T) {
	dir := webData(t)
	id := addTask(t, dir, "Fix")

	seedEvents(t, dir, id, nil, event(engine.TaskStarted, "user", struct{}{}),
		event(engine.Exported, "runner", engine.ExportedPayload{DiffPath: "/old/change.diff", BundlePath: "/old/change.bundle"}),
		event(engine.Exported, "runner", engine.ExportedPayload{DiffPath: "/new/change.diff", BundlePath: "/new/change.bundle"}))

	out := cavuIn(t, dir, "task", "show", id)

	assert.Contains(t, out, "/new/change.diff")
	assert.Contains(t, out, "/new/change.bundle")
	assert.NotContains(t, out, "/old/")
	assert.Contains(t, out, string(engine.StatusInReview))
}

func TestTaskDiffPrintsTheNewestExport(t *testing.T) {
	dir := webData(t)
	id := addTask(t, dir, "Fix")

	code, stdout, _ := runCavu("task", "diff", id, "--data-dir", dir)
	assert.Equal(t, 1, code, "no export yet")
	assert.Empty(t, stdout)

	art := t.TempDir()
	oldDiff, newDiff := filepath.Join(art, "old.diff"), filepath.Join(art, "new.diff")
	require.NoError(t, os.WriteFile(oldDiff, []byte("old\n"), 0o600))
	require.NoError(t, os.WriteFile(newDiff, []byte("diff --git a/x b/x\n"), 0o600))

	seedEvents(t, dir, id, nil, event(engine.TaskStarted, "user", struct{}{}),
		event(engine.Exported, "runner", engine.ExportedPayload{DiffPath: oldDiff}),
		event(engine.Exported, "runner", engine.ExportedPayload{DiffPath: newDiff}))

	assert.Equal(t, "diff --git a/x b/x\n", cavuIn(t, dir, "task", "diff", id))
}

func TestTaskListHidesReplayShadows(t *testing.T) {
	dir := webData(t)
	first := addTask(t, dir, "First task")
	second := addTask(t, dir, "Second task")
	cavuIn(t, dir, "task", "cancel", second)

	a, err := openApp(t.Context(), dir)
	require.NoError(t, err)

	p, err := a.store.ProjectByName(t.Context(), "web")
	require.NoError(t, err)

	_, err = a.store.CreateTask(t.Context(), store.Task{ProjectID: p.ID, Title: "Shadow task", ReplayID: 1})
	require.NoError(t, err)
	a.close()

	lines := strings.Split(strings.TrimSpace(cavuIn(t, dir, "task", "list")), "\n")

	require.Len(t, lines, 3, "a header and the two tasks")
	assert.Equal(t, []string{first, "web", string(engine.StatusDraft)}, strings.Fields(lines[1])[:3])
	assert.Contains(t, lines[1], "First task")
	assert.Equal(t, []string{second, "web", string(engine.StatusCancelled)}, strings.Fields(lines[2])[:3])
}

func TestTaskMark(t *testing.T) {
	dir := webData(t)
	id := addTask(t, dir, "Fix")

	assert.False(t, storedTask(t, dir, id).Reference)

	cavuIn(t, dir, "task", "mark", id)
	assert.True(t, storedTask(t, dir, id).Reference)

	cavuIn(t, dir, "task", "mark", id, "--unmark")
	assert.False(t, storedTask(t, dir, id).Reference)

	code, _, _ := runCavu("task", "mark", "99", "--data-dir", dir)
	assert.Equal(t, 1, code)
}

func TestTaskUsageErrors(t *testing.T) {
	dir := t.TempDir()

	tests := []struct {
		args []string
		code int
	}{
		{[]string{"task"}, 2},
		{[]string{"task", "frobnicate"}, 2},
		{[]string{"task", "show"}, 2},
		{[]string{"task", "show", "1", "extra", "--data-dir", dir}, 2},
		{[]string{"task", "show", "x", "--data-dir", dir}, 1},
	}

	for _, tc := range tests {
		code, _, _ := runCavu(tc.args...)

		assert.Equal(t, tc.code, code, tc.args)
	}
}

// runSetup is a data directory with the project web, which has a passed
// baseline, a draft task on it, and a fake runtime whose image is this
// binary's and whose runner finishes a session with a change.
func runSetup(t *testing.T) (dir, id string, fake *fakeRuntime) {
	t.Helper()

	dir, fake = checkSetup(t, "--verify", "make test")
	storeBaseline(t, dir, "web")

	fake.runner.work = engine.WorkResult{Outcome: engine.OutcomeDone, Changed: true, HeadCommit: "head456", Title: "Fix the login"}

	return dir, addTask(t, dir, "Fix the login"), fake
}

// eventLines splits task run's output into one line per stored event and
// the lines after them, and checks that each event line is that event's.
func eventLines(t *testing.T, stdout string, events []store.Event) (tail []string) {
	t.Helper()

	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	require.Greater(t, len(lines), len(events), stdout)

	for i, e := range events {
		f := strings.Fields(lines[i])
		require.GreaterOrEqual(t, len(f), 2, lines[i])
		assert.Equal(t, []string{e.At.Format(time.TimeOnly), e.Type}, f[:2], "event %d is printed as it is stored", e.ID)
	}

	return lines[len(events):]
}

// refusedRun runs the task and checks that it is refused before any container:
// exit 1, no event, no runner call and no orphan removed. It returns stderr.
func refusedRun(t *testing.T, dir, id string, fake *fakeRuntime) string {
	t.Helper()

	before := storedEvents(t, dir, id)

	code, stdout, stderr := runCavu("task", "run", id, "--data-dir", dir)

	assert.Equal(t, 1, code)
	assert.Empty(t, stdout)
	assert.Equal(t, before, storedEvents(t, dir, id), "no event is appended")
	assert.Empty(t, fake.runner.actions, "no container starts")
	assert.Empty(t, fake.runner.releases)
	assert.Empty(t, fake.removals)

	return stderr
}

func TestTaskRunPrintsEventsAndStatus(t *testing.T) {
	dir, id, fake := runSetup(t)

	code, stdout, stderr := runCavu("task", "run", id, "--data-dir", dir)

	require.Equal(t, 0, code, stderr)
	assert.Equal(t, []engine.ActionKind{engine.KindPrepare, engine.KindWork, engine.KindVerify, engine.KindExport}, fake.runner.actions)
	require.Len(t, fake.runner.releases, 1)
	assert.True(t, fake.runner.releases[0].removeVolumes, "the exported run ends")

	events := storedEvents(t, dir, id)
	assert.Equal(t, engine.TaskStarted, events[0].Type, "the draft is started")
	assert.True(t, slices.ContainsFunc(events, func(e store.Event) bool { return e.Type == engine.AgentText }),
		"progress is recorded, so it is printed with the rest")

	tail := eventLines(t, stdout, events)

	x, ok := newest[engine.ExportedPayload](events, engine.Exported)
	require.True(t, ok)
	assert.Equal(t, []string{"status:", string(engine.StatusInReview)}, strings.Fields(tail[0]))
	assert.Contains(t, strings.Join(tail, "\n"), x.DiffPath)
	assert.True(t, strings.HasPrefix(x.DiffPath, filepath.Join(dir, "artifacts")), "the artifacts are in the data directory")

	diff, err := os.ReadFile(x.DiffPath)
	require.NoError(t, err)
	assert.Equal(t, "diff --git a/login.go b/login.go\n", string(diff))
}

func TestTaskRunBlockedExitCode(t *testing.T) {
	t.Run("a question", func(t *testing.T) {
		dir, id, fake := runSetup(t)
		fake.runner.work = engine.WorkResult{Outcome: engine.OutcomeBlocked, Questions: []string{"Which API version?"}}

		code, stdout, _ := runCavu("task", "run", id, "--data-dir", dir)

		assert.Equal(t, 3, code)
		assert.Equal(t, []engine.ActionKind{engine.KindPrepare, engine.KindWork}, fake.runner.actions)
		require.Len(t, fake.runner.releases, 1)
		assert.False(t, fake.runner.releases[0].removeVolumes, "a question keeps the run")

		events := storedEvents(t, dir, id)
		tail := strings.Join(eventLines(t, stdout, events), "\n")

		s, ok := newest[engine.StoppedPayload](events, engine.Stopped)
		require.True(t, ok)
		assert.Equal(t, engine.StopQuestion, s.Reason)

		for _, v := range []string{string(engine.StatusBlocked), s.Reason, s.Explanation, s.Action} {
			assert.Contains(t, tail, v)
		}
	})

	t.Run("a cancel from another terminal", func(t *testing.T) {
		dir, id, fake := runSetup(t)
		fake.runner.duringWork = func() { cavuIn(t, dir, "task", "cancel", id) }

		code, stdout, _ := runCavu("task", "run", id, "--data-dir", dir)

		assert.Equal(t, 3, code)
		require.Len(t, fake.runner.releases, 1)
		assert.True(t, fake.runner.releases[0].removeVolumes, "a cancel ends the run")
		// The other terminal appended the cancel, so this run prints every event but that one.
		mine := slices.DeleteFunc(storedEvents(t, dir, id), func(e store.Event) bool { return e.Type == engine.HumanCancelled })
		assert.Contains(t, strings.Join(eventLines(t, stdout, mine), "\n"), string(engine.StatusCancelled))
	})
}

func TestTaskRunRefusesWithoutBaseline(t *testing.T) {
	dir, fake := checkSetup(t, "--verify", "make test")
	id := addTask(t, dir, "Fix the login")

	stderr := refusedRun(t, dir, id, fake)

	assert.Contains(t, stderr, "cavu project check web", "the verb that stores a baseline")
	assert.Contains(t, stderr, "cavu project set web --no-verify", "the verb that waives verify")
}

func TestTaskRunRefusesACancelledTask(t *testing.T) {
	dir, id, fake := runSetup(t)
	cavuIn(t, dir, "task", "cancel", id)

	refusedRun(t, dir, id, fake)
}

func TestTaskRunRefusesAStaleImage(t *testing.T) {
	dir, id, fake := runSetup(t)
	fake.labels.CavuVersion = "0.0.1"

	assert.Contains(t, refusedRun(t, dir, id, fake), "cavu image build")
	assert.Equal(t, sandbox.ImageName(version), fake.image)
}

func TestSecondTaskRunIsRefusedByLock(t *testing.T) {
	dir, id, fake := runSetup(t)

	a, err := openApp(t.Context(), dir)
	require.NoError(t, err)
	require.NoError(t, a.lock())
	t.Cleanup(a.close)

	assert.Contains(t, refusedRun(t, dir, id, fake), filepath.Join(dir, "lock"))
}

func TestTaskRunRemovesOrphansKeepingOpenRuns(t *testing.T) {
	dir, id, fake := runSetup(t)
	other := addTask(t, dir, "Another task")

	a, err := openApp(t.Context(), dir)
	require.NoError(t, err)

	n, err := strconv.ParseInt(other, 10, 64)
	require.NoError(t, err)

	open, err := a.store.CreateRun(t.Context(), store.Run{TaskID: n, Reason: "initial"})
	require.NoError(t, err)

	ended, err := a.store.CreateRun(t.Context(), store.Run{TaskID: n, Reason: "retry"})
	require.NoError(t, err)

	at := time.Now()
	ended.EndedAt = &at
	require.NoError(t, a.store.UpdateRun(t.Context(), ended))
	a.close()

	cavuIn(t, dir, "task", "run", id)

	assert.Equal(t, []removal{{instanceID: storedSettings(t, dir)["instance_id"], keepRuns: []int64{open.ID}}}, fake.removals)
}

func TestTaskRunEndsTheRunOfACancelledTask(t *testing.T) {
	dir, blocked, fake := runSetup(t)
	fake.runner.work = engine.WorkResult{Outcome: engine.OutcomeBlocked, Questions: []string{"Which API version?"}}

	code, _, _ := runCavu("task", "run", blocked, "--data-dir", dir)
	require.Equal(t, 3, code)

	task := storedTask(t, dir, blocked)

	a, err := openApp(t.Context(), dir)
	require.NoError(t, err)

	run, ok, err := a.store.OpenRun(t.Context(), task.ID)
	require.NoError(t, err)
	require.True(t, ok, "a question keeps the run")
	a.close()

	cavuIn(t, dir, "task", "cancel", blocked)

	fake.runner.work = engine.WorkResult{Outcome: engine.OutcomeDone, Changed: true, HeadCommit: "head456"}
	fake.runner.releases = nil

	cavuIn(t, dir, "task", "run", addTask(t, dir, "Another task"))

	assert.Contains(t, fake.runner.releases, release{
		ref: engine.RunRef{
			InstanceID: storedSettings(t, dir)["instance_id"], Image: sandbox.ImageName(version),
			TaskID: task.ID, RunID: run.ID, ProjectID: task.ProjectID,
		},
		removeVolumes: true,
	}, "the cancelled task's run ends and its volumes go")
	assert.NotContains(t, fake.removals[len(fake.removals)-1].keepRuns, run.ID)
	assert.Equal(t, engine.StatusCancelled, engine.StatusOf(engine.Reduce(storedEvents(t, dir, blocked))))
}

func TestTaskRunInterruptReleasesAndExits130(t *testing.T) {
	dir, id, fake := runSetup(t)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	fake.runner.duringWork = cancel // as Ctrl-C cancels run's context

	err := runTask(ctx, []string{"run", id, "--data-dir", dir}, nil, io.Discard, io.Discard)

	ee, ok := errors.AsType[exitError](err)
	require.True(t, ok, err)
	assert.Equal(t, 130, ee.code)

	events := storedEvents(t, dir, id)
	facts := engine.Reduce(events)
	assert.Nil(t, facts.OpenStop, "an interrupt is no stop: the cut session is redone")
	assert.True(t, facts.RunOpen)

	a, err := openApp(t.Context(), dir)
	require.NoError(t, err)

	defer a.close()

	task, err := a.store.Task(t.Context(), events[0].TaskID)
	require.NoError(t, err)

	run, ok, err := a.store.OpenRun(t.Context(), task.ID)
	require.NoError(t, err)
	require.True(t, ok, "the run stays open")

	assert.Equal(t, []release{{
		ref: engine.RunRef{
			InstanceID: storedSettings(t, dir)["instance_id"], Image: sandbox.ImageName(version),
			TaskID: task.ID, RunID: run.ID, ProjectID: task.ProjectID,
		},
		removeVolumes: false,
	}}, fake.runner.releases, "the run's containers stop and its volumes stay, on a context the interrupt did not cancel")
}
