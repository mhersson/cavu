package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/mhersson/cavu/internal/engine"
	"github.com/mhersson/cavu/internal/forge"
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
		{"cancel when done", "cancel", []store.Event{event(engine.Exported, "runner", engine.ExportedPayload{}), event(engine.Merged, "user", struct{}{})}},
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

func TestTaskShowPrintsUsageAndCost(t *testing.T) {
	dir := webData(t)
	id := addTask(t, dir, "Fix the login")

	usage := []store.Usage{
		{Source: "proxy", Model: "model-a", Input: 10, Output: 5, CostEstimateUSD: 0.5},
		{Source: "agent", Model: "model-a", Input: 1, Output: 2, CostEstimateUSD: 0.25},
		{Source: "agent", Model: "model-b", Input: 3, Output: 4, CostEstimateUSD: 0.25},
	}

	seedEvents(t, dir, id, usage, event(engine.TaskStarted, "user", struct{}{}),
		event(engine.Stopped, "engine", engine.StoppedPayload{Reason: engine.StopNoChange, Explanation: "nothing changed", Action: engine.ClearRetry}))

	out := cavuIn(t, dir, "task", "show", id)

	// linesWith counts the lines of out that hold every one of values.
	linesWith := func(values ...string) (n int) {
		for line := range strings.SplitSeq(out, "\n") {
			if !slices.ContainsFunc(values, func(v string) bool { return !strings.Contains(line, v) }) {
				n++
			}
		}

		return n
	}

	assert.Equal(t, 1, linesWith("1.00"), "one cost line, with the sum of the estimates")

	for _, u := range usage {
		assert.Equal(t, 1, linesWith(u.Source, u.Model, strconv.FormatInt(u.Input, 10), strconv.FormatInt(u.Output, 10)),
			"one line per source and model, with its values: %+v", u)
	}

	for _, want := range []string{"Fix the login", string(engine.StatusBlocked), engine.StopNoChange, "nothing changed", engine.ClearRetry} {
		assert.Contains(t, out, want)
	}
}

// labelled returns the value of every line of out that starts with label.
func labelled(out, label string) []string {
	var vs []string

	for line := range strings.Lines(out) {
		if v, ok := strings.CutPrefix(line, label); ok {
			vs = append(vs, strings.TrimSpace(v))
		}
	}

	return vs
}

func TestTaskShowPrintsSuggestedAnswers(t *testing.T) {
	dir := webData(t)
	id := addTask(t, dir, "Fix")

	seedEvents(t, dir, id, nil, event(engine.TaskStarted, "user", struct{}{}),
		event(engine.RunStarted, "engine", engine.RunStartedPayload{Reason: "initial"}),
		event(engine.SessionEnded, "runner", engine.SessionEndedPayload{
			Outcome: engine.OutcomeBlocked, Questions: []string{"Which port?", "Is IPv6 needed?"}, SuggestedAnswers: []string{"8080", ""},
		}),
		question())

	var lines []string

	for line := range strings.Lines(cavuIn(t, dir, "task", "show", id)) {
		if strings.HasPrefix(line, "asked:") || strings.HasPrefix(line, "suggest:") {
			lines = append(lines, strings.Join(strings.Fields(line), " "))
		}
	}

	assert.Equal(t, []string{"asked: Which port?", "suggest: 8080", "asked: Is IPv6 needed?"}, lines,
		"each suggested answer under its question, and none for a question without one")
}

func TestTaskShowPrintsTheReview(t *testing.T) {
	trimmed := engine.Finding{Severity: engine.SeverityCritical, Where: "login.go:12", What: "The password is trimmed.", Why: "Valid passwords fail."}
	typo := engine.Finding{Severity: engine.SeverityMinor, Where: "README.md", What: "A typo.", Why: "It reads badly."}

	dir := webData(t)
	id := addTask(t, dir, "Fix")

	seedEvents(t, dir, id, nil, event(engine.TaskStarted, "user", struct{}{}),
		event(engine.RunStarted, "engine", engine.RunStartedPayload{Reason: "initial"}),
		event(engine.ReviewEnded, "runner", engine.ReviewEndedPayload{
			Outcome: engine.ReviewReviewed, Head: "head1", Built: engine.Verdict{Pass: true}, Findings: []engine.Finding{trimmed, typo},
		}))

	out := cavuIn(t, dir, "task", "show", id)

	for label, f := range map[string]engine.Finding{"open:": trimmed, "minor:": typo} {
		lines := labelled(out, label)
		require.Len(t, lines, 1, out)

		for _, v := range []string{f.Severity, f.Where, f.What, f.Why} {
			assert.Contains(t, lines[0], v)
		}
	}

	assert.Empty(t, labelled(out, "review:"), "the review gave a verdict")

	seedEvents(t, dir, id, nil, event(engine.ReviewEnded, "runner", engine.ReviewEndedPayload{
		Outcome: engine.ReviewFailed, Head: "head2", Detail: "The session crashed: exit 1.",
	}))

	out = cavuIn(t, dir, "task", "show", id)
	lines := labelled(out, "review:")
	require.Len(t, lines, 1, out)
	assert.Contains(t, lines[0], "The session crashed: exit 1.")
	assert.Len(t, labelled(out, "open:"), 1, "what the last verdict left open stays open")
}

