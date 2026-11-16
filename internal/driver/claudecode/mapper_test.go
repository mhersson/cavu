package claudecode

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/cavu/internal/driver"
)

func readFixture(t *testing.T, path string) []byte {
	t.Helper()

	data, err := os.ReadFile(filepath.Join("testdata", path))
	require.NoError(t, err)

	return data
}

// replayData feeds data through a mapper line by line and finishes it with the
// given exit code.
func replayData(t *testing.T, data []byte, secrets []string, exit int) ([]driver.Event, driver.Result) {
	t.Helper()

	var evs []driver.Event

	m := newMapper(secrets, func(ev driver.Event) { evs = append(evs, ev) })

	for line := range bytes.Lines(data) {
		if line = bytes.TrimSpace(line); len(line) > 0 && m.feed(line) {
			break
		}
	}

	return evs, m.finish(exit, "")
}

func replay(t *testing.T, path string, exit int) ([]driver.Event, driver.Result) {
	t.Helper()

	return replayData(t, readFixture(t, path), nil, exit)
}

func kinds(evs []driver.Event) []string {
	out := make([]string, 0, len(evs))
	for _, ev := range evs {
		out = append(out, ev.Kind)
	}

	return out
}

func ofKind(evs []driver.Event, k string) []driver.Event {
	var out []driver.Event

	for _, ev := range evs {
		if ev.Kind == k {
			out = append(out, ev)
		}
	}

	return out
}

func sumUsage(evs []driver.Event) []driver.Usage {
	byModel := map[string]*driver.Usage{}

	var order []string

	for _, ev := range ofKind(evs, driver.KindUsage) {
		u := ev.Usage

		t, ok := byModel[u.Model]
		if !ok {
			t = &driver.Usage{Model: u.Model}
			byModel[u.Model] = t
			order = append(order, u.Model)
		}

		t.Input += u.Input
		t.Output += u.Output
		t.CacheRead += u.CacheRead
		t.CacheWrite += u.CacheWrite
	}

	var out []driver.Usage

	for _, m := range order {
		out = append(out, *byModel[m])
	}

	return out
}

func sortedUsage(u []driver.Usage) []driver.Usage {
	out := slices.Clone(u)
	slices.SortFunc(out, func(a, b driver.Usage) int { return strings.Compare(a.Model, b.Model) })

	return out
}

// resultLine builds a result line with the given extra fields.
func resultLine(t *testing.T, fields map[string]any) []byte {
	t.Helper()

	base := map[string]any{"type": "result", "subtype": "success", "is_error": false, "num_turns": 1}
	maps.Copy(base, fields)

	b, err := json.Marshal(base)
	require.NoError(t, err)

	return append(b, '\n')
}

func TestMapperBasicRun(t *testing.T) {
	evs, res := replay(t, "real/t1.jsonl", 0)

	assert.Equal(t, []string{driver.KindStarted, driver.KindRateLimit, driver.KindText, driver.KindUsage}, kinds(evs))

	started := evs[0]
	assert.Equal(t, "e7d93133-865e-4f91-b7e2-348d66e79b59", started.SessionID)
	assert.Equal(t, "claude-haiku-5-5", started.Model)
	assert.NotEmpty(t, started.Capabilities)

	rl := evs[1].RateLimit
	require.NotNil(t, rl)
	assert.Equal(t, "allowed", rl.Status)
	assert.Equal(t, "seven_day", rl.Window)

	assert.Equal(t, "ok", evs[2].Text)
	assert.Equal(t, &driver.Usage{Model: "claude-haiku-5-5", Input: 2, Output: 4, CacheRead: 11382, CacheWrite: 6379}, evs[3].Usage)

	assert.Equal(t, driver.ReasonDone, res.Reason)
	assert.False(t, res.IsError)
	assert.Equal(t, "ok", res.Text)
	assert.Equal(t, started.SessionID, res.SessionID)
	assert.Equal(t, 1, res.Turns)
	assert.InDelta(t, 0.0013918200000000002, res.CostEstimateUSD, 1e-12)
	assert.Equal(t, []driver.Usage{*evs[3].Usage}, res.Usage)
	require.NotNil(t, res.RateLimit)
	assert.Equal(t, "seven_day", res.RateLimit.Window)
	assert.Empty(t, res.Detail)
}

