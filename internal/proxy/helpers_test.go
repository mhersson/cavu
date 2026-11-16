package proxy

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const (
	realKey = "real-credential-xyz"
	standIn = "stand-in-token-abc"
)

func serve(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()

	s := httptest.NewServer(h)
	t.Cleanup(s.Close)

	return s
}

func startProxy(t *testing.T, cfg Config) *httptest.Server {
	t.Helper()

	h, err := New(cfg)
	require.NoError(t, err)

	return serve(t, h)
}

func modelRoute(base, style string) *ModelRoute {
	return &ModelRoute{BaseURL: base, Credential: realKey, AuthHeader: style, StandIn: standIn}
}

// post sends an origin-form request to the proxy carrying the stand-in in the given style.
func post(t *testing.T, proxyURL, path, style string, body io.Reader) *http.Response {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, proxyURL+path, body)
	require.NoError(t, err)
	setStandIn(req, style)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)

	t.Cleanup(func() { _ = resp.Body.Close() })

	return resp
}

func setStandIn(req *http.Request, style string) {
	if style == authBearer {
		req.Header.Set("Authorization", "Bearer "+standIn)
	} else {
		req.Header.Set("X-Api-Key", standIn)
	}
}

// viaProxy returns a client that sends absolute-form requests to the proxy.
func viaProxy(t *testing.T, p *httptest.Server) *http.Client {
	t.Helper()

	u, err := url.Parse(p.URL)
	require.NoError(t, err)

	tr := &http.Transport{Proxy: http.ProxyURL(u)}
	t.Cleanup(tr.CloseIdleConnections)

	return &http.Client{
		Transport:     tr,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// connectTo opens a CONNECT tunnel request on a raw connection to the proxy.
func connectTo(t *testing.T, p *httptest.Server, target string) (net.Conn, *bufio.Reader, *http.Response) {
	t.Helper()

	u, err := url.Parse(p.URL)
	require.NoError(t, err)

	conn, err := net.Dial("tcp", u.Host)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	_, err = io.WriteString(conn, "CONNECT "+target+" HTTP/1.1\r\nHost: "+target+"\r\n\r\n")
	require.NoError(t, err)

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	require.NoError(t, err)

	return conn, br, resp
}

// echoListener accepts connections and echoes their bytes back.
func echoListener(t *testing.T) string {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}

			go func() {
				defer c.Close()

				_, _ = io.Copy(c, c)
			}()
		}
	}()

	return ln.Addr().String()
}

type deniedLog struct {
	mu   sync.Mutex
	list []string
}

func (d *deniedLog) add(h string) {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.list = append(d.list, h)
}

func (d *deniedLog) all() []string {
	d.mu.Lock()
	defer d.mu.Unlock()

	return append([]string(nil), d.list...)
}

// gate blocks upstream handlers until opened.
type gate struct {
	ch   chan struct{}
	once sync.Once
}

func newGate() *gate { return &gate{ch: make(chan struct{})} }

func (g *gate) open() { g.once.Do(func() { close(g.ch) }) }

// lineReader delivers the response lines over a channel so tests can wait with a timer.
func lineReader(resp *http.Response) <-chan string {
	lines := make(chan string, 64)

	go func() {
		defer close(lines)

		br := bufio.NewReader(resp.Body)
		for {
			l, err := br.ReadString('\n')
			if l != "" {
				lines <- strings.TrimRight(l, "\n")
			}

			if err != nil {
				return
			}
		}
	}()

	return lines
}

func nextLine(t *testing.T, lines <-chan string, within time.Duration) string {
	t.Helper()

	select {
	case l, ok := <-lines:
		require.True(t, ok, "stream ended early")

		return l
	case <-time.After(within):
		require.FailNow(t, "no line within deadline")

		return ""
	}
}

func withTimeout(t *testing.T, d time.Duration) context.Context {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), d)
	t.Cleanup(cancel)

	return ctx
}
