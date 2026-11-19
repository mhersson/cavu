package engine

import (
	"context"
	"fmt"
	"math"
	"time"
)

// View is what a person reads about a task, which the API and MCP serve as it
// is. Its lists are never nil, so they go out as [], never null.
type View struct {
	ID                  int64        `json:"id"`
	Project             string       `json:"project"`
	Title               string       `json:"title"`
	Body                string       `json:"body"`
	Requester           string       `json:"requester"`
	CreatedAt           time.Time    `json:"created_at"`
	Status              Status       `json:"status"`
	NeedsYou            bool         `json:"needs_you"`
	Needs               string       `json:"needs"`
	NeedsYouSince       *time.Time   `json:"needs_you_since"`
	Waiting             string       `json:"waiting"`
	WaitingUntil        *time.Time   `json:"waiting_until"`
	Dependencies        []Dep        `json:"dependencies"`
	Stop                *Stop        `json:"stop"`
	Questions           []Question   `json:"questions"`
	Review              *Review      `json:"review"`
	PullRequest         *PullRequest `json:"pull_request"`
	Change              *Change      `json:"change"`
	Turns               int          `json:"turns"`
	DurationMS          int64        `json:"duration_ms"`
	CostEstimateUSD     float64      `json:"cost_estimate_usd"`
	CostMissingSessions int          `json:"cost_missing_sessions"`
	Usage               []UsageRow   `json:"usage"`
	Budget              Budget       `json:"budget"`
}

// Stop is a task's open stop as a person sees it.
type Stop struct {
	Reason      string     `json:"reason"`
	Explanation string     `json:"explanation"`
	Action      string     `json:"action"`
	ResetsAt    *time.Time `json:"resets_at"`
}

// Question is one question the agent asked, with the answer it would assume.
type Question struct {
	Question        string `json:"question"`
	SuggestedAnswer string `json:"suggested_answer"`
}

// Review is the newest run's review as a person sees it: the fix rounds after
// its findings, why it gave no verdict, and the findings left open and minor.
type Review struct {
	Rounds int       `json:"rounds"`
	Failed string    `json:"failed"`
	Open   []Finding `json:"open"`
	Minor  []Finding `json:"minor"`
}

// Change is the change a task shows. Its files' paths are the host's, which
// the command line prints and the API keeps to itself.
type Change struct {
	HeadCommit   string   `json:"head_commit"`
	FilesChanged int      `json:"files_changed"`
	DiffStat     []string `json:"diff_stat"`
	Flagged      Flagged  `json:"flagged"`
	DiffPath     string   `json:"-"`
	BundlePath   string   `json:"-"`
}

// UsageRow is a task's usage of one model as one source counted it.
type UsageRow struct {
	Source          string  `json:"source"`
	Model           string  `json:"model"`
	Input           int64   `json:"input"`
	Output          int64   `json:"output"`
	CacheRead       int64   `json:"cache_read"`
	CacheWrite      int64   `json:"cache_write"`
	CostEstimateUSD float64 `json:"cost_estimate_usd"`
}

// Budget is what the task used since a person last acted, of what it may. The
// money of a cap is left out unless that cap is in force, so that it never
// shows a limit nobody set.
type Budget struct {
	Turns      int      `json:"turns"`
	MaxTurns   int      `json:"max_turns"`
	Minutes    int      `json:"minutes"`
	MaxMinutes int      `json:"max_minutes"`
	USD        *float64 `json:"usd,omitempty"`
	MaxUSD     *float64 `json:"max_usd,omitempty"`
	DayUSD     *float64 `json:"day_usd,omitempty"`
	MaxDayUSD  *float64 `json:"max_day_usd,omitempty"`
}

// Row is a task in a list.
type Row struct {
	ID               int64      `json:"id"`
	Project          string     `json:"project"`
	Title            string     `json:"title"`
	Status           Status     `json:"status"`
	NeedsYou         bool       `json:"needs_you"`
	Needs            string     `json:"needs"`
	NeedsYouSince    *time.Time `json:"needs_you_since"`
	StopReason       string     `json:"stop_reason"`
	Waiting          string     `json:"waiting"`
	WaitingUntil     *time.Time `json:"waiting_until"`
	PullRequestReady bool       `json:"pull_request_ready"`
	CreatedAt        time.Time  `json:"created_at"`
}

