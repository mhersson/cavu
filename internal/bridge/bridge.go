// Package bridge connects the agent container, which has no network, to the
// proxy: it listens on loopback TCP and copies each connection to a Unix socket
// on the hand-off volume.
package bridge

import (
	"context"
	"io"
	"net"
	"sync"
)

// Serve accepts TCP connections on listen and copies each to a new connection
// to the Unix socket, both directions, until ctx ends. The container running it
// is removed by force, so open connections are not drained.
func Serve(ctx context.Context, listen, socket string) error {
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", listen)
	if err != nil {
		return err
	}

	return serve(ctx, ln, socket)
}

func serve(ctx context.Context, ln net.Listener, socket string) error {
	stop := context.AfterFunc(ctx, func() { _ = ln.Close() })
	defer stop()

	for {
		client, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}

			return err
		}

		go handle(ctx, client, socket)
	}
}

func handle(ctx context.Context, client net.Conn, socket string) {
	defer client.Close()

	upstream, err := (&net.Dialer{}).DialContext(ctx, "unix", socket)
	if err != nil {
		return
	}

	defer upstream.Close()

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
