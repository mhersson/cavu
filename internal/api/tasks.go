package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"

	"github.com/mhersson/cavu/internal/engine"
	"github.com/mhersson/cavu/internal/store"
)

// statuses is the status scale a list filters on.
var statuses = []engine.Status{
	engine.StatusDraft, engine.StatusQueued, engine.StatusRunning, engine.StatusBlocked,
	engine.StatusInReview, engine.StatusDone, engine.StatusCancelled,
}

// taskList is a list of tasks as the API shows it.
type taskList struct {
	Tasks []engine.Row `json:"tasks"`
}

func (s *server) tasks(w http.ResponseWriter, r *http.Request) error {
	q := r.URL.Query()
	in := taskQuery{Project: q.Get("project"), Status: engine.Status(q.Get("status"))}

	if v := q.Get("needs_you"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return &apiError{http.StatusBadRequest, fmt.Sprintf("needs_you %q: want true or false", v)}
		}

		in.NeedsYou = b
	}

	l, err := s.listTasks(r.Context(), in)
	if err != nil {
		return err
	}

	reply(w, http.StatusOK, l)

	return nil
}

// listTasks lists every task but a replay's shadows, oldest first, of the
// live project, with the status and needing a person, when the query asks.
func (s *server) listTasks(ctx context.Context, in taskQuery) (taskList, error) {
	var projectID int64

	if in.Project != "" {
		p, err := s.Store.ProjectByName(ctx, in.Project)
		if errors.Is(err, store.ErrNotFound) {
			return taskList{}, &apiError{http.StatusBadRequest, fmt.Sprintf("project %q does not exist", in.Project)}
		}

		if err != nil {
			return taskList{}, err
		}

		projectID = p.ID
	}

	if in.Status != "" && !slices.Contains(statuses, in.Status) {
		return taskList{}, &apiError{http.StatusBadRequest, fmt.Sprintf("status %q: want one of %v", in.Status, statuses)}
	}

	rows, err := s.Loop.List(ctx, projectID, s.Waiting)
	if err != nil {
		return taskList{}, err
	}

	return taskList{Tasks: slices.DeleteFunc(rows, func(r engine.Row) bool {
		return (in.Status != "" && r.Status != in.Status) || (in.NeedsYou && !r.NeedsYou)
	})}, nil
}

// newTask is what a request gives a new task, over REST and MCP. The optional
// fields are omitempty, so that the schema MCP infers does not require them.
type newTask struct {
	Project   string  `json:"project" jsonschema:"the name of the task's project"`
	Title     string  `json:"title" jsonschema:"one line of at most 200 characters"`
	Body      string  `json:"body,omitempty" jsonschema:"what the task asks for, in full"`
	DependsOn []int64 `json:"depends_on,omitempty" jsonschema:"the ids of the tasks that must be done before this one runs"`
	Start     bool    `json:"start,omitempty" jsonschema:"start the task as well, which its project must allow"`
}

func (s *server) createTask(w http.ResponseWriter, r *http.Request) error {
	var in newTask
	if err := decode(w, r, &in); err != nil {
		return err
	}

	id, err := s.addTask(r.Context(), in, "api")
	if err != nil {
		return err
	}

	return s.replyTask(w, r, id, http.StatusCreated)
}

// addTask stores the task with the requester, and starts it when in asks. A
// start the project cannot take creates nothing.
func (s *server) addTask(ctx context.Context, in newTask, requester string) (int64, error) {
	p, err := s.Store.ProjectByName(ctx, in.Project)
	if errors.Is(err, store.ErrNotFound) {
		return 0, &apiError{http.StatusBadRequest, fmt.Sprintf("project %q does not exist", in.Project)}
	}

	if err != nil {
		return 0, fmt.Errorf("project %q: %w", in.Project, err)
	}

	t, err := s.Loop.AddTask(ctx, store.Task{
		ProjectID: p.ID, Title: in.Title, Body: in.Body, Requester: requester,
	}, in.DependsOn, in.Start)
	if errors.Is(err, engine.ErrNoBaseline) {
		return 0, &apiError{http.StatusBadRequest, fmt.Sprintf("%v; check the project, or waive its verify", err)}
	}

	if err != nil {
		return 0, err
	}

	if in.Start {
		s.Nudge()
	}

	return t.ID, nil
}

func (s *server) task(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}

	return s.replyTask(w, r, id, http.StatusOK)
}

// editTask changes only the fields the body names, over the stored task and
// its stored dependencies.
func (s *server) editTask(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Title     *string  `json:"title"`
		Body      *string  `json:"body"`
		DependsOn *[]int64 `json:"depends_on"`
	}

	if err := decode(w, r, &in); err != nil {
		return err
	}

	id, err := pathID(r)
	if err != nil {
		return err
	}

	t, err := s.Store.Task(r.Context(), id)
	if err != nil {
		return fmt.Errorf("task %d: %w", id, err)
	}

	deps, err := s.Store.TaskDeps(r.Context(), id)
	if err != nil {
		return fmt.Errorf("task %d: %w", id, err)
	}

	set(&t.Title, in.Title)
	set(&t.Body, in.Body)
	set(&deps, in.DependsOn)

	if err := engine.EditTask(r.Context(), s.Store, t, deps); err != nil {
		return err
	}

	return s.replyTask(w, r, id, http.StatusOK)
}

func (s *server) act(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Action string `json:"action"`
		Text   string `json:"text"` // the answer
	}

	if err := decode(w, r, &in); err != nil {
		return err
	}

	id, err := pathID(r)
	if err != nil {
		return err
	}

	v, err := s.perform(r.Context(), id, in.Action, in.Text)
	if err != nil {
		return err
	}

	reply(w, http.StatusOK, v)

	return nil
}

// perform records a person's action, named by action, through the engine,
// which refuses one the task's state makes meaningless, and one that lost to a
// write since its read. It gives the task as it is then.
func (s *server) perform(ctx context.Context, id int64, action, text string) (engine.View, error) {
	var err error

	switch action {
	case "start":
		err = s.Loop.Start(ctx, id)
	case "answer":
		err = s.Loop.Answer(ctx, id, text)
	case "retry":
		err = s.Loop.Retry(ctx, id)
	case "cancel":
		err = s.Loop.Cancel(ctx, id)
	case "done":
		err = s.Loop.Done(ctx, id)
	default:
		return engine.View{}, &apiError{http.StatusBadRequest, fmt.Sprintf("action %q: want start, answer, retry, cancel or done", action)}
	}

	if err != nil {
		return engine.View{}, err
	}

	s.Nudge()

	return s.Loop.View(ctx, id, s.Waiting)
}

// replyTask answers with the task as it is now.
func (s *server) replyTask(w http.ResponseWriter, r *http.Request, id int64, status int) error {
	v, err := s.Loop.View(r.Context(), id, s.Waiting)
	if err != nil {
		return err
	}

	reply(w, status, v)

	return nil
}
