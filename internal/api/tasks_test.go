package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mhersson/cavu/internal/engine"
	"github.com/mhersson/cavu/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// site is a runnable project: verify is waived. web, with a verify command and
// no baseline, is not.
const site = `{"name": "site", "repo_url": "https://example.com/site.git", "verify_waived": true}`

// addProject creates a project over the API.
func (f *fixture) addProject(t *testing.T, body string) {
	t.Helper()

	status, got := f.call(t, "POST /api/projects", body)
	require.Equal(t, http.StatusCreated, status, got)
}

// task creates a task over the API and returns its id.
func (f *fixture) task(t *testing.T, body string) int64 {
	t.Helper()

	status, got := f.call(t, "POST /api/tasks", body)
	require.Equal(t, http.StatusCreated, status, got)

	id, ok := object(t, got)["id"].(float64)
	require.True(t, ok, got)

	return int64(id)
}

// add appends one event to the task through the store, at the clock's time,
// with usage rows when given.
func (f *fixture) add(t *testing.T, taskID int64, typ string, payload any, usage ...store.Usage) {
	t.Helper()

	raw, err := json.Marshal(payload)
	require.NoError(t, err)

	_, err = f.store.Append(t.Context(), taskID, -1,
		[]store.Event{{TaskID: taskID, At: f.clock.Now(), Type: typ, Actor: "engine", Payload: raw}}, usage)
	require.NoError(t, err)
}

// ask makes the task one that started and stopped on the agent's questions.
func (f *fixture) ask(t *testing.T, taskID int64, questions ...string) {
	t.Helper()

	f.add(t, taskID, engine.TaskStarted, struct{}{})
	f.add(t, taskID, engine.RunStarted, engine.RunStartedPayload{Reason: "initial"})
	f.add(t, taskID, engine.SessionEnded, engine.SessionEndedPayload{Outcome: engine.OutcomeBlocked, Questions: questions})
	f.add(t, taskID, engine.Stopped, engine.StoppedPayload{
		Reason: engine.StopQuestion, Explanation: questions[0], Action: engine.ClearAnswer,
	})
}

// count counts the task's events of typ.
func (f *fixture) count(t *testing.T, taskID int64, typ string) int {
	t.Helper()

	events, err := f.store.Events(t.Context(), taskID, 0, 0)
	require.NoError(t, err)

	n := 0

	for _, e := range events {
		if e.Type == typ {
			n++
		}
	}

	return n
}

// taskRecord is one stored task with what a request may write for it.
type taskRecord struct {
	Task   store.Task
	Events []store.Event
	Deps   []int64
}

// record reads every stored task, replay shadows included.
func (f *fixture) record(t *testing.T) []taskRecord {
	t.Helper()

	tasks, err := f.store.Tasks(t.Context(), true)
	require.NoError(t, err)

	out := make([]taskRecord, len(tasks))

	for i, task := range tasks {
		events, err := f.store.Events(t.Context(), task.ID, 0, 0)
		require.NoError(t, err)

		deps, err := f.store.TaskDeps(t.Context(), task.ID)
		require.NoError(t, err)

		out[i] = taskRecord{task, events, deps}
	}

	return out
}

// view reads the task over the API.
func (f *fixture) view(t *testing.T, taskID int64) map[string]any {
	t.Helper()

	status, body := f.call(t, fmt.Sprintf("GET /api/tasks/%d", taskID), "")
	require.Equal(t, http.StatusOK, status, body)

	return object(t, body)
}

func rfc3339(at time.Time) string { return at.UTC().Format(time.RFC3339Nano) }

