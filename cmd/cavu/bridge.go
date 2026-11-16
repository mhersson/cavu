package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/mhersson/cavu/internal/bridge"
)

func init() {
	commands["bridge"] = command{
		summary:  "bridge --listen ADDR --socket PATH: forward loopback TCP to the proxy socket",
		internal: true,
		run:      runBridge,
	}
}

func runBridge(ctx context.Context, args []string, _ io.Reader, _, stderr io.Writer) error {
	fs := flag.NewFlagSet("bridge", flag.ContinueOnError)
	fs.SetOutput(stderr)

	listen := fs.String("listen", "127.0.0.1:3128", "address to listen on")
	socket := fs.String("socket", "/handoff/proxy.sock", "the proxy's Unix socket")

	if err := fs.Parse(args); err != nil {
		return exitError{code: 2, err: err}
	}

	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}

	if *listen == "" || *socket == "" {
		return errors.New("--listen and --socket must not be empty")
	}

	return bridge.Serve(ctx, *listen, *socket)
}
