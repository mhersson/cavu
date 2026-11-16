package engine

import (
	"fmt"
	"strings"
	"time"

	"github.com/mhersson/cavu/internal/store"
)

// Switches turn a project's removable steps on or off.
type Switches struct{ Continuations, VerifyFix, WebTools bool }

// Caps bound the automatic rounds counted in Facts.
type Caps struct{ Continuations, VerifyFixes int }

// Snapshot is a task at one moment, as Decide sees it.
type Snapshot struct {
	Task        store.Task
	Project     store.Project
	Facts       Facts
	Now         time.Time
	Switches    Switches
	Caps        Caps
	LastEventID int64 // for the loop; Decide does not read it
}

// ActionKind names what an Action does.
type ActionKind string

// Action kinds.
const (
	KindNone    ActionKind = "none"
	KindPrepare ActionKind = "prepare"
	KindWork    ActionKind = "work"
	KindVerify  ActionKind = "verify"
	KindExport  ActionKind = "export"
	KindFinish  ActionKind = "finish"
	KindWait    ActionKind = "wait"
	KindStop    ActionKind = "stop"
)

// Stop reasons.
const (
	StopQuestion     = "question"
	StopEnvironment  = "environment"
	StopVerifyFailed = "verify_failed"
	StopBudget       = "budget"
	StopTimeout      = "timeout"
	StopNoChange     = "no_change"
	StopAgentError   = "agent_error"
	StopUsageLimit   = "usage_limit"
	StopEndpoint     = "endpoint"
	StopSandbox      = "sandbox"
	StopCancelled    = "cancelled"
)

// UsageLimitFallback is how long a usage limit holds when its reset time is
// unknown or already past, so a usage-limit cycle never resumes without real time passing.
const UsageLimitFallback = 30 * time.Minute

// The actions that clear a stop.
const (
	ClearRetry  = "retry"
	ClearAnswer = "answer"
	ClearNone   = "none"
)

// Action is a task's one next step.
type Action struct {
	Kind      ActionKind
	Why       string    // one line, recorded with the claim
	WorkMode  string    // work: a Mode value
	Automatic bool      // work: an automatic round, recorded on session_started and counted toward its cap
	Detail    string    // work: the answer text, or empty
	NewRun    bool      // prepare: open a new run first
	Until     time.Time // wait
	Stop      string    // stop: the reason
	Explain   string    // stop: one line for a person
	Clear     string    // stop: the one action that clears it
	ResetsAt  time.Time // stop: when a usage limit resets, always after Now
}