func TestMapperAuthFailureOutranksSubtype(t *testing.T) {
	_, res := replay(t, "real/t2.jsonl", 1)

	assert.Equal(t, driver.ReasonAuth, res.Reason)
	assert.True(t, res.IsError)
	assert.NotEmpty(t, res.Detail)
}

func TestMapperRejectedRateLimit(t *testing.T) {
	evs, res := replay(t, "synthetic/rejected.jsonl", 0)

	assert.Equal(t, driver.ReasonUsageLimit, res.Reason)
	assert.True(t, res.IsError)
	require.NotNil(t, res.RateLimit)
	assert.Equal(t, "seven_day", res.RateLimit.Window)
	assert.Equal(t, "rejected", res.RateLimit.Status)
	assert.True(t, res.RateLimit.ResetsAt.Equal(time.Unix(1794510000, 0)))
	assert.Contains(t, kinds(evs), driver.KindRateLimit)
	assert.Equal(t, []driver.Usage{{Model: "claude-haiku-5-5", Input: 2, Output: 4, CacheRead: 11382, CacheWrite: 6379}}, res.Usage, "the usage streamed so far")
}

func TestMapperRejectedHaltsFeeding(t *testing.T) {
	var got int

	m := newMapper(nil, func(driver.Event) { got++ })

	assert.True(t, m.feed([]byte(`{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","rateLimitType":"five_hour"}}`)))

	before := got

	assert.True(t, m.feed([]byte(`{"type":"system","subtype":"init","session_id":"s"}`)))
	assert.Equal(t, before, got, "lines after a usage limit are ignored")
}

func TestMapperStopsAtResult(t *testing.T) {
	m := newMapper(nil, func(driver.Event) {})

	assert.False(t, m.feed([]byte(`{"type":"system","subtype":"init","session_id":"s"}`)))
	assert.True(t, m.feed(resultLine(t, nil)))
	assert.True(t, m.feed([]byte(`{"type":"system","subtype":"init","session_id":"later"}`)))
	assert.Equal(t, driver.ReasonDone, m.finish(0, "").Reason)
}

func TestMapperBudgetAndTurnStops(t *testing.T) {
	for _, tc := range []struct {
		path, reason string
	}{
		{"synthetic/max_turns.jsonl", driver.ReasonMaxTurns},
		{"synthetic/max_budget_end_turn.jsonl", driver.ReasonMaxBudget},
		{"synthetic/max_budget_tool_use.jsonl", driver.ReasonMaxBudget},
	} {
		t.Run(tc.path, func(t *testing.T) {
			_, res := replay(t, tc.path, 1)

			assert.Equal(t, tc.reason, res.Reason)
			assert.True(t, res.IsError)
		})
	}
}

func TestMapperNoResultIsNeverSuccess(t *testing.T) {
	for _, exit := range []int{0, 1} {
		t.Run(fmt.Sprintf("exit %d", exit), func(t *testing.T) {
			evs, res := replay(t, "synthetic/crash.jsonl", exit)

			assert.Equal(t, driver.ReasonCrashed, res.Reason)
			assert.True(t, res.IsError)
			assert.NotEmpty(t, res.Detail)
			assert.Equal(t, "Done.", res.Text)
			assert.Equal(t, 2, res.Turns)
			assert.Equal(t, []driver.Usage{{Model: "claude-sonnet-5-5", Input: 4, Output: 7, CacheRead: 3844, CacheWrite: 3977}}, res.Usage, "the usage streamed so far")
			assert.Equal(t, sumUsage(evs), sortedUsage(res.Usage))
		})
	}
}

func TestMapperDetailIsStderrTail(t *testing.T) {
	m := newMapper([]string{"hunter2hunter2"}, func(driver.Event) {})
	res := m.finish(2, "boom hunter2hunter2\n")

	assert.Equal(t, driver.ReasonCrashed, res.Reason)
	assert.Contains(t, res.Detail, "boom")
	assert.NotContains(t, res.Detail, "hunter2hunter2")
}

