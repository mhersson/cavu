package gitrepo_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/cavu/internal/testkit/gitrepo"
)

// Isolate hides git's repository variables from its test and hands them back
// after it.
func TestIsolateHidesRepoVariables(t *testing.T) {
	gitDir := filepath.Join(t.TempDir(), "elsewhere.git")
	t.Setenv("GIT_DIR", gitDir)

	t.Run("isolated", func(t *testing.T) {
		gitrepo.Isolate(t)

		_, set := os.LookupEnv("GIT_DIR")
		assert.False(t, set)
	})

	assert.Equal(t, gitDir, os.Getenv("GIT_DIR"))
}

// Under a git hook or `rebase --exec`, git's repository variables name another
// repository; New and Commit still act only on their own directory.
func TestIgnoresInheritedRepoVariables(t *testing.T) {
	gitrepo.Isolate(t)

	other := gitrepo.New(t, map[string]string{"kept": "x\n"})
	otherGit := filepath.Join(other, ".git")
	head := gitrepo.Git(t, other, "rev-parse", "HEAD")

	index, err := os.ReadFile(filepath.Join(otherGit, "index"))
	require.NoError(t, err)

	t.Setenv("GIT_DIR", otherGit)
	t.Setenv("GIT_WORK_TREE", other)
	t.Setenv("GIT_INDEX_FILE", filepath.Join(otherGit, "index"))

	dir := gitrepo.New(t, map[string]string{"a": "1\n"})
	second := gitrepo.Commit(t, dir, map[string]string{"b": "2\n"}, "second")

	require.DirExists(t, filepath.Join(dir, ".git"))
	assert.Equal(t, second, gitrepo.Git(t, dir, "rev-parse", "HEAD"))
	assert.Equal(t, "2", gitrepo.Git(t, dir, "rev-list", "--count", "HEAD"))
	assert.Equal(t, "a\nb", gitrepo.Git(t, dir, "ls-files"))

	assert.Equal(t, head, gitrepo.Git(t, other, "rev-parse", "HEAD"), "the other repository's HEAD")

	after, err := os.ReadFile(filepath.Join(otherGit, "index"))
	require.NoError(t, err)
	assert.Equal(t, index, after, "the other repository's index")
}
