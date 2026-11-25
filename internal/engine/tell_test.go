package engine_test

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mhersson/cavu/internal/engine"
	"github.com/mhersson/cavu/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// told is the sentence of an event of the type with the payload.
func told(t *testing.T, typ string, payload any) string {
	t.Helper()

	raw, err := json.Marshal(payload)
	require.NoError(t, err)

	return engine.EventText(store.Event{Type: typ, Payload: raw})
}

func TestEventText(t *testing.T) {
	// A head as git gives it, and its first twelve characters; four requests,
	// a count that no other fact holds.
	head := "fed789abcdef0123456789abcdef0123456789ab"
	short := head[:12]
	requests := []string{"PRRT_kwDOAbc1", "PRRT_kwDOAbc2", "PRR_kwDOAbc3", "PRRT_kwDOAbc4"}
	resets := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)

	tests := []struct {
		typ     string
		payload any
		values  []string
	}{
		{engine.TaskStarted, struct{}{}, nil},
		{engine.HumanAnswered, engine.HumanAnsweredPayload{Text: "Use API version 2."}, []string{"Use API version 2."}},
		{engine.HumanRetried, struct{}{}, nil},
		{engine.HumanCancelled, struct{}{}, nil},
		{engine.HumanRestored, struct{}{}, nil},
		{engine.PrepareStarted, engine.PrepareStartedPayload{Why: "The task has no home yet."}, []string{"The task has no home yet."}},
		{
			engine.Prepared,
			engine.PreparedPayload{BaseCommit: head, Baseline: engine.ResultFailed, Tools: []string{"go"}},
			[]string{short, engine.ResultFailed},
		},
		{engine.VerifyStarted, engine.VerifyStartedPayload{Why: "The agent finished."}, []string{"The agent finished."}},
		{engine.ExportStarted, engine.ExportStartedPayload{Why: "The review passed."}, []string{"The review passed."}},
		{engine.Exported, engine.ExportedPayload{HeadCommit: head, FilesChanged: 14}, []string{short, "14"}},
		{engine.Merged, struct{}{}, nil},
		{engine.RunEnded, engine.RunEndedPayload{Outcome: engine.StopQuestion, Why: "An answer is needed."}, []string{engine.StopQuestion, "An answer is needed."}},
		{engine.RunEnded, engine.RunEndedPayload{Outcome: "exported"}, []string{"exported"}},
		{
			engine.RateLimit,
			engine.RateLimitPayload{Window: "five_hour", Status: "rejected", ResetsAt: resets},
			[]string{"five_hour", "rejected", resets.UTC().Format(time.RFC3339)},
		},
		{engine.EgressDenied, engine.EgressDeniedPayload{Host: "evil.example.com"}, []string{"evil.example.com"}},
		{engine.AgentText, engine.AgentTextPayload{Text: "Reading the handler.\nThen the tests."}, []string{"Reading the handler.", "Then the tests."}},
		{engine.AgentTool, engine.AgentToolPayload{Name: "Bash", Summary: "go test ./..."}, []string{"Bash", "go test ./..."}},
		{engine.SessionStarted, engine.SessionStartedPayload{Mode: engine.ModeFixVerify, SessionID: "abc"}, []string{engine.ModeFixVerify}},
		{engine.SessionEnded, engine.SessionEndedPayload{Outcome: engine.OutcomeUnfinished, Turns: 17}, []string{engine.OutcomeUnfinished, "17"}},
		{engine.VerifyEnded, engine.VerifyEndedPayload{Result: engine.ResultTimeout, Output: "FAIL"}, []string{engine.ResultTimeout}},
		{engine.PrepareFailed, engine.PrepareFailedPayload{Step: "setup", Output: "npm ERR!"}, []string{"setup"}},
		{engine.Stopped, engine.StoppedPayload{Reason: engine.StopQuestion, Explanation: "Which API version?"}, []string{engine.StopQuestion, "Which API version?"}},
		{engine.ReviewStarted, engine.ReviewStartedPayload{SessionID: "r1", Scoped: true, Head: "fed789", Why: "A fix changed the code."}, nil},
		{
			engine.ReviewEnded,
			engine.ReviewEndedPayload{
				Outcome: engine.ReviewReviewed, Scoped: true, Turns: 17, Findings: slices.Repeat([]engine.Finding{{Severity: engine.SeverityImportant}}, 4),
				Addressed: []bool{true, false, true, false, false, false},
			},
			[]string{engine.ReviewReviewed, "17", "4", "2", "6"},
		},
		{engine.SessionCut, engine.SessionCutPayload{Turns: 4}, []string{"4"}},
		{engine.PublishStarted, engine.PublishStartedPayload{Why: "The change is not on GitHub yet."}, []string{"The change is not on GitHub yet."}},
		{
			engine.Published,
			engine.PublishedPayload{Branch: "cavu/fix-the-login", Head: "fed789", Number: 7, URL: "https://github.com/acme/web/pull/7", NodeID: "PR_7"},
			[]string{"cavu/fix-the-login", "https://github.com/acme/web/pull/7"},
		},
		{
			engine.PRObserved,
			engine.Observation{
				State: engine.PROpen, Draft: true, Head: "fed789", Conflict: true, Checks: engine.ChecksFailed, Failed: []string{"lint", "unit-tests"},
				Comments: []engine.Comment{{ID: "c1", Author: "octocat"}, {ID: "c2", Author: "hubot"}},
			},
			[]string{engine.PROpen, engine.ChecksFailed, "lint", "unit-tests", "2"},
		},
		{engine.ReadyStarted, engine.ReadyStartedPayload{Why: "The checks passed."}, []string{"The checks passed."}},
		{engine.PRReady, struct{}{}, nil},
		{engine.ReplyStarted, engine.ReplyStartedPayload{Why: "The follow-up run is over."}, []string{"The follow-up run is over."}},
		{engine.Replied, struct{}{}, nil},
		{engine.HomeRemoved, engine.HomeRemovedPayload{Why: "The task can run no more."}, nil},
		{engine.RunStarted, engine.RunStartedPayload{Reason: "initial"}, nil},
		{engine.RunStarted, engine.RunStartedPayload{Reason: "retry"}, nil},
		{engine.RunStarted, engine.RunStartedPayload{Reason: engine.ReasonCIFix, Head: head}, []string{short}},
		{engine.RunStarted, engine.RunStartedPayload{Reason: engine.ReasonComments, Head: head, Requests: requests}, []string{short, strconv.Itoa(len(requests))}},
	}

	for _, tc := range tests {
		t.Run(tc.typ, func(t *testing.T) {
			raw, err := json.Marshal(tc.payload)
			require.NoError(t, err)

			l := told(t, tc.typ, tc.payload)

			for _, v := range tc.values {
				assert.Contains(t, l, v)
			}

			assert.NotEmpty(t, l)
			assert.NotContains(t, l, string(raw), "the sentence is words, not the payload")
		})
	}
}

