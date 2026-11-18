package mockapi

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testBound caps every wait in these tests.
const testBound = 10 * time.Second

// testClient fails a request that hangs instead of waiting forever.
var testClient = &http.Client{Timeout: testBound}

type event struct {
	name string
	data map[string]any
}

// answer is what one event stream says.
type answer struct {
	events    []string
	messageID string
	text      string
	tool      string
	input     string
	stop      string
	usage     bool // message_start and message_delta both carry usage
	output    int  // the output tokens message_delta's usage reports
}

func parseStream(t *testing.T, r io.Reader) []event {
	t.Helper()

	var (
		out  []event
		name string
	)

	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := sc.Text()

		switch {
		case strings.HasPrefix(line, "event: "):
			name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			var data map[string]any
			require.NoError(t, json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &data))
			require.Equal(t, name, data["type"])

			out = append(out, event{name: name, data: data})
		}
	}

	require.NoError(t, sc.Err())

	return out
}

func summarise(evs []event) answer {
	var a answer

	usage := 0

	for _, ev := range evs {
		a.events = append(a.events, ev.name)

		switch ev.name {
		case "message_start":
			msg, _ := ev.data["message"].(map[string]any)
			a.messageID, _ = msg["id"].(string)

			if msg["usage"] != nil {
				usage++
			}
		case "content_block_start":
			block, _ := ev.data["content_block"].(map[string]any)
			if block["type"] == "tool_use" {
				a.tool, _ = block["name"].(string)
			}
		case "content_block_delta":
			delta, _ := ev.data["delta"].(map[string]any)
			text, _ := delta["text"].(string)
			partial, _ := delta["partial_json"].(string)
			a.text += text
			a.input += partial
		case "message_delta":
			delta, _ := ev.data["delta"].(map[string]any)
			a.stop, _ = delta["stop_reason"].(string)

			if u, ok := ev.data["usage"].(map[string]any); ok {
				usage++

				n, _ := u["output_tokens"].(float64)
				a.output = int(n)
			}
		}
	}

	a.usage = usage == 2

	return a
}

func post(t *testing.T, client *http.Client, target string, body map[string]any, header http.Header) *http.Response {
	t.Helper()

	b, err := json.Marshal(body)
	require.NoError(t, err)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, target, bytes.NewReader(b))
	require.NoError(t, err)

	req.Header.Set("Content-Type", "application/json")

	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}

	resp, err := client.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	return resp
}

func call(t *testing.T, client *http.Client, base string, tools ...string) answer {
	t.Helper()

	defs := make([]map[string]any, 0, len(tools))
	for _, name := range tools {
		defs = append(defs, map[string]any{"name": name, "input_schema": map[string]any{"type": "object"}})
	}

	resp := post(t, client, base+"/v1/messages?beta=true", map[string]any{
		"model": "claude-haiku-5-5", "stream": true, "tools": defs,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))

	return summarise(parseStream(t, resp.Body))
}

var streamShape = []string{
	"message_start", "content_block_start", "content_block_delta", "content_block_stop", "message_delta", "message_stop",
}

func TestMockStreamsScriptedSteps(t *testing.T) {
	s := Start(t, []Step{
		{Text: "hello"},
		{Tool: "Bash", Input: map[string]any{"command": "ls"}},
		{Tool: "$structured", Input: map[string]any{"ok": true}},
	})

	tools := []string{"Bash", "StructuredOutput"}
	answers := make([]answer, 0, 5)

	for range 5 {
		answers = append(answers, call(t, testClient, s.URL(), tools...))
	}

	ids := map[string]bool{}

	for i, a := range answers {
		assert.Equal(t, streamShape, a.events, "answer %d", i)
		assert.True(t, a.usage, "answer %d", i)
		assert.NotEmpty(t, a.messageID, "answer %d", i)

		ids[a.messageID] = true
	}

	assert.Len(t, ids, len(answers), "message ids repeat")

	assert.Equal(t, answer{text: "hello", stop: "end_turn"}, answers[0].content())
	assert.Equal(t, "Bash", answers[1].tool)
	assert.JSONEq(t, `{"command":"ls"}`, answers[1].input)
	assert.Equal(t, "tool_use", answers[1].stop)
	assert.Equal(t, "StructuredOutput", answers[2].tool)
	assert.JSONEq(t, `{"ok":true}`, answers[2].input)
	assert.Equal(t, answer{text: "done", stop: "end_turn"}, answers[3].content())
	assert.Equal(t, answer{text: "done", stop: "end_turn"}, answers[4].content())
}

// content keeps only what the model said and why it stopped.
func (a answer) content() answer {
	return answer{text: a.text, tool: a.tool, input: a.input, stop: a.stop}
}

// reply is one request's outcome, received off the test goroutine.
type reply struct {
	resp *http.Response
	err  error
}

// startCall sends one streaming model call in the background.
func startCall(t *testing.T, s *Server) <-chan reply {
	t.Helper()

	got := make(chan reply, 1)

	go func() {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, s.URL()+"/v1/messages",
			strings.NewReader(`{"model":"claude-haiku-5-5","stream":true,"messages":[]}`))
		if err != nil {
			got <- reply{err: err}

			return
		}

		resp, err := testClient.Do(req)
		got <- reply{resp, err}
	}()

	return got
}

// requireWaiting fails if got delivers an answer within a short while.
func requireWaiting(t *testing.T, got <-chan reply) {
	t.Helper()

	select {
	case <-got:
		t.Fatal("the answer started before release")
	case <-time.After(200 * time.Millisecond):
	}
}

