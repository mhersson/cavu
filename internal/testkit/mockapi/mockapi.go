// Package mockapi serves a scripted Anthropic Messages API for tests: each
// model call answers with the next scripted step as an event stream, and
// every request is logged.
package mockapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// Structured names the request tool that carries the structured-output
// schema, whatever the request calls it.
const Structured = "$structured"

// structuredTool is the name the pinned release is assumed to give the
// structured-output tool.
const structuredTool = "StructuredOutput"

// hostName is how a container reaches a server on its host.
const hostName = "host.docker.internal"

// Step is one model call's answer: a text block, one tool_use block when Tool
// is set, or the API's error body under Status when that is set.
type Step struct {
	Text   string
	Tool   string // a tool name, or Structured
	Input  any    // the tool input; nil is {}
	Status int    // an HTTP error status
}

// errorTypes are the Messages API's error types by status; any other status
// is an api_error.
var errorTypes = map[int]string{
	http.StatusBadRequest:          "invalid_request_error",
	http.StatusUnauthorized:        "authentication_error",
	http.StatusForbidden:           "permission_error",
	http.StatusNotFound:            "not_found_error",
	http.StatusTooManyRequests:     "rate_limit_error",
	http.StatusInternalServerError: "api_error",
	529:                            "overloaded_error",
}

// Request is one logged request.
type Request struct {
	Method   string
	Path     string
	Header   http.Header
	Model    string
	System   string // the system prompt's text blocks, joined by newlines
	Tools    []string
	Messages []json.RawMessage
	// ModelCall is a streaming call the script answered; a side request or a
	// token count is not one.
	ModelCall bool
	Body      []byte
}

// Server is a running mock; it closes when the test ends.
type Server struct {
	t   *testing.T
	srv *httptest.Server
	tls bool
	run string // keeps message and tool ids unique across servers

	mu       sync.Mutex
	steps    []Step
	requests []Request
	holds    []chan struct{} // pending holds, taken by model calls in order
	done     chan struct{}
}

// Start serves steps over plain HTTP on all interfaces, so a container can
// reach it at HostURL.
func Start(t *testing.T, steps []Step) *Server {
	t.Helper()

	s := newServer(t, steps)
	s.srv.Start()

	return s
}

// StartTLSWith serves steps like Start, over TLS with the caller's certificate.
func StartTLSWith(t *testing.T, steps []Step, cert tls.Certificate) *Server {
	t.Helper()

	s := newServer(t, steps)
	s.srv.Config.ErrorLog = log.New(quietHandshakes{os.Stderr}, "", log.LstdFlags)
	s.srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	s.tls = true
	s.srv.StartTLS()

	return s
}

func newServer(t *testing.T, steps []Step) *Server {
	t.Helper()

	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", ":0")
	if err != nil {
		t.Fatalf("mockapi: listen: %v", err)
	}

	s := &Server{t: t, steps: slices.Clone(steps), run: randomHex(3), done: make(chan struct{})}
	s.srv = httptest.NewUnstartedServer(http.HandlerFunc(s.serve))
	_ = s.srv.Listener.Close()
	s.srv.Listener = ln

	t.Cleanup(func() {
		// A held answer would block Close.
		close(s.done)
		s.srv.Close()
	})

	return s
}

// URL is the server as the host reaches it.
func (s *Server) URL() string { return s.scheme() + "://127.0.0.1:" + s.port() }

// HostURL is the server as a container reaches it.
func (s *Server) HostURL() string { return s.scheme() + "://" + hostName + ":" + s.port() }

func (s *Server) scheme() string {
	if s.tls {
		return "https"
	}

	return "http"
}

func (s *Server) port() string {
	_, port, _ := net.SplitHostPort(s.srv.Listener.Addr().String())

	return port
}

// Requests returns every request so far, in arrival order.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()

	return slices.Clone(s.requests)
}

// Hold makes one later model call's answer wait until release is called; the
// request is logged at once. Holds queue: each takes the next model call not
// already held, and each is released on its own.
func (s *Server) Hold() (release func()) {
	ch := make(chan struct{})

	s.mu.Lock()
	s.holds = append(s.holds, ch)
	s.mu.Unlock()

	var once sync.Once

	return func() { once.Do(func() { close(ch) }) }
}

type requestBody struct {
	Model    string            `json:"model"`
	Stream   bool              `json:"stream"`
	System   json.RawMessage   `json:"system"`
	Tools    []toolDef         `json:"tools"`
	Messages []json.RawMessage `json:"messages"`
}

