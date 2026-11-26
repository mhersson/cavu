package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/cavu/internal/engine"
	"github.com/mhersson/cavu/internal/store"
)

// bearer sends every request with the token in it.
type bearer string

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+string(b))

	return http.DefaultTransport.RoundTrip(r)
}

// client connects the SDK's own client to /mcp with the API token.
func (f *fixture) client(t *testing.T) *mcp.ClientSession {
	t.Helper()

	c := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)

	cs, err := c.Connect(t.Context(), &mcp.StreamableClientTransport{
		Endpoint: f.url + "/mcp", HTTPClient: &http.Client{Transport: bearer(f.token)}, MaxRetries: -1,
	}, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = cs.Close() })

	return cs
}

// tool calls the tool, which must answer, and decodes its answer.
func tool(t *testing.T, cs *mcp.ClientSession, name string, args any) map[string]any {
	t.Helper()

	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: args})
	require.NoError(t, err)

	b, err := json.Marshal(res)
	require.NoError(t, err)
	require.False(t, res.IsError, string(b))

	b, err = json.Marshal(res.StructuredContent)
	require.NoError(t, err)

	return object(t, string(b))
}

// refused checks that res is a tool result marked as an error, holding one line.
func refused(t *testing.T, res *mcp.CallToolResult) {
	t.Helper()

	require.True(t, res.IsError)
	require.Len(t, res.Content, 1)

	text, ok := res.Content[0].(*mcp.TextContent)
	require.True(t, ok, "%T", res.Content[0])
	assert.NotEmpty(t, text.Text)
	assert.NotContains(t, text.Text, "\n")
}

func TestMCPTools(t *testing.T) {
	cs := newServer(t).client(t)

	caps := cs.InitializeResult().Capabilities
	require.NotNil(t, caps.Tools)
	assert.False(t, caps.Tools.ListChanged, "a client that sees a changing list opens a stream")

	advertised, err := json.Marshal(caps)
	require.NoError(t, err)
	assert.NotContains(t, string(advertised), "logging")

	res, err := cs.ListTools(t.Context(), nil)
	require.NoError(t, err)

	names := []string{}
	for _, tl := range res.Tools {
		names = append(names, tl.Name)
	}

	assert.ElementsMatch(t, []string{"create_task", "get_task", "list_tasks", "run_task", "cancel_task", "answer_task", "list_projects", "edit_task"}, names)
}

// depIDs lists the ids of a task's dependencies.
func depIDs(task map[string]any) []int64 {
	ids := []int64{}
	for _, d := range task["dependencies"].([]any) {
		ids = append(ids, int64(d.(map[string]any)["id"].(float64)))
	}

	return ids
}

func TestMCPEditTask(t *testing.T) {
	f := newServer(t)
	f.addProject(t, site)

	other := f.task(t, `{"project": "site", "title": "Other"}`)
	another := f.task(t, `{"project": "site", "title": "Another"}`)
	id := f.task(t, fmt.Sprintf(`{"project": "site", "title": "Draft", "body": "Original", "depends_on": [%d]}`, other))
	cs := f.client(t)

	edit := func(args map[string]any) map[string]any {
		t.Helper()

		args["id"] = id
		got := tool(t, cs, "edit_task", args)
		assert.Equal(t, f.view(t, id), got, "the task as REST shows it")

		return got
	}

	got := edit(map[string]any{"title": "Renamed"})
	assert.Equal(t, "Renamed", got["title"])
	assert.Equal(t, "Original", got["body"])
	assert.Equal(t, []int64{other}, depIDs(got))

	got = edit(map[string]any{"body": "New body"})
	assert.Equal(t, "Renamed", got["title"])
	assert.Equal(t, "New body", got["body"])
	assert.Equal(t, []int64{other}, depIDs(got))

	got = edit(map[string]any{"body": ""})
	assert.Equal(t, "Renamed", got["title"])
	assert.Empty(t, got["body"])
	assert.Equal(t, []int64{other}, depIDs(got))

	got = edit(map[string]any{"depends_on": []int64{another}})
	assert.Equal(t, "Renamed", got["title"])
	assert.Equal(t, []int64{another}, depIDs(got))

	got = edit(map[string]any{"depends_on": []int64{}})
	assert.Equal(t, "Renamed", got["title"])
	assert.Empty(t, depIDs(got))
}

