package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"time"

	"github.com/mhersson/cavu/internal/store"
)

// The states of a pull request in an observation.
const (
	PROpen   = "open"
	PRMerged = "merged"
	PRClosed = "closed"
)

// The checks of a pull request's head in an observation, but those the
// project ignores: pending while any runs, else failed when any failed or was
// cancelled, else passed.
const (
	ChecksNone    = "none"
	ChecksPending = "pending"
	ChecksPassed  = "passed"
	ChecksFailed  = "failed"
)

// Observation is the task's pull request as the forge has it: the payload of
// pr_observed. Failed names the failed checks, sorted, and Comments are the
// reviewers' open requests, answered or not.
type Observation struct {
	State    string    `json:"state"`
	Draft    bool      `json:"draft"`
	Head     string    `json:"head"`
	Conflict bool      `json:"conflict"`
	Checks   string    `json:"checks"`
	Failed   []string  `json:"failed,omitempty"`
	Comments []Comment `json:"comments,omitempty"`
}

// RateLimited is the error a Runner's forge call returns while the forge
// limits its requests, until the limit lifts. Find it with errors.As into a
// *RateLimited.
type RateLimited struct{ Until time.Time }

func (e *RateLimited) Error() string { return "rate limited until " + e.Until.Format(time.RFC3339) }

// publish puts the exported change on the task's branch and, for a first
// publish, opens a draft pull request with the text CAVU writes. A rejected
// push stops the task and keeps the run, so a retry publishes from it again.
func (p *pass) publish(ctx context.Context, v view, a Action, o openRun) error {
	f := v.Facts

	if err := p.claim(ctx, v, o, draft{PublishStarted, actorEngine, PublishStartedPayload{Why: a.Why}}); err != nil {
		return err
	}

	bundle, err := os.ReadFile(f.Export.BundlePath)
	if err != nil {
		return p.fault(ctx, o, 0, nil, fmt.Errorf("read the exported change's bundle: %w", err))
	}

	// The title and the text are written once: a person may edit them on the pull request.
	req := PublishRequest{Bundle: bundle, Head: f.Export.HeadCommit, Branch: o.cfg.Repo.Branch}
	if pr := f.PR; pr != nil {
		req.Branch, req.Prev, req.Number = pr.Branch, pr.Head, pr.Number
	} else if req.Title, req.Body, err = p.pullRequestText(v, o); err != nil {
		return fmt.Errorf("task %d: publish: %w", p.taskID, err)
	}

	res, err := p.Runner.Publish(ctx, o.cfg, req)
	if err != nil {
		return p.fault(ctx, o, 0, nil, err)
	}

	if res.Rejected != "" {
		return p.halt(ctx, -1, o.task, &o.row, StoppedPayload{
			Reason: StopPushRejected, Explanation: withDetail("The task's branch was not pushed", p.mask(res.Rejected)),
			Action: ClearRetry, Detail: res.Rejected,
		}, nil)
	}

	pub := PublishedPayload{Branch: res.Branch, Head: res.Head, Number: res.Number, URL: res.URL, NodeID: res.NodeID}
	if pr := f.PR; pr != nil {
		pub.Number, pub.URL, pub.NodeID = pr.Number, pr.URL, pr.NodeID
	}

	return p.result(ctx, o, nil, draft{Published, actorRunner, pub})
}

// Observe reads the task's pull request when Decide says watch, and records
// what it saw when that differs from the last record. fast reports that the
// next look should come soon: no stop is open, and the checks run, or none has
// started on a project with CI and the grace has not passed. A runner error, a
// rate limit among them, is returned as it is and records nothing.
func (l *Loop) Observe(ctx context.Context, taskID int64) (fast bool, err error) {
	s, _, err := l.snapshot(ctx, taskID)
	if err != nil {
		return false, err
	}

	if Decide(s).Kind != KindWatch {
		return false, nil
	}

	// A stop in a fix run keeps the run open, and its pull request is watched.
	pr, runID := s.Facts.PR, int64(0)
	if s.Facts.RunOpen {
		runID = s.Facts.LastRun
	}

	c, err := l.forgeConfig(ctx, s.Task, runID, pr)
	if err != nil {
		return false, fmt.Errorf("task %d: %w", taskID, err)
	}

	o, err := l.Runner.Observe(ctx, c, pr.Number)
	if err != nil {
		return false, err
	}

	p, err := l.newPass(ctx, taskID)
	if err != nil {
		return false, err
	}

	// Compared as the record holds it: masked, a secret in a comment would differ at every look.
	raw, err := p.encode(o)
	if err != nil {
		return false, fmt.Errorf("task %d: encode %s: %w", taskID, PRObserved, err)
	}

	seen := decode[Observation](raw)
	if pr.Seen == nil || !reflect.DeepEqual(seen, *pr.Seen) {
		// A person's action recorded since the snapshot wins, and the next look sees the pull request again.
		err := p.write(ctx, s.LastEventID, runID, nil, draft{PRObserved, actorRunner, seen})
		if err != nil && !errors.Is(err, store.ErrConflict) {
			return false, err
		}
	}

	// Under a stop only a merge or a close counts, and a project without CI waits for no check.
	due := o.Checks == ChecksPending || o.Checks == ChecksNone && !s.Watch.NoCI && s.Now.Before(pr.Since.Add(checksGrace))

	return due && s.Facts.OpenStop == nil, nil
}

// ready marks the task's pull request ready for review. It runs with no run
// open, so its events carry none and a fault of it stops the task with no run.
func (p *pass) ready(ctx context.Context, v view, a Action) error {
	pr := v.Facts.PR

	cfg, err := p.forgeConfig(ctx, v.Task, 0, pr)
	if err != nil {
		return p.unconfigured(ctx, v, a, nil, err)
	}

	o := openRun{task: v.Task, cfg: cfg}

	if err := p.claim(ctx, v, o, draft{ReadyStarted, actorEngine, ReadyStartedPayload{Why: a.Why}}); err != nil {
		return err
	}

	if err := p.Runner.Ready(ctx, o.cfg, pr.NodeID); err != nil {
		return p.fault(ctx, o, 0, nil, err)
	}

	return p.result(ctx, o, nil, draft{PRReady, actorRunner, struct{}{}})
}

// pullRequestText writes the title and the text of the task's pull request
// from the record alone, so a retried publish writes the same.
func (p *pass) pullRequestText(v view, o openRun) (title, body string, err error) {
	// Decide publishes only an export, which follows an ended session.
	f, s := v.Facts, v.Facts.Session

	return PullRequestText(PullRequestData{
		TaskID: v.Task.ID, TaskTitle: v.Task.Title, TaskBody: v.Task.Body,
		Title: s.Title, Summary: s.Summary, WhereToLook: s.WhereToLook, Assumptions: f.Assumptions, Flagged: f.Export.Flagged,
		VerifyCommand: f.VerifyCommand, VerifyWaived: f.Verify == ResultWaived,
		ReadinessOff: !v.Switches.Readiness, ReviewOff: !v.Switches.Review,
		ReviewFailed: f.Review.Failed, Open: f.Review.Open, Minor: f.Review.Minor,
		Model: o.cfg.Agent.Model, Effort: o.cfg.Agent.Effort,
		Duration: untilExport(v.events),
	})
}

// untilExport is the time from the task's start to its newest export.
func untilExport(events []store.Event) time.Duration {
	i := len(events)
	for i > 0 && events[i-1].Type != Exported {
		i--
	}

	_, d := TurnsAndDuration(events[:i])

	return d
}