type toolDef struct {
	Name string `json:"name"`
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)

	var body requestBody

	_ = json.Unmarshal(raw, &body)

	messages := r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/messages")
	countTokens := strings.HasSuffix(r.URL.Path, "/count_tokens")
	n, step, hold := s.record(r, raw, body, messages && !countTokens && body.Stream)

	if !messages {
		http.NotFound(w, r)

		return
	}

	if countTokens {
		writeJSON(w, http.StatusOK, map[string]any{"input_tokens": 10})

		return
	}

	model := body.Model
	if model == "" {
		model = "claude-haiku-5-5"
	}

	// A call that does not stream is a side request, not a model call the
	// script answers.
	if !body.Stream {
		writeJSON(w, http.StatusOK, map[string]any{
			"id": s.id("msg", n), "type": "message", "role": "assistant", "model": model,
			"content": []any{map[string]any{"type": "text", "text": "ok"}}, "stop_reason": "end_turn",
			"stop_sequence": nil, "usage": usage(1),
		})

		return
	}

	if hold != nil {
		select {
		case <-hold:
		case <-s.done:
			return
		case <-r.Context().Done():
			return
		}
	}

	if step.Status != 0 {
		writeJSON(w, step.Status, errorBody(step.Status))

		return
	}

	tool := step.Tool
	if tool == Structured {
		if !slices.Contains(body.Tools, toolDef{Name: structuredTool}) {
			s.t.Errorf("mockapi: request %d has no %s tool for a %s step", n, structuredTool, Structured)
			writeJSON(w, http.StatusBadRequest, map[string]any{"type": "error", "error": map[string]any{
				"type": "invalid_request_error", "message": "mockapi: no " + structuredTool + " tool in the request",
			}})

			return
		}

		tool = structuredTool
	}

	s.stream(w, n, model, step, tool)
}

// record logs r and returns its 1-based number. For a model call it also takes
// the next step, or done after the last, and the oldest pending hold, under the
// same lock, so log order, message ids, steps and holds always pair.
func (s *Server) record(r *http.Request, raw []byte, body requestBody, modelCall bool) (int, Step, chan struct{}) {
	req := Request{
		Method: r.Method, Path: r.URL.Path, Header: r.Header.Clone(),
		Model: body.Model, System: systemText(body.System), Messages: body.Messages,
		ModelCall: modelCall, Body: raw,
	}

	for _, t := range body.Tools {
		req.Tools = append(req.Tools, t.Name)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.requests = append(s.requests, req)
	n := len(s.requests)

	if !modelCall {
		return n, Step{}, nil
	}

	step := Step{Text: "done"}
	if len(s.steps) > 0 {
		step, s.steps = s.steps[0], s.steps[1:]
	}

	var hold chan struct{}
	if len(s.holds) > 0 {
		hold, s.holds = s.holds[0], s.holds[1:]
	}

	return n, step, hold
}

func (s *Server) id(kind string, n int) string {
	return kind + "_mock" + s.run + "_" + strconv.Itoa(n)
}

func usage(output int) map[string]any {
	return map[string]any{
		"input_tokens": 10, "cache_creation_input_tokens": 0, "cache_read_input_tokens": 0, "output_tokens": output,
	}
}

// stream answers in the Messages API event-stream shape: message_start with
// usage, one content block, message_delta with usage and the stop reason,
// message_stop.
func (s *Server) stream(w http.ResponseWriter, n int, model string, step Step, tool string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Connection", "close")
	w.WriteHeader(http.StatusOK)

	send(w, "message_start", map[string]any{"message": map[string]any{
		"id": s.id("msg", n), "type": "message", "role": "assistant", "model": model, "content": []any{},
		"stop_reason": nil, "stop_sequence": nil, "usage": usage(1),
	}})

	stop := "end_turn"

	if tool != "" {
		input := step.Input
		if input == nil {
			input = map[string]any{}
		}

		args, err := json.Marshal(input)
		if err != nil {
			s.t.Errorf("mockapi: step input: %v", err)
		}

		send(w, "content_block_start", map[string]any{"index": 0, "content_block": map[string]any{
			"type": "tool_use", "id": s.id("toolu", n), "name": tool, "input": map[string]any{},
		}})
		send(w, "content_block_delta", map[string]any{"index": 0, "delta": map[string]any{
			"type": "input_json_delta", "partial_json": string(args),
		}})

		stop = "tool_use"
	} else {
		send(w, "content_block_start", map[string]any{"index": 0, "content_block": map[string]any{"type": "text", "text": ""}})
		send(w, "content_block_delta", map[string]any{"index": 0, "delta": map[string]any{"type": "text_delta", "text": step.Text}})
	}

	send(w, "content_block_stop", map[string]any{"index": 0})
	send(w, "message_delta", map[string]any{
		"delta": map[string]any{"stop_reason": stop, "stop_sequence": nil}, "usage": usage(5),
	})
	send(w, "message_stop", map[string]any{})

	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// send writes one server-sent event whose data carries its name as type.
func send(w io.Writer, name string, data map[string]any) {
	data["type"] = name

	b, _ := json.Marshal(data)
	_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, b)
}

// quietHandshakes drops the server's TLS handshake errors, which a client that
// refuses the certificate causes on purpose, and passes every other line on.
type quietHandshakes struct{ w io.Writer }

func (q quietHandshakes) Write(p []byte) (int, error) {
	if bytes.Contains(p, []byte("TLS handshake error")) {
		return len(p), nil
	}

	return q.w.Write(p)
}

// errorBody is the Messages API's error body for status.
func errorBody(status int) map[string]any {
	kind, ok := errorTypes[status]
	if !ok {
		kind = "api_error"
	}

	return map[string]any{"type": "error", "error": map[string]any{
		"type": kind, "message": "mockapi: scripted " + strconv.Itoa(status),
	}}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	b, _ := json.Marshal(v)

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(b)))
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

// systemText joins a system prompt given as a string or as text blocks.
func systemText(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}

	var blocks []struct {
		Text string `json:"text"`
	}

	_ = json.Unmarshal(raw, &blocks)

	parts := make([]string, 0, len(blocks))
	for _, b := range blocks {
		parts = append(parts, b.Text)
	}

	return strings.Join(parts, "\n")
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)

	return hex.EncodeToString(b)
}
