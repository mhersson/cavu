package engine_test

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/mhersson/cavu/internal/engine"
	"github.com/mhersson/cavu/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func ev(typ, payload string) store.Event {
	return store.Event{Type: typ, Payload: json.RawMessage(payload)}
}

// preparedRun is a started task whose first run is prepared, followed by more.
func preparedRun(more ...store.Event) []store.Event {
	return append([]store.Event{
		ev(engine.TaskStarted, ""),
		ev(engine.RunStarted, `{"reason":"initial"}`),
		ev(engine.PrepareStarted, ""),
		ev(engine.Prepared, `{"base_commit":"abc123","tools":["go","node"],"baseline":"passed"}`),
	}, more...)
}

var (
	firstSession = ev(engine.SessionStarted, `{"mode":"start","session_id":"s1"}`)
	doneChanged  = ev(engine.SessionEnded, `{"outcome":"done","changed":true,"title":"Fix login","summary":"Fixed it."}`)
	unfinished   = ev(engine.SessionEnded, `{"outcome":"unfinished"}`)
	autoContinue = ev(engine.SessionStarted, `{"mode":"continue","session_id":"s1","resume":true,"automatic":true}`)
	autoFix      = ev(engine.SessionStarted, `{"mode":"fix_verify","session_id":"s1","resume":true,"automatic":true}`)
	personFix    = ev(engine.SessionStarted, `{"mode":"fix_verify","session_id":"s1","resume":true}`)
	verifying    = ev(engine.VerifyStarted, "")
	verifyFailed = ev(engine.VerifyEnded, `{"result":"failed","command":"make test","exit_code":2,"output":"FAIL TestLogin","duration_ms":900}`)
	verifyPassed = ev(engine.VerifyEnded, `{"result":"passed","command":"make test","exit_code":0}`)
	retried      = ev(engine.HumanRetried, "")
	doneFact     = &engine.SessionFact{Outcome: engine.OutcomeDone, Changed: true, Title: "Fix login", Summary: "Fixed it."}
)

// verifyStop ends with a verify_failed stop after one automatic continuation and one automatic fix.
func verifyStop() []store.Event {
	return preparedRun(firstSession, unfinished, autoContinue, doneChanged, verifying, verifyFailed,
		autoFix, doneChanged, verifying, verifyFailed,
		ev(engine.Stopped, `{"reason":"verify_failed","explanation":"Verify failed.","action":"retry"}`))
}

// questionAnswered ends with a person's answer to the agent's question.
func questionAnswered() []store.Event {
	return preparedRun(firstSession,
		ev(engine.SessionEnded, `{"outcome":"blocked","questions":["Which port?"]}`),
		ev(engine.Stopped, `{"reason":"question","explanation":"Which port?","action":"answer"}`),
		ev(engine.HumanAnswered, `{"text":"Use 8080."}`))
}