func TestMapperResultOutranksExitCode(t *testing.T) {
	_, res := replay(t, "real/t5.jsonl", 1)

	assert.Equal(t, driver.ReasonDone, res.Reason)
	assert.False(t, res.IsError)
}

func TestMapperToolEvents(t *testing.T) {
	evs, res := replay(t, "real/t5.jsonl", 0)

	assert.Equal(t, []string{
		driver.KindStarted, driver.KindRateLimit, driver.KindText, driver.KindUsage, driver.KindToolUse,
		driver.KindToolResult, driver.KindText, driver.KindUsage, driver.KindUsage,
	}, kinds(evs))

	use := ofKind(evs, driver.KindToolUse)
	require.Len(t, use, 1)
	assert.Equal(t, "toolu_014h96jCjuHV3wJj8rGfnEn5", use[0].ToolID)
	assert.Equal(t, "mcp__cmlocal__finish", use[0].ToolName)
	assert.JSONEq(t, `{"commit_message":"chore: answer 2+2 question","summary":"Stated that 2+2 equals 4. No files were changed."}`, string(use[0].ToolInput))

	results := ofKind(evs, driver.KindToolResult)
	require.Len(t, results, 1)
	assert.Equal(t, use[0].ToolID, results[0].ToolID)
	assert.False(t, results[0].ToolError)

	assert.Equal(t, []driver.Usage{{Model: "claude-sonnet-5-5", Input: 4, Output: 125, CacheRead: 3844, CacheWrite: 3977}}, res.Usage)
	assert.Equal(t, sumUsage(evs), res.Usage, "usage events add up to the totals")
}

func TestMapperToolErrorResult(t *testing.T) {
	evs, res := replay(t, "real/p3.jsonl", 0)

	var failed int

	for _, ev := range ofKind(evs, driver.KindToolResult) {
		if ev.ToolError {
			failed++
		}
	}

	assert.Equal(t, 1, failed)
	assert.Equal(t, driver.ReasonDone, res.Reason)
}

func TestMapperLivePartial(t *testing.T) {
	evs, res := replay(t, "real/p4-partial-messages.jsonl", 0)

	assert.Equal(t, []driver.Usage{{Model: "claude-haiku-5-5", Input: 2, Output: 4, CacheRead: 11382, CacheWrite: 6379}}, res.Usage)
	assert.Len(t, ofKind(evs, driver.KindUsage), 1, "no remainder event")
}

func TestMapperPartialAndParallelUsage(t *testing.T) {
	for _, tc := range []struct {
		path  string
		calls int
		total driver.Usage
	}{
		{"synthetic/partial.jsonl", 2, driver.Usage{Model: "claude-sonnet-5-5", Input: 4, Output: 125, CacheRead: 3844, CacheWrite: 3977}},
		{"synthetic/parallel.jsonl", 2, driver.Usage{Model: "claude-haiku-5-5", Input: 20, Output: 10}},
	} {
		t.Run(tc.path, func(t *testing.T) {
			evs, res := replay(t, tc.path, 0)

			assert.Equal(t, driver.ReasonDone, res.Reason)
			assert.Equal(t, []driver.Usage{tc.total}, res.Usage)

			// One event per call and none for a remainder: the streamed usage
			// alone reaches the fixture's totals.
			assert.Len(t, ofKind(evs, driver.KindUsage), tc.calls)
			assert.Equal(t, []driver.Usage{tc.total}, sumUsage(evs))
		})
	}
}

func TestMapperPartialUsagePerCall(t *testing.T) {
	evs, _ := replay(t, "synthetic/partial.jsonl", 0)

	usages := ofKind(evs, driver.KindUsage)
	require.Len(t, usages, 2)
	assert.Equal(t, driver.Usage{Model: "claude-sonnet-5-5", Input: 2, Output: 120, CacheWrite: 3844}, *usages[0].Usage, "call 1 from message_delta")
	assert.Equal(t, driver.Usage{Model: "claude-sonnet-5-5", Input: 2, Output: 5, CacheRead: 3844, CacheWrite: 133}, *usages[1].Usage, "call 2 from message_delta")

	use := ofKind(evs, driver.KindToolUse)
	require.Len(t, use, 1)
	assert.JSONEq(t, `{"commit_message":"chore: answer 2+2 question","summary":"Stated that 2+2 equals 4. No files were changed."}`, string(use[0].ToolInput))
}