func TestMCPEditTaskRefused(t *testing.T) {
	f := newServer(t)
	f.addProject(t, site)

	queued := f.task(t, `{"project": "site", "title": "Queued"}`)
	f.add(t, queued, engine.TaskStarted, struct{}{})

	running := f.task(t, `{"project": "site", "title": "Running"}`)
	f.add(t, running, engine.TaskStarted, struct{}{})
	f.add(t, running, engine.RunStarted, engine.RunStartedPayload{Reason: "initial"})

	for _, id := range []int64{queued, running} {
		before := f.record(t)

		res, err := f.client(t).CallTool(t.Context(), &mcp.CallToolParams{Name: "edit_task", Arguments: map[string]any{"id": id, "title": "Changed"}})
		require.NoError(t, err)
		refused(t, res)
		assert.Equal(t, before, f.record(t))
	}
}

func TestMCPCreateAndStart(t *testing.T) {
	f := newServer(t)
	f.addProject(t, site)

	got := tool(t, f.client(t), "create_task", map[string]any{"project": "site", "title": "Add a footer", "start": true})
	assert.Equal(t, "queued", got["status"])

	id := int64(got["id"].(float64))
	stored, err := f.store.Task(t.Context(), id)
	require.NoError(t, err)
	assert.Equal(t, "mcp", stored.Requester)
	assert.Equal(t, 1, f.count(t, id, engine.TaskStarted))
	assert.Equal(t, int32(1), f.nudges.Load())
	assert.Equal(t, f.view(t, id), got, "the task as REST shows it")
}

func TestMCPRunTask(t *testing.T) {
	f := newServer(t)
	f.addProject(t, site)
	cs := f.client(t)

	t.Run("a draft is started", func(t *testing.T) {
		id := f.task(t, `{"project": "site", "title": "Add a footer"}`)
		nudges := f.nudges.Load()

		got := tool(t, cs, "run_task", map[string]any{"id": id})
		assert.Equal(t, "queued", got["status"])
		assert.Equal(t, 1, f.count(t, id, engine.TaskStarted))
		assert.Equal(t, nudges+1, f.nudges.Load())
	})

	t.Run("a task stopped as verify_failed is retried", func(t *testing.T) {
		id := f.task(t, `{"project": "site", "title": "Add a header"}`)
		f.add(t, id, engine.TaskStarted, struct{}{})
		f.add(t, id, engine.RunStarted, engine.RunStartedPayload{Reason: "initial"})
		f.add(t, id, engine.SessionEnded, engine.SessionEndedPayload{Outcome: engine.OutcomeDone, Changed: true})
		f.add(t, id, engine.VerifyEnded, engine.VerifyEndedPayload{Result: engine.ResultFailed})
		f.add(t, id, engine.Stopped, engine.StoppedPayload{
			Reason: engine.StopVerifyFailed, Explanation: "Verify failed.", Action: engine.ClearRetry,
		})

		got := tool(t, cs, "run_task", map[string]any{"id": id})
		assert.Nil(t, got["stop"])
		assert.Equal(t, 1, f.count(t, id, engine.HumanRetried))
	})

	t.Run("a task stopped on a question is refused", func(t *testing.T) {
		id := f.task(t, `{"project": "site", "title": "Pick a colour"}`)
		f.ask(t, id, "Which colour?")

		before, nudges := f.record(t), f.nudges.Load()

		res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "run_task", Arguments: map[string]any{"id": id}})
		require.NoError(t, err)
		refused(t, res)
		assert.Equal(t, before, f.record(t), "no event is written")
		assert.Equal(t, nudges, f.nudges.Load())
	})
}

func TestMCPAnswerTask(t *testing.T) {
	f := newServer(t)
	f.addProject(t, site)
	cs := f.client(t)

	t.Run("a question is answered", func(t *testing.T) {
		id := f.task(t, `{"project": "site", "title": "Pick a colour"}`)
		f.ask(t, id, "Which colour?")
		nudges := f.nudges.Load()

		got := tool(t, cs, "answer_task", map[string]any{"id": id, "answer": "Blue."})
		assert.Nil(t, got["stop"])
		assert.Equal(t, 1, f.count(t, id, engine.HumanAnswered))
		assert.Equal(t, nudges+1, f.nudges.Load())
		assert.Equal(t, f.view(t, id), got, "the task as REST shows it")
	})

	t.Run("a task with no question is refused", func(t *testing.T) {
		id := f.task(t, `{"project": "site", "title": "Add a footer"}`)
		before, nudges := f.record(t), f.nudges.Load()

		res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "answer_task", Arguments: map[string]any{"id": id, "answer": "Blue."}})
		require.NoError(t, err)
		refused(t, res)
		assert.Equal(t, before, f.record(t), "no event is written")
		assert.Equal(t, nudges, f.nudges.Load())
	})
}

