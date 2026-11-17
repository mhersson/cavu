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
	firstSession  = ev(engine.SessionStarted, `{"mode":"start","session_id":"s1"}`)
	doneChanged   = ev(engine.SessionEnded, `{"outcome":"done","changed":true,"title":"Fix login","summary":"Fixed it.","head_commit":"def456"}`)
	unfinished    = ev(engine.SessionEnded, `{"outcome":"unfinished"}`)
	autoContinue  = ev(engine.SessionStarted, `{"mode":"continue","session_id":"s1","resume":true,"automatic":true}`)
	autoFix       = ev(engine.SessionStarted, `{"mode":"fix_verify","session_id":"s1","resume":true,"automatic":true}`)
	personFix     = ev(engine.SessionStarted, `{"mode":"fix_verify","session_id":"s1","resume":true}`)
	verifying     = ev(engine.VerifyStarted, "")
	verifyFailed  = ev(engine.VerifyEnded, `{"result":"failed","command":"make test","exit_code":2,"output":"FAIL TestLogin","duration_ms":900}`)
	verifyPassed  = ev(engine.VerifyEnded, `{"result":"passed","command":"make test","exit_code":0}`)
	retried       = ev(engine.HumanRetried, "")
	stopAt        = time.Date(2030, 1, 1, 11, 0, 0, 0, time.UTC)
	sandboxStop   = store.Event{Type: engine.Stopped, At: stopAt, Payload: json.RawMessage(`{"reason":"sandbox","explanation":"The runner failed.","action":"retry"}`)}
	resumeSession = ev(engine.SessionStarted, `{"mode":"continue","session_id":"s1","resume":true}`)
	doneFact      = &engine.SessionFact{Outcome: engine.OutcomeDone, Changed: true, Title: "Fix login", Summary: "Fixed it.", HeadCommit: "def456"}
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
			events: []store.Event{ev(engine.HumanCancelled, "")},
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
				OpenStop:      &engine.StopFact{Reason: engine.StopVerifyFailed, Explanation: "Verify failed.", Action: "retry"},
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
			name:   "a retry after a sandbox stop",
			events: preparedRun(firstSession, unfinished, autoContinue, doneChanged, verifying, sandboxStop, retried),
			want:   engine.Facts{Started: true, RunOpen: true, Prepared: true, SessionID: "s1", Session: doneFact},
		},
		{
			name:   "two sandbox stops",
			events: preparedRun(firstSession, sandboxStop, resumeSession, sandboxStop),
			want: engine.Facts{
				Started: true, RunOpen: true, Prepared: true, SessionID: "s1",
				OpenStop: &engine.StopFact{Reason: engine.StopSandbox, Explanation: "The runner failed.", Action: "retry", At: stopAt}, Faults: 2,
			},
		},
		{
			name:   "a retry after two sandbox stops",
			events: preparedRun(firstSession, sandboxStop, resumeSession, sandboxStop, retried),
			want:   engine.Facts{Started: true, RunOpen: true, Prepared: true, SessionID: "s1"},
		},
		{
			name: "a prepare claimed after a sandbox stop",
			events: []store.Event{
				ev(engine.TaskStarted, ""), ev(engine.RunStarted, `{"reason":"initial"}`), ev(engine.PrepareStarted, ""), sandboxStop,
				ev(engine.PrepareStarted, ""),
			},
			want: engine.Facts{Started: true, RunOpen: true, Faults: 1},
		},
		{
			name:   "a verify claimed after a sandbox stop",
			events: preparedRun(firstSession, doneChanged, verifying, sandboxStop, verifying),
			want:   engine.Facts{Started: true, RunOpen: true, Prepared: true, SessionID: "s1", Session: doneFact, Faults: 1},
		},
		{
			name: "an export claimed after a sandbox stop",
			events: preparedRun(firstSession, doneChanged, verifying, verifyPassed, ev(engine.ExportStarted, ""), sandboxStop,
				ev(engine.ExportStarted, "")),
			want: engine.Facts{
				Started: true, RunOpen: true, Prepared: true, SessionID: "s1", Session: doneFact, Verify: engine.ResultPassed, Faults: 1,
			},
		},
		{
			name: "a merge after the export",
			events: preparedRun(firstSession, doneChanged, verifying, verifyPassed, ev(engine.ExportStarted, ""),
				ev(engine.Exported, `{"head_commit":"def456"}`), ev(engine.RunEnded, `{"outcome":"exported"}`), ev(engine.Merged, "")),
			want: engine.Facts{Started: true, Exported: true, Export: engine.ExportedPayload{HeadCommit: "def456"}, Merged: true},
		},
		{
			name:   "a retry with no open stop",
			events: preparedRun(firstSession, doneChanged, retried),
			want:   engine.Facts{Started: true, RunOpen: true, Prepared: true, SessionID: "s1"},
		},
		{
			name:   "a session started and not ended",
			events: preparedRun(firstSession),
			want:   engine.Facts{Started: true, RunOpen: true, Prepared: true, SessionID: "s1"},
		},
		{
			name:   "a session start with no session id",
			events: preparedRun(firstSession, unfinished, ev(engine.SessionStarted, `{"mode":"continue","automatic":true}`)),
			want:   engine.Facts{Started: true, RunOpen: true, Prepared: true, SessionID: "s1", Continuations: 1},
		},
		{
			name:   "an answer after a question",
			events: questionAnswered(),
			want: engine.Facts{
				Started: true, RunOpen: true, Prepared: true, SessionID: "s1",
				Resumed: &engine.Resume{From: engine.StopQuestion, Answer: "Use 8080."}, Answers: []string{"Use 8080."},
			},
		},
		{
			name:   "a finished session after an answer",
			events: append(questionAnswered(), ev(engine.SessionStarted, `{"mode":"answer","session_id":"s1","resume":true}`), doneChanged),
			want:   engine.Facts{Started: true, RunOpen: true, Prepared: true, SessionID: "s1", Session: doneFact, Answers: []string{"Use 8080."}},
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
			want: engine.Facts{
				Started: true, Exported: true,
				Export: engine.ExportedPayload{HeadCommit: "def456", DiffPath: "diff.patch", BundlePath: "task.bundle", FilesChanged: 2},
			},
		},
		{
			name: "the newest of two exports",
			events: []store.Event{
				ev(engine.TaskStarted, ""),
				ev(engine.Exported, `{"head_commit":"aaa111","diff_path":"one.patch","files_changed":1,"diff_stat":["a.go | 1 +"]}`),
				ev(engine.Exported, `{"head_commit":"bbb222","diff_path":"two.patch","bundle_path":"two.bundle","files_changed":3}`),
			},
			want: engine.Facts{
				Started: true, Exported: true,
				Export: engine.ExportedPayload{HeadCommit: "bbb222", DiffPath: "two.patch", BundlePath: "two.bundle", FilesChanged: 3},
			},
		},
		{
			name: "the ecosystems of the prepare",
			events: []store.Event{
				ev(engine.TaskStarted, ""), ev(engine.RunStarted, `{"reason":"initial"}`), ev(engine.PrepareStarted, ""),
				ev(engine.Prepared, `{"base_commit":"abc123","ecosystems":["go","node"]}`),
			},
			want: engine.Facts{Started: true, RunOpen: true, Prepared: true, Ecosystems: []string{"go", "node"}},
		},
		{
			name: "a redone prepare forgets them",
			events: []store.Event{
				ev(engine.TaskStarted, ""), ev(engine.RunStarted, `{"reason":"initial"}`), ev(engine.PrepareStarted, ""),
				ev(engine.Prepared, `{"base_commit":"abc123","ecosystems":["go"]}`), ev(engine.PrepareStarted, ""),
			},
			want: engine.Facts{Started: true, RunOpen: true},
		},
		{
			name: "a later run's prepare replaces them",
			events: []store.Event{
				ev(engine.TaskStarted, ""), ev(engine.RunStarted, `{"reason":"initial"}`), ev(engine.PrepareStarted, ""),
				ev(engine.Prepared, `{"base_commit":"abc123","ecosystems":["go"]}`), ev(engine.RunEnded, `{"outcome":"environment"}`),
				ev(engine.RunStarted, `{"reason":"retry"}`), ev(engine.PrepareStarted, ""),
				ev(engine.Prepared, `{"base_commit":"abc123","ecosystems":["rust"]}`),
			},
			want: engine.Facts{Started: true, RunOpen: true, Prepared: true, Ecosystems: []string{"rust"}},
		},
		{
			name: "a closed run forgets them",
			events: []store.Event{
				ev(engine.TaskStarted, ""), ev(engine.RunStarted, `{"reason":"initial"}`), ev(engine.PrepareStarted, ""),
				ev(engine.Prepared, `{"base_commit":"abc123","ecosystems":["go"]}`), ev(engine.RunEnded, `{"outcome":"cancelled"}`),
			},
			want: engine.Facts{Started: true},
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

func TestReduceCounting(t *testing.T) {
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
		assert.Equal(t, 2, f.VerifyFixes)

		got := engine.Decide(snap(f))
		got.Why, got.Explain = "", ""
		assert.Equal(t, work(engine.ModeContinue, false), got)
	})

	t.Run("a crash during the new run's prepare after a retry from environment prepares that run", func(t *testing.T) {
		f := engine.Reduce([]store.Event{
			ev(engine.TaskStarted, ""), ev(engine.RunStarted, `{"reason":"initial"}`), ev(engine.PrepareStarted, ""),
			ev(engine.PrepareFailed, `{"step":"setup","output":"exit 1"}`),
			ev(engine.Stopped, `{"reason":"environment","explanation":"Setup failed.","action":"retry"}`),
			ev(engine.RunEnded, `{"outcome":"environment"}`),
			retried, ev(engine.RunStarted, `{"reason":"retry"}`), ev(engine.PrepareStarted, ""),
		})

		assert.True(t, f.RunOpen)
		assert.False(t, f.Prepared)
		assert.Nil(t, f.Resumed)

		got := engine.Decide(snap(f))
		got.Why, got.Explain = "", ""
		assert.Equal(t, engine.Action{Kind: engine.KindPrepare}, got)
	})

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
		{
			name:   "a pending session with no session id",
			events: preparedRun(firstSession, unfinished, ev(engine.SessionStarted, `{"mode":"continue","automatic":true}`)),
			want:   work(engine.ModeContinue, false),
		},
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

