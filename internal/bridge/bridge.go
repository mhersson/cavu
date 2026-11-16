// Package bridge connects the agent container, which has no network, to the
// proxy: it listens on loopback TCP and copies each connection to a Unix socket
// on the hand-off volume.
package bridge

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"syscall"
	"time"
)

// Serve accepts TCP connections on listen and copies each to a new connection
// to the Unix socket, both directions, until ctx ends.
func Serve(ctx context.Context, listen, socket string) error {
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", listen)
	if err != nil {
		return err
	}

	return serve(ctx, ln, socket)
}

func serve(ctx context.Context, ln net.Listener, socket string) error {
	var open tracker

	// Closing the listener and every open connection ends the accept loop and
	// each copy.
	stop := context.AfterFunc(ctx, func() {
		_ = ln.Close()

		open.closeAll()
	})
	defer stop()

	var wg sync.WaitGroup
	defer wg.Wait()

	var delay time.Duration

	for {
		client, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}

			if !temporary(err) {
				return err
			}

			// Back off as net/http does, so a descriptor shortage does not end egress.
			delay = min(max(2*delay, 5*time.Millisecond), time.Second)

			select {
			case <-time.After(delay):
				continue
			case <-ctx.Done():
				return nil
			}
		}

		delay = 0

		wg.Go(func() { handle(ctx, client, socket, &open) })
	}
}

// temporary reports an Accept error worth retrying: a timeout or a shortage of
// descriptors.
func temporary(err error) bool {
	if ne, ok := errors.AsType[net.Error](err); ok && ne.Timeout() {
		return true
	}

	return errors.Is(err, syscall.EMFILE) || errors.Is(err, syscall.ENFILE)
}

// tracker holds the open connections so a stop can close them all.
type tracker struct {
	mu     sync.Mutex
	conns  map[net.Conn]struct{}
	closed bool
}

// add registers c and returns its release, or closes c and returns false when
// the bridge is already stopping.
func (t *tracker) add(c net.Conn) (release func(), ok bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.closed {
		_ = c.Close()

		return nil, false
	}

	if t.conns == nil {
		t.conns = map[net.Conn]struct{}{}
	}

	t.conns[c] = struct{}{}

	return func() {
		t.mu.Lock()
		delete(t.conns, c)
		t.mu.Unlock()

		_ = c.Close()
	}, true
}

func (t *tracker) closeAll() {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.closed = true

	for c := range t.conns {
		_ = c.Close()
	}
}

func handle(ctx context.Context, client net.Conn, socket string, open *tracker) {
	release, ok := open.add(client)
	if !ok {
		return
	}

	defer release()

	upstream, err := (&net.Dialer{}).DialContext(ctx, "unix", socket)
	if err != nil {
		return
	}

	release, ok = open.add(upstream)
	if !ok {
		return
	}

	defer release()

	var wg sync.WaitGroup

	wg.Add(2)

	go func() { defer wg.Done(); pipe(upstream, client) }()
	go func() { defer wg.Done(); pipe(client, upstream) }()

	wg.Wait()
}

// pipe copies src to dst, then half-closes dst so the peer sees the end of the
// stream while the other direction keeps flowing.
func pipe(dst, src net.Conn) {
	_, _ = io.Copy(dst, src)

	if cw, ok := dst.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}
}
