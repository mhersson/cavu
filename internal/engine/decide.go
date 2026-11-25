package engine

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/mhersson/cavu/internal/store"
)

// Switches hold a project's on or off choices. Publish is the project's choice
// to put its tasks' changes on the forge as pull requests.
type Switches struct{ Continuations, VerifyFix, WebTools, Readiness, Review, Publish bool }

// Caps are the counts a task's automatic rounds stop at. CIFixes counts runs
// on a pull request's failed checks, CommentRuns runs on its reviewers' requests.
type Caps struct {
	Continuations, VerifyFixes, ReviewFixes, FaultRetries, CIFixes, CommentRuns int
}

// Pause is what other tasks' open stops mean for a task that wants a session.
type Pause struct {
	EndpointTask int64     // another task that holds an open endpoint stop, 0 when none
	LimitedUntil time.Time // the reset time of another task's open usage_limit stop, zero when none
	LimitedBy    int64     // the task whose usage_limit stop sets LimitedUntil
}

// Dep is one dependency of a task, with where it stands.
type Dep struct {
	TaskID int64  `json:"id"`
	Status Status `json:"status"`
}

// Snapshot is a task at one moment, as Decide sees it.
type Snapshot struct {
	Task        store.Task
	Facts       Facts
	Deps        []Dep // in id order
	Pause       Pause
	Now         time.Time
	Switches    Switches
	Caps        Caps
	Limits      Limits
	Spent       Spent
	LastEventID int64 // for the loop; Decide does not read it

	// Watch is the project's CI as the watch of its pull request reads it: none
	// at all, and how long its checks may stay unfinished, 0 for no limit.
	Watch struct {
		NoCI      bool
		CITimeout time.Duration
	}

	prices map[string]Price // for the budget of a run under a money cap; Decide does not read them
}

// ActionKind names what an Action does.
type ActionKind string

// Action kinds.
const (
	KindNone    ActionKind = "none"
	KindPrepare ActionKind = "prepare"
	KindWork    ActionKind = "work"
	KindVerify  ActionKind = "verify"
	KindReview  ActionKind = "review"
	KindExport  ActionKind = "export"
	KindPublish ActionKind = "publish"
	KindFinish  ActionKind = "finish"
	KindWatch   ActionKind = "watch"
	KindReady   ActionKind = "ready"
	KindClean   ActionKind = "clean"
	KindReply   ActionKind = "reply"
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
	StopDependency   = "dependency"

	StopPushRejected     = "push_rejected"     // the forge refused the push of the task's branch
	StopCIFailed         = "ci_failed"         // the pull request's checks still fail after the fix runs
	StopCITimeout        = "ci_timeout"        // the checks stayed unfinished past ci_timeout_minutes
	StopNoChecks         = "no_checks"         // no check started on the pull request
	StopConflict         = "conflict"          // the pull request conflicts with its base
	StopTakenOver        = "taken_over"        // the branch holds a head CAVU did not push
	StopChangesRequested = "changes_requested" // reviewers still ask for changes after the follow-up runs
)

// The waits before a runner fault or a failed model endpoint is retried: after
// the first fault since the last human action, and after the second.
const (
	firstFaultWait  = 30 * time.Second
	secondFaultWait = 2 * time.Minute
)

// checksGrace is how long a pull request's head may show no check before CAVU
// marks the pull request ready, and again after, before the task stops.
const checksGrace = 3 * time.Minute

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
	Fresh     bool      // work fix_review: a new session in role strong
	Scoped    bool      // review: a re-review of the open findings and of what the fix broke
	Detail    string    // work: the answer text, or empty
	NewRun    bool      // prepare: open a new run first
	Reason    string    // prepare of a new run: its reason, "" for the loop's own
	Requests  []string  // prepare of a new comments run, and work comments: the ids of the requests the run is given
	Until     time.Time // wait and watch: when a clock ends it, zero when none
	Stop      string    // stop: the reason
	Explain   string    // stop: one line for a person
	Clear     string    // stop: the one action that clears it
	ResetsAt  time.Time // stop: when a usage limit resets, always after Now
	Fault     bool      // stop: an endpoint that failed, not refused, which Decide retries
}

