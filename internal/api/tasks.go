package api

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"slices"
	"time"

	"github.com/mhersson/cavu/internal/engine"
	"github.com/mhersson/cavu/internal/store"
)

// taskJSON is a task as the API shows it. Its lists are never nil, so they go
// out as [], never null.
type taskJSON struct {
	ID                  int64            `json:"id"`
	Project             string           `json:"project"`
	Title               string           `json:"title"`
	Body                string           `json:"body"`
	Requester           string           `json:"requester"`
	CreatedAt           time.Time        `json:"created_at"`
	Status              engine.Status    `json:"status"`
	Waiting             string           `json:"waiting"`
	DependsOn           []int64          `json:"depends_on"`
	Stop                *stopJSON        `json:"stop"`
	Questions           []questionJSON   `json:"questions"`
	Review              *reviewJSON      `json:"review"`
	PullRequest         *pullRequestJSON `json:"pull_request"`
	Change              *changeJSON      `json:"change"`
	Turns               int              `json:"turns"`
	DurationMS          int64            `json:"duration_ms"`
	CostEstimateUSD     float64          `json:"cost_estimate_usd"`
	CostMissingSessions int              `json:"cost_missing_sessions"`
	Budget              budgetJSON       `json:"budget"`
}

type stopJSON struct {
	Reason      string     `json:"reason"`
	Explanation string     `json:"explanation"`
	Action      string     `json:"action"`
	ResetsAt    *time.Time `json:"resets_at"`
}

type questionJSON struct {
	Question        string `json:"question"`
	SuggestedAnswer string `json:"suggested_answer"`
}

type reviewJSON struct {
	Rounds int              `json:"rounds"`
	Failed string           `json:"failed"`
	Open   []engine.Finding `json:"open"`
	Minor  []engine.Finding `json:"minor"`
}

// budgetJSON leaves the money out unless the task's own money cap is in force,
// so that it never shows a limit nobody set.
type budgetJSON struct {
	Turns      int      `json:"turns"`
	MaxTurns   int      `json:"max_turns"`
	Minutes    int      `json:"minutes"`
	MaxMinutes int      `json:"max_minutes"`
	USD        *float64 `json:"usd,omitempty"`
	MaxUSD     *float64 `json:"max_usd,omitempty"`
}

type pullRequestJSON struct {
	Number       int      `json:"number"`
	URL          string   `json:"url"`
	Branch       string   `json:"branch"`
	Head         string   `json:"head"`
	State        string   `json:"state"`
	Ready        bool     `json:"ready"`
	Checks       string   `json:"checks"`
	FailedChecks []string `json:"failed_checks"`
}

type changeJSON struct {
	HeadCommit   string         `json:"head_commit"`
	FilesChanged int            `json:"files_changed"`
	DiffStat     []string       `json:"diff_stat"`
	Flagged      engine.Flagged `json:"flagged"`
}

