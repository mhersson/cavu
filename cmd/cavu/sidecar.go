package main

import (
	"context"
	"errors"
	"io"

	"github.com/mhersson/cavu/internal/sidecar"
)

func init() {
	commands["sidecar"] = command{
		summary:  "run the sandbox sidecar: read one JSON request on stdin, report JSON lines",
		internal: true,
		run: func(ctx context.Context, args []string, stdin io.Reader, stdout, _ io.Writer) error {
			if len(args) != 0 {
				return errors.New("usage: cavu sidecar (takes no arguments)")
			}

			return sidecar.Run(ctx, stdin, stdout)
		},
	}
}
