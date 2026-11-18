//go:build integration

package claudecode

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/cavu/internal/testkit/mockapi"
)

// TestOldTranscriptStillResumes: a transcript older than the documented default
// of 30 days is still resumed after another session ran in its home, which on
// the pinned release fails without the retention setting.
func TestOldTranscriptStillResumes(t *testing.T) {
	const (
		firstPrompt  = "CONFORMANCE-OLD-SESSION-PROMPT-3b9e"
		firstAnswer  = "CONFORMANCE-OLD-SESSION-ANSWER-c41a"
		secondPrompt = "CONFORMANCE-OTHER-SESSION-PROMPT-8d07"
	)

	b := keyBox(t, mockapi.Step{Text: firstAnswer})
	d := conformanceDriver()

	first := b.spec()
	first.Prompt = firstPrompt
	requireDone(t, b.run(d, first))

	aged := b.sh(`touch -d "@$(( $(date +%s) - 40*86400 ))" ` + confConfig + "/projects/*/" + first.SessionID + ".jsonl")
	require.Equal(t, 0, aged.ExitCode, aged.Output)

	// Shown: a session of one quick model call ends before the sweep, which
	// starts about 6 s into a session, so this one holds a 20 s shell call.
	b.mock.Script(secondPrompt, []mockapi.Step{{Tool: "Bash", Input: map[string]any{"command": "sleep 20"}}, {Text: "done"}})

	second := b.spec()
	second.Prompt = secondPrompt
	requireDone(t, b.run(d, second))

	n := len(modelCalls(b.mock))

	resume := b.spec()
	resume.SessionID, resume.Resume = "", first.SessionID
	requireDone(t, b.run(d, resume))

	calls := modelCalls(b.mock)
	require.Greater(t, len(calls), n, "the resume made no model call")
	assert.True(t, carries(calls[n], firstPrompt), "the resumed call lacks the earlier prompt")
	assert.True(t, carries(calls[n], firstAnswer), "the resumed call lacks the earlier answer")
}
