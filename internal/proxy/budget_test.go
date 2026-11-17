package proxy

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// usageJSON, like sseBody, uses 202 tokens of claude-x.
const usageJSON = `{"id":"m","model":"claude-x","content":[],"usage":{"input_tokens":10,"output_tokens":42,"cache_read_input_tokens":100,"cache_creation_input_tokens":50}}`

// flat prices every claude-x token at one micro-dollar.
var flat = map[string]Price{"claude-x": {Input: 1, Output: 1, CacheRead: 1, CacheWrite: 1}}

// answer is a 200 from the model endpoint.
type answer struct{ contentType, encoding, body string }

func (a answer) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", a.contentType)

	if a.encoding == "" {
		_, _ = io.WriteString(w, a.body)

		return
	}

	w.Header().Set("Content-Encoding", a.encoding)

	zw := gzip.NewWriter(w)
	_, _ = io.WriteString(zw, a.body)
	_ = zw.Close()
}

type namedAnswer struct {
	name  string
	names string // what the reason for refusing must hold
	answer
}

// uncountable are the answers whose spend the proxy cannot count.
var uncountable = []namedAnswer{
	{"compressed", "", answer{"application/json", "gzip", usageJSON}},
	{"plain text", "", answer{"text/plain", "", usageJSON}},
	{"no usage", "", answer{"application/json", "", `{"id":"m","model":"claude-x","content":[]}`}},
	{"no price", "claude-unpriced", answer{"application/json", "", strings.Replace(usageJSON, "claude-x", "claude-unpriced", 1)}},
}

func budgetProxy(t *testing.T, up string, b *Budget) (*httptest.Server, *deniedLog) {
	t.Helper()

	stops := &deniedLog{}

	return startProxy(t, Config{Model: modelRoute(up, authBearer), Budget: b, OnBudget: stops.add}), stops
}

// ask sends one model request and reads its whole answer.
func ask(t *testing.T, p *httptest.Server, path string) (*http.Response, string) {
	t.Helper()

	resp := post(t, p.URL, path, authBearer, strings.NewReader("{}"))
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	return resp, string(body)
}

// stopsOf waits for OnBudget and returns every reason it was called with.
func stopsOf(t *testing.T, stops *deniedLog) []string {
	t.Helper()
	require.Eventually(t, func() bool { return len(stops.all()) > 0 }, 2*time.Second, 5*time.Millisecond)

	return stops.all()
}

// requireRefused sends one model request and checks that the proxy refused it.
func requireRefused(t *testing.T, p *httptest.Server, path string) {
	t.Helper()

	resp, body := ask(t, p, path)
	require.Equal(t, http.StatusPaymentRequired, resp.StatusCode)
	assert.Equal(t, "application/json", resp.Header.Get("Content-Type"))

	var e struct {
		Type  string
		Error struct{ Type, Message string }
	}

	require.NoError(t, json.Unmarshal([]byte(body), &e))
	assert.Equal(t, "error", e.Type)
	assert.Equal(t, "billing_error", e.Error.Type)
	assert.NotEmpty(t, e.Error.Message)
	assert.NotContains(t, e.Error.Message, "\n")
}

func TestBudgetRefusesAtTheCap(t *testing.T) {
	hold := newGate()
	t.Cleanup(hold.open)

	// The first event alone takes the third response past the cap.
	half := strings.Index(sseBody, "event: content_block_delta")

	var n atomic.Int32

	up, rec := capture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sseBody[:half])
		w.(http.Flusher).Flush()

		if n.Add(1) == 3 {
			select {
			case <-hold.ch:
			case <-r.Context().Done():
				return
			}
		}

		_, _ = io.WriteString(w, sseBody[half:])
	})

	p, stops := budgetProxy(t, up.URL, &Budget{CapMicroUSD: 505, Prices: flat})

	for range 2 {
		resp, body := ask(t, p, "/v1/messages")
		require.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Equal(t, sseBody, body)
	}

	resp := post(t, p.URL, "/v1/messages", authBearer, strings.NewReader("{}"))
	lines := lineReader(resp)
	want := strings.Split(strings.TrimSuffix(sseBody, "\n"), "\n")
	got := []string{nextLine(t, lines, 2*time.Second)}

	hold.open()

	for len(got) < len(want) {
		got = append(got, nextLine(t, lines, 2*time.Second))
	}

	assert.Equal(t, want, got)
	assert.Equal(t, []string{""}, stopsOf(t, stops))

	requireRefused(t, p, "/v1/messages")
	requireRefused(t, p, "/v1/messages/count_tokens")

	assert.Len(t, rec.all(), 3)
	assert.Equal(t, []string{""}, stops.all())
}

