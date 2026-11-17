package sidecar

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/cavu/internal/proxy"
	"github.com/mhersson/cavu/internal/testkit/gitrepo"
)

// syncBuffer is a stdout that a test reads while Run writes it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Write(p)
}

func (b *syncBuffer) lines() []Line {
	b.mu.Lock()
	defer b.mu.Unlock()

	var out []Line

	sc := bufio.NewScanner(bytes.NewReader(b.buf.Bytes()))
	for sc.Scan() {
		var l Line
		if json.Unmarshal(sc.Bytes(), &l) == nil {
			out = append(out, l)
		}
	}

	return out
}

func (b *syncBuffer) waitFor(t *testing.T, typ string) Line {
	t.Helper()

	var found Line

	require.Eventually(t, func() bool {
		for _, l := range b.lines() {
			if l.Type == typ {
				found = l

				return true
			}
		}

		return false
	}, 10*time.Second, 5*time.Millisecond, "waiting for a %q line", typ)

	return found
}

// shortDir is a directory with a path short enough for a Unix socket on macOS.
func shortDir(t *testing.T) string {
	t.Helper()

	dir, err := os.MkdirTemp("", "cavu")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	return dir
}

// unixClient talks to the socket; with proxied set it sends absolute-form
// requests, as a client using the socket as its HTTP proxy would.
func unixClient(sock string, proxied bool) *http.Client {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		},
	}
	if proxied {
		tr.Proxy = http.ProxyURL(&url.URL{Scheme: "http", Host: "proxy.invalid"})
	}

	return &http.Client{Transport: tr}
}

func encode(t *testing.T, r Request) io.Reader {
	t.Helper()

	raw, err := json.Marshal(r)
	require.NoError(t, err)

	return bytes.NewReader(raw)
}

type running struct {
	stdin  *io.PipeWriter
	stdout *syncBuffer
	done   chan error
}

func start(t *testing.T, r Request) *running {
	t.Helper()

	pr, pw := io.Pipe()
	out := &syncBuffer{}
	done := make(chan error, 1)

	ctx, cancel := context.WithCancel(context.Background())

	t.Cleanup(func() {
		_ = pw.Close()

		cancel()
	})

	go func() { done <- Run(ctx, pr, out) }()

	raw, err := json.Marshal(r)
	require.NoError(t, err)

	_, err = pw.Write(raw)
	require.NoError(t, err)

	return &running{stdin: pw, stdout: out, done: done}
}

func (r *running) stop(t *testing.T) error {
	t.Helper()

	require.NoError(t, r.stdin.Close())

	select {
	case err := <-r.done:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not end when stdin closed")

		return nil
	}
}

func TestProxyOpServesOnSocket(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "pong")
	}))
	t.Cleanup(upstream.Close)

	sock := filepath.Join(shortDir(t), "p.sock")
	// A stale file from an earlier container is replaced.
	require.NoError(t, os.WriteFile(sock, nil, 0o600))

	run := start(t, Request{Proxy: &ProxyRequest{Socket: sock, AllowHosts: []string{upstream.Listener.Addr().String()}}})
	run.stdout.waitFor(t, "ready")

	info, err := os.Stat(sock)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o666), info.Mode().Perm())

	get := func(target string) *http.Response {
		resp, err := unixClient(sock, true).Get(target)
		require.NoError(t, err)
		t.Cleanup(func() { _ = resp.Body.Close() })

		return resp
	}

	resp := get(upstream.URL)
	body, _ := io.ReadAll(resp.Body)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "pong", string(body))

	resp = get("http://denied.example/")
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)

	denied := run.stdout.waitFor(t, "denied")
	assert.Equal(t, "denied.example:80", denied.Host)

	require.NoError(t, run.stop(t))

	_, err = net.Dial("unix", sock)
	assert.Error(t, err, "the listener is closed")
}

