package engine_test

import (
	"strings"
	"testing"
	"time"

	"github.com/mhersson/cavu/internal/engine"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// differs returns the part of b that is not a's: b's offsets after the start
// and before the end it shares with a.
func differs(a, b string) (from, to int) {
	for from < len(a) && from < len(b) && a[from] == b[from] {
		from++
	}

	to = len(b)
	for i := len(a); i > from && to > from && a[i-1] == b[to-1]; i-- {
		to--
	}

	return from, to
}

func TestPullRequestText(t *testing.T) {
	passed := engine.PullRequestData{
		TaskID: 4242, TaskTitle: "Fix the login", TaskBody: "task-body-1", Title: "session-title-2", Summary: "summary-3",
		WhereToLook: []string{"where-4"}, Assumptions: []string{"assumption-5"},
		Flagged:       engine.Flagged{Workflows: []string{".github/workflows/flagged-6.yml"}},
		VerifyCommand: "verify-7",
		Open:          []engine.Finding{{Severity: engine.SeverityImportant, Where: "login.go:12", What: "what-8", Why: "why-9"}},
		Model:         "model-10", Duration: 3 * time.Minute,
	}

	title, body, err := engine.PullRequestText(passed)
	require.NoError(t, err)
	assert.Equal(t, "session-title-2", title)

	for _, v := range []string{"4242", "what-8", "assumption-5", ".github/workflows/flagged-6.yml", "summary-3", "where-4", "verify-7", "model-10"} {
		assert.Contains(t, body, v)
	}

	assert.Less(t, strings.Index(body, "flagged-6"), strings.Index(body, "summary-3"), "the flags come before the summary")

	t.Run("the title is the session's, else the task's, on one line", func(t *testing.T) {
		d := passed
		d.Title = " Fix the\n login redirect "

		title, _, err := engine.PullRequestText(d)
		require.NoError(t, err)
		assert.Equal(t, "Fix the login redirect", title)

		d.Title = " \n"

		title, _, err = engine.PullRequestText(d)
		require.NoError(t, err)
		assert.Equal(t, passed.TaskTitle, title)
	})

	// Each check that did not run or gave no verdict changes what Checks says,
	// which sits between the task and Where to look.
	checks := map[string]func(d *engine.PullRequestData){
		"a waived verify":          func(d *engine.PullRequestData) { d.VerifyWaived, d.VerifyCommand = true, "" },
		"a review without verdict": func(d *engine.PullRequestData) { d.ReviewFailed = "no-verdict-11" },
		"the review off":           func(d *engine.PullRequestData) { d.ReviewOff = true },
		"the readiness turn off":   func(d *engine.PullRequestData) { d.ReadinessOff = true },
	}

	for name, change := range checks {
		t.Run(name, func(t *testing.T) {
			d := passed
			change(&d)

			_, got, err := engine.PullRequestText(d)
			require.NoError(t, err)
			require.NotEqual(t, body, got)

			from, to := differs(body, got)
			assert.GreaterOrEqual(t, from, strings.Index(got, passed.TaskBody)+len(passed.TaskBody), "the task comes before the change")
			assert.Less(t, to, strings.Index(got, "where-4"), "Where to look comes after it")

			if d.ReviewFailed != "" {
				assert.Contains(t, got[from:to], d.ReviewFailed)
			}
		})
	}
}