// Decide returns the next action for s; the first rule that matches wins, then
// the task's budget stops a session, and the day's money and the pauses hold
// a prepare or a session. It reads nothing but s, so the same snapshot always
// gives the same action.
func Decide(s Snapshot) Action {
	a := decide(s)
	if a.Kind == KindWork || a.Kind == KindReview {
		if stop, ok := overBudget(s); ok {
			return stop
		}
	}

	if a.Kind != KindPrepare && a.Kind != KindWork && a.Kind != KindReview {
		return a
	}

	if wait, ok := dayUsed(s); ok {
		return wait
	}

	// A task's own retry of its failed endpoint goes ahead: in an outage every
	// running session fails, and the stops would hold each other's retries.
	retrying := s.Facts.OpenStop != nil && s.Facts.OpenStop.Fault
	if t := s.Pause.EndpointTask; t != 0 && !retrying {
		return Action{Kind: KindWait, Why: fmt.Sprintf("Task %d stopped on the model endpoint; retrying or cancelling it lifts the pause.", t)}
	}

	if s.Now.Before(s.Pause.LimitedUntil) {
		return Action{
			Kind: KindWait, Why: fmt.Sprintf("Task %d hit the usage limit, which has not reset yet.", s.Pause.LimitedBy), Until: s.Pause.LimitedUntil,
		}
	}

	return a
}

