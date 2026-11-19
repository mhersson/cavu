package engine

import (
	"encoding/json"
	"slices"
	"time"

	"github.com/mhersson/cavu/internal/store"
)

// Facts is what a task's events add up to.
type Facts struct {
	Started, Cancelled, Merged bool
	OpenStop                   *StopFact // newest stop with no human action, claim or merge seen after it
	RunOpen                    bool
	LastRun                    int64    // the task's newest run, open or ended
	Home                       bool     // the newest run ended and kept the task's home, which is still there; never while a run is open
	Prepared                   bool     // in the open run
	Ecosystems                 []string // of the open run's prepare, which Work, Verify and Export run with
	PrepareFailure             *PrepareFailure
	SessionID                  string       // the task's session, which a fix run goes on with; "" until a run that is no fix run starts one
	Session                    *SessionFact // newest ended session in the open run; nil if none, if a human action other than a retry that is no resume came after it, or while a newer session is pending
	Verify                     string       // result for the newest ended session; "" while a session or a verify is pending
	Exported                   bool
	Export                     ExportedPayload // of the newest export, meaningful when Exported
	Replied                    bool            // the open run answered on the pull request
	Resumed                    *Resume         // a human retry or answer after a stop, other than a retry that is no resume, not yet followed by a finished session or prepare, or by a new run when it came from environment
	LastVerifyOutput           string          // of the newest verify_ended, for the next fix prompt
	Continuations, VerifyFixes int             // session starts marked automatic, per mode, since the last human action
	CIFixes, CommentRuns       int             // runs started on the pull request's failed checks and on its reviewers' requests, since the last human action
	ReviewFixes                int             // automatic fix_review session starts since the last human action
	Faults                     int             // sandbox stops and failed endpoints since the last human action
	Turns                      int             // of sessions and reviews since the last human action
	SessionTime                time.Duration   // from each session's or review's start to its end, since the last human action
	CostMissing                int             // sessions and reviews of the whole task that ended without a cost
	LastHumanEventID           int64           // the newest human action's event; the task's spend counts the usage after it
	Review                     ReviewFact
	Role                       string   // of the task's session, strong after a fresh fix session; "" means work
	Answers                    []string // every answer a person gave on the task
	Assumptions                []string // of the open run's sessions
	PR                         *PRFact  // the task's pull request, nil before its first publish
	RunReason                  string   // of the newest run, kept past its end so a retry after its failed prepare starts its like
	RunRequests                []string // of the newest run, kept past its end: the ids of the requests a comments run was given
	RunHead                    string   // of the open run: the pull request's head a fix run starts from
	VerifyCommand              string   // of the newest verify_ended in the open run

	started time.Time // of the newest session or review that has not ended, zero when none
}

// StopFact is the open stop.
type StopFact struct {
	Reason, Explanation string
	Action              string // the one action that clears the stop
	ResetsAt            time.Time
	At                  time.Time // when the stop was recorded
	Fault               bool      // an endpoint stop the endpoint's failure gave, which Decide retries
}

// PRFact is the task's pull request.
type PRFact struct {
	Number                    int
	URL, NodeID, Branch, Head string
	Heads                     []string        // every head CAVU published or took back
	Since                     time.Time       // the latest of the head's publish, CAVU's ready-marking and a person's action
	Export                    ExportedPayload // the export CAVU published last
	Seen                      *Observation    // the newest observation since the head was published; nil when none
	Looked                    bool            // Seen was recorded after the event Since is the time of
	TakeBack                  bool            // a retry of a takeover or a conflict gives CAVU the head the next look shows
	MarkedReady               bool            // CAVU marked the pull request ready for review
	FixedHead                 string          // the head a run on failed checks was started from since the last human action
	Answered                  []string        // the ids of the requests follow-up runs were given; never cleared
}

// PrepareFailure is the newest failed prepare in the open run.
type PrepareFailure struct {
	Step, Output string
}