func TestTasksRoundTrip(t *testing.T) {
	f := newServer(t)
	f.addProject(t, site)

	status, created := f.call(t, "POST /api/tasks", `{"project": "site", "title": "Add a footer", "body": "Below the fold."}`)
	require.Equal(t, http.StatusCreated, status, created)

	stored, err := f.store.Task(t.Context(), 1)
	require.NoError(t, err)
	assert.Equal(t, "api", stored.Requester)

	at := rfc3339(stored.CreatedAt)
	assert.JSONEq(t, `{"id": 1, "project": "site", "title": "Add a footer", "body": "Below the fold.", "requester": "api",
		"created_at": "`+at+`", "status": "draft", "waiting": "", "depends_on": [], "stop": null, "questions": [],
		"change": null, "turns": 0, "duration_ms": 0, "cost_estimate_usd": 0}`, created)

	status, body := f.call(t, "GET /api/tasks/1", "")
	require.Equal(t, http.StatusOK, status, body)
	assert.JSONEq(t, created, body)

	status, body = f.call(t, "GET /api/tasks", "")
	require.Equal(t, http.StatusOK, status, body)
	assert.JSONEq(t, `{"tasks": [{"id": 1, "project": "site", "title": "Add a footer", "status": "draft",
		"stop_reason": "", "waiting": "", "created_at": "`+at+`"}]}`, body)

	t.Run("a PATCH changes only the fields it names", func(t *testing.T) {
		second := f.task(t, `{"project": "site", "title": "Add a header", "body": "On top.", "depends_on": [1]}`)
		want := f.view(t, second)
		assert.Equal(t, []any{1.0}, want["depends_on"])

		for _, tc := range []struct {
			patch, field string
			value        any
		}{
			{`{"title": "Add a sticky header"}`, "title", "Add a sticky header"},
			{`{"body": "It stays on top."}`, "body", "It stays on top."},
			{`{"depends_on": []}`, "depends_on", []any{}},
		} {
			status, body := f.call(t, fmt.Sprintf("PATCH /api/tasks/%d", second), tc.patch)
			require.Equal(t, http.StatusOK, status, body)

			want[tc.field] = tc.value
			assert.Equal(t, want, object(t, body), tc.patch)
		}

		assert.Equal(t, want, f.view(t, second))
	})
}

func TestCreateAndStart(t *testing.T) {
	f := newServer(t)
	f.addProject(t, site)
	f.addProject(t, web)

	status, body := f.call(t, "POST /api/tasks", `{"project": "site", "title": "Add a footer", "start": true}`)
	require.Equal(t, http.StatusCreated, status, body)
	assert.Equal(t, "queued", object(t, body)["status"])
	assert.Equal(t, 1, f.count(t, 1, engine.TaskStarted))
	assert.Equal(t, int32(1), f.nudges.Load())

	t.Run("a project with no baseline", func(t *testing.T) {
		before := f.record(t)

		status, body := f.call(t, "POST /api/tasks", `{"project": "web", "title": "Add a footer", "start": true}`)
		require.Equal(t, http.StatusBadRequest, status, body)
		assertError(t, body)
		assert.Equal(t, before, f.record(t), "no task is created")
		assert.Equal(t, int32(1), f.nudges.Load())
	})
}

