package proxy

import (
	"bytes"
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type seen struct {
	method, uri, host, auth, apiKey, version, beta, body string
}

type capturing struct {
	mu   sync.Mutex
	reqs []seen
}

func (c *capturing) all() []seen {
	c.mu.Lock()
	defer c.mu.Unlock()

	return append([]seen(nil), c.reqs...)
}

func capture(t *testing.T, h http.HandlerFunc) (*httptest.Server, *capturing) {
	t.Helper()

	c := &capturing{}
	srv := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)

		c.mu.Lock()
		c.reqs = append(c.reqs, seen{
			method: r.Method, uri: r.URL.RequestURI(), host: r.Host,
			auth: r.Header.Get("Authorization"), apiKey: r.Header.Get("X-Api-Key"),
			version: r.Header.Get("Anthropic-Version"), beta: r.Header.Get("Anthropic-Beta"),
			body: string(b),
		})
		c.mu.Unlock()

		if h != nil {
			h(w, r)
		}
	}))

	return srv, c
}

func TestModelRouteSwapsCredential(t *testing.T) {
	for _, tc := range []struct {
		style, wantAuth, wantKey string
	}{
		{authBearer, "Bearer " + realKey, ""},
		{authAPIKey, "", realKey},
	} {
		t.Run(tc.style, func(t *testing.T) {
			up, rec := capture(t, nil)
			p := startProxy(t, Config{Model: modelRoute(up.URL+"/anthropic/", tc.style)})

			// The agent may present the stand-in in either style.
			for _, client := range []string{authBearer, authAPIKey} {
				req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, p.URL+"/v1/messages?beta=true", strings.NewReader(`{"a":1}`))
				require.NoError(t, err)
				setStandIn(req, client)
				req.Header.Set("Anthropic-Version", "2023-06-01")
				req.Header.Set("Anthropic-Beta", "tools-2024")

				resp, err := http.DefaultClient.Do(req)
				require.NoError(t, err)

				_ = resp.Body.Close()
				assert.Equal(t, http.StatusOK, resp.StatusCode)
			}

			got := rec.all()
			require.Len(t, got, 2)

			upHost := strings.TrimPrefix(up.URL, "http://")

			for _, s := range got {
				assert.Equal(t, tc.wantAuth, s.auth)
				assert.Equal(t, tc.wantKey, s.apiKey)
				assert.Equal(t, "/anthropic/v1/messages?beta=true", s.uri)
				assert.Equal(t, upHost, s.host)
				assert.Equal(t, "2023-06-01", s.version)
				assert.Equal(t, "tools-2024", s.beta)
				assert.Equal(t, `{"a":1}`, s.body)
				assert.NotContains(t, s.auth+s.apiKey, standIn)
			}
		})
	}
}

func TestModelRouteCountTokens(t *testing.T) {
	up, rec := capture(t, nil)
	p := startProxy(t, Config{Model: modelRoute(up.URL, authAPIKey)})

	resp := post(t, p.URL, "/v1/messages/count_tokens", authAPIKey, strings.NewReader("{}"))
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	require.Len(t, rec.all(), 1)
	assert.Equal(t, "/v1/messages/count_tokens", rec.all()[0].uri)
}

func TestModelRouteRejectsWrongStandIn(t *testing.T) {
	up, rec := capture(t, nil)
	p := startProxy(t, Config{Model: modelRoute(up.URL, authBearer)})

	for _, hdr := range []http.Header{
		{},
		{"Authorization": {"Bearer wrong"}},
		{"X-Api-Key": {"wrong"}},
		{"Authorization": {"Bearer " + realKey}},
		{"Authorization": {standIn}},
	} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, p.URL+"/v1/messages", strings.NewReader("{}"))
		require.NoError(t, err)

		req.Header = hdr

		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)

		_ = resp.Body.Close()

		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode, "%v", hdr)
	}

	assert.Empty(t, rec.all())
}