// SessionFact is the newest ended session.
type SessionFact struct {
	Outcome                string
	Changed                bool
	Title, Summary, Detail string
	HeadCommit             string // the commit the session froze, which an export takes
	Questions              []string
	SuggestedAnswers       []string // one per question, the answer the agent would assume
	WhereToLook            []string // the parts of the change the session says need a person's eyes first
	ResetsAt               time.Time
}

// Resume is a person's retry or answer that cleared a stop.
type Resume struct {
	From   string // the stop reason that was cleared
	Answer string
}

// Reduce folds a task's events, oldest first, into its facts in one pass.
func Reduce(events []store.Event) Facts {
	var f Facts

	for _, e := range events {
		f.apply(e)
	}

	return f
}

// TurnsAndDuration is how much work a task was: the turns of all its sessions,
// and the time from its newest start to its last event, 0 if it never started.
func TurnsAndDuration(events []store.Event) (turns int, d time.Duration) {
	var started time.Time

	for _, e := range events {
		switch e.Type {
		case TaskStarted:
			started = e.At
		case SessionEnded:
			turns += decode[SessionEndedPayload](e.Payload).Turns
		case ReviewEnded:
			turns += decode[ReviewEndedPayload](e.Payload).Turns
		}
	}

	if started.IsZero() {
		return turns, 0
	}

	return turns, events[len(events)-1].At.Sub(started)
}

// apply ignores run-scoped results while no run is open, since those facts
// describe the open run.
func (f *Facts) apply(e store.Event) {
	switch e.Type {
	case TaskStarted:
		f.Started = true
		f.humanAction(e)
	case HumanAnswered:
		text := decode[HumanAnsweredPayload](e.Payload).Text
		f.resume(text)
		f.humanAction(e)
		f.Answers = append(f.Answers, text)
	case HumanRetried:
		// A retry of a takeover gives the branch back to CAVU, and so does one of a
		// conflict, which a person resolves with a commit, maybe after the last look.
		if f.PR != nil && (f.stoppedFor(StopTakenOver) || f.stoppedFor(StopConflict)) {
			f.PR.TakeBack = true
		}

		// A sandbox stop is a fault that cut an action short, a review's halt cut
		// a review, and a pull request's stop came after the export: none is the
		// work session's result, so the retry is no resume and keeps the session,
		// and Decide redoes what came after it.
		if f.OpenStop != nil && keepsSession[f.OpenStop.Reason] || f.Review.Halt != nil {
			f.clearStop(e)
		} else {
			f.resume("")
			f.humanAction(e)
		}
	case HumanCancelled:
		f.Cancelled = true
		f.humanAction(e)
	case RunStarted:
		// The new run is what a resume from environment asked for.
		if f.resumedFrom(StopEnvironment) {
			f.Resumed = nil
		}

		// A fix run, started from the pull request's head, goes on with the
		// task's session; any other run starts afresh.
		p := decode[RunStartedPayload](e.Payload)
		if p.Head == "" {
			f.SessionID, f.Role, f.Review = "", "", ReviewFact{}
		}

		f.resetRun()
		f.RunOpen, f.Exported, f.RunReason, f.RunRequests, f.RunHead = true, false, p.Reason, p.Requests, p.Head
		f.LastRun, f.Home = e.RunID, false
		f.fixRunStarted(p)
	case RunEnded:
		f.resetRun()
		f.RunOpen, f.Home = false, decode[RunEndedPayload](e.Payload).KeptHome
	case HomeRemoved:
		// A result, recorded after the release: it closes no stop, or a cancelled
		// task would be stopped again.
		f.Home = false
	case PrepareStarted:
		f.claimed()

		if f.RunOpen {
			f.Prepared, f.PrepareFailure, f.Ecosystems = false, nil, nil
		}
	case Prepared:
		f.Resumed = nil
		if f.RunOpen {
			f.Prepared = true
			f.Ecosystems = decode[PreparedPayload](e.Payload).Ecosystems
		}
	case PrepareFailed:
		f.Resumed = nil
		if f.RunOpen {
			p := decode[PrepareFailedPayload](e.Payload)
			f.PrepareFailure = &PrepareFailure{Step: p.Step, Output: p.Output}
		}
	case SessionStarted:
		f.sessionStarted(decode[SessionStartedPayload](e.Payload))
		f.started = e.At
	case SessionEnded:
		p := decode[SessionEndedPayload](e.Payload)
		f.sessionEnded(e.At, p.Turns, p.CostMissing)

		f.Resumed = nil
		if f.RunOpen {
			f.Verify = ""
			f.Session = &SessionFact{
				Outcome: p.Outcome, Changed: p.Changed, Title: p.Title, Summary: p.Summary, Detail: p.Detail,
				HeadCommit: p.HeadCommit, Questions: p.Questions, SuggestedAnswers: p.SuggestedAnswers, WhereToLook: p.WhereToLook,
				ResetsAt: p.ResetsAt,
			}
			f.Assumptions = append(f.Assumptions, p.Assumptions...)
		}
	case VerifyStarted:
		f.claimed()

		if f.RunOpen {
			f.Verify = ""
		}
	case VerifyEnded:
		if f.RunOpen {
			p := decode[VerifyEndedPayload](e.Payload)
			f.Verify, f.LastVerifyOutput, f.VerifyCommand = p.Result, p.Output, p.Command
		}
	case ReviewStarted:
		f.claimed()
		f.started = e.At
	case ReviewEnded:
		p := decode[ReviewEndedPayload](e.Payload)
		f.sessionEnded(e.At, p.Turns, p.CostMissing)

		if f.RunOpen {
			f.reviewEnded(p)
		}
	case ExportStarted, PublishStarted, ReadyStarted, ReplyStarted:
		f.claimed()
	case Replied:
		f.Replied = true
	case Exported:
		f.Exported, f.Export = true, decode[ExportedPayload](e.Payload)
	case Published:
		f.published(decode[PublishedPayload](e.Payload), e.At)
	case PRObserved:
		f.observed(decode[PRObservedPayload](e.Payload))
	case PRReady:
		if f.PR != nil {
			f.PR.MarkedReady, f.PR.Since, f.PR.Looked = true, e.At, false
		}
	case Merged:
		f.Merged = true
	case Stopped:
		p := decode[StoppedPayload](e.Payload)
		f.OpenStop = &StopFact{Reason: p.Reason, Explanation: p.Explanation, Action: p.Action, ResetsAt: p.ResetsAt, At: e.At, Fault: p.Fault}

		if p.Reason == StopSandbox || p.Fault {
			f.Faults++
		}
	}
}