func decide(s Snapshot) Action {
	f := s.Facts

	if !f.Started {
		return Action{Kind: KindNone, Why: "The task has not been started."}
	}

	if f.Cancelled && !f.stoppedFor(StopCancelled) {
		return halt(StopCancelled, ClearNone, "The task was cancelled.", cancelled(f.PR))
	}

	// A fix run may be open when the merge is seen; it ends, whatever it did.
	if f.Merged && f.RunOpen {
		return Action{Kind: KindFinish, Why: "The change was merged and its run is still open."}
	}

	// The home holds the session a fix run on the pull request resumes. With no
	// pull request after the export, as for a replay's shadow, no such run comes.
	if f.Home && (f.Merged || f.Cancelled || f.Exported && f.PR == nil) {
		return Action{Kind: KindClean, Why: "The task no longer needs the previous run's home."}
	}

	if f.Merged {
		return Action{Kind: KindNone, Why: "The change was merged."}
	}

	if f.stoppedFor(StopUsageLimit) && s.Now.Before(f.OpenStop.ResetsAt) {
		return Action{Kind: KindWait, Why: "The usage limit has not reset yet.", Until: f.OpenStop.ResetsAt}
	}

	if f.stoppedFor(StopUsageLimit) {
		if h := f.Review.Halt; h == nil || h.Outcome != OutcomeUsageLimit {
			return work(ModeContinue, "The usage limit has reset.")
		}

		// The limit ended a review. The claim decided below closes the stop; the
		// halt goes with it here, or it would give the same stop again.
		f.OpenStop, f.Review.Halt = nil, nil
	}

	resumeWhy := "A person asked the agent to carry on."

	if f.retriesFault(s.Caps) {
		backoff := firstFaultWait
		if f.Faults > 1 {
			backoff = secondFaultWait
		}

		if until := f.OpenStop.At.Add(backoff); s.Now.Before(until) {
			why := "The runner failed; the action it cut is retried after a pause."
			if f.OpenStop.Fault {
				why = "The model endpoint failed; the session it ended is tried again after a pause."
			}

			return Action{Kind: KindWait, Why: why, Until: until}
		}

		// A failed endpoint's session is resumed as a person's retry resumes it, but
		// the task keeps its caps and its budget's counts. A review is redone.
		if f.OpenStop.Fault && f.Review.Halt == nil {
			f.resume("")

			resumeWhy = "The model endpoint failed; after the pause the agent carries on with its session."
		}

		// The claim of the action decided below closes the stop and a review's halt.
		f.OpenStop, f.Review.Halt = nil, nil
	}

	// A cancelled task is over: its pull request, open or closed, is not watched.
	if f.OpenStop != nil && f.PR != nil && !f.Cancelled {
		return Action{Kind: KindWatch, Why: "The task is stopped until a person acts; its pull request is watched for a merge or a close."}
	}

	if f.OpenStop != nil {
		return Action{Kind: KindNone, Why: "The task is stopped until a person acts."}
	}

	// Before the publish: the open run's export cannot descend from a head a retry
	// takes back. A retry that gives the branch back takes the next look's head.
	if pr := f.PR; pr != nil && pr.Seen != nil && !pr.TakeBack && !slices.Contains(pr.Heads, pr.Seen.Head) {
		return halt(StopTakenOver, ClearRetry, "The task's branch holds a commit CAVU did not push.", takenOver(pr, f.RunOpen))
	}

	// A replay's shadow reruns an old task to compare outcomes: its change is never published.
	if f.Exported && f.RunOpen && s.Switches.Publish && s.Task.ReplayID == 0 && (f.PR == nil || f.PR.Head != f.Export.HeadCommit) {
		return Action{Kind: KindPublish, Why: "The exported change is not yet on the task's branch on GitHub."}
	}

	// A follow-up run answers once its work is over: its change published, or
	// its session done without moving the head. Then it finishes.
	unchanged := f.Session != nil && f.Session.Outcome == OutcomeDone && !f.Session.Changed
	if f.RunOpen && f.RunReason == ReasonComments && !f.Replied && (f.Exported || unchanged) {
		return Action{Kind: KindReply, Why: "The follow-up run's work is over, and the reviewers have not had its answer on the pull request."}
	}

	if f.Exported && f.RunOpen {
		return Action{Kind: KindFinish, Why: "The change is exported and its run is still open."}
	}

	// A fix run that changed nothing is no stop: the watch decides what follows.
	if f.RunOpen && f.RunHead != "" && unchanged {
		return Action{Kind: KindFinish, Why: "The fix run's session changed nothing, so the run ends."}
	}

	// Not only an exported task: a fix run that changed nothing ends without an
	// export. A retry after a fix run's failed prepare goes on to start it again.
	if f.PR != nil && !f.RunOpen && !f.resumedFrom(StopEnvironment) {
		return watch(s, f.PR)
	}

	if f.Exported {
		return Action{Kind: KindNone, Why: "The change is exported."}
	}

	if !f.RunOpen {
		for _, d := range s.Deps {
			if d.Status == StatusCancelled {
				return halt(StopDependency, ClearRetry, "A dependency was cancelled.",
					fmt.Sprintf("Task %d, which this task depends on, was cancelled.", d.TaskID))
			}
		}

		for _, d := range s.Deps {
			if d.Status != StatusDone {
				return Action{Kind: KindWait, Why: fmt.Sprintf("Task %d, which this task depends on, is not done.", d.TaskID)}
			}
		}
	}

	if f.resumedFrom(StopEnvironment) || !f.RunOpen {
		return newRun(f)
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
		return work(ModeContinue, resumeWhy)
	}

	if f.Session == nil && f.SessionID == "" && s.Switches.Readiness {
		return work(ModeReadiness, "The agent has not yet checked the task for missing facts.")
	}

	if f.Session == nil && f.RunReason == ReasonCIFix {
		return work(ModeFixCI, "The fix run's session has not yet worked on the failed checks.")
	}

	if f.Session == nil && f.RunReason == ReasonComments {
		a := work(ModeComments, "The follow-up run's session has not yet worked on the reviewers' requests.")
		a.Requests = f.RunRequests

		return a
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
	passed := f.Verify == ResultPassed || f.Verify == ResultWaived

	if a, ok := sessionStop(sess, s); ok {
		return a
	}

	// The money rules then stop the task or hold it, or it works on under a new budget.
	if sess.Outcome == OutcomeBudget {
		return work(ModeContinue, "The session spent the budget it was given.")
	}

	if sess.Outcome == OutcomeBlocked {
		return halt(StopQuestion, ClearAnswer, "The agent asked a question.",
			lineOr(first(sess.Questions), "The agent is blocked and needs an answer."))
	}

	if sess.Outcome == OutcomeFailed {
		return halt(StopAgentError, ClearRetry, "The agent reported a failure.",
			lineOr(sess.Summary, "The agent reported that it could not do the task."))
	}

	if sess.Outcome == OutcomeReady {
		return work(ModeStart, "The agent found nothing missing in the task.")
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

	// A fix run was reviewed with the change; verify, CI and the reviewer judge it.
	if passed && s.Switches.Review && f.RunHead == "" {
		return reviewLoop(s, f)
	}

	if passed {
		return Action{Kind: KindExport, Why: "The change passed verify, or verify is waived."}
	}

	return halt(StopAgentError, ClearRetry, "No rule matches the task's facts.",
		"The task reached a state CAVU has no rule for.")
}

// newRun opens a run. With a pull request only a person's retry after a fix
// run's failed prepare comes here: the run starts again as it was, and a
// comments run is given the requests that came since too.
func newRun(f Facts) Action {
	if f.PR == nil || f.RunReason != ReasonCIFix && f.RunReason != ReasonComments {
		return Action{Kind: KindPrepare, Why: "The task needs a fresh run.", NewRun: true}
	}

	a := Action{Kind: KindPrepare, Why: "A person retried the fix run whose prepare failed.", NewRun: true, Reason: f.RunReason}
	if a.Reason == ReasonComments {
		a.Requests = append(slices.Clone(f.RunRequests), unanswered(f.PR)...)
	}

	return a
}

// watch decides on what was seen of the pull request of a task with no run
// open. Since is when the newest of its head, CAVU's ready-marking and a
// person's action came: its checks get their time from it, and only a look
// recorded after it is acted on.
func watch(s Snapshot, pr *PRFact) Action {
	f, seen := s.Facts, pr.Seen

	if !pr.Looked {
		return Action{Kind: KindWatch, Why: "CAVU waits for its next look at the pull request on GitHub: it acts only on a look taken after its own last push or ready-marking and after a person's last action."}
	}

	// An earlier head of CAVU's own is what GitHub showed before the newest push.
	if seen.Head != pr.Head {
		return Action{Kind: KindWatch, Why: "GitHub does not show the pull request's newest commit yet."}
	}

	if seen.Conflict {
		return halt(StopConflict, ClearRetry, "The pull request conflicts with its base branch.",
			line(fmt.Sprintf("Pull request %s conflicts with its base branch. CAVU resolves no conflict: "+
				"resolve it on the pull request's branch, and a retry goes on from your commit.", pr.URL)))
	}

	// Every check has finished when they read failed, so one fix run sees every failure.
	if seen.Checks == ChecksFailed && (pr.FixedHead == pr.Head || f.CIFixes >= s.Caps.CIFixes) {
		return halt(StopCIFailed, ClearRetry, "The pull request's checks still fail after CAVU's fix runs.",
			line(fmt.Sprintf("Checks failed on pull request %s: %s.", pr.URL, strings.Join(seen.Failed, ", "))))
	}

	if seen.Checks == ChecksFailed {
		return Action{Kind: KindPrepare, Why: "The pull request's checks failed; a fix run starts from its head.", NewRun: true, Reason: ReasonCIFix}
	}

	requests := unanswered(pr)
	if len(requests) > 0 && f.CommentRuns >= s.Caps.CommentRuns {
		return halt(StopChangesRequested, ClearRetry, "Reviewers still ask for changes after the follow-up runs.",
			line(fmt.Sprintf("Reviewers still ask for changes on pull request %s after %d follow-up runs.", pr.URL, f.CommentRuns)))
	}

	if len(requests) > 0 {
		return Action{
			Kind: KindPrepare, Why: "Reviewers asked for changes on the pull request; a follow-up run starts from its head.", NewRun: true,
			Reason: ReasonComments, Requests: requests,
		}
	}

	if seen.Checks == ChecksPending {
		return pending(s, pr)
	}

	if seen.Checks == ChecksNone && !s.Watch.NoCI {
		return noChecks(s, pr)
	}

	// Checks none here is a project without CI, which has nothing to wait for.
	if (seen.Checks == ChecksPassed || seen.Checks == ChecksNone) && seen.Draft && !pr.MarkedReady {
		return Action{Kind: KindReady, Why: "The pull request's checks passed, or its project has no CI, so it is ready for review."}
	}

	return Action{Kind: KindWatch, Why: "The pull request waits for a person to merge it."}
}

// pending waits for checks that have not finished, up to ci_timeout_minutes.
func pending(s Snapshot, pr *PRFact) Action {
	if s.Watch.CITimeout == 0 {
		return Action{Kind: KindWatch, Why: "The pull request's checks are running."}
	}

	until := pr.Since.Add(s.Watch.CITimeout)
	if s.Now.Before(until) {
		return Action{Kind: KindWatch, Why: "The pull request's checks are running.", Until: until}
	}

	return halt(StopCITimeout, ClearRetry, "The pull request's checks did not finish in time.",
		line(fmt.Sprintf("The checks on pull request %s did not finish within %d minutes, the setting ci_timeout_minutes.",
			pr.URL, int(s.Watch.CITimeout.Minutes()))))
}

// noChecks waits a grace for a first check, marks the pull request ready once,
// since many repositories run no CI on a draft, and waits the grace again.
func noChecks(s Snapshot, pr *PRFact) Action {
	until := pr.Since.Add(checksGrace)

	switch {
	case s.Now.Before(until) && !pr.MarkedReady:
		return Action{Kind: KindWatch, Why: "No check has started on the pull request yet.", Until: until}
	case s.Now.Before(until):
		return Action{Kind: KindWatch, Why: "No check has started on the pull request since CAVU marked it ready.", Until: until}
	case !pr.MarkedReady:
		return Action{Kind: KindReady, Why: "No check started on the pull request; it is marked ready, since many repositories run no CI on a draft."}
	}

	return halt(StopNoChecks, ClearRetry, "No check started on the pull request.",
		line(fmt.Sprintf("No check started on pull request %s after CAVU marked it ready. A project whose pull requests "+
			"may start no check, as with CI only for some paths, is marked no_ci: cavu project set NAME --no-ci on.", pr.URL)))
}

// unanswered is the ids of the requests seen holds that no follow-up run was
// given, in seen's order.
func unanswered(pr *PRFact) []string {
	var ids []string

	for _, c := range pr.Seen.Comments {
		if !slices.Contains(pr.Answered, c.ID) {
			ids = append(ids, c.ID)
		}
	}

	return ids
}

// cancelled explains a cancel: a person's, which leaves the pull request
// open, or the pull request's close.
func cancelled(pr *PRFact) string {
	switch {
	case pr == nil:
		return "The task was cancelled."
	case pr.Seen != nil && pr.Seen.State == PRClosed:
		return line(fmt.Sprintf("Pull request %s was closed without a merge.", pr.URL))
	}

	return line(fmt.Sprintf("The task was cancelled; pull request %s stays open.", pr.URL))
}

// takenOver explains a branch that holds a head CAVU did not push.
func takenOver(pr *PRFact, runOpen bool) string {
	text := fmt.Sprintf("The branch of pull request %s holds commit %s, which CAVU did not push; a retry lets CAVU go on from the commit the branch then holds.",
		pr.URL, pr.Seen.Head[:min(12, len(pr.Seen.Head))])
	if runOpen {
		text += " The open run's commits that were not pushed are dropped."
	}

	return line(text)
}

// sessionStop is the stop a session that ended as sess calls for, when its
// outcome is one that stops the task; a review's halt is decided by it too. A
// session's time and turns are what the task's budget left it.
func sessionStop(sess *SessionFact, s Snapshot) (Action, bool) {
	switch sess.Outcome {
	case OutcomeUsageLimit:
		resets := sess.ResetsAt
		if !resets.After(s.Now) {
			resets = s.Now.Add(UsageLimitFallback)
		}

		a := halt(StopUsageLimit, ClearNone, "The session hit a usage limit.",
			withDetail("The model's usage limit was reached; work resumes at "+resets.UTC().Format(time.RFC3339), sess.Detail))
		a.ResetsAt = resets

		return a, true
	case OutcomeEndpoint:
		return halt(StopEndpoint, ClearRetry, "The session could not use the model endpoint.",
			withDetail("The model endpoint refused the agent's requests", sess.Detail)), true
	case OutcomeEndpointFailed:
		a := halt(StopEndpoint, ClearRetry, "The model endpoint failed.",
			withDetail("The model endpoint failed", sess.Detail))
		a.Fault = true

		return a, true
	case OutcomeTimeout:
		return halt(StopTimeout, ClearRetry, "The session timed out.", timeUsed(s)), true
	case OutcomeMaxTurns:
		return halt(StopBudget, ClearRetry, "The session used all its turns.", turnsUsed(s)), true
	case OutcomeUnmetered:
		return halt(StopBudget, ClearRetry, "The proxy could not count the session's spend.",
			lineOr(sess.Detail, "The proxy could not count the session's spend against the money cap.")), true
	}

	return Action{}, false
}

func (f Facts) stoppedFor(reason string) bool {
	return f.OpenStop != nil && f.OpenStop.Reason == reason
}

// retriesFault reports a runner or endpoint fault Decide still retries after a pause.
func (f Facts) retriesFault(c Caps) bool {
	return (f.stoppedFor(StopSandbox) || f.OpenStop != nil && f.OpenStop.Fault) && f.Faults <= c.FaultRetries
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