// View reads the task as a person sees it now. waiting is the scheduler's
// reason a task does not act, "" for none; nil off a server.
func (l *Loop) View(ctx context.Context, taskID int64, waiting func(int64) string) (View, error) {
	s, events, err := l.snapshot(ctx, taskID)
	if err != nil {
		return View{}, err
	}

	t, f := s.Task, s.Facts

	p, err := l.Store.Project(ctx, t.ProjectID)
	if err != nil {
		return View{}, fmt.Errorf("task %d: project %d: %w", taskID, t.ProjectID, err)
	}

	usage, err := l.Store.UsageTotals(ctx, taskID)
	if err != nil {
		return View{}, fmt.Errorf("task %d: %w", taskID, err)
	}

	turns, d := TurnsAndDuration(events)
	v := View{
		ID: t.ID, Project: p.Name, Title: t.Title, Body: t.Body, Requester: t.Requester, CreatedAt: t.CreatedAt.UTC(),
		Status: StatusOf(f), Dependencies: s.Deps, Questions: []Question{},
		Turns: turns, DurationMS: d.Milliseconds(), CostMissingSessions: f.CostMissing,
		Usage: make([]UsageRow, len(usage)), Budget: budgetOf(s),
	}
	v.Needs, v.NeedsYouSince = needsOf(s)
	v.NeedsYou = v.Needs != ""
	v.Waiting, v.WaitingUntil = waitingOf(s, waiting)

	if st := f.OpenStop; st != nil {
		v.Stop = &Stop{Reason: st.Reason, Explanation: st.Explanation, Action: st.Action}

		if !st.ResetsAt.IsZero() {
			v.Stop.ResetsAt = new(st.ResetsAt.UTC())
		}

		// The stop's explanation holds only the first question.
		if st.Reason == StopQuestion && f.Session != nil {
			suggested := f.Session.SuggestedAnswers

			for i, q := range f.Session.Questions {
				var answer string
				if i < len(suggested) {
					answer = suggested[i]
				}

				v.Questions = append(v.Questions, Question{q, answer})
			}
		}
	}

	if r := f.Review; r.Head != "" {
		v.Review = &Review{
			Rounds: f.ReviewFixes, Failed: r.Failed, Open: append([]Finding{}, r.Open...), Minor: append([]Finding{}, r.Minor...),
		}
	}

	if pr := PullRequestOf(f, s.Watch.NoCI); pr != nil {
		pr.Failed = append([]string{}, pr.Failed...)
		v.PullRequest = pr
	}

	if x := ChangeOf(f); x != nil {
		v.Change = &Change{
			HeadCommit: x.HeadCommit, FilesChanged: x.FilesChanged, DiffStat: append([]string{}, x.DiffStat...),
			Flagged: Flagged{
				Workflows: append([]string{}, x.Flagged.Workflows...), Instructions: append([]string{}, x.Flagged.Instructions...),
				Large: append([]string{}, x.Flagged.Large...),
			},
			DiffPath: x.DiffPath, BundlePath: x.BundlePath,
		}
	}

	for i, u := range usage {
		v.Usage[i] = UsageRow{u.Source, u.Model, u.Input, u.Output, u.CacheRead, u.CacheWrite, u.CostEstimateUSD}
		v.CostEstimateUSD += u.CostEstimateUSD
	}

	return v, nil
}

// List lists every task but a replay's shadows, oldest first, of the project
// projectID when it is not 0. A removed project's tasks keep its name.
func (l *Loop) List(ctx context.Context, projectID int64, waiting func(int64) string) ([]Row, error) {
	names, err := l.Store.ProjectNames(ctx)
	if err != nil {
		return nil, err
	}

	ts, err := l.Store.Tasks(ctx, false)
	if err != nil {
		return nil, err
	}

	rows := []Row{}

	for _, t := range ts {
		if projectID != 0 && t.ProjectID != projectID {
			continue
		}

		s, err := l.Snapshot(ctx, t.ID)
		if err != nil {
			return nil, err
		}

		r := Row{
			ID: t.ID, Project: names[t.ProjectID], Title: t.Title, Status: StatusOf(s.Facts), CreatedAt: t.CreatedAt.UTC(),
		}
		r.Needs, r.NeedsYouSince = needsOf(s)
		r.NeedsYou = r.Needs != ""
		r.Waiting, r.WaitingUntil = waitingOf(s, waiting)

		if st := s.Facts.OpenStop; st != nil {
			r.StopReason = st.Reason
		}

		if pr := PullRequestOf(s.Facts, s.Watch.NoCI); pr != nil {
			r.PullRequestReady = pr.Ready
		}

		rows = append(rows, r)
	}

	return rows, nil
}

// needsOf is Needs with its time as a view carries it: UTC, nil for no need.
func needsOf(s Snapshot) (string, *time.Time) {
	way, since := Needs(s)
	if way == "" {
		return "", nil
	}

	return way, new(since.UTC())
}

// waitingOf is why the task does not act and until when: the wait or the
// watch Decide gives, with its end when a clock has one. The scheduler tells
// those in Decide's words, and its own reasons, such as every slot taken or a
// failed look at a pull request, which come first with no end.
func waitingOf(s Snapshot, waiting func(int64) string) (string, *time.Time) {
	var (
		why   string
		until *time.Time
	)

	if a := Decide(s); a.Kind == KindWait || a.Kind == KindWatch {
		why = a.Why

		if !a.Until.IsZero() {
			until = new(a.Until.UTC())
		}
	}

	if waiting != nil {
		if told := waiting(s.Task.ID); told != "" && told != why {
			return told, nil
		}
	}

	return why, until
}

// budgetOf is what the snapshot's task used of its budget.
func budgetOf(s Snapshot) Budget {
	b := Budget{
		Turns: s.Facts.Turns, MaxTurns: s.Limits.Turns,
		// Rounded to even, as cavu task show rounds them.
		Minutes: int(math.RoundToEven(s.Facts.SessionTime.Minutes())), MaxMinutes: int(s.Limits.SessionTime / time.Minute),
	}

	if c := s.Limits.TaskMicroUSD; c > 0 {
		b.USD, b.MaxUSD = usd(s.Spent.TaskMicroUSD), usd(c)
	}

	if c := s.Limits.DayMicroUSD; c > 0 {
		b.DayUSD, b.MaxDayUSD = usd(s.Spent.DayMicroUSD), usd(c)
	}

	return b
}

func usd(microUSD int64) *float64 { return new(float64(microUSD) / 1e6) }