// Hiding the session before a person's action keeps Decide from stopping again
// on the result the person just acted on.
func (f *Facts) humanAction(e store.Event) {
	f.clearStop(e)
	f.Session = nil
}

// clearStop closes the open stop and the review's halt, and restarts the
// count of the caps, of the budget and of the watch's clock from the human
// action e, after which the watch waits for a new look. The requests answered
// stay answered, and what was seen stays seen: a fix run whose failed prepare
// a person retries takes the requests that came since from it.
func (f *Facts) clearStop(e store.Event) {
	f.OpenStop, f.Review.Halt = nil, nil
	f.Continuations, f.VerifyFixes, f.ReviewFixes, f.Faults, f.CIFixes, f.CommentRuns = 0, 0, 0, 0, 0, 0
	f.Turns, f.SessionTime, f.LastHumanEventID = 0, 0, e.ID

	if f.PR != nil {
		f.PR.FixedHead, f.PR.Since, f.PR.Looked = "", e.At, false
	}
}

// sessionEnded counts a session or review that ended at end: its turns and
// time toward the budget, and a missing cost. One a fault cut never ends, so it
// counts nothing.
func (f *Facts) sessionEnded(end time.Time, turns int, costMissing bool) {
	f.Turns += turns
	if !f.started.IsZero() {
		f.SessionTime += end.Sub(f.started)
	}

	if costMissing {
		f.CostMissing++
	}

	f.started = time.Time{}
}