func TestModelRouteOnlyMessagesPaths(t *testing.T) {
	up, rec := capture(t, nil)
	p := startProxy(t, Config{Model: modelRoute(up.URL, authBearer)})

	resp := post(t, p.URL, "/v1/models", authBearer, nil)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)

	resp = post(t, p.URL, "/v1/messages/extra", authBearer, nil)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, p.URL+"/v1/messages", nil)
	require.NoError(t, err)
	setStandIn(req, authBearer)

	got, err := http.DefaultClient.Do(req)
	require.NoError(t, err)

	_ = got.Body.Close()
	assert.Equal(t, http.StatusNotFound, got.StatusCode)

	assert.Empty(t, rec.all())
}

func TestModelRoutePassesUpstreamErrors(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     int
		retryAfter string
		body       string
	}{
		{"401", 401, "", `{"error":{"message":"bad key"}}`},
		{"403", 403, "", `{"error":{"message":"nope"}}`},
		{"429", 429, "30", `{"error":{"message":"slow down"}}`},
		{"500", 500, "", `{"error":{"message":"oops"}}`},
		{"529", 529, "", `{"type":"error","error":{"type":"overloaded_error"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := serve(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tc.retryAfter != "" {
					w.Header().Set("Retry-After", tc.retryAfter)
				}

				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))

			p := startProxy(t, Config{Model: modelRoute(up.URL, authBearer)})

			resp := post(t, p.URL, "/v1/messages", authBearer, strings.NewReader("{}"))
			out, err := io.ReadAll(resp.Body)
			require.NoError(t, err)

			assert.Equal(t, tc.status, resp.StatusCode)
			assert.Equal(t, tc.body, string(out))
			assert.Equal(t, tc.retryAfter, resp.Header.Get("Retry-After"))
		})
	}
}

func TestModelRouteStreamCutKeepsBytesSent(t *testing.T) {
	const sent = "event: message_start\ndata: {\"type\":\"mess"

	up := serve(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sent)
		w.(http.Flusher).Flush()

		panic(http.ErrAbortHandler)
	}))

	p := startProxy(t, Config{Model: modelRoute(up.URL, authBearer)})

	resp := post(t, p.URL, "/v1/messages", authBearer, strings.NewReader("{}"))
	out, err := io.ReadAll(resp.Body)
	require.Error(t, err)

	assert.Equal(t, sent, string(out))
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestModelRouteDialFailureIsBadGateway(t *testing.T) {
	up := httptest.NewServer(http.NotFoundHandler())
	base := up.URL
	up.Close()

	p := startProxy(t, Config{Model: modelRoute(base, authBearer)})

	resp := post(t, p.URL, "/v1/messages", authBearer, strings.NewReader("{}"))
	assert.Equal(t, http.StatusBadGateway, resp.StatusCode)
}

func TestCredentialNeverInErrors(t *testing.T) {
	var logs bytes.Buffer

	prev := log.Writer()

	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(prev) })

	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()

	cut := serve(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: x\n")
		w.(http.Flusher).Flush()

		panic(http.ErrAbortHandler)
	}))

	// httptest's certificate is in no system store, so the proxy refuses it.
	untrusted := httptest.NewUnstartedServer(http.NotFoundHandler())
	untrusted.Config.ErrorLog = log.New(io.Discard, "", 0)
	untrusted.StartTLS()
	t.Cleanup(untrusted.Close)

	var sb strings.Builder

	for _, base := range []string{deadURL, cut.URL, untrusted.URL} {
		p := startProxy(t, Config{Model: modelRoute(base, authBearer)})

		resp := post(t, p.URL, "/v1/messages", authBearer, strings.NewReader("{}"))
		b, _ := io.ReadAll(resp.Body)

		sb.Write(b)
		sb.WriteString(resp.Status)

		for k, v := range resp.Header {
			sb.WriteString(k + strings.Join(v, ","))
		}
	}

	sb.WriteString(logs.String())

	assert.NotContains(t, sb.String(), realKey)
	assert.NotContains(t, sb.String(), standIn)
	// The reverse proxy would log a cut stream to the default logger if its ErrorLog were not discarded.
	assert.Empty(t, logs.String())
}

func TestSSEFlush(t *testing.T) {
	hold := newGate()
	up := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")

		fl := w.(http.Flusher)

		_, _ = io.WriteString(w, ": keepalive\n\n")

		fl.Flush()

		_, _ = io.WriteString(w, "data: {\"x\":1}\n\n")

		fl.Flush()

		select {
		case <-hold.ch:
		case <-r.Context().Done():
		}
	}))

	p := startProxy(t, Config{Model: modelRoute(up.URL, authBearer)})
	t.Cleanup(hold.open)

	req, err := http.NewRequestWithContext(withTimeout(t, 2*time.Second), http.MethodPost, p.URL+"/v1/messages", strings.NewReader("{}"))
	require.NoError(t, err)
	setStandIn(req, authBearer)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	lines := lineReader(resp)

	assert.Equal(t, ": keepalive", nextLine(t, lines, 2*time.Second))
	assert.Empty(t, nextLine(t, lines, 2*time.Second))
	assert.JSONEq(t, `{"x":1}`, strings.TrimPrefix(nextLine(t, lines, 2*time.Second), "data: "))
}

// The request body can still be in flight when the upstream starts its
// response; a proxy that drops it at the first response write never streams.
func TestRequestBodyForwardedAfterResponseStarts(t *testing.T) {
	hold := newGate()
	gotBody := make(chan string, 1)

	up := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.NoError(t, http.NewResponseController(w).EnableFullDuplex())
		w.Header().Set("Content-Type", "text/event-stream")

		_, _ = io.WriteString(w, ": keepalive\n\n")

		w.(http.Flusher).Flush()

		b, err := io.ReadAll(r.Body)
		assert.NoError(t, err)

		select {
		case gotBody <- string(b):
		default:
		}

		_, _ = io.WriteString(w, "data: {\"x\":1}\n\n")

		w.(http.Flusher).Flush()

		select {
		case <-hold.ch:
		case <-r.Context().Done():
		}
	}))

	p := startProxy(t, Config{Model: modelRoute(up.URL, authBearer)})
	t.Cleanup(hold.open)

	ctx := withTimeout(t, 2*time.Second)
	pr, pw := io.Pipe()
	stop := context.AfterFunc(ctx, func() { pw.CloseWithError(ctx.Err()) })

	t.Cleanup(func() { stop(); _ = pw.Close() })

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.URL+"/v1/messages", pr)
	require.NoError(t, err)

	req.ContentLength = 2
	setStandIn(req, authBearer)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	lines := lineReader(resp)

	assert.Equal(t, ": keepalive", nextLine(t, lines, 2*time.Second))

	_, err = io.WriteString(pw, "{}")
	require.NoError(t, err)
	require.NoError(t, pw.Close())

	assert.Empty(t, nextLine(t, lines, 2*time.Second))
	assert.JSONEq(t, `{"x":1}`, strings.TrimPrefix(nextLine(t, lines, 2*time.Second), "data: "))
	assert.Equal(t, "{}", <-gotBody)
}

func TestBodiesByteIdentical(t *testing.T) {
	const respBody = `{ "usage" :{"input_tokens":10,"output_tokens":5} ,"id":"msg-1",  "content":[ ]}`

	reqBody := []byte(`{"model":"m","messages":[{"role":"user","content":[{"type":"image","data":"`)
	for i := 0; len(reqBody) < 3<<20; i++ {
		reqBody = append(reqBody, byte('A'+i%26))
	}

	reqBody = append(reqBody, `"}]}]}`...)

	var got []byte

	up := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = io.ReadAll(r.Body)

		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, respBody)
	}))

	p := startProxy(t, Config{Model: modelRoute(up.URL, authBearer), OnUsage: func(Usage) {}})

	resp := post(t, p.URL, "/v1/messages", authBearer, bytes.NewReader(reqBody))
	out, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.True(t, bytes.Equal(reqBody, got), "request body altered (%d vs %d bytes)", len(reqBody), len(got))
	assert.True(t, bytes.Equal([]byte(respBody), out), "response body altered: %q", out)
}

func TestConcurrentStreamsKeepTheirOwnBody(t *testing.T) {
	const streams = 16

	all := newGate()

	var arrived atomic.Int32

	up := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		assert.Equal(t, "Bearer "+realKey, r.Header.Get("Authorization"))

		if arrived.Add(1) == streams {
			all.open()
		}

		select {
		case <-all.ch:
		case <-r.Context().Done():
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: "+string(b)+"\n\n")
		w.(http.Flusher).Flush()
	}))

	p := startProxy(t, Config{Model: modelRoute(up.URL, authBearer)})
	t.Cleanup(all.open)

	ctx := withTimeout(t, 5*time.Second)
	results := make(chan [2]string, streams)

	var wg sync.WaitGroup

	for i := range streams {
		wg.Go(func() {
			id := strings.Repeat("x", i+1)

			req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.URL+"/v1/messages", strings.NewReader(id))
			if err != nil {
				results <- [2]string{id, err.Error()}

				return
			}

			setStandIn(req, authBearer)

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				results <- [2]string{id, err.Error()}

				return
			}
			defer resp.Body.Close()

			b, _ := io.ReadAll(resp.Body)
			results <- [2]string{id, string(b)}
		})
	}

	wg.Wait()
	close(results)

	for r := range results {
		assert.Equal(t, "data: "+r[0]+"\n\n", r[1])
	}
}