func TestBudgetCountsEveryBucket(t *testing.T) {
	for _, tc := range []struct {
		name  string
		used  Usage
		price Price
		want  int64
	}{
		{"sonnet", Usage{Input: 1000, Output: 2000, CacheRead: 10000, CacheWrite: 4000}, Price{3, 15, 0.3, 3.75}, 51000},
		{"opus", Usage{Input: 100, Output: 200, CacheRead: 1000, CacheWrite: 40}, Price{15, 75, 1.5, 18.75}, 18750},
		// Each bucket rounds on its own: 0.4 + 7.5 + 0.3 + 0.45 costs 8, not 9.
		{"rounded", Usage{Input: 1, Output: 3, CacheRead: 1, CacheWrite: 1}, Price{0.4, 2.5, 0.3, 0.45}, 8},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := tc.used
			up := serve(t, answer{"application/json", "", fmt.Sprintf(
				`{"model":"m","usage":{"input_tokens":%d,"output_tokens":%d,"cache_read_input_tokens":%d,"cache_creation_input_tokens":%d}}`,
				u.Input, u.Output, u.CacheRead, u.CacheWrite)})
			prices := map[string]Price{"m": tc.price}

			// A cap at the cost is spent by the one response.
			p, stops := budgetProxy(t, up.URL, &Budget{CapMicroUSD: tc.want, Prices: prices})
			resp, _ := ask(t, p, "/v1/messages")
			require.Equal(t, http.StatusOK, resp.StatusCode)
			assert.Equal(t, []string{""}, stopsOf(t, stops))
			requireRefused(t, p, "/v1/messages")

			// A cap one above it is not.
			p, stops = budgetProxy(t, up.URL, &Budget{CapMicroUSD: tc.want + 1, Prices: prices})

			for range 2 {
				resp, _ = ask(t, p, "/v1/messages")
				require.Equal(t, http.StatusOK, resp.StatusCode)
			}

			assert.Equal(t, []string{""}, stopsOf(t, stops))
		})
	}
}

func TestBudgetFailsClosed(t *testing.T) {
	tooLarge := namedAnswer{"too large", "", answer{"application/json", "", `{"model":"claude-x","usage":{"input_tokens":1},"pad":"` + strings.Repeat("a", maxJSON) + `"}`}}

	for _, tc := range append([]namedAnswer{tooLarge}, uncountable...) {
		t.Run(tc.name, func(t *testing.T) {
			up, rec := capture(t, tc.ServeHTTP)
			p, stops := budgetProxy(t, up.URL, &Budget{CapMicroUSD: 1_000_000, Prices: flat})

			resp, body := ask(t, p, "/v1/messages")
			require.Equal(t, http.StatusOK, resp.StatusCode)
			assert.Equal(t, tc.body, body)

			reasons := stopsOf(t, stops)
			require.Len(t, reasons, 1)
			assert.NotEmpty(t, reasons[0])

			if tc.names != "" {
				assert.Contains(t, reasons[0], tc.names)
			}

			requireRefused(t, p, "/v1/messages")
			assert.Len(t, rec.all(), 1)
		})
	}
}

func TestBudgetLeavesCountTokensAndErrorsFree(t *testing.T) {
	const overloaded = `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`

	up, rec := capture(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/messages/count_tokens":
			answer{"application/json", "", `{"input_tokens":42}`}.ServeHTTP(w, r)
		case r.URL.Query().Has("overloaded"):
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(529)
			_, _ = io.WriteString(w, overloaded)
		case r.URL.Query().Has("stream-error"):
			// The streaming form of an error response: a 200 whose stream holds an error event.
			answer{"text/event-stream", "", "event: error\ndata: " + overloaded + "\n\n"}.ServeHTTP(w, r)
		default:
			answer{"application/json", "", usageJSON}.ServeHTTP(w, r)
		}
	})

	// Any response that costs, or cannot be counted, spends a cap this small.
	p, stops := budgetProxy(t, up.URL, &Budget{CapMicroUSD: 1, Prices: flat})

	for range 10 {
		resp, _ := ask(t, p, "/v1/messages/count_tokens")
		require.Equal(t, http.StatusOK, resp.StatusCode)
	}

	resp, _ := ask(t, p, "/v1/messages?overloaded")
	require.Equal(t, 529, resp.StatusCode)

	resp, _ = ask(t, p, "/v1/messages?stream-error")
	require.Equal(t, http.StatusOK, resp.StatusCode)

	assert.Empty(t, stops.all())

	resp, _ = ask(t, p, "/v1/messages")
	require.Equal(t, http.StatusOK, resp.StatusCode)

	assert.Len(t, rec.all(), 13)
	assert.Equal(t, []string{""}, stopsOf(t, stops))
}

func TestNoBudgetRefusesNothing(t *testing.T) {
	answers := []answer{{"application/json", "", usageJSON}}
	for _, a := range uncountable {
		answers = append(answers, a.answer)
	}

	up := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i, _ := strconv.Atoi(r.URL.Query().Get("answer"))
		answers[i].ServeHTTP(w, r)
	}))

	var stops deniedLog

	p := startProxy(t, Config{Model: modelRoute(up.URL, authBearer), OnBudget: stops.add})

	for i := range 1000 {
		a := i % len(answers)
		resp, body := ask(t, p, "/v1/messages?answer="+strconv.Itoa(a))
		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.Equal(t, answers[a].body, body)
	}

	assert.Empty(t, stops.all())
}
