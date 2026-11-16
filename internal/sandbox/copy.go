package sandbox

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"path"
	"slices"
	"time"
)

var errNoFile = errors.New("no file in the stream")

// checkPath wants an absolute, clean path below the root.
func checkPath(p string) error {
	if !path.IsAbs(p) || path.Clean(p) != p || p == "/" {
		return fmt.Errorf("path %q: want an absolute, clean path", p)
	}

	return nil
}

// CopyIn writes files (0600, owned by the exec user) through an exec that
// unpacks a tar stream from stdin. The tar holds file entries only, so
// existing parent directories keep their owner and mode. It needs a running
// container and works on volumes and on a tmpfs.
func (c Container) CopyIn(ctx context.Context, files map[string][]byte) error {
	if err := checkArg("container name", c.name); err != nil {
		return err
	}

	var buf bytes.Buffer

	tw := tar.NewWriter(&buf)
	now := time.Now()

	for _, p := range slices.Sorted(maps.Keys(files)) {
		if err := checkPath(p); err != nil {
			return err
		}

		h := &tar.Header{Typeflag: tar.TypeReg, Name: p[1:], Mode: 0o600, Size: int64(len(files[p])), ModTime: now}
		if err := tw.WriteHeader(h); err != nil {
			return fmt.Errorf("copy %s: %w", p, err)
		}

		if _, err := tw.Write(files[p]); err != nil {
			return fmt.Errorf("copy %s: %w", p, err)
		}
	}

	if err := tw.Close(); err != nil {
		return fmt.Errorf("copy into %s: %w", c.name, err)
	}

	if len(files) == 0 {
		return nil
	}

	args := []string{"exec", "--interactive", c.name, "tar", "-x", "-f", "-", "-C", "/"}
	if err := c.r.run(ctx, call{args: args, stdin: buf.Bytes()}); err != nil {
		return fmt.Errorf("copy into %s: %w", c.name, err)
	}

	return nil
}

// CopyOut runs `tar -c` on the one path and reads the stream on the host:
// exactly one entry, a regular file, or it is an error (tar never follows a
// symlink, so there is no check-then-read race). The host uses the entry's
// type and bytes only, never its name. It needs a running container and works
// on volumes and on a tmpfs. A caller copying a path the agent controls bounds
// the copy with its writer: a writer that fails stops the client.
func (c Container) CopyOut(ctx context.Context, p string, w io.Writer) error {
	if err := checkArg("container name", c.name); err != nil {
		return err
	}

	if err := checkPath(p); err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	pr, pw := io.Pipe()
	done := make(chan error, 1)

	go func() {
		err := c.r.run(ctx, call{args: []string{"exec", c.name, "tar", "-c", "-f", "-", p}, stdout: pw})
		_ = pw.CloseWithError(err)

		done <- err
	}()

	readErr := readOne(pr, w)

	switch {
	case readErr == nil:
		// Drain tar's end-of-archive padding so the client can finish cleanly.
		_, readErr = io.Copy(io.Discard, pr)
	case errors.Is(readErr, errNoFile):
		// An empty archive is what tar sends for a path it cannot read, so
		// the client finishes and its reason is kept.
		_, _ = io.Copy(io.Discard, pr)
	default:
		// A refused stream is never read to its end; stopping the client ends it.
		cancel()
	}

	_ = pr.CloseWithError(errors.New("stream abandoned"))

	// A failed tar's stderr says why; a client that only lost its reader
	// adds nothing to the read error, and the drain may already hold the
	// client's own error.
	err := readErr
	if runErr := <-done; (err == nil || isClientError(runErr)) && !errors.Is(err, runErr) {
		err = errors.Join(err, runErr)
	}

	if err != nil {
		return fmt.Errorf("copy %s out of %s: %w", p, c.name, err)
	}

	return nil
}

// readOne copies the stream's only entry to w, refusing any entry that is not
// a regular file.
func readOne(r io.Reader, w io.Writer) error {
	tr := tar.NewReader(r)

	h, err := tr.Next()
	if errors.Is(err, io.EOF) {
		return errNoFile
	}

	if err != nil {
		return err
	}

	if h.Typeflag != tar.TypeReg {
		return fmt.Errorf("not a regular file (tar type %q)", h.Typeflag)
	}

	if _, err := io.Copy(w, tr); err != nil { //nolint:gosec // no decompression: the bytes are the file's, and w decides how many to keep
		return err
	}

	switch _, err := tr.Next(); {
	case err == nil:
		return errors.New("more than one entry in the stream")
	case !errors.Is(err, io.EOF):
		return err
	}

	return nil
}
