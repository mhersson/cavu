package engine

import (
	"encoding/json"
	"time"

	"github.com/mhersson/cavu/internal/store"
)

// Facts is what a task's events add up to.
type Facts struct {
	Started, Cancelled         bool
	OpenStop                   *StopFact // newest stop with no human action and no session start after it
	RunOpen                    bool
	Prepared                   bool     // in the open run
	Ecosystems                 []string // of the open run's prepare, which Work, Verify and Export run with
	PrepareFailure             *PrepareFailure
	Pending                    string       // an action claimed and not finished: "", "prepare", "work", "verify", "export"
	SessionID                  string       // of the open run, "" before the first session
	Session                    *SessionFact // newest ended session in the open run; nil if none, if a human action other than a sandbox retry came after it, or while a newer session is pending
	Verify                     string       // result for the newest ended session; "" while a session or a verify is pending
	Exported                   bool
	Resumed                    *Resume // a human retry or answer after a stop, other than a sandbox retry, not yet followed by a finished session or prepare, or by a new run when it came from environment
	LastVerifyOutput           string  // of the newest verify_ended, for the next fix prompt
	Continuations, VerifyFixes int     // session starts marked automatic, per mode, since the last human action
}

// StopFact is the open stop.
type StopFact struct {
	Reason, Explanation string
	ResetsAt            time.Time
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

// apply ignores run-scoped results while no run is open, since those facts
// describe the open run.
func (f *Facts) apply(e store.Event) {
	switch e.Type {
	case TaskStarted:
		f.Started = true
		f.humanAction()
	case HumanAnswered:
		f.resume(decode[HumanAnsweredPayload](e.Payload).Text)
		f.humanAction()
	case HumanRetried:
		// A sandbox stop is a fault that cut an action short, not a session's
		// result: its retry is no resume and keeps the session, so Decide redoes that action.
		if f.stoppedFor(StopSandbox) {
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
		f.RunOpen = true
	case RunEnded:
		f.resetRun()
		f.RunOpen = false
	case PrepareStarted:
		if f.RunOpen {
			f.Pending, f.Prepared, f.PrepareFailure, f.Ecosystems = string(KindPrepare), false, nil, nil
		}
	case Prepared:
		f.Resumed = nil
		if f.RunOpen {
			f.Pending, f.Prepared = "", true
			f.Ecosystems = decode[PreparedPayload](e.Payload).Ecosystems
		}
	case PrepareFailed:
		f.Resumed = nil
		if f.RunOpen {
			p := decode[PrepareFailedPayload](e.Payload)
			f.Pending, f.PrepareFailure = "", &PrepareFailure{Step: p.Step, Output: p.Output}
		}
	case SessionStarted:
		f.sessionStarted(decode[SessionStartedPayload](e.Payload))
	case SessionEnded:
		f.Resumed = nil
		if f.RunOpen {
			p := decode[SessionEndedPayload](e.Payload)
			f.Pending, f.Verify = "", ""
			f.Session = &SessionFact{
				Outcome: p.Outcome, Changed: p.Changed, Title: p.Title, Summary: p.Summary,
				Detail: p.Detail, HeadCommit: p.HeadCommit, Questions: p.Questions, ResetsAt: p.ResetsAt,
			}
		}
	case VerifyStarted:
		if f.RunOpen {
			f.Pending, f.Verify = string(KindVerify), ""
		}
	case VerifyEnded:
		if f.RunOpen {
			p := decode[VerifyEndedPayload](e.Payload)
			f.Pending, f.Verify, f.LastVerifyOutput = "", p.Result, p.Output
		}
	case ExportStarted:
		if f.RunOpen {
			f.Pending = string(KindExport)
		}
	case Exported:
		f.Exported, f.Pending = true, ""
	case Stopped:
		// A stop is also the result of whatever action was pending.
		p := decode[StoppedPayload](e.Payload)
		f.OpenStop = &StopFact{Reason: p.Reason, Explanation: p.Explanation, ResetsAt: p.ResetsAt}
		f.Pending = ""
	}
}

// Hiding the session before a person's action keeps Decide from stopping again
// on the result the person just acted on.
func (f *Facts) humanAction() {
	f.clearStop()
	f.Session = nil
}

// clearStop closes the open stop and restarts the caps' count, which runs from
// the last human action.
func (f *Facts) clearStop() {
	f.OpenStop = nil
	f.Continuations, f.VerifyFixes = 0, 0
}

// resume records a retry or answer that clears an open stop; it must run
// before humanAction closes that stop.
func (f *Facts) resume(answer string) {
	if f.OpenStop != nil {
		f.Resumed = &Resume{From: f.OpenStop.Reason, Answer: answer}
	}
}

func (f *Facts) sessionStarted(p SessionStartedPayload) {
	f.OpenStop = nil

	if p.Automatic {
		switch p.Mode {
		case ModeContinue:
			f.Continuations++
		case ModeFixVerify:
			f.VerifyFixes++
		}
	}

	if f.RunOpen {
		f.Pending, f.Session, f.Verify = string(KindWork), nil, ""
		if p.SessionID != "" {
			f.SessionID = p.SessionID
		}
	}
}

func (f *Facts) resetRun() {
	f.Prepared, f.PrepareFailure, f.Pending, f.Ecosystems = false, nil, "", nil
	f.SessionID, f.Session, f.Verify, f.LastVerifyOutput = "", nil, "", ""
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
