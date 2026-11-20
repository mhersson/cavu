package api

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"slices"
	"strings"

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

	var err error
	if in.NeedsYou, err = boolean("needs_you", q.Get("needs_you"), false); err != nil {
		return err
	}

	limit, err := number("limit", q.Get("limit"))
	if err != nil {
		return err
	}

	newest, err := newestFirst(q.Get("order"))
	if err != nil {
		return err
	}

	l, err := s.listTasks(r.Context(), in, int(limit), newest)
	if err != nil {
		return err
	}

	reply(w, http.StatusOK, l)

	return nil
}

// listTasks lists the tasks the query keeps, of the live project, with any of
// the statuses separated by commas: oldest first, or newest first with newest,
// and at most limit of them when it is not 0.
func (s *server) listTasks(ctx context.Context, in taskQuery, limit int, newest bool) (taskList, error) {
	q := engine.ListQuery{NeedsYou: in.NeedsYou, Newest: newest, Limit: limit}

	if in.Project != "" {
		p, err := s.Store.ProjectByName(ctx, in.Project)
		if errors.Is(err, store.ErrNotFound) {
			return taskList{}, &apiError{http.StatusBadRequest, fmt.Sprintf("project %q does not exist", in.Project)}
		}

		if err != nil {
			return taskList{}, err
		}

		q.ProjectID = p.ID
	}

	if in.Status != "" {
		for st := range strings.SplitSeq(string(in.Status), ",") {
			if !slices.Contains(statuses, engine.Status(st)) {
				return taskList{}, &apiError{http.StatusBadRequest, fmt.Sprintf("status %q: want one of %v, or several separated by commas", st, statuses)}
			}

			q.Statuses = append(q.Statuses, engine.Status(st))
		}
	}

	rows, err := s.Loop.List(ctx, q, s.Waiting)
	if err != nil {
		return taskList{}, err
	}

	return taskList{Tasks: rows}, nil
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

// change hands over the task's exported change, as a diff or a git bundle,
// streamed from the file the export wrote.
func (s *server) change(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}

	format := r.URL.Query().Get("format")
	if format != "diff" && format != "bundle" {
		return &apiError{http.StatusBadRequest, `format is required: "diff" or "bundle"`}
	}

	v, err := s.Loop.View(r.Context(), id, nil)
	if err != nil {
		return err
	}

	if v.Change == nil {
		return &apiError{http.StatusNotFound, "the task shows no change"}
	}

	path, typ := v.Change.DiffPath, "text/x-diff"
	if format == "bundle" {
		path, typ = v.Change.BundlePath, "application/octet-stream"
	}

	file, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return &apiError{http.StatusNotFound, "the change's file is gone"}
	}

	if err != nil {
		return err
	}

	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return err
	}

	w.Header().Set("Content-Type", typ)
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="task-%d.%s"`, id, format))
	http.ServeContent(w, r, "", info.ModTime(), file)

	return nil
}

// taskEdit names the fields of a task a person may change; one left out keeps its value.
type taskEdit struct {
	Title     *string  `json:"title,omitempty" jsonschema:"the new title"`
	Body      *string  `json:"body,omitempty" jsonschema:"the new body; an empty string empties it"`
	DependsOn *[]int64 `json:"depends_on,omitempty" jsonschema:"the ids of the tasks it waits for, replacing the list; an empty list removes every dependency"`
}

func (s *server) editTask(w http.ResponseWriter, r *http.Request) error {
	var in taskEdit

	if err := decode(w, r, &in); err != nil {
		return err
	}

	id, err := pathID(r)
	if err != nil {
		return err
	}

	if err := s.edit(r.Context(), id, in); err != nil {
		return err
	}

	return s.replyTask(w, r, id, http.StatusOK)
}

// edit changes only the fields that `in` sets, over the stored task and its dependencies.
func (s *server) edit(ctx context.Context, id int64, in taskEdit) error {
	t, err := s.Store.Task(ctx, id)
	if err != nil {
		return fmt.Errorf("task %d: %w", id, err)
	}

	deps, err := s.Store.TaskDeps(ctx, id)
	if err != nil {
		return fmt.Errorf("task %d: %w", id, err)
	}

	set(&t.Title, in.Title)
	set(&t.Body, in.Body)
	set(&deps, in.DependsOn)

	return engine.EditTask(ctx, s.Store, t, deps)
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