func TestTaskShowPrintsTheNewestExport(t *testing.T) {
	dir := webData(t)
	id := addTask(t, dir, "Fix")
	stat := []string{"login.go | 2 +-", "app.bin  | Bin 0 -> 23456789 bytes", "2 files changed, 1 insertion(+), 1 deletion(-)"}

	seedEvents(t, dir, id, nil, event(engine.TaskStarted, "user", struct{}{}),
		event(engine.Exported, "runner", engine.ExportedPayload{
			DiffPath: "/old/change.diff", BundlePath: "/old/change.bundle", DiffStat: []string{"old.go | 1 +"},
		}),
		event(engine.Exported, "runner", engine.ExportedPayload{DiffPath: "/new/change.diff", BundlePath: "/new/change.bundle", DiffStat: stat}))

	out := cavuIn(t, dir, "task", "show", id)

	assert.Equal(t, []string{"/new/change.diff"}, labelled(out, "diff:"))
	assert.Equal(t, []string{"/new/change.bundle"}, labelled(out, "bundle:"))
	assert.Equal(t, stat, labelled(out, "changed:"), "a line per line of git's summary, of the newest export only")
	assert.Equal(t, []string{string(engine.StatusInReview)}, labelled(out, "status:"))
}

func TestTaskShowPrintsTurnsAndDuration(t *testing.T) {
	dir := webData(t)
	id := addTask(t, dir, "Fix")

	out := cavuIn(t, dir, "task", "show", id)
	assert.Equal(t, []string{"0"}, labelled(out, "turns:"), "a draft")
	assert.Equal(t, []string{"0s"}, labelled(out, "duration:"), "a draft")

	t0 := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	at := func(e store.Event, d time.Duration) store.Event {
		e.At = t0.Add(d)

		return e
	}

	seedEvents(t, dir, id, nil,
		at(event(engine.TaskStarted, "user", struct{}{}), 0),
		at(event(engine.SessionEnded, "runner", engine.SessionEndedPayload{Outcome: engine.OutcomeUnfinished, Turns: 3}), time.Minute),
		at(event(engine.SessionEnded, "runner", engine.SessionEndedPayload{Outcome: engine.OutcomeDone, Turns: 4}), 4*time.Minute),
		at(question(), 5*time.Minute+3400*time.Millisecond))

	out = cavuIn(t, dir, "task", "show", id)
	assert.Equal(t, []string{"7"}, labelled(out, "turns:"))
	assert.Equal(t, []string{"5m3s"}, labelled(out, "duration:"))
}

// numbers is every whole number in text, in order.
var numbers = regexp.MustCompile(`[0-9]+`)

func TestTaskShowPrintsTheBudgetAndAMissingCost(t *testing.T) {
	dir := webData(t)
	cavuIn(t, dir, "settings", "set", "task_max_turns", "400")
	cavuIn(t, dir, "settings", "set", "task_timeout_minutes", "120")
	id := addTask(t, dir, "Fix")

	t0 := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	at := func(e store.Event, d time.Duration) store.Event {
		e.At = t0.Add(d)

		return e
	}

	seedEvents(t, dir, id, []store.Usage{{Source: "agent", Model: "model-a", Input: 1, CostEstimateUSD: 0.5}},
		at(event(engine.TaskStarted, "user", struct{}{}), 0),
		at(event(engine.RunStarted, "engine", engine.RunStartedPayload{Reason: "initial"}), 0),
		at(event(engine.SessionStarted, "engine", engine.SessionStartedPayload{Mode: engine.ModeStart, SessionID: "s1"}), time.Minute),
		at(event(engine.SessionEnded, "runner", engine.SessionEndedPayload{Outcome: engine.OutcomeUnfinished, Turns: 20}), 11*time.Minute))

	out := cavuIn(t, dir, "task", "show", id)

	lines := labelled(out, "budget:")
	require.Len(t, lines, 1)
	assert.Equal(t, []string{"20", "400", "10", "120"}, numbers.FindAllString(lines[0], -1), "turns and minutes used, of how many")

	cost := labelled(out, "cost (estimate):")
	require.Len(t, cost, 1)
	assert.Equal(t, []string{"0", "5000"}, numbers.FindAllString(cost[0], -1), "every session had a cost")

	seedEvents(t, dir, id, nil,
		event(engine.SessionEnded, "runner", engine.SessionEndedPayload{Outcome: engine.OutcomeCrashed, CostMissing: true}),
		event(engine.ReviewEnded, "runner", engine.ReviewEndedPayload{Outcome: engine.ReviewFailed, CostMissing: true}))

	cost = labelled(cavuIn(t, dir, "task", "show", id), "cost (estimate):")
	require.Len(t, cost, 1)
	assert.Equal(t, []string{"0", "5000", "2"}, numbers.FindAllString(cost[0], -1), "the cost line ends with how many sessions it leaves out")
}

