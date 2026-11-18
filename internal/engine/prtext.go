package engine

import (
	"cmp"
	"time"

	"github.com/mhersson/cavu/internal/prompts"
)

// PullRequestData is what the text of a task's pull request says: the task,
// the session's account of its change, the checks the change passed or did
// not get, the review's findings left open, and what the task took.
type PullRequestData struct {
	TaskID                  int64
	TaskTitle, TaskBody     string
	Title, Summary          string // the session's
	WhereToLook             []string
	Assumptions             []string
	Flagged                 Flagged
	VerifyCommand           string
	VerifyWaived            bool
	ReadinessOff, ReviewOff bool
	ReviewFailed            string // why the review gave no verdict, "" when it gave one
	Open, Minor             []Finding
	Model, Effort           string
	CostEstimateUSD         float64
	CostMissing             int // sessions the estimate leaves out
	Duration                time.Duration
}

// PullRequestText returns the title of a task's pull request, the session's
// title, else the task's, on one bounded line, and its text, which
// pull_request.md holds.
func PullRequestText(d PullRequestData) (title, body string, err error) {
	d.Duration = d.Duration.Round(time.Second)

	if body, err = prompts.Render(prompts.PullRequest, d); err != nil {
		return "", "", err
	}

	return cmp.Or(line(d.Title), line(d.TaskTitle)), body, nil
}
