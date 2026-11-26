package sandbox

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/cavu/internal/proc"
)

func TestCopyInSendsFileEntries(t *testing.T) {
	r, f := fakeRuntime(t, "docker")

	files := map[string][]byte{
		"/run/cavu/a":      []byte("x\x00y"),
		"/workspace/dir/b": []byte("b"),
	}
	require.NoError(t, r.Container("cavu-agent-r1").CopyIn(t.Context(), files))

	c := f.only(t)
	assert.Equal(t, []string{"exec", "--interactive", "cavu-agent-r1", "tar", "-x", "-f", "-", "-C", "/"}, c.argv)

	got := map[string][]byte{}
	tr := tar.NewReader(bytes.NewReader(c.stdin))

	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}

		require.NoError(t, err)
		assert.Equal(t, byte(tar.TypeReg), h.Typeflag)
		assert.Equal(t, int64(0o600), h.Mode)

		b, err := io.ReadAll(tr)
		require.NoError(t, err)

		got["/"+h.Name] = b
	}

	assert.Equal(t, files, got)
}

// The stream is written as the client reads it, so a copy allocates far less
// than the files hold.
func TestCopyInStreamsTheFiles(t *testing.T) {
	r, f := fakeRuntime(t, "docker")

	const each = 8 << 20

	files := map[string][]byte{}
	for i := range 4 {
		files[fmt.Sprintf("/run/cavu/images/%d.png", i)] = bytes.Repeat([]byte{byte(i)}, each)
	}

	var before, after runtime.MemStats

	runtime.ReadMemStats(&before)
	require.NoError(t, r.Container("cavu-agent-r1").CopyIn(t.Context(), files))
	runtime.ReadMemStats(&after)

	assert.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(each), "the copy allocated a file's worth or more")
	assert.Len(t, f.only(t).stdin, 4*each+4*512+1024, "four headers, four files and the end of the archive")
}

func TestCopyInEndsWithAClientThatReadsNothing(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "docker")
	require.NoError(t, os.WriteFile(bin, []byte("#!/bin/sh\nexit 3\n"), 0o700))

	files := map[string][]byte{"/run/cavu/a": bytes.Repeat([]byte{1}, 8<<20)}

	err := Runtime{Bin: bin, Kind: "docker"}.Container("cavu-agent-r1").CopyIn(t.Context(), files)

	var exit *proc.ExitError
	require.ErrorAs(t, err, &exit)
	assert.Equal(t, 3, exit.Code)
}

func TestCopyInRefusesBadPaths(t *testing.T) {
	for _, p := range []string{"relative", "/a/../b", "/a/", "/", "//a", ""} {
		t.Run(p, func(t *testing.T) {
			r, f := fakeRuntime(t, "docker")
			require.Error(t, r.Container("cavu-agent-r1").CopyIn(t.Context(), map[string][]byte{p: nil}))
			assert.Empty(t, f.calls(t), "the runtime was called")
		})
	}
}

type entry struct {
	typ  byte
	name string
	body string
}

func tarOf(t *testing.T, entries ...entry) []byte {
	t.Helper()

	var buf bytes.Buffer

	tw := tar.NewWriter(&buf)

	for _, e := range entries {
		h := &tar.Header{Typeflag: e.typ, Name: e.name, Mode: 0o600, Size: int64(len(e.body))}
		switch e.typ {
		case tar.TypeReg:
		case tar.TypeSymlink, tar.TypeLink:
			h.Size = 0
			h.Linkname = "/etc/passwd"
		default:
			h.Size = 0
		}

		require.NoError(t, tw.WriteHeader(h))
		_, err := tw.Write([]byte(e.body)[:h.Size])
		require.NoError(t, err)
	}

	require.NoError(t, tw.Close())

	return buf.Bytes()
}