func TestTaskShowCostLineCountsMissingSessionsInTheRightNumber(t *testing.T) {
	dir := webData(t)
	id := addTask(t, dir, "Fix")

	noun := regexp.MustCompile(`leaving out [0-9]+ (sessions?)\b`)
	nounFor := func() string {
		cost := labelled(cavuIn(t, dir, "task", "show", id), "cost (estimate):")
		require.Len(t, cost, 1)

		if m := noun.FindStringSubmatch(cost[0]); m != nil {
			return m[1]
		}

		return ""
	}

	assert.Empty(t, nounFor(), "nothing is left out")

	seedEvents(t, dir, id, nil,
		event(engine.SessionEnded, "runner", engine.SessionEndedPayload{Outcome: engine.OutcomeCrashed, CostMissing: true}))
	assert.Equal(t, "session", nounFor())

	seedEvents(t, dir, id, nil,
		event(engine.SessionEnded, "runner", engine.SessionEndedPayload{Outcome: engine.OutcomeCrashed, CostMissing: true}))
	assert.Equal(t, "sessions", nounFor())
}

// amounts is every dollar amount in text, in order.
var amounts = regexp.MustCompile(`\$[0-9]+\.[0-9]{2}`)

func TestTaskShowPrintsTheMoney(t *testing.T) {
	dir := webData(t)

	for k, v := range map[string]string{
		"endpoint_mode": "key", "endpoint_url": "https://gateway.example", "endpoint_auth_header": "x-api-key",
		"task_budget_usd": "5", "model_prices": `{"*": {"input": 1, "output": 1, "cache_read": 1, "cache_write": 1}}`,
	} {
		cavuIn(t, dir, "settings", "set", k, v)
	}

	id := addTask(t, dir, "Fix")

	// Long before today, so the day's spend is none of it.
	t0 := time.Date(2001, 1, 1, 9, 0, 0, 0, time.UTC)
	at := func(e store.Event, d time.Duration) store.Event {
		e.At = t0.Add(d)

		return e
	}

	seedEvents(t, dir, id, []store.Usage{{Source: "proxy", Model: "claude-x", Input: 3_200_000}},
		at(event(engine.TaskStarted, "user", struct{}{}), 0),
		at(event(engine.RunStarted, "engine", engine.RunStartedPayload{Reason: "initial"}), 0),
		at(event(engine.SessionStarted, "engine", engine.SessionStartedPayload{Mode: engine.ModeStart, SessionID: "s1"}), time.Minute),
		at(event(engine.SessionEnded, "runner", engine.SessionEndedPayload{Outcome: engine.OutcomeUnfinished, Turns: 20}), 11*time.Minute))

	lines := labelled(cavuIn(t, dir, "task", "show", id), "budget:")
	require.Len(t, lines, 1)
	assert.Equal(t, []string{"$3.20", "$5.00"}, amounts.FindAllString(lines[0], -1), "the task's spend of its cap")

	cavuIn(t, dir, "settings", "set", "daily_budget_usd", "20")

	lines = labelled(cavuIn(t, dir, "task", "show", id), "budget:")
	require.Len(t, lines, 1)
	assert.Equal(t, []string{"$3.20", "$5.00", "$0.00", "$20.00"}, amounts.FindAllString(lines[0], -1), "and the day's of the daily cap")

	cavuIn(t, dir, "settings", "set", "endpoint_mode", "subscription")

	lines = labelled(cavuIn(t, dir, "task", "show", id), "budget:")
	require.Len(t, lines, 1)
	assert.Empty(t, amounts.FindAllString(lines[0], -1), "no money cap under a subscription")
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
	// The fake answers every session alike, so the readiness turn is off, and no
	// forge is behind the repository, so the publish is off: no test here is about either.
	cavuIn(t, dir, "project", "set", "web", "--switch", "readiness=off", "--switch", "publish=off")

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
// exit 1, no event and no runner call. It returns stderr.
func refusedRun(t *testing.T, dir, id string, fake *fakeRuntime) string {
	t.Helper()

	before := storedEvents(t, dir, id)

	code, stdout, stderr := runCavu("task", "run", id, "--data-dir", dir)

	assert.Equal(t, 1, code)
	assert.Empty(t, stdout)
	assert.Equal(t, before, storedEvents(t, dir, id), "no event is appended")
	assert.Empty(t, fake.runner.actions, "no container starts")
	assert.Empty(t, fake.runner.releases)

	return stderr
}

func TestTaskRunDrivesWhenTheLockIsFree(t *testing.T) {
	dir, id, fake := runSetup(t)

	code, stdout, stderr := runCavu("task", "run", id, "--data-dir", dir)

	require.Equal(t, 0, code, stderr)
	assert.Equal(t, []engine.ActionKind{engine.KindPrepare, engine.KindWork, engine.KindVerify, engine.KindReview, engine.KindExport}, fake.runner.actions)
	require.Len(t, fake.runner.releases, 2)
	assert.Equal(t, engine.RemoveRun, fake.runner.releases[0].remove, "the exported run ends")
	assert.Equal(t, engine.RemoveAll, fake.runner.releases[1].remove, "nothing publishes the change, so the task's home goes")

	events := storedEvents(t, dir, id)
	assert.Equal(t, engine.TaskStarted, events[0].Type, "the draft is started")
	assert.True(t, slices.ContainsFunc(events, func(e store.Event) bool { return e.Type == engine.AgentText }),
		"progress is recorded, so it is printed with the rest")

	tail := eventLines(t, stdout, events)

	x, ok := newest[engine.ExportedPayload](events, engine.Exported)
	require.True(t, ok)
	assert.Equal(t, []string{"status:", string(engine.StatusInReview)}, strings.Fields(tail[0]))
	assert.Contains(t, strings.Join(tail, "\n"), x.DiffPath)
	assert.Equal(t, []string{"login.go | 1 +", "1 file changed, 1 insertion(+)"}, x.DiffStat)
	assert.Equal(t, x.DiffStat, labelled(strings.Join(tail, "\n"), "changed:"), "the end of the run shows the change's summary")
	assert.True(t, strings.HasPrefix(x.DiffPath, filepath.Join(dir, "artifacts")), "the artifacts are in the data directory")

	diff, err := os.ReadFile(x.DiffPath)
	require.NoError(t, err)
	assert.Equal(t, "diff --git a/login.go b/login.go\n", string(diff))
}

func TestTaskRunBlockedExitCode(t *testing.T) {
	t.Run("a question", func(t *testing.T) {
		dir, id, fake := runSetup(t)
		questions := []string{"Which API version?", "Should the old route stay?"}
		fake.runner.work = engine.WorkResult{Outcome: engine.OutcomeBlocked, Questions: questions}

		code, stdout, _ := runCavu("task", "run", id, "--data-dir", dir)

		assert.Equal(t, 3, code)
		assert.Equal(t, []engine.ActionKind{engine.KindPrepare, engine.KindWork}, fake.runner.actions)
		require.Len(t, fake.runner.releases, 1)
		assert.Equal(t, engine.KeepVolumes, fake.runner.releases[0].remove, "a question keeps the run")

		events := storedEvents(t, dir, id)
		tail := eventLines(t, stdout, events)

		s, ok := newest[engine.StoppedPayload](events, engine.Stopped)
		require.True(t, ok)
		assert.Equal(t, engine.StopQuestion, s.Reason)

		for _, v := range []string{string(engine.StatusBlocked), s.Reason, s.Explanation, s.Action} {
			assert.Contains(t, strings.Join(tail, "\n"), v)
		}

		for _, q := range questions {
			assert.True(t, slices.ContainsFunc(tail, func(l string) bool { return strings.HasSuffix(l, " "+q) }),
				"each question is on a line of its own: %q", q)
		}
	})

	t.Run("a cancel from another terminal", func(t *testing.T) {
		dir, id, fake := runSetup(t)
		// The session runs on after the cancel, as a real one does, until the scheduler cuts it.
		fake.runner.duringWork = func(ctx context.Context) {
			// This runs on the scheduler's goroutine, where a failure must not stop it.
			code, _, stderr := runCavu("task", "cancel", id, "--data-dir", dir)
			assert.Equal(t, 0, code, stderr)

			select {
			case <-ctx.Done():
			case <-time.After(within):
			}
		}

		code, stdout, _ := runCavu("task", "run", id, "--data-dir", dir)

		assert.Equal(t, 3, code)
		require.Len(t, fake.runner.releases, 2)
		assert.Equal(t, engine.RemoveRun, fake.runner.releases[0].remove, "a cancel ends the run")
		assert.Equal(t, engine.RemoveAll, fake.runner.releases[1].remove, "a cancelled task's home goes")
		// The lines come from the store, so the other terminal's cancel is printed too.
		assert.Contains(t, strings.Join(eventLines(t, stdout, storedEvents(t, dir, id)), "\n"), string(engine.StatusCancelled))
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

// holdLock holds the data directory's lock as role until the test ends, as another process would.
func holdLock(t *testing.T, dir, role string) *app {
	t.Helper()

	a, err := openApp(t.Context(), dir)
	require.NoError(t, err)
	require.NoError(t, a.lock(role))
	t.Cleanup(a.close)

	return a
}

func TestTaskRunRefusedBesideAnotherVerb(t *testing.T) {
	dir, id, fake := runSetup(t)
	holdLock(t, dir, "verb")

	assert.Contains(t, refusedRun(t, dir, id, fake), filepath.Join(dir, "lock"))
	assert.Empty(t, storedEvents(t, dir, id), "the task is not started")
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
		remove: engine.RemoveAll,
	}, "the cancelled task's run ends and its volumes go")
	assert.NotContains(t, fake.removals[len(fake.removals)-1].keepRuns, run.ID)
	assert.Equal(t, engine.StatusCancelled, engine.StatusOf(engine.Reduce(storedEvents(t, dir, blocked))))
}

func TestTaskRunInterruptReleasesAndExits130(t *testing.T) {
	dir, id, fake := runSetup(t)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	fake.runner.duringWork = func(context.Context) { cancel() } // as Ctrl-C cancels run's context

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
		remove: engine.KeepVolumes,
	}}, fake.runner.releases, "the run's containers stop and its volumes stay, on a context the interrupt did not cancel")
}

