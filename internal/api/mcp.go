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

// taskAnswer answers the question a task stopped on.
type taskAnswer struct {
	ID     int64  `json:"id" jsonschema:"the task's id"`
	Answer string `json:"answer" jsonschema:"the answer to the task's questions"`
}

type projectList struct {
	Projects []projectItem `json:"projects"`
}

type projectItem struct {
	Name       string `json:"name"`
	RepoURL    string `json:"repo_url"`
	BaseBranch string `json:"base_branch"`
	Runnable   bool   `json:"runnable"`
}

// tools returns the handler of the MCP tools. It is stateless, so it keeps no
// session between requests, and answers in JSON. Each tool calls what the
// matching REST handler calls, and gives a task as REST gives it.
func (s *server) tools() http.Handler {
	// The SDK's defaults advertise listChanged and logging, which makes a current
	// client open a stream that lasts its whole connection. The list never changes.
	srv := mcp.NewServer(&mcp.Implementation{Name: "cavu", Version: s.Version},
		&mcp.ServerOptions{Capabilities: &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{}}})

	addTool(srv, "create_task", "Create a draft task in a project; with start, start it as well. The agent starts from the project's base branch, never from the caller's working tree or unpushed commits. The title and body appear in the pull request CAVU opens.",
		func(ctx context.Context, in newTask) (taskJSON, error) {
			id, err := s.addTask(ctx, in, "mcp")
			if err != nil {
				return taskJSON{}, err
			}

			return s.taskView(ctx, id)
		})
	addTool(srv, "get_task", "Show a task: its status, its open stop and the action that clears it, its questions, its change, its review and its budget.",
		func(ctx context.Context, in taskRef) (taskJSON, error) { return s.taskView(ctx, in.ID) })
	addTool(srv, "list_tasks", "List the tasks, oldest first; a project and a status, when given, narrow the list.",
		func(ctx context.Context, in taskQuery) (taskList, error) {
			return s.listTasks(ctx, in.Project, in.Status)
		})
	addTool(srv, "run_task", "Start a draft task, or retry a stopped one; a task stopped on a question needs an answer instead.",
		s.runTask)
	addTool(srv, "cancel_task", "Cancel a task.",
		func(ctx context.Context, in taskRef) (taskJSON, error) { return s.perform(ctx, in.ID, "cancel", "") })
	addTool(srv, "answer_task", "Answer the questions a task stopped on; any other task is refused.",
		s.answerTask)
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
				"task %d stopped on a question and needs an answer: the tool answer_task, the action answer of POST /api/tasks/%d/actions, or cavu task answer %d",
				in.ID, in.ID, in.ID)}
		}

		action = "retry"
	}

	return s.perform(ctx, in.ID, action, "")
}

// answerTask answers the open question of a task. The engine refuses a task with
// none too; this line names the stop it has, which the engine's does not.
func (s *server) answerTask(ctx context.Context, in taskAnswer) (taskJSON, error) {
	v, err := s.taskView(ctx, in.ID)
	if err != nil {
		return taskJSON{}, err
	}

	if v.Stop == nil || v.Stop.Action != engine.ClearAnswer {
		have := "no open stop"
		if v.Stop != nil {
			have = "the open stop " + v.Stop.Reason
		}

		return taskJSON{}, &apiError{http.StatusBadRequest, fmt.Sprintf("task %d has %s, not a question to answer", in.ID, have)}
	}

	return s.perform(ctx, in.ID, "answer", in.Answer)
}

func (s *server) listProjects(ctx context.Context, _ struct{}) (projectList, error) {
	ps, err := s.Store.Projects(ctx)
	if err != nil {
		return projectList{}, err
	}

	l := projectList{Projects: make([]projectItem, len(ps))}
	for i, p := range ps {
		l.Projects[i] = projectItem{p.Name, p.RepoURL, p.BaseBranch, engine.Runnable(p)}
	}

	return l, nil
}
