package proxy

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type usages struct {
	mu   sync.Mutex
	list []Usage
}

func (u *usages) add(x Usage) {
	u.mu.Lock()
	defer u.mu.Unlock()

	u.list = append(u.list, x)
}

func (u *usages) all() []Usage {
	u.mu.Lock()
	defer u.mu.Unlock()

	return append([]Usage(nil), u.list...)
}

func (u *usages) wait(t *testing.T, n int) []Usage {
	t.Helper()
	require.Eventually(t, func() bool { return len(u.all()) >= n }, 2*time.Second, 10*time.Millisecond)

	return u.all()
}

const sseBody = `event: message_start
data: {"type":"message_start","message":{"id":"m","model":"claude-x","usage":{"input_tokens":10,"output_tokens":1,"cache_read_input_tokens":100,"cache_creation_input_tokens":50}}}

event: content_block_delta
data: {"type":"content_block_delta","delta":{"text":"hi"}}

event: message_delta
data: {"type":"message_delta","usage":{"output_tokens":5}}

event: message_delta
data: {"type":"message_delta","usage":{"output_tokens":42}}

event: message_stop
data: {"type":"message_stop"}

`

func sseUpstream(t *testing.T, body string) string {
	t.Helper()

	return serve(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		_, _ = io.WriteString(w, body)
	})).URL
}

func TestMeterStream(t *testing.T) {
	var u usages

	p := startProxy(t, Config{Model: modelRoute(sseUpstream(t, sseBody), authBearer), OnUsage: u.add})

	resp := post(t, p.URL, "/v1/messages", authBearer, strings.NewReader("{}"))
	out, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	assert.Equal(t, sseBody, string(out))
	assert.Equal(t, []Usage{{Model: "claude-x", Input: 10, Output: 42, CacheRead: 100, CacheWrite: 50}}, u.wait(t, 1))
}

func TestMeterJSON(t *testing.T) {
	const body = `{"id":"m","model":"claude-y","content":[],"usage":{"input_tokens":7,"output_tokens":9,"cache_read_input_tokens":3,"cache_creation_input_tokens":2}}`

	up := serve(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))

	var u usages

	p := startProxy(t, Config{Model: modelRoute(up.URL, authBearer), OnUsage: u.add})

	resp := post(t, p.URL, "/v1/messages", authBearer, strings.NewReader("{}"))
	out, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	assert.True(t, bytes.Equal([]byte(body), out), "response body altered: %q", out)
	assert.Equal(t, []Usage{{Model: "claude-y", Input: 7, Output: 9, CacheRead: 3, CacheWrite: 2}}, u.wait(t, 1))
}

func TestMeterDoesNotDelayStream(t *testing.T) {
	second := newGate()
	up := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":1}}}\n\n")
		w.(http.Flusher).Flush()

		select {
		case <-second.ch:
		case <-r.Context().Done():
			return
		}

		_, _ = io.WriteString(w, "data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":2}}\n\n")
	}))

	var u usages

	p := startProxy(t, Config{Model: modelRoute(up.URL, authBearer), OnUsage: u.add})
	t.Cleanup(second.open)

	resp := post(t, p.URL, "/v1/messages", authBearer, strings.NewReader("{}"))
	lines := lineReader(resp)

	// The first event arrives while the upstream still holds the second back.
	assert.Contains(t, nextLine(t, lines, 2*time.Second), "message_start")
	assert.Empty(t, u.all())

	second.open()
	assert.Empty(t, nextLine(t, lines, 2*time.Second))
	assert.Contains(t, nextLine(t, lines, 2*time.Second), "message_delta")
	assert.Equal(t, []Usage{{Input: 1, Output: 2}}, u.wait(t, 1))
}

func TestMeterIgnoresGarbage(t *testing.T) {
	body := "data: not json at all\n\n" + sseBody + "data: {\"type\":\n\n: comment\n\nevent: x\n"

	var u usages

	p := startProxy(t, Config{Model: modelRoute(sseUpstream(t, body), authBearer), OnUsage: u.add})

	resp := post(t, p.URL, "/v1/messages", authBearer, strings.NewReader("{}"))
	out, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	assert.Equal(t, body, string(out))
	assert.Equal(t, []Usage{{Model: "claude-x", Input: 10, Output: 42, CacheRead: 100, CacheWrite: 50}}, u.wait(t, 1))
}

func TestMeterReportsNothingWithoutUsage(t *testing.T) {
	var u usages

	p := startProxy(t, Config{Model: modelRoute(sseUpstream(t, "data: garbage\n\n"), authBearer), OnUsage: u.add})

	resp := post(t, p.URL, "/v1/messages", authBearer, strings.NewReader("{}"))
	_, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	_ = resp.Body.Close()

	time.Sleep(100 * time.Millisecond)
	assert.Empty(t, u.all())
}

func TestMeterHandlesSplitWrites(t *testing.T) {
	m := &meter{sse: true}

	for _, chunk := range []string{"data: {\"type\":\"message_st", "art\",\"message\":{\"model\":\"m\",\"usage\":{\"input_tokens\":4}}}\r", "\n\r\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":6}}"} {
		m.write([]byte(chunk))
	}

	u, ok := m.result()
	assert.True(t, ok)
	assert.Equal(t, Usage{Model: "m", Input: 4, Output: 6}, u)
}

func TestMeterSkipsOversizedLines(t *testing.T) {
	m := &meter{sse: true}
	m.write([]byte("data: " + strings.Repeat("a", maxLine+10) + "\n"))
	m.write([]byte("data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":3}}\n"))

	u, ok := m.result()
	assert.True(t, ok)
	assert.EqualValues(t, 3, u.Output)
}

func TestMeterReportsUnmetered(t *testing.T) {
	gz := []byte{0x1f, 0x8b, 0x08, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0xff, 0x01, 0x02, 0x03}

	up := serve(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = w.Write(gz)
	}))

	var (
		u    usages
		errs modelErrors
	)

	p := startProxy(t, Config{Model: modelRoute(up.URL, authBearer), OnUsage: u.add, OnModelError: errs.add})

	// An explicit Accept-Encoding stops the test client from decoding the body.
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, p.URL+"/v1/messages", strings.NewReader("{}"))
	require.NoError(t, err)
	setStandIn(req, authBearer)
	req.Header.Set("Accept-Encoding", "gzip")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)

	out, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	_ = resp.Body.Close()

	assert.Equal(t, gz, out)
	assert.Equal(t, "gzip", resp.Header.Get("Content-Encoding"))
	assert.Equal(t, []string{"200:unmetered"}, errs.all())
	assert.Empty(t, u.all())
}