func TestEveryEventTypeHasASentence(t *testing.T) {
	types := []string{
		engine.TaskStarted, engine.HumanAnswered, engine.HumanRetried, engine.HumanCancelled, engine.HumanRestored, engine.RunStarted, engine.PrepareStarted,
		engine.Prepared, engine.PrepareFailed, engine.SessionStarted, engine.AgentText, engine.AgentTool, engine.RateLimit, engine.EgressDenied,
		engine.SessionEnded, engine.SessionCut, engine.VerifyStarted, engine.VerifyEnded, engine.ReviewStarted, engine.ReviewEnded,
		engine.ExportStarted, engine.Exported, engine.PublishStarted, engine.Published, engine.PRObserved, engine.ReadyStarted, engine.PRReady,
		engine.ReplyStarted, engine.Replied, engine.Merged, engine.Stopped, engine.RunEnded, engine.HomeRemoved,
	}

	for _, typ := range types {
		assert.NotEqual(t, "{}", engine.EventText(store.Event{Type: typ, Payload: json.RawMessage(`{}`)}), typ)
	}

	assert.Equal(t, `{"a":1}`, engine.EventText(store.Event{Type: "from_a_later_version", Payload: json.RawMessage(`{"a":1}`)}), "a type the engine does not know")
}

func TestEventTextOfAPayloadThatDoesNotFit(t *testing.T) {
	assert.NotPanics(t, func() {
		engine.EventText(store.Event{Type: engine.Exported, Payload: json.RawMessage(`[1`)})
	})
}