func TestMapperParallelToolCalls(t *testing.T) {
	evs, _ := replay(t, "synthetic/parallel.jsonl", 0)

	uses := ofKind(evs, driver.KindToolUse)
	results := ofKind(evs, driver.KindToolResult)

	require.Len(t, uses, 5)
	require.Len(t, results, 5)

	seen := map[string]bool{}

	for _, u := range uses {
		assert.Equal(t, "Bash", u.ToolName)
		assert.True(t, json.Valid(u.ToolInput))

		seen[u.ToolID] = true
	}

	for _, r := range results {
		assert.True(t, seen[r.ToolID], "a result answers a tool use")
	}
}

func TestMapperTwoModels(t *testing.T) {
	evs, res := replay(t, "synthetic/two_models.jsonl", 0)

	assert.Equal(t, []driver.Usage{
		{Model: "claude-haiku-5-5", Input: 350, Output: 12},
		{Model: "claude-sonnet-5-5", Input: 4, Output: 125, CacheRead: 3844, CacheWrite: 3977},
	}, res.Usage)
	assert.InDelta(t, 0.0175914, res.CostEstimateUSD, 1e-12)
	assert.Equal(t, sortedUsage(sumUsage(evs)), res.Usage)
}

func TestMapperTotalsPreferThisProcess(t *testing.T) {
	const init = `{"type":"system","subtype":"init","session_id":"s","model":"m1"}`

	fields := map[string]any{
		"total_cost_usd": 9.0,
		"modelUsage": map[string]any{
			"m1": map[string]any{"inputTokens": 100, "outputTokens": 50, "costUSD": 9.0},
		},
		"model_usage_since_last_result": map[string]any{
			"m1": map[string]any{"inputTokens": 10, "outputTokens": 5, "costUSD": 0.5},
		},
	}

	evs, res := replayData(t, append([]byte(init+"\n"), resultLine(t, fields)...), nil, 0)

	assert.Equal(t, []driver.Usage{{Model: "m1", Input: 10, Output: 5}}, res.Usage)
	assert.InDelta(t, 0.5, res.CostEstimateUSD, 1e-12)
	assert.Equal(t, sumUsage(evs), res.Usage)

	t.Run("falls back to modelUsage and the total cost", func(t *testing.T) {
		delete(fields, "model_usage_since_last_result")

		_, res := replayData(t, append([]byte(init+"\n"), resultLine(t, fields)...), nil, 0)

		assert.Equal(t, []driver.Usage{{Model: "m1", Input: 100, Output: 50}}, res.Usage)
		assert.InDelta(t, 9.0, res.CostEstimateUSD, 1e-12)
	})

	t.Run("falls back to result.usage under the init model", func(t *testing.T) {
		line := resultLine(t, map[string]any{"usage": map[string]any{"input_tokens": 7, "output_tokens": 3}})

		_, res := replayData(t, append([]byte(init+"\n"), line...), nil, 0)

		assert.Equal(t, []driver.Usage{{Model: "m1", Input: 7, Output: 3}}, res.Usage)
	})
}

func TestMapperIgnoresUnknownLines(t *testing.T) {
	want, wantRes := replay(t, "real/t1.jsonl", 0)
	got, gotRes := replay(t, "synthetic/unknown.jsonl", 0)

	assert.Equal(t, want, got)
	assert.Equal(t, wantRes, gotRes)
}

func TestMapperIgnoresUndecodableLines(t *testing.T) {
	data := readFixture(t, "real/t1.jsonl")
	noise := []byte("not json\n\"a string\"\n[1,2]\n{\"type\":\"assistant\",\"message\":\"text\"}\n" +
		`{"type":"user","isReplay":true,"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"x","content":"y"}]}}` + "\n")

	want, wantRes := replayData(t, data, nil, 0)
	got, gotRes := replayData(t, append(noise, data...), nil, 0)

	assert.Equal(t, want, got)
	assert.Equal(t, wantRes, gotRes)
}

