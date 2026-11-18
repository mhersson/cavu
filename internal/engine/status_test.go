package engine_test

import (
	"testing"

	"github.com/mhersson/cavu/internal/engine"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStatusOf(t *testing.T) {
	tests := []struct {
		name  string
		facts engine.Facts
		want  engine.Status
	}{
		{"not started", engine.Facts{}, engine.StatusDraft},
		{"cancelled before it started", engine.Facts{Cancelled: true}, engine.StatusCancelled},
		{
			"cancelled while running",
			engine.Facts{Started: true, Cancelled: true, RunOpen: true, OpenStop: &engine.StopFact{Reason: engine.StopCancelled}},
			engine.StatusCancelled,
		},
		{"cancelled after it was merged", engine.Facts{Started: true, Exported: true, Merged: true, Cancelled: true}, engine.StatusCancelled},
		{"merged", engine.Facts{Started: true, Exported: true, Merged: true}, engine.StatusDone},
		{"stopped", engine.Facts{Started: true, RunOpen: true, OpenStop: &engine.StopFact{Reason: engine.StopQuestion}}, engine.StatusBlocked},
		{"stopped with no run", engine.Facts{Started: true, OpenStop: &engine.StopFact{Reason: engine.StopDependency}}, engine.StatusBlocked},
		{"exported", engine.Facts{Started: true, Exported: true}, engine.StatusInReview},
		{"started with no run", engine.Facts{Started: true}, engine.StatusQueued},
		{"running", engine.Facts{Started: true, RunOpen: true}, engine.StatusRunning},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, engine.StatusOf(tc.facts))
		})
	}
}

// pullRequest is the facts of a task whose pull request CAVU published at head
// "h2", after an earlier head "h1", and which a look showed as seen, if given.
func pullRequest(seen *engine.Observation) engine.Facts {
	return engine.Facts{PR: &engine.PRFact{
		Number: 12, URL: "https://github.com/acme/web/pull/12", Branch: "cavu/footer", Head: "h2",
		Heads: []string{"h1", "h2"}, Seen: seen,
	}}
}

func with(f engine.Facts, mutate func(*engine.Facts)) engine.Facts {
	mutate(&f)

	return f
}

