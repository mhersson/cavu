package git

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/cavu/internal/testkit/gitrepo"
)

func TestMain(m *testing.M) { gitrepo.Main(m) }

// childDirVar names the directory the re-run test binary below works in.
const childDirVar = "CAVU_TEST_CHILD_DIR"

// A hook, `rebase --exec` or `bisect run` hands the test binary a GIT_DIR naming
// the operator's repository. TestMain drops it before any test runs, so git
// started by run acts on its own directory. The test re-runs its own binary
// under a GIT_DIR naming a repository it made, so a failed guard harms only that.
func TestInheritedGitDirDoesNotReachRun(t *testing.T) {
	t.Parallel()

	if dir := os.Getenv(childDirVar); dir != "" {
		_, err := run(context.Background(), call{dir: dir, args: []string{"init", "--quiet"}})
		require.NoError(t, err)

		out, err := run(context.Background(), call{dir: dir, args: []string{"rev-parse", "--absolute-git-dir"}, read: true})
		require.NoError(t, err)
		assert.Equal(t, filepath.Join(dir, ".git"), strings.TrimSpace(string(out)))

		return
	}

	decoy := gitrepo.New(t, map[string]string{"f": "x\n"})
	head := gitrepo.Git(t, decoy, "rev-parse", "HEAD")

	dir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)

	child := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^"+t.Name()+"$", "-test.count=1") //nolint:gosec // re-runs this test binary

	child.Env = append(os.Environ(), childDirVar+"="+dir, "GIT_DIR="+filepath.Join(decoy, ".git"), "GIT_WORK_TREE="+decoy)

	out, err := child.CombinedOutput()
	require.NoError(t, err, "%s", out)

	assert.FileExists(t, filepath.Join(dir, ".git", "HEAD"), "the child's repository")
	assert.Equal(t, head, gitrepo.Git(t, decoy, "rev-parse", "HEAD"), "the decoy's HEAD")
}
