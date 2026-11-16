package main

import (
	"context"
	"fmt"
	"io"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func init() {
	commands["version"] = command{
		summary: "print the cavu version",
		run: func(_ context.Context, _ []string, _ io.Reader, stdout, _ io.Writer) error {
			_, err := fmt.Fprintf(stdout, "cavu %s\n", version)

			return err
		},
	}
}