func TestCopyOutReadsOneRegularFile(t *testing.T) {
	t.Run("a regular file", func(t *testing.T) {
		r, f := fakeRuntime(t, "docker")
		f.set(t, "stdout", tarOf(t, entry{tar.TypeReg, "ignored/name", "bin\x00ary"}))

		var out bytes.Buffer
		require.NoError(t, r.Container("cavu-agent-r1").CopyOut(t.Context(), "/handoff/diff.patch", &out))
		assert.Equal(t, "bin\x00ary", out.String())
		assert.Equal(t, []string{"exec", "cavu-agent-r1", "tar", "-c", "-f", "-", "/handoff/diff.patch"}, f.only(t).argv)
	})

	failures := map[string][]byte{
		"a directory":     tarOf(t, entry{tar.TypeDir, "handoff/", ""}),
		"a symlink":       tarOf(t, entry{tar.TypeSymlink, "handoff/link", ""}),
		"a hard link":     tarOf(t, entry{tar.TypeLink, "handoff/link", ""}),
		"two files":       tarOf(t, entry{tar.TypeReg, "a", "1"}, entry{tar.TypeReg, "b", "2"}),
		"an empty stream": tarOf(t),
		"not a tar":       []byte("garbage"),
	}

	for name, stream := range failures {
		t.Run(name, func(t *testing.T) {
			r, f := fakeRuntime(t, "docker")
			f.set(t, "stdout", stream)

			require.Error(t, r.Container("cavu-agent-r1").CopyOut(t.Context(), "/handoff/x", io.Discard))
		})
	}

	t.Run("a failed tar", func(t *testing.T) {
		r, f := fakeRuntime(t, "docker")
		f.set(t, "stdout", tarOf(t, entry{tar.TypeReg, "a", "1"}))
		f.set(t, "exit", []byte("2"))

		require.Error(t, r.Container("cavu-agent-r1").CopyOut(t.Context(), "/handoff/x", io.Discard))
	})

	t.Run("a relative path", func(t *testing.T) {
		r, f := fakeRuntime(t, "docker")
		require.Error(t, r.Container("cavu-agent-r1").CopyOut(t.Context(), "handoff/x", io.Discard))
		assert.Empty(t, f.calls(t), "the runtime was called")
	})
}

type failingWriter struct{}

var errWriterFull = errors.New("writer full")

func (failingWriter) Write([]byte) (int, error) { return 0, errWriterFull }

func TestCopyOutStopsARefusedStream(t *testing.T) {
	cases := map[string]struct {
		stream []byte
		w      io.Writer
	}{
		"a directory":      {stream: tarOf(t, entry{tar.TypeDir, "handoff/", ""}), w: io.Discard},
		"a failing writer": {stream: tarOf(t, entry{tar.TypeReg, "a", strings.Repeat("x", 4096)}), w: failingWriter{}},
		"a second entry":   {stream: tarOf(t, entry{tar.TypeReg, "a", "1"}, entry{tar.TypeReg, "b", "2"}), w: io.Discard},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r, f := fakeRuntime(t, "docker")
			f.set(t, "stdout", tc.stream)
			f.set(t, "linger", nil)

			begun := time.Now()
			err := r.Container("cavu-agent-r1").CopyOut(t.Context(), "/handoff/x", tc.w)
			require.Error(t, err)
			assert.Less(t, time.Since(begun), 10*time.Second, "the stream was read to its end")

			if tc.w == (failingWriter{}) {
				assert.ErrorIs(t, err, errWriterFull)
			}
		})
	}
}

func TestCopyOutKeepsTarsReason(t *testing.T) {
	const reason = "tar-reason-marker: Permission denied"

	for range 5 {
		r, f := fakeRuntime(t, "docker")
		f.set(t, "stdout", tarOf(t))
		f.set(t, "stderr", []byte(reason+"\n"))
		f.set(t, "exit", []byte("2"))

		err := r.Container("cavu-agent-r1").CopyOut(t.Context(), "/handoff/x", io.Discard)
		require.Error(t, err)
		assert.Contains(t, err.Error(), reason)
	}
}

func TestCopyOutJoinsTheClientErrorOnce(t *testing.T) {
	const reason = "tar-reason-marker: file changed as we read it"

	r, f := fakeRuntime(t, "docker")
	f.set(t, "stdout", tarOf(t, entry{tar.TypeReg, "a", "1"}))
	f.set(t, "stderr", []byte(reason+"\n"))
	f.set(t, "exit", []byte("1"))

	err := r.Container("cavu-agent-r1").CopyOut(t.Context(), "/handoff/x", io.Discard)
	require.Error(t, err)
	assert.Equal(t, 1, strings.Count(err.Error(), reason))
}