func TestLineTooLong(t *testing.T) {
	long := []byte(`{"type":"assistant","message":{"id":"msg_long","model":"m","content":[{"type":"text","text":"` +
		strings.Repeat("x", 2<<20) + `"}]}}`)
	data := append(append(long, '\n'), readFixture(t, "real/t1.jsonl")...)

	want, wantRes := replay(t, "real/t1.jsonl", 0)
	got, gotRes := replayData(t, data, nil, 0)

	assert.Equal(t, want, got)
	assert.Equal(t, wantRes, gotRes)
}

func TestStructuredOutput(t *testing.T) {
	// Spacing and key order change under any re-encoding.
	const out = `{"verdict": "ship",  "items":[1, 2], "a":null}`

	line := `{"type":"result","subtype":"success","is_error":false,"num_turns":1,"structured_output":` + out + "}\n"

	_, res := replayData(t, []byte(line), nil, 0)

	assert.Equal(t, driver.ReasonDone, res.Reason)
	assert.True(t, bytes.Equal([]byte(out), res.Structured), "copied byte for byte")

	_, res = replayData(t, resultLine(t, nil), nil, 0)
	assert.Nil(t, res.Structured)
}

func TestMapperEndpointFailures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		text   string
		reason string
	}{
		{"unauthorized", 401, "Invalid bearer token", driver.ReasonAuth},
		{"rate limited", 429, "Too many requests", driver.ReasonEndpoint},
		{"server error", 500, "Internal server error", driver.ReasonEndpoint},
		{"overloaded", 529, "Overloaded", driver.ReasonEndpoint},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, res := replayData(t, resultLine(t, map[string]any{
				"is_error": true, "api_error_status": tc.status, "result": tc.text, "terminal_reason": "api_error",
			}), nil, 1)

			assert.Equal(t, tc.reason, res.Reason)
			assert.True(t, res.IsError)
			assert.NotEmpty(t, res.Detail)
		})
	}

	t.Run("retry line", func(t *testing.T) {
		evs, _ := replayData(t, []byte(`{"type":"system","subtype":"api_retry","attempt":2,"max_retries":10,"retry_delay_ms":1000,"error_status":529,"error":"server_error"}`+"\n"), nil, 1)

		retries := ofKind(evs, driver.KindRetry)
		require.Len(t, retries, 1)
		assert.Equal(t, &driver.Retry{Attempt: 2, Max: 10, Kind: "server_error", Status: 529}, retries[0].Retry)
	})

	t.Run("retry without a status", func(t *testing.T) {
		evs, _ := replayData(t, []byte(`{"type":"system","subtype":"api_retry","attempt":1,"max_retries":3,"error_status":null,"error":"unknown"}`+"\n"), nil, 1)

		require.Len(t, evs, 1)
		assert.Zero(t, evs[0].Retry.Status)
	})
}

func TestMapperUsageLimitFromResult(t *testing.T) {
	for _, tc := range []struct{ text, window string }{
		{"You've hit your weekly limit - resets Friday", "seven_day"},
		{"You've hit your 5-hour limit", "five_hour"},
		{"You've hit your something new", ""},
		{"Your usage limit was reached", ""},
	} {
		t.Run(tc.text, func(t *testing.T) {
			_, res := replayData(t, resultLine(t, map[string]any{
				"is_error": true, "api_error_status": 429, "result": tc.text,
			}), nil, 1)

			assert.Equal(t, driver.ReasonUsageLimit, res.Reason)
			require.NotNil(t, res.RateLimit)
			assert.Equal(t, tc.window, res.RateLimit.Window)
			assert.True(t, res.RateLimit.ResetsAt.IsZero())
		})
	}
}

