package engine

import (
	"encoding/json"
	"time"

	"github.com/mhersson/cavu/internal/store"
)

// Facts is what a task's events add up to.
type Facts struct {
	Started, Cancelled, Merged bool
	OpenStop                   *StopFact // newest stop with no human action and no claim after it
	RunOpen                    bool
	Prepared                   bool     // in the open run
	Ecosystems                 []string // of the open run's prepare, which Work, Verify and Export run with
	PrepareFailure             *PrepareFailure
	SessionID                  string       // of the open run, "" before the first session
	Session                    *SessionFact // newest ended session in the open run; nil if none, if a human action other than a retry that is no resume came after it, or while a newer session is pending
	Verify                     string       // result for the newest ended session; "" while a session or a verify is pending
	Exported                   bool
	Resumed                    *Resume // a human retry or answer after a stop, other than a retry that is no resume, not yet followed by a finished session or prepare, or by a new run when it came from environment
	LastVerifyOutput           string  // of the newest verify_ended, for the next fix prompt
	Continuations, VerifyFixes int     // session starts marked automatic, per mode, since the last human action
	ReviewFixes                int     // automatic fix_review session starts since the last human action
	Faults                     int     // sandbox stops since the last human action
	Review                     ReviewFact
	Role                       string   // of the run's session, strong after a fresh fix session; "" means work
	Answers                    []string // every answer a person gave on the task
	Assumptions                []string // of the open run's sessions
}

// StopFact is the open stop.
type StopFact struct {
	Reason, Explanation string
	ResetsAt            time.Time
	At                  time.Time // when the stop was recorded
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
		f.humanAction()
	case HumanAnswered:
		text := decode[HumanAnsweredPayload](e.Payload).Text
		f.resume(text)
		f.humanAction()
		f.Answers = append(f.Answers, text)
	case HumanRetried:
		// A sandbox stop is a fault that cut an action short, and a review's halt
		// cut a review: neither is the work session's result, so the retry is no
		// resume and keeps the session, and Decide redoes the cut action.
		if f.stoppedFor(StopSandbox) || f.Review.Halt != nil {
			f.clearStop()
		} else {
			f.resume("")
			f.humanAction()
		}
	case HumanCancelled:
		f.Cancelled = true
		f.humanAction()
	case RunStarted:
		// The new run is what a resume from environment asked for.
		if f.resumedFrom(StopEnvironment) {
			f.Resumed = nil
		}

		f.resetRun()
		f.RunOpen, f.Review = true, ReviewFact{}
	case RunEnded:
		f.resetRun()
		f.RunOpen = false
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
	case SessionEnded:
		f.Resumed = nil
		if f.RunOpen {
			p := decode[SessionEndedPayload](e.Payload)
			f.Verify = ""
			f.Session = &SessionFact{
				Outcome: p.Outcome, Changed: p.Changed, Title: p.Title, Summary: p.Summary, Detail: p.Detail,
				HeadCommit: p.HeadCommit, Questions: p.Questions, SuggestedAnswers: p.SuggestedAnswers, ResetsAt: p.ResetsAt,
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
			f.Verify, f.LastVerifyOutput = p.Result, p.Output
		}
	case ReviewStarted:
		f.claimed()
	case ReviewEnded:
		if f.RunOpen {
			f.reviewEnded(decode[ReviewEndedPayload](e.Payload))
		}
	case ExportStarted:
		f.claimed()
	case Exported:
		f.Exported = true
	case Merged:
		f.Merged = true
	case Stopped:
		p := decode[StoppedPayload](e.Payload)
		f.OpenStop = &StopFact{Reason: p.Reason, Explanation: p.Explanation, ResetsAt: p.ResetsAt, At: e.At}

		if p.Reason == StopSandbox {
			f.Faults++
		}
	}
}

// Hiding the session before a person's action keeps Decide from stopping again
// on the result the person just acted on.
func (f *Facts) humanAction() {
	f.clearStop()
	f.Session = nil
}

// clearStop closes the open stop and the review's halt, and restarts the caps'
// count, which runs from the last human action.
func (f *Facts) clearStop() {
	f.OpenStop, f.Review.Halt = nil, nil
	f.Continuations, f.VerifyFixes, f.ReviewFixes, f.Faults = 0, 0, 0, 0
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

// resetRun forgets the run; the review is kept past the run's end, for the
// export that ends it, and forgotten when a new run starts.
func (f *Facts) resetRun() {
	f.Prepared, f.PrepareFailure, f.Ecosystems = false, nil, nil
	f.SessionID, f.Session, f.Verify, f.LastVerifyOutput = "", nil, "", ""
	f.Role, f.Assumptions = "", nil
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