func TestTaskRequestsRefused(t *testing.T) {
	f := newServer(t)
	f.addProject(t, site)

	draft := f.task(t, `{"project": "site", "title": "Draft"}`)
	dependent := f.task(t, fmt.Sprintf(`{"project": "site", "title": "Dependent", "depends_on": [%d]}`, draft))

	running := f.task(t, `{"project": "site", "title": "Running"}`)
	f.add(t, running, engine.TaskStarted, struct{}{})
	f.add(t, running, engine.RunStarted, engine.RunStartedPayload{Reason: "initial"})

	asked := f.task(t, `{"project": "site", "title": "Asked"}`)
	f.ask(t, asked, "Which colour?")

	done := f.task(t, `{"project": "site", "title": "Done"}`)
	f.add(t, done, engine.TaskStarted, struct{}{})
	f.add(t, done, engine.Exported, engine.ExportedPayload{HeadCommit: "4b825dc"})
	f.add(t, done, engine.Merged, struct{}{})

	edit := func(id int64) string { return fmt.Sprintf("PATCH /api/tasks/%d", id) }
	act := func(id int64) string { return fmt.Sprintf("POST /api/tasks/%d/actions", id) }

	for _, tc := range []struct {
		name, route, body string
		want              int
	}{
		{"a title with a line break", "POST /api/tasks", `{"project": "site", "title": "Add\na footer"}`, http.StatusBadRequest},
		{"a title of 201 characters", "POST /api/tasks", `{"project": "site", "title": "` + strings.Repeat("x", 201) + `"}`, http.StatusBadRequest},
		{"a project that does not exist", "POST /api/tasks", `{"project": "nope", "title": "Add a footer"}`, http.StatusBadRequest},
		{"a dependency on a missing task", "POST /api/tasks", `{"project": "site", "title": "Add a footer", "depends_on": [99]}`, http.StatusBadRequest},
		{"a dependency on itself", edit(draft), fmt.Sprintf(`{"depends_on": [%d]}`, draft), http.StatusBadRequest},
		{"a dependency that closes a cycle", edit(draft), fmt.Sprintf(`{"depends_on": [%d]}`, dependent), http.StatusBadRequest},
		{"an edit after the run started", edit(running), `{"title": "Something else"}`, http.StatusBadRequest},
		{"an unknown action", act(draft), `{"action": "merge"}`, http.StatusBadRequest},
		{"an answer with no open question", act(running), `{"action": "answer", "text": "Blue."}`, http.StatusBadRequest},
		{"a blank answer", act(asked), `{"action": "answer", "text": " \n "}`, http.StatusBadRequest},
		{"a retry with no stop", act(running), `{"action": "retry"}`, http.StatusBadRequest},
		{"done on a running task", act(running), `{"action": "done"}`, http.StatusBadRequest},
		{"a cancel of a done task", act(done), `{"action": "cancel"}`, http.StatusBadRequest},
		{"an unknown task", "GET /api/tasks/99", "", http.StatusNotFound},
		{"an unknown task, edited", edit(99), `{"title": "Something else"}`, http.StatusNotFound},
		{"an unknown task, acted on", act(99), `{"action": "cancel"}`, http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := f.record(t)

			status, body := f.call(t, tc.route, tc.body)
			require.Equal(t, tc.want, status, body)
			assertError(t, body)
			assert.Equal(t, before, f.record(t), "a refused request writes nothing")
			assert.Zero(t, f.nudges.Load())
		})
	}
}

func TestTaskActions(t *testing.T) {
	f := newServer(t)
	f.addProject(t, site)

	for _, tc := range []struct {
		action, status, event string
		seed                  func(t *testing.T, id int64)
	}{
		{"start", "queued", engine.TaskStarted, func(*testing.T, int64) {}},
		{"retry", "running", engine.HumanRetried, func(t *testing.T, id int64) { f.ask(t, id, "Which colour?") }},
		{"cancel", "cancelled", engine.HumanCancelled, func(*testing.T, int64) {}},
		{"done", "done", engine.Merged, func(t *testing.T, id int64) {
			f.add(t, id, engine.TaskStarted, struct{}{})
			f.add(t, id, engine.Exported, engine.ExportedPayload{HeadCommit: "4b825dc"})
		}},
	} {
		t.Run(tc.action, func(t *testing.T) {
			id := f.task(t, `{"project": "site", "title": "Add a footer"}`)
			tc.seed(t, id)

			before, err := f.store.Events(t.Context(), id, 0, 0)
			require.NoError(t, err)

			status, body := f.call(t, fmt.Sprintf("POST /api/tasks/%d/actions", id), fmt.Sprintf(`{"action": %q}`, tc.action))
			require.Equal(t, http.StatusOK, status, body)
			assert.Equal(t, tc.status, object(t, body)["status"])
			assert.Nil(t, object(t, body)["stop"])

			after, err := f.store.Events(t.Context(), id, 0, 0)
			require.NoError(t, err)
			require.Len(t, after, len(before)+1, "one event is written")
			assert.Equal(t, tc.event, after[len(before)].Type)
			assert.Equal(t, "user", after[len(before)].Actor)
		})
	}
}

// racer is the store the handler's Loop records through. Before the first
// Append it is asked for, it lets another event land through the real store,
// as the scheduler's would between the action's read and its write.
type racer struct {
	*store.Store

	once sync.Once
}

func (r *racer) Append(ctx context.Context, taskID, expectLast int64, events []store.Event, usage []store.Usage) (int64, error) {
	var err error

	// A progress event leaves the question open but moves the newest event id.
	r.once.Do(func() {
		_, err = r.Store.Append(ctx, taskID, -1, []store.Event{{
			TaskID: taskID, Type: engine.AgentText, Actor: "agent", Payload: json.RawMessage(`{"text": "Still here."}`),
		}}, nil)
	})

	if err != nil {
		return 0, err
	}

	return r.Store.Append(ctx, taskID, expectLast, events, usage)
}