// A retry after a sandbox stop redoes, in the same run, the action the fault cut.
func TestReduceRetryAfterSandboxStop(t *testing.T) {
	answering := ev(engine.SessionStarted, `{"mode":"answer","session_id":"s1","resume":true}`)

	tests := []struct {
		name   string
		events []store.Event
		want   engine.Action
	}{
		{
			name: "a prepare",
			events: []store.Event{
				ev(engine.TaskStarted, ""), ev(engine.RunStarted, `{"reason":"initial"}`), ev(engine.PrepareStarted, ""), sandboxStop, retried,
			},
			want: engine.Action{Kind: engine.KindPrepare},
		},
		{name: "a session", events: preparedRun(firstSession, sandboxStop, retried), want: work(engine.ModeContinue, false)},
		{
			name:   "a verify",
			events: preparedRun(firstSession, doneChanged, verifying, sandboxStop, retried),
			want:   engine.Action{Kind: engine.KindVerify},
		},
		{
			name:   "an export",
			events: preparedRun(firstSession, doneChanged, verifying, verifyPassed, ev(engine.ExportStarted, ""), sandboxStop, retried),
			want:   engine.Action{Kind: engine.KindExport},
		},
		{
			name:   "an answer session",
			events: append(questionAnswered(), answering, sandboxStop, retried),
			want:   engine.Action{Kind: engine.KindWork, WorkMode: engine.ModeAnswer, Detail: "Use 8080."},
		},
		{
			name:   "an automatic fix session",
			events: preparedRun(firstSession, doneChanged, verifying, verifyFailed, autoFix, sandboxStop, retried),
			want:   work(engine.ModeContinue, false),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := engine.Reduce(tc.events)
			assert.True(t, f.RunOpen)

			got := engine.Decide(snap(f))
			got.Why, got.Explain = "", ""
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestTurnsAndDuration(t *testing.T) {
	t0 := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)

	// at is e recorded m minutes after t0.
	at := func(e store.Event, m int) store.Event {
		e.At = t0.Add(time.Duration(m) * time.Minute)

		return e
	}

	tests := []struct {
		name     string
		events   []store.Event
		turns    int
		duration time.Duration
	}{
		{name: "nothing", events: nil},
		{name: "never started", events: []store.Event{at(ev(engine.HumanCancelled, ""), 5)}},
		{
			name: "every session's turns, reviews' too, timed from the newest start to the last event",
			events: []store.Event{
				at(ev(engine.TaskStarted, ""), 0),
				at(ev(engine.SessionEnded, `{"outcome":"unfinished","turns":3}`), 10),
				at(ev(engine.TaskStarted, ""), 20),
				at(ev(engine.SessionEnded, `{"outcome":"done","turns":4}`), 30),
				at(ev(engine.ReviewEnded, `{"outcome":"reviewed","turns":2}`), 31),
				at(ev(engine.Exported, `{}`), 32),
			},
			turns: 9, duration: 12 * time.Minute,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			turns, d := engine.TurnsAndDuration(tc.events)
			assert.Equal(t, tc.turns, turns)
			assert.Equal(t, tc.duration, d)
		})
	}
}