// syncBuffer is a verb's output that a test reads while the verb writes it.
type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.b.String()
}

// taskInBackground runs the task verb with args until it returns or ctx ends.
func taskInBackground(ctx context.Context, args ...string) (*syncBuffer, <-chan error) {
	out, done := &syncBuffer{}, make(chan error, 1)

	go func() { done <- runTask(ctx, args, nil, out, io.Discard) }()

	return out, done
}

// within bounds how long a test waits for a verb.
const within = 10 * time.Second

// exitCodeOf waits for the verb's result and returns its exit code.
func exitCodeOf(t *testing.T, done <-chan error) int {
	t.Helper()

	select {
	case err := <-done:
		if err == nil {
			return 0
		}

		ee, ok := errors.AsType[exitError](err)
		require.True(t, ok, err)

		return ee.code
	case <-time.After(within):
		require.FailNow(t, "the verb did not end")

		return 0
	}
}

// waitReasons are the reasons of the lines of out that report a wait.
func waitReasons(out string) []string {
	var rs []string

	for l := range strings.Lines(out) {
		if f := strings.Fields(l); len(f) > 1 && f[1] == "wait" {
			rs = append(rs, strings.Join(f[2:], " "))
		}
	}

	return rs
}

// names reports whether text holds the task id as a whole number.
func names(text, id string) bool {
	return slices.Contains(strings.FieldsFunc(text, func(r rune) bool { return !unicode.IsDigit(r) }), id)
}

