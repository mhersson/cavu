package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/mhersson/cavu/internal/engine"
	"github.com/mhersson/cavu/internal/store"
)

// taskJSON is a task as the API shows it. Its lists are never nil, so they go
// out as [], never null.
type taskJSON struct {
	ID              int64          `json:"id"`
	Project         string         `json:"project"`
	Title           string         `json:"title"`
	Body            string         `json:"body"`
	Requester       string         `json:"requester"`
	CreatedAt       time.Time      `json:"created_at"`
	Status          engine.Status  `json:"status"`
	Waiting         string         `json:"waiting"`
	DependsOn       []int64        `json:"depends_on"`
	Stop            *stopJSON      `json:"stop"`
	Questions       []questionJSON `json:"questions"`
	Change          *changeJSON    `json:"change"`
	Turns           int            `json:"turns"`
	DurationMS      int64          `json:"duration_ms"`
	CostEstimateUSD float64        `json:"cost_estimate_usd"`
}

type stopJSON struct {
	Reason      string     `json:"reason"`
	Explanation string     `json:"explanation"`
	Action      string     `json:"action"`
	ResetsAt    *time.Time `json:"resets_at"`
}

type questionJSON struct {
	Question string `json:"question"`
}

type changeJSON struct {
	HeadCommit   string   `json:"head_commit"`
	FilesChanged int      `json:"files_changed"`
	DiffStat     []string `json:"diff_stat"`
}

// taskItemJSON is a task in a list.
type taskItemJSON struct {
	ID         int64         `json:"id"`
	Project    string        `json:"project"`
	Title      string        `json:"title"`
	Status     engine.Status `json:"status"`
	StopReason string        `json:"stop_reason"`
	Waiting    string        `json:"waiting"`
	CreatedAt  time.Time     `json:"created_at"`
}

// statuses is the status scale a list filters on.
var statuses = []engine.Status{
	engine.StatusDraft, engine.StatusQueued, engine.StatusRunning, engine.StatusBlocked,
	engine.StatusInReview, engine.StatusDone, engine.StatusCancelled,
}

// taskView renders the task with id.
func (s *server) taskView(ctx context.Context, id int64) (taskJSON, error) {
	snap, err := s.Loop.Snapshot(ctx, id)
	if err != nil {
		return taskJSON{}, err
	}

	t, f := snap.Task, snap.Facts

	p, err := s.Store.Project(ctx, t.ProjectID)
	if err != nil {
		return taskJSON{}, fmt.Errorf("task %d: project %d: %w", id, t.ProjectID, err)
	}

	events, err := s.Store.EventsOmitting(ctx, id, engine.ProgressTypes...)
	if err != nil {
		return taskJSON{}, fmt.Errorf("task %d: %w", id, err)
	}

	usage, err := s.Store.UsageTotals(ctx, id)
	if err != nil {
		return taskJSON{}, fmt.Errorf("task %d: %w", id, err)
	}

	turns, d := engine.TurnsAndDuration(events)
	v := taskJSON{
		ID: t.ID, Project: p.Name, Title: t.Title, Body: t.Body, Requester: t.Requester, CreatedAt: t.CreatedAt.UTC(),
		Status: engine.StatusOf(f), Waiting: s.waiting(snap), DependsOn: []int64{}, Questions: []questionJSON{},
		Turns: turns, DurationMS: d.Milliseconds(),
	}

	for _, dep := range snap.Deps {
		v.DependsOn = append(v.DependsOn, dep.TaskID)
	}

	if st := f.OpenStop; st != nil {
		// The facts keep no stop's action, so it is read from the stop's event.
		sp, _ := newest[engine.StoppedPayload](events, engine.Stopped)
		v.Stop = &stopJSON{Reason: st.Reason, Explanation: st.Explanation, Action: sp.Action}

		if !st.ResetsAt.IsZero() {
			v.Stop.ResetsAt = new(st.ResetsAt.UTC())
		}

		// The stop's explanation holds only the first question.
		if st.Reason == engine.StopQuestion && f.Session != nil {
			for _, q := range f.Session.Questions {
				v.Questions = append(v.Questions, questionJSON{q})
			}
		}
	}

	if x, ok := newest[engine.ExportedPayload](events, engine.Exported); ok {
		v.Change = &changeJSON{HeadCommit: x.HeadCommit, FilesChanged: x.FilesChanged, DiffStat: append([]string{}, x.DiffStat...)}
	}

	for _, u := range usage {
		v.CostEstimateUSD += u.CostEstimateUSD
	}

	return v, nil
}

// waiting is why the task is not acting: the scheduler's reason, which also
// knows the other tasks' pauses, else the wait Decide gives on snap, else "".
func (s *server) waiting(snap engine.Snapshot) string {
	if why := s.Waiting(snap.Task.ID); why != "" {
		return why
	}

	if a := engine.Decide(snap); a.Kind == engine.KindWait {
		return a.Why
	}

	return ""
}

