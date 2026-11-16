// Package sidecar is the program the sidecar container runs: it reads one JSON
// request, then either serves the egress proxy on a Unix socket or runs the git
// clone that needs the network, and reports what happens as JSON lines. Real
// credentials reach it only in that request.
package sidecar

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/mhersson/cavu/internal/git"
	"github.com/mhersson/cavu/internal/proxy"
)

// shutdownGrace bounds how long the proxy waits for in-flight responses.
const shutdownGrace = 3 * time.Second

// writer serialises lines: the proxy calls back from concurrent requests.
type writer struct {
	mu  sync.Mutex
	enc *json.Encoder
}

func (w *writer) write(l Line) {
	w.mu.Lock()
	defer w.mu.Unlock()

	_ = w.enc.Encode(l)
}

// Run reads one Request from stdin and performs it, writing Lines to stdout.
// The proxy op runs until stdin closes or ctx ends. A failure writes an error
// line, with the request's secrets and URLs scrubbed, and returns that message.
func Run(ctx context.Context, stdin io.Reader, stdout io.Writer) error {
	out := &writer{enc: json.NewEncoder(stdout)}
	dec := json.NewDecoder(stdin)

	var req Request
	if err := dec.Decode(&req); err != nil {
		return fail(out, nil, errors.New("request: not a JSON document"))
	}

	// Whatever the decoder buffered past the request still counts as stdin.
	rest := io.MultiReader(dec.Buffered(), stdin)

	var err error

	switch {
	case req.Op == "proxy" && req.Proxy != nil:
		err = runProxy(ctx, out, rest, req.Proxy)
	case req.Op == "clone" && req.Clone != nil:
		err = runClone(ctx, out, req.Clone)
	default:
		err = errors.New("request: unknown op or missing body")
	}

	if err != nil {
		return fail(out, &req, err)
	}

	return nil
}

func fail(out *writer, req *Request, err error) error {
	msg := scrub(err.Error(), req)
	out.write(Line{Type: "error", Error: msg})

	return errors.New(msg)
}

func runProxy(ctx context.Context, out *writer, stdin io.Reader, p *ProxyRequest) error {
	if p.Socket == "" {
		return errors.New("proxy: socket required")
	}

	h, err := proxy.New(proxy.Config{
		AllowHosts:   p.AllowHosts,
		Model:        p.Model,
		CABundle:     []byte(p.CABundle),
		OnDenied:     func(host string) { out.write(Line{Type: "denied", Host: host}) },
		OnUsage:      func(u proxy.Usage) { out.write(Line{Type: "usage", Usage: &u}) },
		OnModelError: func(status int, kind string) { out.write(Line{Type: "model_error", Status: status, Kind: kind}) },
	})
	if err != nil {
		return fmt.Errorf("proxy: %w", err)
	}

	// The hand-off volume outlives a container restart, so a socket may be left over.
	if err := os.Remove(p.Socket); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("proxy: replace the socket: %w", err)
	}

	ln, err := (&net.ListenConfig{}).Listen(ctx, "unix", p.Socket)
	if err != nil {
		return fmt.Errorf("proxy: listen on the socket: %w", err)
	}

	// The agent's uid may differ from the sidecar's.
	if err := os.Chmod(p.Socket, 0o666); err != nil { //nolint:gosec // the socket must be connectable by another uid
		_ = ln.Close()

		return fmt.Errorf("proxy: set the socket mode: %w", err)
	}

	srv := &http.Server{Handler: h, ReadHeaderTimeout: 30 * time.Second}
	served := make(chan error, 1)

	go func() { served <- srv.Serve(ln) }()

	out.write(Line{Type: "ready"})

	eof := make(chan struct{})

	go func() {
		_, _ = io.Copy(io.Discard, stdin)

		close(eof)
	}()

	var result error

	select {
	case <-eof:
	case <-ctx.Done():
	case err := <-served:
		result = fmt.Errorf("proxy: %w", err)
	}

	// Let in-flight responses finish so their usage lines are written before
	// Run returns; a stream that outlasts the grace is cut.
	shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownGrace)
	defer cancel()

	if srv.Shutdown(shutdown) != nil {
		_ = srv.Close()
	}

	return result
}

func runClone(ctx context.Context, out *writer, c *CloneRequest) error {
	base, err := git.Clone(ctx, git.CloneSpec{
		URL: c.URL, Token: c.Token, BaseBranch: c.BaseBranch, Commit: c.Commit, Branch: c.Branch,
		MirrorDir: c.MirrorDir, WorkspaceDir: c.WorkspaceDir, CABundle: c.CABundle,
		Name: c.Name, Email: c.Email,
	})
	if err != nil {
		return err
	}

	out.write(Line{Type: "cloned", Commit: base})

	return nil
}

var urlRE = regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*://\S*`)

// scrub removes every secret and URL the request carried, and any other URL,
// from msg.
func scrub(msg string, req *Request) string {
	var secrets []string

	if req != nil {
		if c := req.Clone; c != nil {
			secrets = append(secrets, c.Token, c.URL, c.CABundle)
		}

		if p := req.Proxy; p != nil && p.Model != nil {
			secrets = append(secrets, p.Model.Credential, p.Model.StandIn, p.Model.BaseURL)
		}
	}

	for _, s := range secrets {
		if s != "" {
			msg = strings.ReplaceAll(msg, s, "<redacted>")
		}
	}

	return urlRE.ReplaceAllString(msg, "<url>")
}
