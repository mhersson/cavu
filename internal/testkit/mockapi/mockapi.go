// Package mockapi serves a scripted Anthropic Messages API for tests: each
// model call answers with the next scripted step as an event stream, and
// every request is logged.
package mockapi

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Structured names the request tool that carries the structured-output
// schema, whatever the request calls it.
const Structured = "$structured"

// structuredTool is the name the pinned release is assumed to give the
// structured-output tool.
const structuredTool = "StructuredOutput"

// hostName is how a container reaches a server on its host.
const hostName = "host.docker.internal"

// Step is one model call's answer: a text block, or one tool_use block when
// Tool is set.
type Step struct {
	Text  string
	Tool  string // a tool name, or Structured
	Input any    // the tool input; nil is {}
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

// StartTLS serves steps like Start, over TLS with a certificate for
// host.docker.internal (and loopback) signed by a fresh test CA, returned as
// PEM.
func StartTLS(t *testing.T, steps []Step) (*Server, []byte) {
	t.Helper()

	caPEM, leaf := certificates(t)

	s := newServer(t, steps)
	s.srv.TLS = &tls.Config{Certificates: []tls.Certificate{leaf}, MinVersion: tls.VersionTLS12}
	s.tls = true
	s.srv.StartTLS()

	return s, caPEM
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
	n, step, hold := s.record(r, body, messages && !countTokens && body.Stream)

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
func (s *Server) record(r *http.Request, body requestBody, modelCall bool) (int, Step, chan struct{}) {
	req := Request{
		Method: r.Method, Path: r.URL.Path, Header: r.Header.Clone(),
		Model: body.Model, System: systemText(body.System), Messages: body.Messages,
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

// certificates returns a fresh CA as PEM and a server certificate it signed.
func certificates(t *testing.T) ([]byte, tls.Certificate) {
	t.Helper()

	now := time.Now()

	caKey := newKey(t)
	caTmpl := &x509.Certificate{
		SerialNumber: serial(t), Subject: pkix.Name{CommonName: "mockapi test CA"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}

	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("mockapi: create CA: %v", err)
	}

	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatalf("mockapi: parse CA: %v", err)
	}

	leafKey := newKey(t)
	leafTmpl := &x509.Certificate{
		SerialNumber: serial(t), Subject: pkix.Name{CommonName: hostName},
		DNSNames: []string{hostName, "localhost"}, IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}

	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, ca, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("mockapi: create certificate: %v", err)
	}

	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})

	return caPEM, tls.Certificate{Certificate: [][]byte{leafDER}, PrivateKey: leafKey}
}

func newKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("mockapi: generate key: %v", err)
	}

	return key
}

func serial(t *testing.T) *big.Int {
	t.Helper()

	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		t.Fatalf("mockapi: serial: %v", err)
	}

	return n
}