// newest decodes the payload of the newest event of typ, if there is one.
func newest[T any](events []store.Event, typ string) (T, bool) {
	var v T

	for _, e := range slices.Backward(events) {
		if e.Type == typ {
			return v, json.Unmarshal(e.Payload, &v) == nil
		}
	}

	return v, false
}

// tasks lists every task but a replay's shadows, oldest first, of the project
// and with the status the query names, when it names them.
func (s *server) tasks(w http.ResponseWriter, r *http.Request) error {
	ps, err := s.Store.Projects(r.Context())
	if err != nil {
		return err
	}

	names := make(map[int64]string, len(ps))
	for _, p := range ps {
		names[p.ID] = p.Name
	}

	q := r.URL.Query()
	project, status := q.Get("project"), engine.Status(q.Get("status"))

	if project != "" && !slices.ContainsFunc(ps, func(p store.Project) bool { return p.Name == project }) {
		return &apiError{http.StatusBadRequest, fmt.Sprintf("project %q does not exist", project)}
	}

	if status != "" && !slices.Contains(statuses, status) {
		return &apiError{http.StatusBadRequest, fmt.Sprintf("status %q: want one of %v", status, statuses)}
	}

	ts, err := s.Store.Tasks(r.Context(), false)
	if err != nil {
		return err
	}

	out := []taskItemJSON{}

	for _, t := range ts {
		if project != "" && names[t.ProjectID] != project {
			continue
		}

		snap, err := s.Loop.Snapshot(r.Context(), t.ID)
		if err != nil {
			return err
		}

		item := taskItemJSON{
			ID: t.ID, Project: names[t.ProjectID], Title: t.Title, Status: engine.StatusOf(snap.Facts),
			Waiting: s.waiting(snap), CreatedAt: t.CreatedAt.UTC(),
		}

		if status != "" && item.Status != status {
			continue
		}

		if st := snap.Facts.OpenStop; st != nil {
			item.StopReason = st.Reason
		}

		out = append(out, item)
	}

	reply(w, http.StatusOK, map[string][]taskItemJSON{"tasks": out})

	return nil
}

// createTask asks whether the project is runnable before it stores the task,
// so a start that would be refused creates nothing.
func (s *server) createTask(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Project   string  `json:"project"`
		Title     string  `json:"title"`
		Body      string  `json:"body"`
		DependsOn []int64 `json:"depends_on"`
		Start     bool    `json:"start"`
	}

	if err := decode(w, r, &in); err != nil {
		return err
	}

	p, err := s.Store.ProjectByName(r.Context(), in.Project)
	if errors.Is(err, store.ErrNotFound) {
		return &apiError{http.StatusBadRequest, fmt.Sprintf("project %q does not exist", in.Project)}
	}

	if err != nil {
		return fmt.Errorf("project %q: %w", in.Project, err)
	}

	if in.Start && !engine.Runnable(p) {
		return &apiError{http.StatusBadRequest, fmt.Sprintf(
			"project %q has %v; check the project, or waive its verify", p.Name, engine.ErrNoBaseline)}
	}

	t, err := engine.AddTask(r.Context(), s.Store, store.Task{
		ProjectID: p.ID, Title: in.Title, Body: in.Body, Requester: "api",
	}, in.DependsOn)
	if err != nil {
		return err
	}

	if in.Start {
		if err := s.Loop.Start(r.Context(), t.ID); err != nil {
			return err
		}

		s.Nudge()
	}

	return s.replyTask(w, r, t.ID, http.StatusCreated)
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

// act records a person's action through the engine, which refuses one the
// task's state makes meaningless, and one that lost to a write since its read.
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

	switch ctx := r.Context(); in.Action {
	case "start":
		err = s.Loop.Start(ctx, id)
	case "answer":
		err = s.Loop.Answer(ctx, id, in.Text)
	case "retry":
		err = s.Loop.Retry(ctx, id)
	case "cancel":
		err = s.Loop.Cancel(ctx, id)
	case "done":
		err = s.Loop.Done(ctx, id)
	default:
		return &apiError{http.StatusBadRequest, fmt.Sprintf("action %q: want start, answer, retry, cancel or done", in.Action)}
	}

	if err != nil {
		return err
	}

	s.Nudge()

	return s.replyTask(w, r, id, http.StatusOK)
}

// replyTask answers with the task as it is now.
func (s *server) replyTask(w http.ResponseWriter, r *http.Request, id int64, status int) error {
	v, err := s.taskView(r.Context(), id)
	if err != nil {
		return err
	}

	reply(w, status, v)

	return nil
}