func TestMCPCancelAndRead(t *testing.T) {
	f := newServer(t)
	f.addProject(t, site)
	f.addProject(t, web)
	cs := f.client(t)

	id := f.task(t, `{"project": "site", "title": "Add a footer"}`)
	f.task(t, `{"project": "site", "title": "Add a header"}`)

	got := tool(t, cs, "cancel_task", map[string]any{"id": id})
	assert.Equal(t, "cancelled", got["status"])
	assert.Equal(t, 1, f.count(t, id, engine.HumanCancelled))

	got = tool(t, cs, "get_task", map[string]any{"id": id})
	assert.Equal(t, "cancelled", got["status"])
	assert.Equal(t, f.view(t, id), got, "the task as REST shows it")

	list := tool(t, cs, "list_tasks", map[string]any{"status": "cancelled"})
	require.Len(t, list["tasks"], 1)
	assert.EqualValues(t, id, list["tasks"].([]any)[0].(map[string]any)["id"])

	status, body := f.call(t, "GET /api/tasks?status=cancelled", "")
	require.Equal(t, http.StatusOK, status, body)
	assert.Equal(t, object(t, body), list, "the list as REST shows it")

	list = tool(t, cs, "list_tasks", map[string]any{"status": "cancelled,draft"})
	require.Len(t, list["tasks"], 2, "several statuses, separated by commas")

	status, body = f.call(t, "GET /api/tasks?status=cancelled,draft", "")
	require.Equal(t, http.StatusOK, status, body)
	assert.Equal(t, object(t, body), list, "the list as REST shows it")

	assert.Equal(t, map[string]any{"projects": []any{
		map[string]any{"name": "site", "repo_url": "https://example.com/site.git", "base_branch": "main", "runnable": true},
		map[string]any{"name": "web", "repo_url": "https://example.com/web.git", "base_branch": "main", "runnable": false},
	}}, tool(t, cs, "list_projects", nil))
}

func TestMCPListsTheTasksThatNeedYou(t *testing.T) {
	f := newServer(t)
	f.addProject(t, site)

	asked := f.task(t, `{"project": "site", "title": "Pick a colour"}`)
	f.ask(t, asked, "Which colour?")
	f.task(t, `{"project": "site", "title": "Add a footer"}`)

	list := tool(t, f.client(t), "list_tasks", map[string]any{"needs_you": true})
	require.Len(t, list["tasks"], 1)
	assert.EqualValues(t, asked, list["tasks"].([]any)[0].(map[string]any)["id"])

	status, body := f.call(t, "GET /api/tasks?needs_you=true", "")
	require.Equal(t, http.StatusOK, status, body)
	assert.Equal(t, object(t, body), list, "the list as REST shows it")
}

func TestMCPGetTaskShowsTheSame(t *testing.T) {
	f := newServer(t)
	f.addProject(t, site)

	for k, v := range map[string]string{
		"endpoint_mode": "key", "task_budget_usd": "5",
		"model_prices": `{"*": {"input": 1, "output": 10, "cache_read": 0.1, "cache_write": 1.25}}`,
	} {
		require.NoError(t, f.store.SetSetting(t.Context(), k, v))
	}

	id := f.task(t, `{"project": "site", "title": "Add a footer"}`)
	f.reviewed(t, id, engine.ReviewEndedPayload{
		Outcome: engine.ReviewReviewed, Head: "c1", Asked: engine.Verdict{Pass: true},
		Findings: []engine.Finding{{Severity: engine.SeverityImportant, Where: "a.go:10", What: "The footer hides the banner."}},
	}, store.Usage{Source: "proxy", Model: "sonnet", Input: 10000, Output: 40000})

	got := tool(t, f.client(t), "get_task", map[string]any{"id": id})
	assert.NotNil(t, got["review"])
	assert.Contains(t, got["budget"], "usd")
	assert.Equal(t, f.view(t, id), got, "the task as REST shows it")
}