func TestChecksText(t *testing.T) {
	unread := engine.ChecksText("", nil)
	assert.NotEmpty(t, unread)

	for _, state := range []string{engine.ChecksNone, engine.ChecksPending, engine.ChecksPassed, engine.ChecksFailed} {
		assert.NotContains(t, unread, state, "unread checks are none of the states")
		assert.Contains(t, engine.ChecksText(state, nil), state)
	}

	failed := engine.ChecksText(engine.ChecksFailed, []string{"lint", "unit"})
	for _, v := range []string{engine.ChecksFailed, "lint", "unit"} {
		assert.Contains(t, failed, v)
	}
}

func TestCountedIsSingularForOne(t *testing.T) {
	assert.Equal(t, "0 turns", engine.Counted(0, "turn"))
	assert.Equal(t, "1 turn", engine.Counted(1, "turn"))
	assert.Equal(t, "2 findings", engine.Counted(2, "finding"))
	assert.Equal(t, "1 finding", engine.Counted(1, "finding"))
}

func TestEventTextRunStartsAreDistinct(t *testing.T) {
	lines := map[string]bool{}

	for _, reason := range []string{"initial", "retry", engine.ReasonCIFix, engine.ReasonComments} {
		lines[told(t, engine.RunStarted, engine.RunStartedPayload{Reason: reason})] = true
	}

	assert.Len(t, lines, 4, "each reason its own line")
}

func TestEventTextNamesNoRequestIDAndNoWholeHead(t *testing.T) {
	head := "fed789abcdef0123456789abcdef0123456789ab"
	requests := []string{"PRRT_kwDOAbc1", "PRRT_kwDOAbc2"}

	for _, p := range []engine.RunStartedPayload{{Reason: engine.ReasonCIFix, Head: head}, {Reason: engine.ReasonComments, Head: head, Requests: requests}} {
		l := told(t, engine.RunStarted, p)

		assert.NotContains(t, l, head, p.Reason)

		for _, r := range requests {
			assert.NotContains(t, l, r, p.Reason)
		}
	}

	l := told(t, engine.Exported, engine.ExportedPayload{HeadCommit: head, FilesChanged: 2})
	assert.NotContains(t, l, head)
}

func TestEventTextCountsAReviewersRequestsAndNeverQuotesThem(t *testing.T) {
	bodies := []string{"Rename the handler, please.", "Why is the timeout 30 seconds?"}
	o := engine.Observation{State: engine.PROpen, Head: "fed789", Checks: engine.ChecksPassed}

	for i, b := range bodies {
		o.Comments = append(o.Comments, engine.Comment{ID: fmt.Sprintf("c%d", i), Author: "octocat", Path: "login.go", Line: 12, Body: b})
	}

	l := told(t, engine.PRObserved, o)

	assert.Contains(t, l, strconv.Itoa(len(bodies)))

	for _, b := range bodies {
		for w := range strings.FieldsSeq(b) {
			if len(w) > 4 {
				assert.NotContains(t, l, w, "a word of a request's text")
			}
		}
	}
}

func TestEventTextOfAReviewTellsItsScope(t *testing.T) {
	full := told(t, engine.ReviewStarted, engine.ReviewStartedPayload{SessionID: "r1", Head: "def456", Why: "Not reviewed yet."})

	assert.Equal(t, full, told(t, engine.ReviewStarted, engine.ReviewStartedPayload{SessionID: "r2", Head: "fed789", Why: "Another reason."}), "the scope alone sets the sentence")
	assert.NotEqual(t, full, told(t, engine.ReviewStarted, engine.ReviewStartedPayload{SessionID: "r1", Scoped: true, Head: "def456", Why: "Not reviewed yet."}))
}