func TestClientHangupCancelsUpstream(t *testing.T) {
	hold := newGate()
	cancelled := make(chan struct{})

	up := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: first\n\n")
		w.(http.Flusher).Flush()

		select {
		case <-r.Context().Done():
			close(cancelled)
		case <-hold.ch:
		}
	}))

	p := startProxy(t, Config{Model: modelRoute(up.URL, authBearer)})
	t.Cleanup(hold.open)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.URL+"/v1/messages", strings.NewReader("{}"))
	require.NoError(t, err)
	setStandIn(req, authBearer)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	lines := lineReader(resp)
	assert.Equal(t, "data: first", nextLine(t, lines, 2*time.Second))

	cancel()

	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		require.FailNow(t, "upstream request not cancelled after client hangup")
	}
}

func TestModelRouteAsksForIdentity(t *testing.T) {
	for _, clientEnc := range []string{"", "gzip, deflate, br", "identity"} {
		t.Run("client sends "+clientEnc, func(t *testing.T) {
			var got atomic.Value

			up := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got.Store(r.Header["Accept-Encoding"])
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, "{}")
			}))
			p := startProxy(t, Config{Model: modelRoute(up.URL, authBearer)})

			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, p.URL+"/v1/messages", strings.NewReader("{}"))
			require.NoError(t, err)
			setStandIn(req, authBearer)

			if clientEnc != "" {
				req.Header.Set("Accept-Encoding", clientEnc)
			}

			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)

			_ = resp.Body.Close()

			assert.Equal(t, []string{"identity"}, got.Load())
		})
	}
}

func TestModelRouteMergesQuery(t *testing.T) {
	up, rec := capture(t, nil)
	p := startProxy(t, Config{Model: modelRoute(up.URL+"/gw?tenant=a", authBearer)})

	resp := post(t, p.URL, "/v1/messages?beta=true", authBearer, strings.NewReader("{}"))
	_, _ = io.Copy(io.Discard, resp.Body)

	require.Len(t, rec.all(), 1)
	assert.Equal(t, "/gw/v1/messages?tenant=a&beta=true", rec.all()[0].uri)
}