func TestProxyOpUsageLines(t *testing.T) {
	t.Parallel()

	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"model":"m1","usage":{"input_tokens":3,"output_tokens":5}}`)
	}))
	t.Cleanup(model.Close)

	sock := filepath.Join(shortDir(t), "p.sock")
	run := start(t, Request{Proxy: &ProxyRequest{
		Socket: sock,
		Model:  &proxy.ModelRoute{BaseURL: model.URL, Credential: "real", AuthHeader: "x-api-key", StandIn: "stand-in"},
	}})
	run.stdout.waitFor(t, "ready")

	c := unixClient(sock, false)

	req, err := http.NewRequest(http.MethodPost, "http://model.invalid/v1/messages", strings.NewReader(`{}`))
	require.NoError(t, err)
	req.Header.Set("x-api-key", "stand-in")

	resp, err := c.Do(req)
	require.NoError(t, err)

	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	usage := run.stdout.waitFor(t, "usage")
	require.NotNil(t, usage.Usage)
	assert.EqualValues(t, 3, usage.Usage.Input)
	assert.EqualValues(t, 5, usage.Usage.Output)

	require.NoError(t, run.stop(t))

	n := 0

	for _, l := range run.stdout.lines() {
		if l.Type == "usage" {
			n++
		}
	}

	assert.Equal(t, 1, n)
}

func TestSidecarWritesTheBudgetLine(t *testing.T) {
	t.Parallel()

	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"model":"m1","usage":{"input_tokens":3,"output_tokens":5}}`)
	}))
	t.Cleanup(model.Close)

	sock := filepath.Join(shortDir(t), "p.sock")
	run := start(t, Request{Proxy: &ProxyRequest{
		Socket: sock,
		Model:  &proxy.ModelRoute{BaseURL: model.URL, Credential: "real", AuthHeader: "x-api-key", StandIn: "stand-in"},
		Budget: &proxy.Budget{CapMicroUSD: 8, Prices: map[string]proxy.Price{"m1": {Input: 1, Output: 1}}},
	}})
	run.stdout.waitFor(t, "ready")

	c := unixClient(sock, false)
	ask := func() int {
		req, err := http.NewRequest(http.MethodPost, "http://model.invalid/v1/messages", strings.NewReader(`{}`))
		require.NoError(t, err)
		req.Header.Set("x-api-key", "stand-in")

		resp, err := c.Do(req)
		require.NoError(t, err)

		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()

		return resp.StatusCode
	}

	assert.Equal(t, http.StatusOK, ask())
	run.stdout.waitFor(t, "budget")
	assert.Equal(t, http.StatusPaymentRequired, ask())
	assert.Equal(t, http.StatusPaymentRequired, ask())

	require.NoError(t, run.stop(t))

	lines := run.stdout.lines()

	var types []string
	for _, l := range lines {
		types = append(types, l.Type)
	}

	// The response that spent the cap is reported before the cap.
	assert.Equal(t, []string{"ready", "usage", "budget"}, types)
	assert.Empty(t, lines[2].Error)
}