func TestPullRequestOf(t *testing.T) {
	look := func(head, checks string, mutate ...func(*engine.Observation)) *engine.Observation {
		o := &engine.Observation{State: engine.PROpen, Head: head, Checks: checks}
		for _, m := range mutate {
			m(o)
		}

		return o
	}
	draft := func(o *engine.Observation) { o.Draft = true }
	state := func(s string) func(*engine.Observation) { return func(o *engine.Observation) { o.State = s } }
	failed := func(o *engine.Observation) { o.Failed = []string{"lint", "unit"} }

	tests := []struct {
		name           string
		facts          engine.Facts
		noCI           bool
		state, checks  string
		failed         []string
		ready          bool
		head           string
		hasPullRequest bool
	}{
		{name: "no pull request", facts: engine.Facts{Started: true, Exported: true}},
		{name: "just published", facts: pullRequest(nil), hasPullRequest: true, state: engine.PROpen, head: "h2"},
		{
			name: "a look at an earlier head of its own", facts: pullRequest(look("h1", engine.ChecksPassed)),
			hasPullRequest: true, state: engine.PROpen, head: "h2",
		},
		{
			name: "failed checks named", facts: pullRequest(look("h2", engine.ChecksFailed, failed)),
			hasPullRequest: true, state: engine.PROpen, checks: engine.ChecksFailed, failed: []string{"lint", "unit"}, head: "h2",
		},
		{
			name: "pending", facts: pullRequest(look("h2", engine.ChecksPending)),
			hasPullRequest: true, state: engine.PROpen, checks: engine.ChecksPending, head: "h2",
		},
		{
			name: "passed and no draft", facts: pullRequest(look("h2", engine.ChecksPassed)),
			hasPullRequest: true, state: engine.PROpen, checks: engine.ChecksPassed, ready: true, head: "h2",
		},
		{
			name: "passed and a draft", facts: pullRequest(look("h2", engine.ChecksPassed, draft)),
			hasPullRequest: true, state: engine.PROpen, checks: engine.ChecksPassed, head: "h2",
		},
		{
			name: "none checks on a project with CI", facts: pullRequest(look("h2", engine.ChecksNone)),
			hasPullRequest: true, state: engine.PROpen, checks: engine.ChecksNone, head: "h2",
		},
		{
			name: "none checks on a project with no_ci", facts: pullRequest(look("h2", engine.ChecksNone)), noCI: true,
			hasPullRequest: true, state: engine.PROpen, checks: engine.ChecksNone, ready: true, head: "h2",
		},
		{
			name: "none checks and a draft on a project with no_ci", facts: pullRequest(look("h2", engine.ChecksNone, draft)), noCI: true,
			hasPullRequest: true, state: engine.PROpen, checks: engine.ChecksNone, head: "h2",
		},
		{
			name: "marked ready is not ready", facts: func() engine.Facts {
				f := pullRequest(look("h2", engine.ChecksPending))
				f.PR.MarkedReady = true

				return f
			}(),
			hasPullRequest: true, state: engine.PROpen, checks: engine.ChecksPending, head: "h2",
		},
		{
			name: "merged", facts: pullRequest(look("h2", engine.ChecksPassed, state(engine.PRMerged))),
			hasPullRequest: true, state: engine.PRMerged, checks: engine.ChecksPassed, head: "h2",
		},
		{
			name: "closed", facts: pullRequest(look("h2", engine.ChecksFailed, failed, state(engine.PRClosed))),
			hasPullRequest: true, state: engine.PRClosed, checks: engine.ChecksFailed, failed: []string{"lint", "unit"}, head: "h2",
		},
		{
			name: "merged on an earlier head of its own", facts: pullRequest(look("h1", engine.ChecksPassed, state(engine.PRMerged))),
			hasPullRequest: true, state: engine.PRMerged, head: "h2",
		},
		{
			name: "a head CAVU never published", facts: pullRequest(look("theirs", engine.ChecksPassed)),
			hasPullRequest: true, state: engine.PROpen, head: "h2",
		},
		{
			name: "a run is open on a ready-looking pull request", facts: with(pullRequest(look("h2", engine.ChecksPassed)), func(f *engine.Facts) { f.RunOpen = true }),
			hasPullRequest: true, state: engine.PROpen, checks: engine.ChecksPassed, head: "h2",
		},
		{
			name: "a stop is open on a pull request whose checks passed",
			facts: with(pullRequest(look("h2", engine.ChecksPassed)), func(f *engine.Facts) {
				f.OpenStop = &engine.StopFact{Reason: engine.StopConflict}
			}),
			hasPullRequest: true, state: engine.PROpen, checks: engine.ChecksPassed, head: "h2",
		},
		{
			name: "a cancelled task whose pull request stays open", facts: with(pullRequest(look("h2", engine.ChecksPassed)), func(f *engine.Facts) { f.Cancelled = true }),
			hasPullRequest: true, state: engine.PROpen, checks: engine.ChecksPassed, head: "h2",
		},
		{
			name: "a merged task", facts: with(pullRequest(look("h2", engine.ChecksPassed)), func(f *engine.Facts) { f.Merged = true }),
			hasPullRequest: true, state: engine.PROpen, checks: engine.ChecksPassed, head: "h2",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := engine.PullRequestOf(tc.facts, tc.noCI)
			if !tc.hasPullRequest {
				assert.Nil(t, got)

				return
			}

			require.NotNil(t, got)
			assert.Equal(t, 12, got.Number)
			assert.Equal(t, "https://github.com/acme/web/pull/12", got.URL)
			assert.Equal(t, "cavu/footer", got.Branch)
			assert.Equal(t, tc.head, got.Head)
			assert.Equal(t, tc.state, got.State)
			assert.Equal(t, tc.checks, got.Checks)
			assert.Equal(t, tc.ready, got.Ready)
			assert.ElementsMatch(t, tc.failed, got.Failed)
		})
	}

	t.Run("the facts cannot be changed through it", func(t *testing.T) {
		f := pullRequest(look("h2", engine.ChecksFailed, failed))
		engine.PullRequestOf(f, false).Failed[0] = "changed"

		assert.Equal(t, []string{"lint", "unit"}, f.PR.Seen.Failed)
	})
}

func TestChangeOf(t *testing.T) {
	published := engine.ExportedPayload{HeadCommit: "c1", FilesChanged: 2, DiffStat: []string{"a | 1 +"}, Flagged: engine.Flagged{Workflows: []string{"w"}}}
	newer := engine.ExportedPayload{HeadCommit: "c2"}

	tests := []struct {
		name  string
		facts engine.Facts
		want  *engine.ExportedPayload
	}{
		{"no export", engine.Facts{Started: true, RunOpen: true}, nil},
		{"an export and no pull request", engine.Facts{Exported: true, Export: newer}, &newer},
		{"a pull request in the middle of a fix run", engine.Facts{RunOpen: true, PR: &engine.PRFact{Export: published}}, &published},
		{
			"an open run exported a newer change not yet published",
			engine.Facts{RunOpen: true, Exported: true, Export: newer, PR: &engine.PRFact{Export: published}},
			&published,
		},
		{"an export that a stop left, with no pull request", engine.Facts{Exported: false, Export: newer}, nil},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, engine.ChangeOf(tc.facts))
		})
	}

	t.Run("the facts cannot be changed through it", func(t *testing.T) {
		f := engine.Facts{PR: &engine.PRFact{Export: published}}
		got := engine.ChangeOf(f)
		got.DiffStat[0], got.Flagged.Workflows[0], got.HeadCommit = "x", "x", "x"

		assert.Equal(t, "a | 1 +", f.PR.Export.DiffStat[0])
		assert.Equal(t, "w", f.PR.Export.Flagged.Workflows[0])
		assert.Equal(t, "c1", f.PR.Export.HeadCommit)
	})
}