// requireAnswer reads got's answer and returns its text.
func requireAnswer(t *testing.T, got <-chan reply) string {
	t.Helper()

	select {
	case r := <-got:
		require.NoError(t, r.err)
		t.Cleanup(func() { _ = r.resp.Body.Close() })

		return summarise(parseStream(t, r.resp.Body)).text
	case <-time.After(testBound):
		t.Fatal("no answer after release")

		return ""
	}
}

func requireRequests(t *testing.T, s *Server, n int) {
	t.Helper()

	require.Eventually(t, func() bool { return len(s.Requests()) == n }, testBound, 10*time.Millisecond)
}

func TestMockHoldsQueue(t *testing.T) {
	s := Start(t, []Step{{Text: "first"}, {Text: "second"}})
	releaseFirst, releaseSecond := s.Hold(), s.Hold()

	first := startCall(t, s)
	requireRequests(t, s, 1)

	second := startCall(t, s)
	requireRequests(t, s, 2)
	requireWaiting(t, first)
	requireWaiting(t, second)

	releaseSecond()
	assert.Equal(t, "second", requireAnswer(t, second))
	requireWaiting(t, first)

	releaseFirst()
	assert.Equal(t, "first", requireAnswer(t, first))

	assert.Equal(t, "done", call(t, testClient, s.URL()).text)
}

func TestMockScriptsByMarker(t *testing.T) {
	s := Start(t, []Step{{Text: "unmarked"}})
	s.Script("MARK-A", []Step{{Text: "a1"}, {Text: "a2"}})
	s.Script("MARK-B", []Step{{Text: "b1"}})

	user := func(content any) map[string]any { return map[string]any{"role": "user", "content": content} }
	text := func(s string) []any { return []any{map[string]any{"type": "text", "text": s}} }
	later := []any{user("hi"), map[string]any{"role": "assistant", "content": "ok"}, user("MARK-B")}

	for i, c := range []struct {
		system   any
		messages []any
		answer   string
	}{
		{nil, []any{user("do MARK-B")}, "b1"},
		{nil, []any{user(append(text("context"), text("do MARK-A")...))}, "a1"},
		{text("you serve MARK-A"), []any{user("hi")}, "a2"},
		{nil, later, "unmarked"},
		{"MARK-A", []any{user("hi")}, "done"},
	} {
		body := map[string]any{"model": "claude-haiku-5-5", "stream": true, "messages": c.messages}
		if c.system != nil {
			body["system"] = c.system
		}

		resp := post(t, testClient, s.URL()+"/v1/messages", body, nil)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Equal(t, c.answer, summarise(parseStream(t, resp.Body)).text, "call %d", i+1)
	}

	var scripts []string
	for _, r := range s.Requests() {
		scripts = append(scripts, r.Script)
	}

	assert.Equal(t, []string{"MARK-B", "MARK-A", "MARK-A", "", "MARK-A"}, scripts)
}

func TestMockReportsStepOutputTokens(t *testing.T) {
	s := Start(t, []Step{{Text: "costly", OutputTokens: 20000}, {Text: "plain"}})

	assert.Equal(t, 20000, call(t, testClient, s.URL()).output)
	assert.Equal(t, 5, call(t, testClient, s.URL()).output)
	assert.Equal(t, 5, call(t, testClient, s.URL()).output, "the step after the last")
}

func TestMockErrorStatus(t *testing.T) {
	s := Start(t, []Step{{Status: http.StatusTooManyRequests}, {Status: 529}, {Status: 418}, {Text: "after"}})

	for _, want := range []struct {
		status int
		kind   string
	}{
		{http.StatusTooManyRequests, "rate_limit_error"},
		{529, "overloaded_error"},
		{418, "api_error"},
	} {
		resp := post(t, testClient, s.URL()+"/v1/messages", map[string]any{
			"model": "claude-haiku-5-5", "stream": true, "messages": []any{},
		}, nil)
		require.Equal(t, want.status, resp.StatusCode)

		var body struct {
			Type  string `json:"type"`
			Error struct {
				Type string `json:"type"`
			} `json:"error"`
		}
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
		assert.Equal(t, "error", body.Type)
		assert.Equal(t, want.kind, body.Error.Type)
	}

	assert.Equal(t, "after", call(t, testClient, s.URL()).text)
}

func TestMockMarksModelCalls(t *testing.T) {
	s := Start(t, nil)

	call(t, testClient, s.URL())

	side := map[string]any{"model": "claude-haiku-5-5", "messages": []any{}}
	for _, path := range []string{"/v1/messages", "/v1/messages/count_tokens"} {
		resp := post(t, testClient, s.URL()+path, side, nil)
		_, _ = io.Copy(io.Discard, resp.Body)
	}

	reqs := s.Requests()
	require.Len(t, reqs, 3)
	assert.Equal(t, []bool{true, false, false}, []bool{reqs[0].ModelCall, reqs[1].ModelCall, reqs[2].ModelCall})

	var body map[string]any
	require.NoError(t, json.Unmarshal(reqs[0].Body, &body))
	assert.Equal(t, true, body["stream"])
	assert.JSONEq(t, `{"model":"claude-haiku-5-5","messages":[]}`, string(reqs[1].Body))
}

func TestQuietHandshakesPassesOtherLines(t *testing.T) {
	var out strings.Builder

	q := quietHandshakes{&out}

	for _, line := range []string{
		"http: TLS handshake error from 172.17.0.2:36886: EOF\n",
		"http: panic serving 127.0.0.1:1: boom\n",
	} {
		n, err := q.Write([]byte(line))
		require.NoError(t, err)
		assert.Equal(t, len(line), n)
	}

	assert.Equal(t, "http: panic serving 127.0.0.1:1: boom\n", out.String())
}