// refuseRuntime fails the test if a verb asks for the container runtime.
func refuseRuntime(t *testing.T) {
	t.Helper()

	prev := detectRuntime
	detectRuntime = func(context.Context, string, *forge.Client) (containerRuntime, error) {
		t.Error("the verb asked for the container runtime")

		return nil, errors.New("no runtime here")
	}

	t.Cleanup(func() { detectRuntime = prev })
}

func taskID(t *testing.T, id string) int64 {
	t.Helper()

	n, err := strconv.ParseInt(id, 10, 64)
	require.NoError(t, err)

	return n
}

func TestTaskRunPrintsFromThePersonsNewestAction(t *testing.T) {
	dir, id, _ := runSetup(t)
	seedEvents(t, dir, id, nil, event(engine.TaskStarted, "user", struct{}{}),
		event(engine.AgentText, "agent", engine.AgentTextPayload{Text: "an earlier session's line"}), question())

	code, _, stderr := withStdin("Use the v2 API.", "task", "answer", id, "--data-dir", dir)
	require.Equal(t, 0, code, stderr)

	code, stdout, stderr := runCavu("task", "run", id, "--data-dir", dir)

	require.Equal(t, 0, code, stderr)
	assert.NotContains(t, stdout, "an earlier session's line")

	events := storedEvents(t, dir, id)
	tail := eventLines(t, stdout, events[slices.IndexFunc(events, func(e store.Event) bool { return e.Type == engine.HumanAnswered }):])
	assert.Equal(t, []string{"status:", string(engine.StatusInReview)}, strings.Fields(tail[0]))
}