// Decide returns the next action for s; the first rule that matches wins. It
// reads nothing but s, so the same snapshot always gives the same action.
func Decide(s Snapshot) Action {
	f := s.Facts

	if !f.Started {
		return Action{Kind: KindNone, Why: "The task has not been started."}
	}

	if f.Cancelled && !f.stoppedFor(StopCancelled) {
		return halt(StopCancelled, ClearNone, "The task was cancelled.", "The task was cancelled.")
	}

	if f.stoppedFor(StopUsageLimit) && s.Now.Before(f.OpenStop.ResetsAt) {
		return Action{Kind: KindWait, Why: "The usage limit has not reset yet.", Until: f.OpenStop.ResetsAt}
	}

	if f.stoppedFor(StopUsageLimit) {
		return work(ModeContinue, "The usage limit has reset.")
	}

	if f.OpenStop != nil {
		return Action{Kind: KindNone, Why: "The task is stopped until a person acts."}
	}

	if f.Exported && f.RunOpen {
		return Action{Kind: KindFinish, Why: "The change is exported and its run is still open."}
	}

	if f.Exported {
		return Action{Kind: KindNone, Why: "The change is exported."}
	}

	if f.resumedFrom(StopEnvironment) || !f.RunOpen {
		return Action{Kind: KindPrepare, Why: "The task needs a fresh run.", NewRun: true}
	}

	if !f.Prepared && f.PrepareFailure == nil {
		return Action{Kind: KindPrepare, Why: "The run is not prepared."}
	}

	if f.PrepareFailure != nil {
		return halt(StopEnvironment, ClearRetry, "Preparing the run failed.",
			fmt.Sprintf("Preparing the workspace failed at step %q.", f.PrepareFailure.Step))
	}

	if f.resumedFrom(StopQuestion) && f.Resumed.Answer != "" {
		a := work(ModeAnswer, "A person answered the agent's question.")
		a.Detail = f.Resumed.Answer

		return a
	}

	if f.resumedFrom(StopVerifyFailed) {
		return work(ModeFixVerify, "A person asked for another fix after verify failed.")
	}

	if f.Resumed != nil {
		return work(ModeContinue, "A person asked the agent to carry on.")
	}

	if f.Session == nil {
		mode := ModeStart
		if f.SessionID != "" {
			mode = ModeContinue
		}

		return work(mode, "The run has no finished session to act on.")
	}

	sess := f.Session
	cut := sess.Outcome == OutcomeUnfinished || sess.Outcome == OutcomeCrashed
	failed := f.Verify == ResultFailed || f.Verify == ResultTimeout

	if sess.Outcome == OutcomeUsageLimit {
		resets := sess.ResetsAt
		if !resets.After(s.Now) {
			resets = s.Now.Add(UsageLimitFallback)
		}

		a := halt(StopUsageLimit, ClearNone, "The session hit a usage limit.",
			withDetail("The model's usage limit was reached; work resumes at "+resets.UTC().Format(time.RFC3339), sess.Detail))
		a.ResetsAt = resets

		return a
	}

	if sess.Outcome == OutcomeEndpoint {
		return halt(StopEndpoint, ClearRetry, "The session could not use the model endpoint.",
			withDetail("The model endpoint failed or refused the agent's requests", sess.Detail))
	}

	if sess.Outcome == OutcomeTimeout {
		return halt(StopTimeout, ClearRetry, "The session timed out.", "The agent session ran past its time limit.")
	}

	if sess.Outcome == OutcomeMaxTurns {
		return halt(StopBudget, ClearRetry, "The session used all its turns.", "The agent session used all of its turns.")
	}

	if sess.Outcome == OutcomeBlocked {
		return halt(StopQuestion, ClearAnswer, "The agent asked a question.",
			lineOr(first(sess.Questions), "The agent is blocked and needs an answer."))
	}

	if sess.Outcome == OutcomeFailed {
		return halt(StopAgentError, ClearRetry, "The agent reported a failure.",
			lineOr(sess.Summary, "The agent reported that it could not do the task."))
	}

	if cut && s.Switches.Continuations && f.Continuations < s.Caps.Continuations {
		a := work(ModeContinue, "The session stopped early and a continuation is left.")
		a.Automatic = true

		return a
	}

	if cut {
		return halt(StopAgentError, ClearRetry, "The session stopped early.",
			withDetail("The agent session ended before the work was finished", sess.Detail))
	}

	if sess.Outcome == OutcomeDone && !sess.Changed {
		return halt(StopNoChange, ClearRetry, "The agent changed nothing.", "The agent finished without changing any files.")
	}

	if sess.Outcome == OutcomeDone && f.Verify == "" {
		return Action{Kind: KindVerify, Why: "The change has not been verified."}
	}

	if failed && s.Switches.VerifyFix && f.VerifyFixes < s.Caps.VerifyFixes {
		a := work(ModeFixVerify, "Verify failed and a fix round is left.")
		a.Automatic = true

		return a
	}

	if failed {
		return halt(StopVerifyFailed, ClearRetry, "Verify failed.", "The change did not pass the verify command.")
	}

	if f.Verify == ResultPassed || f.Verify == ResultWaived {
		return Action{Kind: KindExport, Why: "The change passed verify, or verify is waived."}
	}

	return halt(StopAgentError, ClearRetry, "No rule matches the task's facts.",
		"The task reached a state CAVU has no rule for.")
}

func (f Facts) stoppedFor(reason string) bool {
	return f.OpenStop != nil && f.OpenStop.Reason == reason
}

func (f Facts) resumedFrom(reason string) bool {
	return f.Resumed != nil && f.Resumed.From == reason
}

func work(mode, why string) Action {
	return Action{Kind: KindWork, Why: why, WorkMode: mode}
}

func halt(reason, clears, why, explain string) Action {
	return Action{Kind: KindStop, Why: why, Stop: reason, Explain: explain, Clear: clears}
}

func first(texts []string) string {
	if len(texts) == 0 {
		return ""
	}

	return texts[0]
}

// maxExplain bounds a stop's explanation, in bytes.
const maxExplain = 300

// line folds text onto one line of at most maxExplain bytes. Text from the
// runner must be masked first, so a cut cannot leave the head of a secret behind.
func line(text string) string {
	return clip(strings.Join(strings.Fields(text), " "), maxExplain)
}

// lineOr returns text as one line, or fallback when it is blank, so every stop
// carries an explanation.
func lineOr(text, fallback string) string {
	if l := line(text); l != "" {
		return l
	}

	return fallback
}

// withDetail ends sentence with the runner's detail, such as an upstream
// status, so a person can tell a refused credential from an outage.
func withDetail(sentence, detail string) string {
	if line(detail) == "" {
		return sentence + "."
	}

	return line(sentence + ": " + detail)
}