func TestActionRacesTheLoop(t *testing.T) {
	f := newServer(t, func(s *store.Store) engine.Store { return &racer{Store: s} })
	f.addProject(t, site)

	id := f.task(t, `{"project": "site", "title": "Pick a colour"}`)
	f.ask(t, id, "Which colour?")

	answer := fmt.Sprintf("POST /api/tasks/%d/actions", id)

	status, body := f.call(t, answer, `{"action": "answer", "text": "Blue."}`)
	require.Equal(t, http.StatusConflict, status, body)
	assertError(t, body)
	assert.Equal(t, 1, f.count(t, id, engine.AgentText), "the other event landed")
	assert.Zero(t, f.count(t, id, engine.HumanAnswered), "the action that lost writes nothing")
	assert.Zero(t, f.nudges.Load())

	status, body = f.call(t, answer, `{"action": "answer", "text": "Blue."}`)
	require.Equal(t, http.StatusOK, status, body)
	assert.Nil(t, object(t, body)["stop"])
	assert.Equal(t, 1, f.count(t, id, engine.HumanAnswered))
	assert.Equal(t, int32(1), f.nudges.Load())
}

func TestTaskViews(t *testing.T) {
	f := newServer(t)
	f.addProject(t, site)

	t.Run("a question stop shows every question", func(t *testing.T) {
		id := f.task(t, `{"project": "site", "title": "Pick a colour"}`)
		f.ask(t, id, "Which colour?", "Which shade?")

		got := f.view(t, id)
		assert.Equal(t, "blocked", got["status"])
		assert.Equal(t, map[string]any{
			"reason": "question", "explanation": "Which colour?", "action": "answer", "resets_at": nil,
		}, got["stop"])
		assert.Equal(t, []any{map[string]any{"question": "Which colour?"}, map[string]any{"question": "Which shade?"}}, got["questions"])
		assert.Empty(t, got["waiting"])
	})

	t.Run("a usage-limit stop shows its reset time", func(t *testing.T) {
		id := f.task(t, `{"project": "site", "title": "Add a footer"}`)
		resets := f.clock.Now().Add(time.Hour).In(time.FixedZone("CET", 3600))

		f.add(t, id, engine.TaskStarted, struct{}{})
		f.add(t, id, engine.RunStarted, engine.RunStartedPayload{Reason: "initial"})
		f.add(t, id, engine.SessionEnded, engine.SessionEndedPayload{Outcome: engine.OutcomeUsageLimit, ResetsAt: resets})
		f.add(t, id, engine.Stopped, engine.StoppedPayload{
			Reason: engine.StopUsageLimit, Explanation: "The usage limit was reached.", Action: engine.ClearNone, ResetsAt: resets,
		})

		got := f.view(t, id)
		assert.Equal(t, "blocked", got["status"])
		assert.Equal(t, map[string]any{
			"reason": "usage_limit", "explanation": "The usage limit was reached.", "action": "none", "resets_at": "2030-01-02T04:04:05Z",
		}, got["stop"])
		assert.Equal(t, []any{}, got["questions"])
		assert.NotEmpty(t, got["waiting"], "the task waits for the reset")
	})

	t.Run("a task waiting on a dependency shows the reason", func(t *testing.T) {
		first := f.task(t, `{"project": "site", "title": "Add a footer"}`)
		then := f.task(t, fmt.Sprintf(`{"project": "site", "title": "Style the footer", "depends_on": [%d], "start": true}`, first))

		got := f.view(t, then)
		assert.Equal(t, "queued", got["status"])
		assert.Equal(t, []any{float64(first)}, got["depends_on"])
		assert.NotEmpty(t, got["waiting"], "with no reason from the scheduler, the wait Decide gives")

		f.waiting.Store(then, engine.WaitingForSlot)
		assert.Equal(t, engine.WaitingForSlot, f.view(t, then)["waiting"], "the scheduler's reason comes first")
		f.waiting.Delete(then)

		f.add(t, first, engine.TaskStarted, struct{}{})
		f.add(t, first, engine.Exported, engine.ExportedPayload{HeadCommit: "4b825dc"})
		f.add(t, first, engine.Merged, struct{}{})
		assert.Empty(t, f.view(t, then)["waiting"], "a done dependency holds nothing")
	})

	t.Run("an exported task shows the change", func(t *testing.T) {
		id := f.task(t, `{"project": "site", "title": "Add a footer"}`)

		f.add(t, id, engine.TaskStarted, struct{}{})
		f.add(t, id, engine.RunStarted, engine.RunStartedPayload{Reason: "initial"})
		f.add(t, id, engine.SessionEnded, engine.SessionEndedPayload{Outcome: engine.OutcomeDone, Changed: true, Turns: 33},
			store.Usage{Source: "proxy", Model: "sonnet", CostEstimateUSD: 0.3}, store.Usage{Source: "agent", Model: "sonnet", CostEstimateUSD: 0.025})
		f.add(t, id, engine.VerifyEnded, engine.VerifyEndedPayload{Result: engine.ResultPassed})
		f.clock.add(226 * time.Second)
		f.add(t, id, engine.Exported, engine.ExportedPayload{
			HeadCommit: "4b825dc", FilesChanged: 3, DiffStat: []string{"footer.html | 12 ++++++"},
		})
		f.add(t, id, engine.RunEnded, engine.RunEndedPayload{Outcome: engine.RunExported})

		got := f.view(t, id)
		assert.Equal(t, "in_review", got["status"])
		assert.Equal(t, map[string]any{
			"head_commit": "4b825dc", "files_changed": 3.0, "diff_stat": []any{"footer.html | 12 ++++++"},
		}, got["change"])
		assert.Nil(t, got["stop"])
		assert.EqualValues(t, 33, got["turns"])
		assert.EqualValues(t, 226000, got["duration_ms"])
		assert.InDelta(t, 0.325, got["cost_estimate_usd"], 1e-9)
	})
}