func TestTaskRunFollowsUnderAServer(t *testing.T) {
	dir, id, _ := runSetup(t)
	server := holdLock(t, dir, "serve")
	refuseRuntime(t)

	n := taskID(t, id)
	head := "head456"

	ctx, cancel := context.WithCancel(t.Context())
	served := make(chan struct{})

	// The server drives the task once the verb has started it, unless the verb ended first.
	go func() {
		defer close(served)

		started := func() bool {
			last, err := server.store.LastEventID(ctx, n)

			return ctx.Err() != nil || err == nil && last > 0
		}

		if assert.Eventually(t, started, within, time.Millisecond) && ctx.Err() == nil {
			_, err := server.store.Append(ctx, n, -1, []store.Event{
				event(engine.SessionStarted, "engine", engine.SessionStartedPayload{Mode: engine.ModeStart}),
				event(engine.AgentText, "agent", engine.AgentTextPayload{Text: "Reading the login handler."}),
				event(engine.SessionEnded, "runner", engine.SessionEndedPayload{Outcome: engine.OutcomeDone, Changed: true, HeadCommit: head, Turns: 3}),
				event(engine.VerifyEnded, "runner", engine.VerifyEndedPayload{Result: engine.ResultPassed}),
				event(engine.Exported, "runner", engine.ExportedPayload{HeadCommit: head, DiffPath: "/srv/diff.patch"}),
			}, nil)
			assert.NoError(t, err)
		}
	}()

	code, stdout, stderr := runCavu("task", "run", id, "--data-dir", dir)

	cancel()
	<-served
	require.Equal(t, 0, code, stderr)

	events := storedEvents(t, dir, id)
	assert.Equal(t, engine.TaskStarted, events[0].Type, "the verb records the start")
	assert.Len(t, events, 6)

	tail := eventLines(t, stdout, events)
	assert.Equal(t, []string{"status:", string(engine.StatusInReview)}, strings.Fields(tail[0]))
}

func TestTaskRunPrintsAWaitOnce(t *testing.T) {
	dir, _, fake := runSetup(t)
	dep := addTask(t, dir, "First")
	id := strings.TrimSpace(cavuIn(t, dir, "task", "add", "web", "--title", "Second", "--depends-on", dep))

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	out, done := taskInBackground(ctx, "run", id, "--data-dir", dir)

	require.Eventually(t, func() bool { return len(waitReasons(out.String())) > 0 }, within, time.Millisecond)
	assert.True(t, names(waitReasons(out.String())[0], dep), "the wait names the task it waits on")

	// Each progress line it prints is a later poll that found the task waiting still.
	for i := range 3 {
		text := fmt.Sprintf("still waiting %d", i)
		seedEvents(t, dir, id, nil, event(engine.AgentText, "agent", engine.AgentTextPayload{Text: text}))
		require.Eventually(t, func() bool { return strings.Contains(out.String(), text) }, within, time.Millisecond)
	}

	cancel()

	assert.Equal(t, 130, exitCodeOf(t, done))
	assert.Len(t, waitReasons(out.String()), 1, out.String())
	assert.Empty(t, fake.runner.actions)
}

func TestTaskRunWaitNamesThePausingTask(t *testing.T) {
	for _, server := range []bool{false, true} {
		t.Run(map[bool]string{false: "its own scheduler", true: "a server"}[server], func(t *testing.T) {
			dir, id, fake := runSetup(t)
			other := addTask(t, dir, "Other")
			seedEvents(t, dir, other, nil, event(engine.TaskStarted, "user", struct{}{}),
				event(engine.Stopped, "engine", engine.StoppedPayload{Reason: engine.StopEndpoint, Explanation: "502", Action: engine.ClearRetry}))

			if server {
				holdLock(t, dir, "serve")
				refuseRuntime(t)
			}

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			out, done := taskInBackground(ctx, "run", id, "--data-dir", dir)

			require.Eventually(t, func() bool {
				return slices.ContainsFunc(waitReasons(out.String()), func(r string) bool { return names(r, other) })
			}, within, time.Millisecond, "the wait names the task whose endpoint stop pauses the queue")

			cancel()

			assert.Equal(t, 130, exitCodeOf(t, done))
			assert.Empty(t, fake.runner.actions, "nothing starts while the queue is paused")
		})
	}
}

func TestTaskAddDependsOnAndStart(t *testing.T) {
	dir := webData(t)
	first, second := addTask(t, dir, "First"), addTask(t, dir, "Second")

	id := strings.TrimSpace(cavuIn(t, dir, "task", "add", "web", "--title", "Third", "--depends-on", first+","+second, "--start"))

	a, err := openApp(t.Context(), dir)
	require.NoError(t, err)

	defer a.close()

	deps, err := a.store.TaskDeps(t.Context(), taskID(t, id))
	require.NoError(t, err)
	assert.Equal(t, []int64{taskID(t, first), taskID(t, second)}, deps)

	events := storedEvents(t, dir, id)
	require.Len(t, events, 1)
	assert.Equal(t, engine.TaskStarted, events[0].Type)

	code, stdout, _ := runCavu("task", "add", "web", "--title", "Fourth", "--depends-on", "999", "--data-dir", dir)
	assert.Equal(t, 1, code)
	assert.Empty(t, stdout)

	tasks, err := a.store.Tasks(t.Context(), true)
	require.NoError(t, err)
	assert.Len(t, tasks, 3, "no task is created")
}