func TestMapperErrorReasons(t *testing.T) {
	for _, tc := range []struct {
		name   string
		fields map[string]any
		reason string
		detail string
	}{
		{"status 400", map[string]any{"is_error": true, "api_error_status": 400, "result": "bad"}, driver.ReasonEndpoint, "bad"},
		{"status 404", map[string]any{"is_error": true, "api_error_status": 404, "result": "no model"}, driver.ReasonEndpoint, "no model"},
		{"connection failure", map[string]any{"is_error": true, "terminal_reason": "api_error", "result": "connect ECONNREFUSED"}, driver.ReasonEndpoint, "connect ECONNREFUSED"},
		{"execution error", map[string]any{"is_error": true, "subtype": "error_during_execution", "errors": []string{"it broke"}}, driver.ReasonCrashed, "it broke"},
		{"unknown error", map[string]any{"is_error": true, "subtype": "error_future", "terminal_reason": "something_new"}, driver.ReasonCrashed, "error_future"},
		{"not logged in", map[string]any{"is_error": true, "result": "Not logged in · Please run /login"}, driver.ReasonAuth, "Not logged in"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, res := replayData(t, resultLine(t, tc.fields), nil, 1)

			assert.Equal(t, tc.reason, res.Reason)
			assert.True(t, res.IsError)
			assert.Contains(t, res.Detail, tc.detail)
			assert.NotEqual(t, driver.ReasonDone, res.Reason)
		})
	}
}

func TestMapperAuthenticationFailedAssistant(t *testing.T) {
	data := []byte(`{"type":"assistant","error":"authentication_failed","message":{"id":"m1","model":"<synthetic>","content":[{"type":"text","text":"Not logged in"}]}}` + "\n")

	_, res := replayData(t, data, nil, 1)

	assert.Equal(t, driver.ReasonAuth, res.Reason)
	assert.True(t, res.IsError)
}

func TestMaskAppliesToEveryString(t *testing.T) {
	const token = "tok-SECRET-0123456789"

	lines := []string{
		`{"type":"system","subtype":"init","session_id":"sess-` + token + `","model":"m-` + token + `","capabilities":["c-` + token + `"]}`,
		`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed-` + token + `","rateLimitType":"w-` + token + `"}}`,
		`{"type":"system","subtype":"api_retry","attempt":1,"max_retries":2,"error":"e-` + token + `"}`,
		`{"type":"assistant","message":{"id":"msg1","model":"m-` + token + `","content":[` +
			`{"type":"text","text":"said ` + token + `"},` +
			`{"type":"tool_use","id":"id-` + token + `","name":"n-` + token + `","input":{"k":"` + token + `"}}],"usage":{"input_tokens":1}}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"id-` + token + `","content":"` + token + `"}]}}`,
		string(resultLine(t, map[string]any{
			"result": "answer " + token, "session_id": "sess-" + token,
			"structured_output": map[string]any{"k": token},
			"modelUsage":        map[string]any{"m-" + token: map[string]any{"inputTokens": 5}},
		})),
	}

	evs, res := replayData(t, []byte(strings.Join(lines, "\n")), []string{token}, 0)

	require.NotEmpty(t, evs)

	for _, ev := range evs {
		b, err := json.Marshal(ev)
		require.NoError(t, err)
		assert.NotContains(t, string(b), token, ev.Kind)
	}

	b, err := json.Marshal(res)
	require.NoError(t, err)
	assert.NotContains(t, string(b), token)
	assert.Contains(t, string(b), redactedSecret)
}

func TestMaskCoversNoResultAndLimits(t *testing.T) {
	const token = "tok-SECRET-0123456789"

	m := newMapper([]string{token}, func(driver.Event) {})
	m.feed([]byte(`{"type":"assistant","message":{"id":"m1","model":"x","content":[{"type":"text","text":"` + token + `"}]}}`))
	m.feed([]byte(`{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","rateLimitType":"` + token + `"}}`))

	b, err := json.Marshal(m.finish(1, token))
	require.NoError(t, err)
	assert.NotContains(t, string(b), token)
}

func TestMaskOrdersLongerSecretsFirst(t *testing.T) {
	mask := newMask([]string{"abc", "", "abcdef"})

	assert.Equal(t, redactedSecret+" "+redactedSecret, mask("abcdef abc"))
	assert.Equal(t, "plain", newMask(nil)("plain"))
}

func TestMaskJSONKeepsResultValid(t *testing.T) {
	mask := newMask([]string{`"`})

	assert.True(t, json.Valid(maskJSON(mask, json.RawMessage(`{"a":"b"}`))))
	assert.Equal(t, json.RawMessage(nil), maskJSON(mask, nil))
}

func dropLines(data []byte, markers ...string) []byte {
	var out []byte

	for line := range bytes.Lines(data) {
		if !slices.ContainsFunc(markers, func(mk string) bool { return bytes.Contains(line, []byte(mk)) }) {
			out = append(out, line...)
		}
	}

	return out
}

