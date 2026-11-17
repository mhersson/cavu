package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mhersson/cavu/internal/engine"
)

// taskRef names a task.
type taskRef struct {
	ID int64 `json:"id" jsonschema:"the task's id"`
}

// taskQuery narrows a list of tasks.
type taskQuery struct {
	Project string        `json:"project,omitempty" jsonschema:"keep the tasks of the project with this name"`
	Status  engine.Status `json:"status,omitempty" jsonschema:"keep the tasks with this status: draft, queued, running, blocked, in_review, done or cancelled"`
}

type projectList struct {
	Projects []projectItem `json:"projects"`
}

type projectItem struct {
	Name     string `json:"name"`
	Runnable bool   `json:"runnable"`
}

// tools returns the handler of the MCP tools. It is stateless, so it keeps no
// session between requests, and answers in JSON. Each tool calls what the
// matching REST handler calls, and gives a task as REST gives it.
func (s *server) tools() http.Handler {
	srv := mcp.NewServer(&mcp.Implementation{Name: "cavu", Version: s.Version}, nil)

	addTool(srv, "create_task", "Create a draft task in a project; with start, start it as well.",
		func(ctx context.Context, in newTask) (taskJSON, error) {
			id, err := s.addTask(ctx, in, "mcp")
			if err != nil {
				return taskJSON{}, err
			}

			return s.taskView(ctx, id)
		})
	addTool(srv, "get_task", "Show a task: its status, its open stop and the action that clears it, its questions and its change.",
		func(ctx context.Context, in taskRef) (taskJSON, error) { return s.taskView(ctx, in.ID) })
	addTool(srv, "list_tasks", "List the tasks, oldest first; a project and a status, when given, narrow the list.",
		func(ctx context.Context, in taskQuery) (taskList, error) {
			return s.listTasks(ctx, in.Project, in.Status)
		})
	addTool(srv, "run_task", "Start a draft task, or retry a stopped one; a task stopped on a question needs an answer instead.",
		s.runTask)
	addTool(srv, "cancel_task", "Cancel a task.",
		func(ctx context.Context, in taskRef) (taskJSON, error) { return s.perform(ctx, in.ID, "cancel", "") })
	addTool(srv, "list_projects", "List the projects, and whether each is runnable: whether its tasks may start.",
		s.listProjects)

	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, &mcp.StreamableHTTPOptions{
		Stateless: true, JSONResponse: true, MaxRequestBodyBytes: maxBody,
		// A page holds no bearer token, which defeats DNS rebinding; the protection would
		// refuse a TLS proxy on this host, which forwards its public Host to 127.0.0.1.
		DisableLocalhostProtection: true,
	})
}

// addTool adds the tool that do serves. The SDK gives do's error to the client
// as a result marked as an error, so it is first cut to the line REST answers.
func addTool[In, Out any](srv *mcp.Server, name, description string, do func(context.Context, In) (Out, error)) {
	mcp.AddTool(srv, &mcp.Tool{Name: name, Description: description},
		func(ctx context.Context, _ *mcp.CallToolRequest, in In) (*mcp.CallToolResult, Out, error) {
			out, err := do(ctx, in)
			if err != nil {
				_, line := classify(ctx, "/mcp "+name, err)
				err = errors.New(line)
			}

			return nil, out, err
		})
}

// runTask records a start for a task with no open stop, which the engine
// refuses for a cancelled task and leaves alone for a started one, and else a
// retry. A retry would leave a question unanswered, so that stop is refused.
func (s *server) runTask(ctx context.Context, in taskRef) (taskJSON, error) {
	v, err := s.taskView(ctx, in.ID)
	if err != nil {
		return taskJSON{}, err
	}

	action := "start"

	if v.Stop != nil {
		if v.Stop.Action == engine.ClearAnswer {
			return taskJSON{}, &apiError{http.StatusBadRequest, fmt.Sprintf(
				"task %d stopped on a question and needs an answer: the action answer of POST /api/tasks/%d/actions, or cavu task answer %d",
				in.ID, in.ID, in.ID)}
		}

		action = "retry"
	}

	return s.perform(ctx, in.ID, action, "")
}

func (s *server) listProjects(ctx context.Context, _ struct{}) (projectList, error) {
	ps, err := s.Store.Projects(ctx)
	if err != nil {
		return projectList{}, err
	}

	l := projectList{Projects: make([]projectItem, len(ps))}
	for i, p := range ps {
		l.Projects[i] = projectItem{p.Name, engine.Runnable(p)}
	}

	return l, nil
}