func TestTaskEdit(t *testing.T) {
	dir, dep, fake := runSetup(t)
	bodyFile := filepath.Join(t.TempDir(), "body.md")
	require.NoError(t, os.WriteFile(bodyFile, []byte("A new body."), 0o600))

	id := strings.TrimSpace(cavuIn(t, dir, "task", "add", "web", "--title", "Second", "--depends-on", dep, "--body-file", bodyFile, "--start"))

	cavuIn(t, dir, "task", "edit", id, "--title", "Renamed")

	got := storedTask(t, dir, id)
	assert.Equal(t, "Renamed", got.Title)
	assert.Equal(t, "A new body.", got.Body, "the body is as stored")
	assert.Equal(t, []int64{taskID(t, dep)}, storedDeps(t, dir, id), "the dependencies are as stored")

	cavuIn(t, dir, "task", "cancel", dep)

	code, _, _ := runCavu("task", "run", id, "--data-dir", dir)
	require.Equal(t, 3, code, "a cancelled dependency stops the task")

	cavuIn(t, dir, "task", "edit", id, "--depends-on", "")
	assert.Empty(t, storedDeps(t, dir, id))

	cavuIn(t, dir, "task", "retry", id)

	fake.runner.actions = nil

	cavuIn(t, dir, "task", "run", id)
	assert.Equal(t, engine.KindPrepare, fake.runner.actions[0])

	code, _, stderr := runCavu("task", "edit", id, "--title", "Too late", "--data-dir", dir)
	assert.Equal(t, 1, code)
	assert.NotEmpty(t, stderr)
	assert.Equal(t, "Renamed", storedTask(t, dir, id).Title, "an edit after the first run is refused")
}

func storedDeps(t *testing.T, dir, id string) []int64 {
	t.Helper()

	a, err := openApp(t.Context(), dir)
	require.NoError(t, err)

	defer a.close()

	deps, err := a.store.TaskDeps(t.Context(), taskID(t, id))
	require.NoError(t, err)

	return deps
}

func TestTaskDone(t *testing.T) {
	dir := webData(t)

	review := addTask(t, dir, "In review")
	seedEvents(t, dir, review, nil, event(engine.TaskStarted, "user", struct{}{}), event(engine.Exported, "runner", engine.ExportedPayload{}))

	cavuIn(t, dir, "task", "done", review)
	assert.Equal(t, engine.StatusDone, engine.StatusOf(engine.Reduce(storedEvents(t, dir, review))))

	running := addTask(t, dir, "Running")
	seedEvents(t, dir, running, nil, event(engine.TaskStarted, "user", struct{}{}), event(engine.RunStarted, "engine", engine.RunStartedPayload{Reason: "initial"}))
	before := storedEvents(t, dir, running)

	code, _, _ := runCavu("task", "done", running, "--data-dir", dir)
	assert.Equal(t, 1, code)
	assert.Equal(t, before, storedEvents(t, dir, running), "no event is appended")
}

func TestTaskShowPrintsDependenciesAndTheWait(t *testing.T) {
	dir := webData(t)
	dep := addTask(t, dir, "First")
	id := strings.TrimSpace(cavuIn(t, dir, "task", "add", "web", "--title", "Second", "--depends-on", dep, "--start"))

	out := cavuIn(t, dir, "task", "show", id)
	require.Len(t, labelled(out, "depends:"), 1)
	assert.Contains(t, labelled(out, "depends:")[0], dep)
	require.Len(t, labelled(out, "wait:"), 1)
	assert.True(t, names(labelled(out, "wait:")[0], dep), "the wait names the task it waits on")

	seedEvents(t, dir, dep, nil, event(engine.TaskStarted, "user", struct{}{}), event(engine.Exported, "runner", engine.ExportedPayload{}),
		event(engine.Merged, "user", struct{}{}))

	out = cavuIn(t, dir, "task", "show", id)
	assert.Contains(t, labelled(out, "depends:")[0], string(engine.StatusDone))
	assert.Empty(t, labelled(out, "wait:"), "a task whose dependencies are done does not wait")
}