func TestReduce(t *testing.T) {
	tests := []struct {
		name   string
		events []store.Event
		want   engine.Facts
	}{
		{
			name:   "nothing",
			events: nil,
			want:   engine.Facts{},
		},
		{
			name:   "a cancel before the start",
			events: []store.Event{ev(engine.TaskCreated, ""), ev(engine.HumanCancelled, "")},
			want:   engine.Facts{Cancelled: true},
		},
		{
			name:   "a failed verify",
			events: preparedRun(firstSession, doneChanged, verifying, verifyFailed),
			want: engine.Facts{
				Started: true, RunOpen: true, Prepared: true, SessionID: "s1", Session: doneFact,
				Verify: engine.ResultFailed, LastVerifyOutput: "FAIL TestLogin",
			},
		},
		{
			name:   "an automatic fix after a failed verify",
			events: preparedRun(firstSession, doneChanged, verifying, verifyFailed, autoFix, doneChanged),
			want: engine.Facts{
				Started: true, RunOpen: true, Prepared: true, SessionID: "s1", Session: doneFact,
				LastVerifyOutput: "FAIL TestLogin", VerifyFixes: 1,
			},
		},
		{
			name:   "a verify_failed stop",
			events: verifyStop(),
			want: engine.Facts{
				Started: true, RunOpen: true, Prepared: true, SessionID: "s1", Session: doneFact,
				Verify: engine.ResultFailed, LastVerifyOutput: "FAIL TestLogin",
				OpenStop:      &engine.StopFact{Reason: engine.StopVerifyFailed, Explanation: "Verify failed."},
				Continuations: 1, VerifyFixes: 1,
			},
		},
		{
			name:   "a retry after a verify_failed stop",
			events: append(verifyStop(), retried),
			want: engine.Facts{
				Started: true, RunOpen: true, Prepared: true, SessionID: "s1",
				Verify: engine.ResultFailed, LastVerifyOutput: "FAIL TestLogin",
				Resumed: &engine.Resume{From: engine.StopVerifyFailed},
			},
		},
		{
			name:   "a retry with no open stop",
			events: preparedRun(firstSession, doneChanged, retried),
			want:   engine.Facts{Started: true, RunOpen: true, Prepared: true, SessionID: "s1"},
		},
		{
			name:   "a session started and not ended",
			events: preparedRun(firstSession),
			want:   engine.Facts{Started: true, RunOpen: true, Prepared: true, Pending: "work", SessionID: "s1"},
		},
		{
			name:   "an answer after a question",
			events: questionAnswered(),
			want: engine.Facts{
				Started: true, RunOpen: true, Prepared: true, SessionID: "s1",
				Resumed: &engine.Resume{From: engine.StopQuestion, Answer: "Use 8080."},
			},
		},
		{
			name:   "a finished session after an answer",
			events: append(questionAnswered(), ev(engine.SessionStarted, `{"mode":"answer","session_id":"s1","resume":true}`), doneChanged),
			want:   engine.Facts{Started: true, RunOpen: true, Prepared: true, SessionID: "s1", Session: doneFact},
		},
		{
			name: "a failed prepare",
			events: []store.Event{
				ev(engine.TaskStarted, ""), ev(engine.RunStarted, `{"reason":"initial"}`), ev(engine.PrepareStarted, ""),
				ev(engine.PrepareFailed, `{"step":"setup","output":"npm ERR! missing script"}`),
			},
			want: engine.Facts{
				Started: true, RunOpen: true,
				PrepareFailure: &engine.PrepareFailure{Step: "setup", Output: "npm ERR! missing script"},
			},
		},
		{
			name:   "a closed run",
			events: preparedRun(firstSession, doneChanged, ev(engine.RunEnded, `{"outcome":"agent_error"}`)),
			want:   engine.Facts{Started: true},
		},
		{
			name: "a prepare after its run ended",
			events: []store.Event{
				ev(engine.TaskStarted, ""), ev(engine.RunStarted, `{"reason":"initial"}`), ev(engine.PrepareStarted, ""),
				ev(engine.RunEnded, `{"outcome":"cancelled"}`), ev(engine.Prepared, `{"base_commit":"abc123"}`),
			},
			want: engine.Facts{Started: true},
		},
		{
			name: "an export outlives its run",
			events: preparedRun(firstSession, doneChanged, verifying, verifyPassed, ev(engine.ExportStarted, ""),
				ev(engine.Exported, `{"head_commit":"def456","diff_path":"diff.patch","bundle_path":"task.bundle","files_changed":2}`),
				ev(engine.RunEnded, `{"outcome":"exported"}`)),
			want: engine.Facts{Started: true, Exported: true},
		},
		{
			name:   "a session end that does not decode",
			events: preparedRun(firstSession, ev(engine.SessionEnded, `{"outcome":7,"changed":true}`)),
			want:   engine.Facts{Started: true, RunOpen: true, Prepared: true, SessionID: "s1", Session: &engine.SessionFact{}},
		},
		{
			name:   "a stop that does not decode",
			events: preparedRun(firstSession, ev(engine.Stopped, `{"reason":["question"]}`)),
			want:   engine.Facts{Started: true, RunOpen: true, Prepared: true, SessionID: "s1", OpenStop: &engine.StopFact{}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, engine.Reduce(tc.events))
		})
	}
}

