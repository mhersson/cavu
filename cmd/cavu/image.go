package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"

	"github.com/mhersson/cavu/internal/driver/claudecode"
	"github.com/mhersson/cavu/internal/sandbox"
)

func init() {
	commands["image"] = command{
		summary: "image build [--cavu-binary PATH]: build the sandbox image",
		run:     runImage,
	}
}

func runImage(ctx context.Context, args []string, _ io.Reader, stdout, stderr io.Writer) error {
	f := newVerbFlags("cavu image build [--cavu-binary PATH] [--data-dir DIR]", stderr)

	binary := f.String("cavu-binary", "", "a Linux cavu executable to put in the image (default: this executable, on Linux)")

	if len(args) == 0 || args[0] != "build" {
		return f.usageError()
	}

	if _, err := f.parse(args[1:], 0); err != nil {
		return err
	}

	bin, err := defaultBinary(runtime.GOOS, *binary, os.Executable)
	if err != nil {
		return err
	}

	a, err := f.open(ctx)
	if err != nil {
		return err
	}
	defer a.close()

	bundle, err := a.caBundle(ctx)
	if err != nil {
		return err
	}

	o := sandbox.BuildOptions{
		CavuBinary:   bin,
		CavuVersion:  version,
		AgentVersion: claudecode.Version,
		AgentSHA512:  claudecode.SHA512,
		CABundle:     bundle,
	}

	rt, err := a.runtime(ctx)
	if err != nil {
		return err
	}

	if err := rt.BuildImage(ctx, o, stdout); err != nil {
		return err
	}

	_, err = fmt.Fprintf(stdout, "built %s\n", sandbox.ImageName(version))

	return err
}

// defaultBinary is the flag's value, else this executable on Linux: on another
// OS the running binary cannot run in the image.
func defaultBinary(goos, flagValue string, executable func() (string, error)) (string, error) {
	if flagValue != "" {
		return flagValue, nil
	}

	if goos != "linux" {
		return "", errors.New("--cavu-binary is required off Linux: pass a cavu built with GOOS=linux")
	}

	return executable()
}
