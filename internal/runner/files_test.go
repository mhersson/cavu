package runner

import (
	"path"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/cavu/internal/env"
	"github.com/mhersson/cavu/internal/verify"
)

func TestParseFiles(t *testing.T) {
	files := map[string]string{
		"go.mod":       "module x\n",
		"package.json": "{}",
		"Makefile":     "",
		"My App.sln":   "\x00binary\n7 not a header\n",
	}

	got, err := parseFiles(frame(files))
	require.NoError(t, err)
	require.Len(t, got, len(files))

	for name, content := range files {
		assert.Equal(t, content, string(got[name]), name)
	}

	got, err = parseFiles(nil)
	require.NoError(t, err)
	assert.Empty(t, got)

	for name, stream := range map[string]string{
		"no newline":          "3 go.mod",
		"no name":             "3\nabc",
		"a size past the end": "9 go.mod\nabc",
		"a negative size":     "-1 go.mod\n",
		"a size not a number": "x go.mod\nabc",
	} {
		_, err := parseFiles([]byte(stream))
		assert.Error(t, err, name)
	}
}

func TestReadFiles(t *testing.T) {
	t.Run("the files at the commit", func(t *testing.T) {
		f := newFake()

		got, err := readFiles(t.Context(), fakeContainer{f: f}, baseCommit)
		require.NoError(t, err)
		assert.Equal(t, map[string][]byte{"go.mod": []byte(rootFiles()["go.mod"]), "Makefile": []byte(rootFiles()["Makefile"])}, got)
		assert.Equal(t, []string{baseCommit}, f.reads)
	})

	t.Run("too large", func(t *testing.T) {
		f := newFake()
		f.files["package.json"] = strings.Repeat("x", maxFilesSize)

		_, err := readFiles(t.Context(), fakeContainer{f: f}, baseCommit)
		require.ErrorIs(t, err, errFilesTooLarge)
	})

	t.Run("a command that fails", func(t *testing.T) {
		f := newFake()
		f.readCode = 1

		_, err := readFiles(t.Context(), fakeContainer{f: f}, baseCommit)
		require.Error(t, err)
	})

	t.Run("a commit that is not a full id", func(t *testing.T) {
		for _, commit := range []string{"", "HEAD", "--output=/tmp/x", baseCommit[:12], strings.ToUpper(baseCommit)} {
			f := newFake()

			_, err := readFiles(t.Context(), fakeContainer{f: f}, commit)
			require.Error(t, err, commit)
			assert.Empty(t, f.calls(), "nothing ran for %q", commit)
		}
	})
}

// The read covers every file detection and the verify suggestion use.
func TestFilePatterns(t *testing.T) {
	patterns := filePatterns()

	for _, name := range append(env.VersionFiles(), verify.SuggestFiles()...) {
		assert.True(t, slices.ContainsFunc(patterns, func(p string) bool {
			ok, _ := path.Match(p, name)

			return ok || p == name
		}), name)
	}
}