// taskItemJSON is a task in a list.
type taskItemJSON struct {
	ID               int64         `json:"id"`
	Project          string        `json:"project"`
	Title            string        `json:"title"`
	Status           engine.Status `json:"status"`
	StopReason       string        `json:"stop_reason"`
	Waiting          string        `json:"waiting"`
	PullRequestReady bool          `json:"pull_request_ready"`
	CreatedAt        time.Time     `json:"created_at"`
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
		Turns: turns, DurationMS: d.Milliseconds(), CostMissingSessions: f.CostMissing,
		Budget: budgetJSON{
			Turns: f.Turns, MaxTurns: snap.Limits.Turns,
			// Rounded to even, as cavu task show's %.0f rounds.
			Minutes: int(math.RoundToEven(f.SessionTime.Minutes())), MaxMinutes: int(snap.Limits.SessionTime / time.Minute),
		},
	}

	for _, dep := range snap.Deps {
		v.DependsOn = append(v.DependsOn, dep.TaskID)
	}

	if st := f.OpenStop; st != nil {
		v.Stop = &stopJSON{Reason: st.Reason, Explanation: st.Explanation, Action: st.Action}

		if !st.ResetsAt.IsZero() {
			v.Stop.ResetsAt = new(st.ResetsAt.UTC())
		}

		// The stop's explanation holds only the first question.
		if st.Reason == engine.StopQuestion && f.Session != nil {
			suggested := f.Session.SuggestedAnswers

			for i, q := range f.Session.Questions {
				var answer string
				if i < len(suggested) {
					answer = suggested[i]
				}

				v.Questions = append(v.Questions, questionJSON{q, answer})
			}
		}
	}

	if r := f.Review; r.Head != "" {
		v.Review = &reviewJSON{
			Rounds: f.ReviewFixes, Failed: r.Failed,
			Open: append([]engine.Finding{}, r.Open...), Minor: append([]engine.Finding{}, r.Minor...),
		}
	}

	if c := snap.Limits.TaskMicroUSD; c > 0 {
		v.Budget.USD, v.Budget.MaxUSD = new(float64(snap.Spent.TaskMicroUSD)/1e6), new(float64(c)/1e6)
	}

	if pr := engine.PullRequestOf(f, snap.Watch.NoCI); pr != nil {
		v.PullRequest = &pullRequestJSON{
			Number: pr.Number, URL: pr.URL, Branch: pr.Branch, Head: pr.Head, State: pr.State, Ready: pr.Ready,
			Checks: pr.Checks, FailedChecks: append([]string{}, pr.Failed...),
		}
	}

	if x := engine.ChangeOf(f); x != nil {
		v.Change = &changeJSON{
			HeadCommit: x.HeadCommit, FilesChanged: x.FilesChanged, DiffStat: append([]string{}, x.DiffStat...),
			Flagged: engine.Flagged{
				Workflows: append([]string{}, x.Flagged.Workflows...), Instructions: append([]string{}, x.Flagged.Instructions...),
				Large: append([]string{}, x.Flagged.Large...),
			},
		}
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

// taskList is a list of tasks as the API shows it.
type taskList struct {
	Tasks []taskItemJSON `json:"tasks"`
}

func (s *server) tasks(w http.ResponseWriter, r *http.Request) error {
	q := r.URL.Query()

	l, err := s.listTasks(r.Context(), q.Get("project"), engine.Status(q.Get("status")))
	if err != nil {
		return err
	}

	reply(w, http.StatusOK, l)

	return nil
}

// listTasks lists every task but a replay's shadows, oldest first, of the
// project and with the status, when they are given.
func (s *server) listTasks(ctx context.Context, project string, status engine.Status) (taskList, error) {
	ps, err := s.Store.Projects(ctx)
	if err != nil {
		return taskList{}, err
	}

	names := make(map[int64]string, len(ps))
	for _, p := range ps {
		names[p.ID] = p.Name
	}

	if project != "" && !slices.ContainsFunc(ps, func(p store.Project) bool { return p.Name == project }) {
		return taskList{}, &apiError{http.StatusBadRequest, fmt.Sprintf("project %q does not exist", project)}
	}

	if status != "" && !slices.Contains(statuses, status) {
		return taskList{}, &apiError{http.StatusBadRequest, fmt.Sprintf("status %q: want one of %v", status, statuses)}
	}

	ts, err := s.Store.Tasks(ctx, false)
	if err != nil {
		return taskList{}, err
	}

	l := taskList{Tasks: []taskItemJSON{}}

	for _, t := range ts {
		if project != "" && names[t.ProjectID] != project {
			continue
		}

		snap, err := s.Loop.Snapshot(ctx, t.ID)
		if err != nil {
			return taskList{}, err
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

		if pr := engine.PullRequestOf(snap.Facts, snap.Watch.NoCI); pr != nil {
			item.PullRequestReady = pr.Ready
		}

		l.Tasks = append(l.Tasks, item)
	}

	return l, nil
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

// addTask stores the task with the requester, and starts it when in asks. It
// asks whether the project is runnable before it stores the task, so a start
// that would be refused creates nothing.
func (s *server) addTask(ctx context.Context, in newTask, requester string) (int64, error) {
	p, err := s.Store.ProjectByName(ctx, in.Project)
	if errors.Is(err, store.ErrNotFound) {
		return 0, &apiError{http.StatusBadRequest, fmt.Sprintf("project %q does not exist", in.Project)}
	}

	if err != nil {
		return 0, fmt.Errorf("project %q: %w", in.Project, err)
	}

	if in.Start && !engine.Runnable(p) {
		return 0, &apiError{http.StatusBadRequest, fmt.Sprintf(
			"project %q has %v; check the project, or waive its verify", p.Name, engine.ErrNoBaseline)}
	}

	t, err := engine.AddTask(ctx, s.Store, store.Task{
		ProjectID: p.ID, Title: in.Title, Body: in.Body, Requester: requester,
	}, in.DependsOn)
	if err != nil {
		return 0, err
	}

	if in.Start {
		if err := s.Loop.Start(ctx, t.ID); err != nil {
			return 0, err
		}

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
func (s *server) perform(ctx context.Context, id int64, action, text string) (taskJSON, error) {
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
		return taskJSON{}, &apiError{http.StatusBadRequest, fmt.Sprintf("action %q: want start, answer, retry, cancel or done", action)}
	}

	if err != nil {
		return taskJSON{}, err
	}

	s.Nudge()

	return s.taskView(ctx, id)
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