// claimed closes what an action's claim closes: the open stop, and the
// review's halt, which is no longer the newest thing that happened.
func (f *Facts) claimed() {
	f.OpenStop, f.Review.Halt = nil, nil
}

// resume records a retry or answer that clears an open stop; it must run
// before humanAction closes that stop.
func (f *Facts) resume(answer string) {
	if f.OpenStop != nil {
		f.Resumed = &Resume{From: f.OpenStop.Reason, Answer: answer}
	}
}

func (f *Facts) sessionStarted(p SessionStartedPayload) {
	f.claimed()

	if p.Automatic {
		switch p.Mode {
		case ModeContinue:
			f.Continuations++
		case ModeFixVerify:
			f.VerifyFixes++
		case ModeFixReview:
			f.ReviewFixes++
		}
	}

	if f.RunOpen {
		f.Session, f.Verify, f.Role = nil, "", p.Role
		if p.SessionID != "" {
			f.SessionID = p.SessionID
		}

		if p.Mode == ModeFixReview {
			f.Review.FixRan = true
		}
	}
}

// keepsSession are the stops whose retry is no resume.
var keepsSession = map[string]bool{
	StopSandbox: true, StopPushRejected: true, StopCIFailed: true, StopCITimeout: true, StopNoChecks: true, StopConflict: true,
	StopTakenOver: true, StopChangesRequested: true,
}

// published records the head a publish put on the task's branch, the export
// it took there, and its pull request, whose new head nothing has been seen of yet.
func (f *Facts) published(p PublishedPayload, at time.Time) {
	pr := f.PR
	if pr == nil {
		pr = &PRFact{}
	}

	pr.Number, pr.URL, pr.NodeID, pr.Branch, pr.Head = p.Number, p.URL, p.NodeID, p.Branch, p.Head
	pr.Heads, pr.Since, pr.Export, pr.Seen = append(pr.Heads, p.Head), at, f.Export, nil
	pr.Looked, pr.TakeBack = false, false
	f.PR = pr
}

// observed records what the forge showed of the pull request, and gives CAVU
// the head it shows when a retry owes that and the head is not one of CAVU's.
// A merge closes the open stop: a done task has nothing left to retry.
func (f *Facts) observed(o Observation) {
	switch o.State {
	case PRMerged:
		f.Merged, f.OpenStop = true, nil
	case PRClosed:
		f.Cancelled = true
	}

	if pr := f.PR; pr != nil {
		if pr.TakeBack && !slices.Contains(pr.Heads, o.Head) {
			pr.Head, pr.Heads = o.Head, append(pr.Heads, o.Head)
		}

		pr.Seen, pr.Looked, pr.TakeBack = &o, true, false
	}
}

// fixRunStarted counts a run on the pull request's failed checks, which
// handles its head, and one on its reviewers' requests, which answers them.
func (f *Facts) fixRunStarted(p RunStartedPayload) {
	if f.PR == nil {
		return
	}

	switch p.Reason {
	case ReasonCIFix:
		f.CIFixes++
		f.PR.FixedHead = p.Head
	case ReasonComments:
		f.CommentRuns++
		f.PR.Answered = append(f.PR.Answered, p.Requests...)
	}
}

// resetRun forgets the run but the task's session, its role and the review,
// which outlive it: the export that ends a run reads the review, and a fix run
// goes on with all three. A run that is no fix run forgets them as it starts.
// The run's reason and requests outlive it too, for the run that replaces it.
func (f *Facts) resetRun() {
	f.Prepared, f.PrepareFailure, f.Ecosystems = false, nil, nil
	f.Session, f.Verify, f.LastVerifyOutput, f.VerifyCommand = nil, "", "", ""
	f.Assumptions, f.RunHead, f.Replied = nil, "", false
}

// decode reads a payload. One that does not decode reads as empty, so the event
// still counts by its type: a garbled stop or session end still stops the task.
func decode[T any](raw json.RawMessage) T {
	var p T
	if err := json.Unmarshal(raw, &p); err != nil {
		var empty T

		return empty
	}

	return p
}