func TestEventLine(t *testing.T) {
	at := time.Date(2026, 11, 17, 9, 30, 15, 0, time.Local)

	tests := []struct {
		typ     string
		payload any
		values  []string
	}{
		{engine.AgentText, engine.AgentTextPayload{Text: "Reading the handler.\nThen the tests."}, []string{"Reading the handler.", "Then the tests."}},
		{engine.AgentTool, engine.AgentToolPayload{Name: "Bash", Summary: "go test ./..."}, []string{"Bash", "go test ./..."}},
		{engine.SessionStarted, engine.SessionStartedPayload{Mode: engine.ModeFixVerify, SessionID: "abc"}, []string{engine.ModeFixVerify}},
		{engine.SessionEnded, engine.SessionEndedPayload{Outcome: engine.OutcomeUnfinished, Turns: 17}, []string{engine.OutcomeUnfinished, "17"}},
		{engine.VerifyEnded, engine.VerifyEndedPayload{Result: engine.ResultTimeout, Output: "FAIL"}, []string{engine.ResultTimeout}},
		{engine.PrepareFailed, engine.PrepareFailedPayload{Step: "setup", Output: "npm ERR!"}, []string{"setup"}},
		{engine.Stopped, engine.StoppedPayload{Reason: engine.StopQuestion, Explanation: "Which API version?"}, []string{engine.StopQuestion, "Which API version?"}},
		{engine.Prepared, engine.PreparedPayload{BaseCommit: "base123"}, []string{"base123"}},
		{engine.ReviewStarted, engine.ReviewStartedPayload{SessionID: "r1", Scoped: true, Head: "fed789", Why: "A fix changed the code."}, nil},
		{
			engine.ReviewEnded,
			engine.ReviewEndedPayload{
				Outcome: engine.ReviewReviewed, Scoped: true, Turns: 17, Findings: slices.Repeat([]engine.Finding{{Severity: engine.SeverityImportant}}, 4),
				Addressed: []bool{true, false, true, false, false, false},
			},
			[]string{engine.ReviewReviewed, "17", "4", "2", "6"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.typ, func(t *testing.T) {
			raw, err := json.Marshal(tc.payload)
			require.NoError(t, err)

			l := eventLine(store.Event{At: at, Type: tc.typ, Payload: raw})

			assert.Equal(t, []string{at.Format(time.TimeOnly), tc.typ}, strings.Fields(l)[:2])

			for _, v := range tc.values {
				assert.Contains(t, l, v)
			}

			assert.NotContains(t, l, "\n")
			assert.LessOrEqual(t, utf8.RuneCountInString(l), 200)
		})
	}

	t.Run("a review's start tells its scope", func(t *testing.T) {
		line := func(p engine.ReviewStartedPayload) string {
			raw, err := json.Marshal(p)
			require.NoError(t, err)

			return eventLine(store.Event{At: at, Type: engine.ReviewStarted, Payload: raw})
		}

		full := line(engine.ReviewStartedPayload{SessionID: "r1", Head: "def456", Why: "Not reviewed yet."})
		assert.Equal(t, full, line(engine.ReviewStartedPayload{SessionID: "r2", Head: "fed789", Why: "Another reason."}), "the scope alone sets the line")
		assert.NotEqual(t, full, line(engine.ReviewStartedPayload{SessionID: "r1", Scoped: true, Head: "def456", Why: "Not reviewed yet."}))
	})

	t.Run("control characters", func(t *testing.T) {
		raw, err := json.Marshal(engine.AgentToolPayload{Name: "Bash", Summary: "printf '\x1b[2J\x1b[31mred\x1b[0m\x07\u009b'"})
		require.NoError(t, err)

		l := eventLine(store.Event{At: at, Type: engine.AgentTool, Payload: raw})

		assert.False(t, strings.ContainsFunc(l, unicode.IsControl), "%q", l)
		assert.Contains(t, l, "red")
	})

	t.Run("a long text", func(t *testing.T) {
		// One of the two puts a two-byte character across the cut, whatever the line's head.
		for _, text := range []string{strings.Repeat("x", 5000), strings.Repeat("å", 2500), "x" + strings.Repeat("å", 2500)} {
			raw, err := json.Marshal(engine.AgentTextPayload{Text: text})
			require.NoError(t, err)

			l := eventLine(store.Event{At: at, Type: engine.AgentText, Payload: raw})

			assert.LessOrEqual(t, utf8.RuneCountInString(l), 200)
			assert.Greater(t, utf8.RuneCountInString(l), 100, "the line keeps the head of the text")
			assert.True(t, utf8.ValidString(l), l)
		}
	})
}

// newest decodes the newest event of the type, if any, independently of the facts.
func newest[T any](events []store.Event, typ string) (T, bool) {
	var v T

	for _, e := range slices.Backward(events) {
		if e.Type == typ {
			return v, json.Unmarshal(e.Payload, &v) == nil
		}
	}

	return v, false
}

func TestCountedIsSingularForOne(t *testing.T) {
	assert.Equal(t, "0 turns", counted(0, "turn"))
	assert.Equal(t, "1 turn", counted(1, "turn"))
	assert.Equal(t, "2 findings", counted(2, "finding"))
	assert.Equal(t, "1 finding", counted(1, "finding"))
}