func TestProxyOpDrainsInFlightResponsesBeforeReturning(t *testing.T) {
	t.Parallel()

	started, release := make(chan struct{}), make(chan struct{})
	releaseOnce := sync.OnceFunc(func() { close(release) })

	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"model":"m1",`)
		_ = http.NewResponseController(w).Flush()

		close(started)
		<-release

		_, _ = io.WriteString(w, `"usage":{"input_tokens":3,"output_tokens":5}}`)
	}))
	t.Cleanup(model.Close)
	// Before model.Close, which waits for the handler.
	t.Cleanup(releaseOnce)

	sock := filepath.Join(shortDir(t), "p.sock")
	run := start(t, Request{Proxy: &ProxyRequest{
		Socket: sock,
		Model:  &proxy.ModelRoute{BaseURL: model.URL, Credential: "real", AuthHeader: "x-api-key", StandIn: "stand-in"},
	}})
	run.stdout.waitFor(t, "ready")

	req, err := http.NewRequest(http.MethodPost, "http://model.invalid/v1/messages", strings.NewReader(`{}`))
	require.NoError(t, err)
	req.Header.Set("x-api-key", "stand-in")

	body := make(chan []byte, 1)

	go func() {
		resp, err := unixClient(sock, false).Do(req)
		if err != nil {
			body <- nil

			return
		}
		defer resp.Body.Close()

		b, _ := io.ReadAll(resp.Body)
		body <- b
	}()

	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("the response never started")
	}

	require.NoError(t, run.stdin.Close())

	select {
	case err := <-run.done:
		t.Fatalf("Run returned with a response in flight: %v", err)
	case <-time.After(200 * time.Millisecond):
	}

	releaseOnce()

	select {
	case err := <-run.done:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not end after the response")
	}

	// Read the moment Run returns: the usage line must already be there.
	var usage []Line

	for _, l := range run.stdout.lines() {
		if l.Type == "usage" {
			usage = append(usage, l)
		}
	}

	require.Len(t, usage, 1)
	require.NotNil(t, usage[0].Usage)
	assert.EqualValues(t, 5, usage[0].Usage.Output)
	assert.JSONEq(t, `{"model":"m1","usage":{"input_tokens":3,"output_tokens":5}}`, string(<-body))
}

// gitServer serves up over HTTPS and points git at a CA file that trusts it,
// through the environment, so its caller cannot run in parallel.
func gitServer(t *testing.T, up string) (url, repo string) {
	t.Helper()

	bare := strings.TrimPrefix(gitrepo.Bare(t, up), "file://")
	srv := httptest.NewTLSServer(gitrepo.Handler(t, bare))
	t.Cleanup(srv.Close)

	ca := filepath.Join(t.TempDir(), "ca.pem")
	require.NoError(t, os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600))
	t.Setenv("GIT_SSL_CAINFO", ca)

	return srv.URL, "/" + filepath.Base(bare)
}

func TestCloneOpWritesClonedLine(t *testing.T) {
	up := gitrepo.New(t, map[string]string{"a.txt": "one\n"})
	base, repo := gitServer(t, up)
	root := t.TempDir()

	var out syncBuffer

	err := Run(context.Background(), encode(t, Request{Clone: &CloneRequest{
		URL: base + repo, Token: "tok", BaseBranch: "main", Branch: "cavu/x",
		MirrorDir: filepath.Join(root, "mirror"), WorkspaceDir: filepath.Join(root, "ws"),
		Name: "Cavu", Email: "cavu@example.com",
	}}), &out)
	require.NoError(t, err)

	lines := out.lines()
	require.Len(t, lines, 1)
	assert.Equal(t, "cloned", lines[0].Type)
	assert.Equal(t, gitrepo.Git(t, up, "rev-parse", "HEAD"), lines[0].Commit)
	assert.Equal(t, lines[0].Commit, gitrepo.Git(t, filepath.Join(root, "ws"), "rev-parse", "HEAD"))
}

func TestErrorLineScrubsToken(t *testing.T) {
	t.Parallel()

	const token = "tok-secret-1234567890"

	for _, target := range []string{
		"https://127.0.0.1:1/o/r.git",
		"https://x-access-token:" + token + "@127.0.0.1:1/o/r.git",
	} {
		root := t.TempDir()

		var out syncBuffer

		err := Run(context.Background(), encode(t, Request{Clone: &CloneRequest{
			URL: target, Token: token, BaseBranch: "main", Branch: "cavu/x",
			MirrorDir: filepath.Join(root, "mirror"), WorkspaceDir: filepath.Join(root, "ws"),
			Name: "Cavu", Email: "cavu@example.com",
		}}), &out)
		require.Error(t, err)

		lines := out.lines()
		require.Len(t, lines, 1)
		assert.Equal(t, "error", lines[0].Type)
		assert.NotEmpty(t, lines[0].Error)

		for _, s := range []string{lines[0].Error, err.Error()} {
			assert.NotContains(t, s, token)
			assert.NotContains(t, s, "://")
			assert.NotContains(t, s, "x-access-token")
		}
	}
}

func TestRequestWithoutAnOp(t *testing.T) {
	t.Parallel()

	for _, in := range []io.Reader{encode(t, Request{}), strings.NewReader("{not json")} {
		var out syncBuffer

		require.Error(t, Run(context.Background(), in, &out))

		lines := out.lines()
		require.Len(t, lines, 1)
		assert.Equal(t, "error", lines[0].Type)
	}
}