func TestReducePendingAndCounting(t *testing.T) {
	resets := time.Date(2030, 1, 1, 13, 0, 0, 0, time.UTC)
	usageLimit := preparedRun(firstSession, unfinished, autoContinue,
		ev(engine.SessionEnded, `{"outcome":"usage_limit","resets_at":"2030-01-01T13:00:00Z"}`),
		ev(engine.Stopped, `{"reason":"usage_limit","explanation":"Usage limit.","action":"none","resets_at":"2030-01-01T13:00:00Z"}`))
	resumeAfterLimit := ev(engine.SessionStarted, `{"mode":"continue","session_id":"s1","resume":true}`)

	t.Run("a usage limit stop carries its reset time", func(t *testing.T) {
		f := engine.Reduce(usageLimit)

		require.NotNil(t, f.OpenStop)
		assert.Equal(t, resets, f.OpenStop.ResetsAt)
		require.NotNil(t, f.Session)
		assert.Equal(t, resets, f.Session.ResetsAt)
		assert.Equal(t, 1, f.Continuations)
	})

	t.Run("a session start after a usage limit closes the stop and is not counted", func(t *testing.T) {
		f := engine.Reduce(append(slices.Clone(usageLimit), resumeAfterLimit))

		assert.Nil(t, f.OpenStop)
		assert.Equal(t, 1, f.Continuations)
		assert.Equal(t, "work", f.Pending)
	})

	t.Run("only automatic fix sessions count", func(t *testing.T) {
		f := engine.Reduce(preparedRun(firstSession, doneChanged, verifying, verifyFailed,
			autoFix, doneChanged, verifying, verifyFailed,
			personFix, doneChanged, verifying, verifyFailed,
			autoFix, doneChanged))

		assert.Equal(t, 2, f.VerifyFixes)
		assert.Equal(t, 0, f.Continuations)
	})

	t.Run("the last allowed automatic fix pending", func(t *testing.T) {
		f := engine.Reduce(preparedRun(firstSession, doneChanged, verifying, verifyFailed,
			autoFix, doneChanged, verifying, verifyFailed, autoFix))

		assert.Nil(t, f.Session)
		assert.Empty(t, f.Verify)
		assert.Equal(t, "work", f.Pending)
		assert.Equal(t, 2, f.VerifyFixes)

		got := engine.Decide(snap(f))
		got.Why, got.Explain = "", ""
		assert.Equal(t, work(engine.ModeContinue, false), got)
	})

	reprepares := []struct {
		name    string
		stopped []store.Event
	}{
		{
			name: "environment",
			stopped: []store.Event{
				ev(engine.TaskStarted, ""), ev(engine.RunStarted, `{"reason":"initial"}`), ev(engine.PrepareStarted, ""),
				ev(engine.PrepareFailed, `{"step":"setup","output":"exit 1"}`),
				ev(engine.Stopped, `{"reason":"environment","explanation":"Setup failed.","action":"retry"}`),
				ev(engine.RunEnded, `{"outcome":"environment"}`),
			},
		},
		{
			name: "sandbox",
			stopped: preparedRun(firstSession,
				ev(engine.Stopped, `{"reason":"sandbox","explanation":"The agent container died.","action":"retry"}`),
				ev(engine.RunEnded, `{"outcome":"sandbox"}`)),
		},
	}

	for _, tc := range reprepares {
		t.Run("a crash during the new run's prepare after a retry from "+tc.name+" prepares that run", func(t *testing.T) {
			events := append(slices.Clone(tc.stopped), retried, ev(engine.RunStarted, `{"reason":"retry"}`), ev(engine.PrepareStarted, ""))
			f := engine.Reduce(events)

			assert.True(t, f.RunOpen)
			assert.False(t, f.Prepared)
			assert.Equal(t, "prepare", f.Pending)
			assert.Nil(t, f.Resumed)

			got := engine.Decide(snap(f))
			got.Why, got.Explain = "", ""
			assert.Equal(t, engine.Action{Kind: engine.KindPrepare}, got)
		})
	}

	crashes := []struct {
		name   string
		events []store.Event
		want   engine.Action
	}{
		{
			name:   "a pending prepare",
			events: []store.Event{ev(engine.TaskStarted, ""), ev(engine.RunStarted, `{"reason":"initial"}`), ev(engine.PrepareStarted, "")},
			want:   engine.Action{Kind: engine.KindPrepare},
		},
		{name: "a pending first session", events: preparedRun(firstSession), want: work(engine.ModeContinue, false)},
		{name: "a pending automatic continuation", events: preparedRun(firstSession, unfinished, autoContinue), want: work(engine.ModeContinue, false)},
		{name: "a pending resume after a usage limit", events: append(slices.Clone(usageLimit), resumeAfterLimit), want: work(engine.ModeContinue, false)},
		{name: "a pending fix after a retry", events: append(verifyStop(), retried, personFix), want: work(engine.ModeFixVerify, false)},
		{
			name:   "a pending answer",
			events: append(questionAnswered(), ev(engine.SessionStarted, `{"mode":"answer","session_id":"s1","resume":true}`)),
			want:   engine.Action{Kind: engine.KindWork, WorkMode: engine.ModeAnswer, Detail: "Use 8080."},
		},
		{name: "a pending verify", events: preparedRun(firstSession, doneChanged, verifying), want: engine.Action{Kind: engine.KindVerify}},
		{
			name:   "a pending export",
			events: preparedRun(firstSession, doneChanged, verifying, verifyPassed, ev(engine.ExportStarted, "")),
			want:   engine.Action{Kind: engine.KindExport},
		},
	}

	for _, tc := range crashes {
		t.Run("a crash with "+tc.name+" decides it again", func(t *testing.T) {
			got := engine.Decide(snap(engine.Reduce(tc.events)))
			got.Why, got.Explain = "", ""
			assert.Equal(t, tc.want, got)
		})
	}
}