func TestMapperUnknownTerminalReason(t *testing.T) {
	t1 := readFixture(t, "real/t1.jsonl")
	want, wantRes := replayData(t, t1, nil, 0)

	for name, data := range map[string][]byte{
		"absent":  bytes.ReplaceAll(t1, []byte(`"terminal_reason":"completed",`), nil),
		"unknown": bytes.ReplaceAll(t1, []byte(`"terminal_reason":"completed"`), []byte(`"terminal_reason":"x_future_reason"`)),
	} {
		t.Run(name, func(t *testing.T) {
			require.NotEqual(t, t1, data)

			got, gotRes := replayData(t, data, nil, 0)
			assert.Equal(t, want, got)
			assert.Equal(t, wantRes, gotRes)
		})
	}
}

func TestMapperErrorResult(t *testing.T) {
	t.Run("auth failure makes no call", func(t *testing.T) {
		// The assistant message of an auth failure is <synthetic>: no API call.
		evs, res := replay(t, "real/t2.jsonl", 1)

		assert.Empty(t, ofKind(evs, driver.KindText))
		assert.Empty(t, ofKind(evs, driver.KindUsage))
		assert.Empty(t, res.Usage)
		assert.Equal(t, driver.ReasonAuth, res.Reason)
	})

	t.Run("is_error outranks a completed run", func(t *testing.T) {
		data := bytes.Replace(readFixture(t, "real/t1.jsonl"), []byte(`"is_error":false`), []byte(`"is_error":true`), 1)
		require.NotEqual(t, readFixture(t, "real/t1.jsonl"), data)

		evs, res := replayData(t, data, nil, 0)

		assert.NotEqual(t, driver.ReasonDone, res.Reason)
		assert.True(t, res.IsError)
		assert.Len(t, ofKind(evs, driver.KindUsage), 1)
	})
}

func TestMapperCallClosesOnNextCall(t *testing.T) {
	for _, tc := range []struct {
		name string
		data []byte
		want []string
		call driver.Usage
	}{
		{
			"assistant lines", dropLines(readFixture(t, "real/t5.jsonl"), `"type":"user"`),
			[]string{
				driver.KindStarted, driver.KindRateLimit, driver.KindText, driver.KindUsage, driver.KindToolUse,
				driver.KindText, driver.KindUsage, driver.KindUsage,
			},
			driver.Usage{Model: "claude-sonnet-5-5", Input: 2, Output: 2, CacheWrite: 3844},
		},
		{
			"stream events", dropLines(readFixture(t, "synthetic/partial.jsonl"), `"type":"user"`, `"type":"message_stop"`),
			[]string{
				driver.KindStarted, driver.KindRateLimit, driver.KindText, driver.KindUsage, driver.KindToolUse,
				driver.KindText, driver.KindUsage,
			},
			driver.Usage{Model: "claude-sonnet-5-5", Input: 2, Output: 120, CacheWrite: 3844},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			evs, res := replayData(t, tc.data, nil, 0)

			assert.Equal(t, tc.want, kinds(evs))
			assert.Equal(t, tc.call, *ofKind(evs, driver.KindUsage)[0].Usage)
			assert.Equal(t, []driver.Usage{{Model: "claude-sonnet-5-5", Input: 4, Output: 125, CacheRead: 3844, CacheWrite: 3977}}, res.Usage)
		})
	}
}

func TestMapperUsageLimitResultIgnoresAllowedWindow(t *testing.T) {
	// The five_hour window was fine; the weekly one stopped the run.
	allowed := `{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","resetsAt":1794510000,"rateLimitType":"five_hour"}}` + "\n"
	data := append([]byte(allowed), resultLine(t, map[string]any{
		"is_error": true, "api_error_status": 429, "result": "You've hit your weekly limit",
	})...)

	_, res := replayData(t, data, nil, 1)

	assert.Equal(t, driver.ReasonUsageLimit, res.Reason)
	require.NotNil(t, res.RateLimit)
	assert.Equal(t, "seven_day", res.RateLimit.Window)
	assert.True(t, res.RateLimit.ResetsAt.IsZero(), "the allowed window's reset must not be reported")
}