func TestMCPNeedsABearerToken(t *testing.T) {
	f := newServer(t)
	session := f.login(t).Value

	withToken := func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+f.token) }

	for _, tc := range []struct {
		name, method string
		add          func(r *http.Request)
		want         int
	}{
		{"no token", http.MethodPost, func(*http.Request) {}, http.StatusUnauthorized},
		{"a valid session cookie alone", http.MethodPost, func(r *http.Request) { withCookie(r, session, false) }, http.StatusUnauthorized},
		{"a valid session cookie and the CSRF header", http.MethodPost, func(r *http.Request) {
			withCookie(r, session, true)
		}, http.StatusUnauthorized},
		{"an unknown token beside a valid cookie", http.MethodPost, func(r *http.Request) {
			withCookie(r, session, true)
			r.Header.Set("Authorization", "Bearer cavu_"+strings.Repeat("A", 43))
		}, http.StatusUnauthorized},
		{"the token", http.MethodPost, withToken, http.StatusOK},
		{"the token on a GET, which only the MCP server refuses", http.MethodGet, withToken, http.StatusMethodNotAllowed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := f.request(t, tc.method+" /mcp", `{"jsonrpc": "2.0", "id": 1, "method": "tools/list"}`)
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Accept", "application/json, text/event-stream")
			tc.add(r)

			resp, body := send(t, r)
			require.Equal(t, tc.want, resp.StatusCode, body)

			if tc.want == http.StatusUnauthorized {
				assertError(t, body)
				assert.Empty(t, resp.Cookies(), "a refused request sets no cookie")
			}
		})
	}
}

func TestMCPRequestsRefused(t *testing.T) {
	f := newServer(t)
	f.addProject(t, site)
	f.addProject(t, web)

	for _, tc := range []struct {
		name string
		args map[string]any
		sdk  bool // refused by the SDK before the tool runs, in a form the test leaves open
	}{
		{"a body over 1 MiB", map[string]any{"project": "site", "title": "Add a footer", "body": strings.Repeat("a", 1<<20)}, true},
		{"an unknown argument", map[string]any{"project": "site", "title": "Add a footer", "requester": "api"}, true},
		{"a title with a line break", map[string]any{"project": "site", "title": "Add\na footer"}, false},
		{"a dependency on a missing task", map[string]any{"project": "site", "title": "Add a footer", "depends_on": []int{99}}, false},
		{"a project that does not exist", map[string]any{"project": "nope", "title": "Add a footer"}, false},
		{"a start in a project with no baseline", map[string]any{"project": "web", "title": "Add a footer", "start": true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := f.record(t)

			res, err := f.client(t).CallTool(t.Context(), &mcp.CallToolParams{Name: "create_task", Arguments: tc.args})
			if tc.sdk {
				assert.True(t, err != nil || res.IsError, "refused")
			} else {
				require.NoError(t, err)
				refused(t, res)
			}

			assert.Equal(t, before, f.record(t), "a refused call writes nothing")
			assert.Zero(t, f.nudges.Load())
		})
	}
}

// failing is the store the handler's Loop reads through, failing as a broken disk would.
type failing struct{ *store.Store }

var errDisk = errors.New("disk I/O error in /var/lib/cavu/cavu.db")

func (failing) LastEventID(context.Context, int64) (int64, error) { return 0, errDisk }

func TestMCPInternalErrorsHoldNoDetail(t *testing.T) {
	f := newServer(t, func(s *store.Store) engine.Store { return failing{s} })
	f.addProject(t, site)

	p, err := f.store.ProjectByName(t.Context(), "site")
	require.NoError(t, err)

	task, err := f.store.CreateTask(t.Context(), store.Task{ProjectID: p.ID, Title: "Add a footer"})
	require.NoError(t, err)

	res, err := f.client(t).CallTool(t.Context(), &mcp.CallToolParams{Name: "get_task", Arguments: map[string]any{"id": task.ID}})
	require.NoError(t, err)
	refused(t, res)
	assert.NotContains(t, res.Content[0].(*mcp.TextContent).Text, errDisk.Error())
}
