package engine

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/mhersson/cavu/internal/store"
)

// EventText is the one sentence that tells an event to a person. A type the
// engine does not know is its raw payload.
func EventText(e store.Event) string {
	switch e.Type {
	case TaskStarted:
		return "the task is started"
	case HumanAnswered:
		return "a person answered: " + decode[HumanAnsweredPayload](e.Payload).Text
	case HumanRetried:
		return "a person asked for another try"
	case HumanCancelled:
		return "a person cancelled the task"
	case HumanRestored:
		return "a person requested a return to draft"
	case PrepareStarted:
		return decode[PrepareStartedPayload](e.Payload).Why
	case Prepared:
		p := decode[PreparedPayload](e.Payload)

		return fmt.Sprintf("prepared on base commit %s; the baseline verify %s", shortCommit(p.BaseCommit), p.Baseline)
	case VerifyStarted:
		return decode[VerifyStartedPayload](e.Payload).Why
	case ExportStarted:
		return decode[ExportStartedPayload](e.Payload).Why
	case Exported:
		p := decode[ExportedPayload](e.Payload)

		return fmt.Sprintf("exported %s, head %s", counted(p.FilesChanged, "changed file"), shortCommit(p.HeadCommit))
	case Merged:
		return "a person marked the task's change as merged"
	case RunEnded:
		p := decode[RunEndedPayload](e.Payload)
		if p.Why == "" {
			return "the run ended: " + p.Outcome
		}

		return fmt.Sprintf("the run ended: %s, %s", p.Outcome, p.Why)
	case RateLimit:
		p := decode[RateLimitPayload](e.Payload)
		s := fmt.Sprintf("the %s usage window is %s", p.Window, p.Status)

		if !p.ResetsAt.IsZero() {
			s += ", resetting " + p.ResetsAt.UTC().Format(time.RFC3339)
		}

		return s
	case EgressDenied:
		return "the sandbox was denied a connection to " + decode[EgressDeniedPayload](e.Payload).Host
	case AgentText:
		return decode[AgentTextPayload](e.Payload).Text
	case AgentTool:
		p := decode[AgentToolPayload](e.Payload)

		return p.Name + " " + p.Summary
	case SessionStarted:
		return decode[SessionStartedPayload](e.Payload).Mode
	case SessionEnded:
		p := decode[SessionEndedPayload](e.Payload)

		return fmt.Sprintf("%s after %s", p.Outcome, counted(p.Turns, "turn"))
	case ReviewStarted:
		if decode[ReviewStartedPayload](e.Payload).Scoped {
			return "the fix and the findings it was to address"
		}

		return "the whole change"
	case ReviewEnded:
		p := decode[ReviewEndedPayload](e.Payload)
		s := fmt.Sprintf("%s after %s with %s", p.Outcome, counted(p.Turns, "turn"), counted(len(p.Findings), "finding"))

		if p.Scoped && p.Outcome == ReviewReviewed {
			addressed := 0

			for _, a := range p.Addressed {
				if a {
					addressed++
				}
			}

			s += fmt.Sprintf("; %d of the %d open ones addressed", addressed, len(p.Addressed))
		}

		return s
	case VerifyEnded:
		return decode[VerifyEndedPayload](e.Payload).Result
	case PrepareFailed:
		return decode[PrepareFailedPayload](e.Payload).Step
	case Stopped:
		p := decode[StoppedPayload](e.Payload)

		return p.Reason + ": " + p.Explanation
	case RunStarted:
		return runText(decode[RunStartedPayload](e.Payload))
	case SessionCut:
		return "cut after " + counted(decode[SessionCutPayload](e.Payload).Turns, "turn")
	case PublishStarted, ReadyStarted, ReplyStarted:
		return decode[struct{ Why string }](e.Payload).Why
	case Published:
		p := decode[PublishedPayload](e.Payload)

		return fmt.Sprintf("pull request %s on branch %s", p.URL, p.Branch)
	case PRObserved:
		return observedText(decode[Observation](e.Payload))
	case PRReady:
		return "the pull request is marked ready for review"
	case Replied:
		return "the reviewers have CAVU's answer in a comment on the pull request"
	case HomeRemoved:
		return "the task's home was removed after the run ended"
	}

	return string(e.Payload)
}

// runText says why a run started: "retry" is a fresh run of a task that ran before.
func runText(p RunStartedPayload) string {
	switch p.Reason {
	case "initial":
		return "the task's first run"
	case "retry":
		return "a fresh run of the task, which ran before"
	case ReasonCIFix:
		return "a fix run on the pull request's failed checks, from " + shortCommit(p.Head)
	case ReasonComments:
		return fmt.Sprintf("a follow-up run on %s of reviewers, from %s", counted(len(p.Requests), "request"), shortCommit(p.Head))
	}

	return p.Reason
}

// shortCommit is a commit id's first twelve characters, enough to tell it in a line.
func shortCommit(id string) string {
	return id[:min(12, len(id))]
}

// observedText is what a look showed of the pull request. A reviewer's
// request is counted and never quoted.
func observedText(o Observation) string {
	s := o.State
	if o.Draft {
		s += ", a draft"
	}

	if o.Conflict {
		s += ", in conflict with its base branch"
	}

	return fmt.Sprintf("%s; checks %s; %s of reviewers", s, ChecksText(o.Checks, o.Failed), counted(len(o.Comments), "open request"))
}

// ChecksText is a pull request's checks, the failed ones named.
func ChecksText(checks string, failed []string) string {
	switch {
	case checks == "":
		return "not read yet"
	case len(failed) == 0:
		return checks
	}

	return checks + ": " + strings.Join(failed, ", ")
}

// counted returns n with noun, pluralised unless n is 1.
func counted(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}

	return strconv.Itoa(n) + " " + noun + "s"
}
