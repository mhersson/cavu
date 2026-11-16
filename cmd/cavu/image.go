package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"

	"github.com/mhersson/cavu/internal/driver/claudecode"
	"github.com/mhersson/cavu/internal/sandbox"
)

func init() {
	commands["image"] = command{
		summary: "image build [--cavu-binary PATH] [--ca-bundle PATH]: build the sandbox image",
		run:     runImage,
	}
}

func runImage(ctx context.Context, args []string, _ io.Reader, stdout, stderr io.Writer) error {
	if len(args) == 0 || args[0] != "build" {
		return errors.New("usage: cavu image build [--cavu-binary PATH] [--ca-bundle PATH]")
	}

	fs := flag.NewFlagSet("image build", flag.ContinueOnError)
	fs.SetOutput(stderr)

	binary := fs.String("cavu-binary", "", "a Linux cavu executable to put in the image (default: this executable, on Linux)")
	caPath := fs.String("ca-bundle", "", "a PEM file of CA certificates to trust, for a network that inspects TLS")

	if err := fs.Parse(args[1:]); err != nil {
		return exitError{code: 2, err: err}
	}

	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}

	bin, err := defaultBinary(runtime.GOOS, *binary, os.Executable)
	if err != nil {
		return err
	}

	o := sandbox.BuildOptions{
		CavuBinary:   bin,
		CavuVersion:  version,
		AgentVersion: claudecode.Version,
		AgentSHA512:  claudecode.SHA512,
	}

	if *caPath != "" {
		if o.CABundle, err = os.ReadFile(*caPath); err != nil {
			return fmt.Errorf("--ca-bundle: %w", err)
		}
	}

	rt, err := sandbox.Detect(ctx, "")
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
