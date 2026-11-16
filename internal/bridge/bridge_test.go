package bridge

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// socketServer serves handler on a Unix socket in a directory short enough for macOS.
func socketServer(t *testing.T, handler http.Handler) string {
	t.Helper()

	dir, err := os.MkdirTemp("", "cavu")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	sock := filepath.Join(dir, "s.sock")
	ln, err := net.Listen("unix", sock)
	require.NoError(t, err)

	srv := &http.Server{Handler: handler, ReadHeaderTimeout: time.Second}

	go func() { _ = srv.Serve(ln) }()

	t.Cleanup(func() { _ = srv.Close() })

	return sock
}

func startBridge(t *testing.T, sock string) (addr string, stop context.CancelFunc, done <-chan error) {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	errc := make(chan error, 1)

	go func() { errc <- serve(ctx, ln, sock) }()

	return ln.Addr().String(), cancel, errc
}

func TestBridgeCopiesBothWays(t *testing.T) {
	t.Parallel()

	sock := socketServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_, _ = w.Write(append([]byte("echo:"), body...))
	}))
	addr, _, _ := startBridge(t, sock)

	resp, err := http.Post("http://"+addr+"/", "text/plain", strings.NewReader("hello"))
	require.NoError(t, err)

	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	assert.Equal(t, "echo:hello", string(body))

	// A client that half-closes after its request still gets the response.
	conn, err := net.Dial("tcp", addr)
	require.NoError(t, err)

	defer conn.Close()

	_, err = io.WriteString(conn, "GET / HTTP/1.0\r\nHost: x\r\n\r\n")
	require.NoError(t, err)
	require.NoError(t, conn.(*net.TCPConn).CloseWrite())

	require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))

	r, err := http.ReadResponse(bufio.NewReader(conn), nil)
	require.NoError(t, err)

	got, _ := io.ReadAll(r.Body)
	assert.Equal(t, "echo:", string(got))
}

func TestBridgeStopsWithContext(t *testing.T) {
	t.Parallel()

	sock := socketServer(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	addr, stop, done := startBridge(t, sock)

	stop()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return")
	}

	_, err := net.DialTimeout("tcp", addr, time.Second)
	assert.Error(t, err)
}

func TestServeReportsListenError(t *testing.T) {
	t.Parallel()

	assert.Error(t, Serve(context.Background(), "256.0.0.1:1", "/nonexistent"))
}

// failing is a listener whose Accept always fails.
type failing struct {
	net.Listener
	err error
}

func (f failing) Accept() (net.Conn, error) { return nil, f.err }

func TestServeStopsOnAcceptError(t *testing.T) {
	t.Parallel()

	inner, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	t.Cleanup(func() { _ = inner.Close() })

	boom := errors.New("boom")

	assert.ErrorIs(t, serve(context.Background(), failing{Listener: inner, err: boom}, "/nonexistent"), boom)
}