func TestListFilters(t *testing.T) {
	f := newServer(t)
	f.addProject(t, site)
	f.addProject(t, `{"name": "api", "repo_url": "https://example.com/api.git", "verify_waived": true}`)

	f.task(t, `{"project": "site", "title": "A draft"}`)
	f.task(t, `{"project": "site", "title": "A queued task", "start": true}`)
	f.task(t, `{"project": "api", "title": "Another draft"}`)
	asked := f.task(t, `{"project": "api", "title": "A question"}`)
	f.ask(t, asked, "Which colour?")

	p, err := f.store.ProjectByName(t.Context(), "site")
	require.NoError(t, err)

	shadow, err := f.store.CreateTask(t.Context(), store.Task{ProjectID: p.ID, Title: "A replay's shadow", ReplayID: 1})
	require.NoError(t, err)
	f.add(t, shadow.ID, engine.TaskStarted, struct{}{})

	list := func(t *testing.T, query string) []any {
		t.Helper()

		status, body := f.call(t, "GET /api/tasks"+query, "")
		require.Equal(t, http.StatusOK, status, body)

		tasks, ok := object(t, body)["tasks"].([]any)
		require.True(t, ok, body)

		return tasks
	}

	for _, tc := range []struct {
		query string
		want  []float64
	}{
		{"", []float64{1, 2, 3, 4}},
		{"?project=site", []float64{1, 2}},
		{"?status=draft", []float64{1, 3}},
		{"?status=queued", []float64{2}},
		{"?project=api&status=blocked", []float64{4}},
		{"?status=done", []float64{}},
	} {
		t.Run("filtered by "+tc.query, func(t *testing.T) {
			ids := []float64{}
			for _, task := range list(t, tc.query) {
				ids = append(ids, task.(map[string]any)["id"].(float64))
			}

			assert.Equal(t, tc.want, ids)
		})
	}

	t.Run("an item", func(t *testing.T) {
		stored, err := f.store.Task(t.Context(), asked)
		require.NoError(t, err)

		assert.Equal(t, []any{map[string]any{
			"id": 4.0, "project": "api", "title": "A question", "status": "blocked", "stop_reason": "question",
			"waiting": "", "created_at": rfc3339(stored.CreatedAt),
		}}, list(t, "?status=blocked"))
	})

	for _, query := range []string{"?project=nope", "?status=paused"} {
		t.Run("refused: "+query, func(t *testing.T) {
			status, body := f.call(t, "GET /api/tasks"+query, "")
			require.Equal(t, http.StatusBadRequest, status, body)
			assertError(t, body)
		})
	}
}
