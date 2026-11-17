package engine

import (
	"cmp"
	"context"
)

// Finding severities. Critical and important findings start fix rounds;
// minor ones are recorded and never do.
const (
	SeverityCritical  = "critical"
	SeverityImportant = "important"
	SeverityMinor     = "minor"
)

// strongAfter is how many fix rounds resume the working session; each later
// round is a fresh session in role strong.
const strongAfter = 3

// Finding is one thing a reviewer found.
type Finding struct {
	Severity string `json:"severity"`
	Where    string `json:"where"`
	What     string `json:"what"`
	Why      string `json:"why"`
}

// Verdict is one of a review's two judgements.
type Verdict struct {
	Pass   bool   `json:"pass"`
	Reason string `json:"reason"`
}

// ReviewFact is where the open run's review stands.
type ReviewFact struct {
	Head   string       // the commit the newest review that ended reviewed or failed saw
	Open   []Finding    // critical and important, not addressed
	Minor  []Finding    // recorded; they never extend the loop
	Failed string       // why the newest review gave no verdict, "" when it gave one
	Halt   *SessionFact // the newest review ended as a session outcome that stops; nil once an action is claimed
}

// reviewEnded folds a review's end into the facts.
func (f *Facts) reviewEnded(p ReviewEndedPayload) {
	r := &f.Review

	switch p.Outcome {
	case ReviewReviewed:
		open, minor := bySeverity(p.Findings)
		if p.Scoped {
			open = append(unaddressed(r.Open, p.Addressed), open...)
		} else if !p.Asked.Pass && len(open) == 0 {
			// A failed first verdict always starts the fix loop with something to fix.
			open = []Finding{{Severity: SeverityImportant, What: p.Asked.Reason}}
		}

		r.Head, r.Failed, r.Open, r.Minor = p.Head, "", open, append(r.Minor, minor...)
	case ReviewFailed:
		r.Head, r.Failed = p.Head, cmp.Or(p.Detail, "The reviewer gave no verdict.")
	default:
		r.Halt = &SessionFact{Outcome: p.Outcome, Detail: p.Detail, ResetsAt: p.ResetsAt}
	}
}

// bySeverity splits findings into those that start fix rounds and the minor ones.
func bySeverity(findings []Finding) (open, minor []Finding) {
	for _, x := range findings {
		if x.Severity == SeverityMinor {
			minor = append(minor, x)
		} else {
			open = append(open, x)
		}
	}

	return open, minor
}

// unaddressed keeps each open finding not ruled addressed; one the verdicts
// leave out is not addressed.
func unaddressed(open []Finding, addressed []bool) []Finding {
	var kept []Finding

	for i, x := range open {
		if i >= len(addressed) || !addressed[i] {
			kept = append(kept, x)
		}
	}

	return kept
}

// reviewLoop decides a change that passed verify, or whose verify is waived,
// while the review is on. The review never blocks the task: a review without
// a verdict, and the end of the fix rounds, export what is there.
func reviewLoop(s Snapshot, f Facts) Action {
	r := f.Review

	if r.Halt != nil {
		if a, ok := sessionStop(r.Halt, s.Now); ok {
			return a
		}
	}

	switch {
	case r.Head != f.Session.HeadCommit && len(r.Open) > 0:
		return Action{Kind: KindReview, Why: "A fix changed the code; the open findings are reviewed again.", Scoped: true}
	case r.Head != f.Session.HeadCommit:
		return Action{Kind: KindReview, Why: "The change passed verify and has not been reviewed."}
	case r.Failed != "":
		return Action{Kind: KindExport, Why: "The review gave no verdict, so the change is exported without one."}
	case len(r.Open) > 0 && f.ReviewFixes < s.Caps.ReviewFixes:
		a := work(ModeFixReview, "The review left findings open and a fix round is left.")
		a.Automatic, a.Fresh = true, f.ReviewFixes >= strongAfter

		return a
	case len(r.Open) > 0:
		return Action{Kind: KindExport, Why: "The fix rounds are used up; the findings left open are kept with the change."}
	}

	return Action{Kind: KindExport, Why: "The review left no finding open."}
}

// review runs a fresh reviewer on the session's head: on the whole change, or,
// scoped, on the fix since the head the previous review saw and on the
// findings it left open.
func (p *pass) review(ctx context.Context, v view, a Action, o openRun) error {
	// Decide reviews only after an ended session, so Session is set.
	f := v.Facts
	r := ReviewRequest{
		SessionID: NewSessionID(), Head: f.Session.HeadCommit, Answers: f.Answers, Assumptions: f.Assumptions,
		Summary: f.Session.Summary,
	}

	if a.Scoped {
		r.FixBase, r.Findings = f.Review.Head, f.Review.Open
	}

	if err := p.claim(ctx, v, o, draft{ReviewStarted, actorEngine, ReviewStartedPayload{
		SessionID: r.SessionID, Scoped: a.Scoped, Head: r.Head, Why: a.Why,
	}}); err != nil {
		return err
	}

	add, stop := p.progress(ctx, o.row.ID)
	res, err := p.Runner.Review(ctx, o.cfg, r, add)
	p.hold(stop())

	if err != nil {
		return p.fault(ctx, o, err)
	}

	return p.result(ctx, o, runUsage(o.row.ID, res.Usage), draft{ReviewEnded, actorRunner, ReviewEndedPayload{
		Outcome: res.Outcome, Head: r.Head, Scoped: a.Scoped, Asked: res.Asked, Built: res.Built, Findings: res.Findings,
		Addressed: res.Addressed, Turns: res.Turns, CostEstimateUSD: res.CostEstimateUSD, ResetsAt: res.ResetsAt, Detail: res.Detail,
	}})
}
