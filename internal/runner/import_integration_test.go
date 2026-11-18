//go:build integration

package runner

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/cavu/internal/engine"
)

// importRun prepares a run of a repository holding notes.txt and returns its
// state, with the agent phase up to make commits and bundles in.
func importRun(t *testing.T) (*run, engine.RunConfig) {
	t.Helper()

	r, c, _ := fixtureRun(t, map[string]string{"notes.txt": "first line\n"})
	c.Verify.Waived = true

	res, err := r.Prepare(t.Context(), c)
	require.NoError(t, err)
	require.Empty(t, res.FailedStep, res.Output)

	c.Repo.BaseCommit, c.Ecosystems = res.BaseCommit, res.Ecosystems

	return r.state(c.RunID), c
}

// commit runs script in the workspace with the task branch as $1, commits
// what it changed on the branch it leaves checked out, and returns that commit.
func commit(t *testing.T, st *run, c engine.RunConfig, script string) string {
	t.Helper()

	out := runIn(t, st, "sh", "-c", script+" && git add -A && git commit -qm change && git rev-parse HEAD", "sh", c.Repo.Branch)
	require.Equal(t, 0, out.ExitCode, out.Output)

	return strings.TrimSpace(out.Output[strings.LastIndex(strings.TrimSpace(out.Output), "\n")+1:])
}

// bundleOf makes a bundle of revs, as git bundle create takes them, in the
// agent container and copies it out.
func bundleOf(t *testing.T, st *run, revs ...string) []byte {
	t.Helper()

	const at = "/tmp/test.bundle"

	out := runIn(t, st, append([]string{"git", "bundle", "create", "-q", at}, revs...)...)
	require.Equal(t, 0, out.ExitCode, out.Output)

	b, problem, err := copyOut(t.Context(), st.agent, at)
	require.NoError(t, err)
	require.Empty(t, problem)

	return b
}

// branchBundle is a bundle of the task branch from the base commit.
func branchBundle(t *testing.T, st *run, c engine.RunConfig) []byte {
	t.Helper()

	return bundleOf(t, st, c.Repo.BaseCommit+"..refs/heads/"+c.Repo.Branch)
}

// mirrorRefs lists the refs of the project's mirror, read in an import container.
func mirrorRefs(t *testing.T, st *run) []string {
	t.Helper()

	imp, err := st.sb.Importer(t.Context())
	require.NoError(t, err)

	defer func() { assert.NoError(t, st.sb.RemoveImporter(context.Background())) }()

	out, err := imp.Run(t.Context(), importCommand("git for-each-ref --format='ref %(objectname) %(refname)'"), time.Minute)
	require.NoError(t, err)
	require.Equal(t, 0, out.ExitCode, out.Output)

	var refs []string

	// A rootless Podman client can add warnings of its own.
	for l := range strings.Lines(out.Output) {
		if r, ok := strings.CutPrefix(strings.TrimSpace(l), "ref "); ok {
			refs = append(refs, r)
		}
	}

	return refs
}

// taken imports a change of the task branch, which the import takes, and
// returns its head.
func taken(t *testing.T, st *run, c engine.RunConfig) string {
	t.Helper()

	head := commit(t, st, c, "echo CAVU-TAKEN >> notes.txt")

	res, problem, err := st.change(t.Context(), c, branchBundle(t, st, c), head)
	require.NoError(t, err)
	require.Empty(t, problem)
	require.Contains(t, string(res.Diff), "\n+CAVU-TAKEN\n")
	require.Contains(t, mirrorRefs(t, st), head+" "+taskRef(c.TaskID))

	return head
}

// requireRefused imports bundle as the change to head and wants a problem that
// names each of names, no change, and every ref of the mirror where it was.
func requireRefused(t *testing.T, st *run, c engine.RunConfig, bundle []byte, head string, names ...string) {
	t.Helper()

	before := mirrorRefs(t, st)

	res, problem, err := st.change(t.Context(), c, bundle, head)
	require.NoError(t, err)
	require.NotEmpty(t, problem)

	for _, n := range names {
		assert.Contains(t, problem, n)
	}

	assert.Equal(t, engine.ExportResult{}, res, "no change comes with a refusal")
	assert.Equal(t, before, mirrorRefs(t, st), "the refusal moved a ref")
}

func TestImportRefusesAHeadTheGateDidNotTest(t *testing.T) {
	st, c := importRun(t)
	taken(t, st, c)

	tested := commit(t, st, c, "echo CAVU-TESTED >> notes.txt")
	later := commit(t, st, c, "echo CAVU-LATER >> notes.txt")

	requireRefused(t, st, c, branchBundle(t, st, c), tested, tested, later)
}

func TestImportRefusesABranchThatDoesNotDescend(t *testing.T) {
	st, c := importRun(t)
	taken(t, st, c)

	lone := commit(t, st, c, `git checkout -q --orphan lone && echo CAVU-LONE > lone.txt`)
	git := runIn(t, st, "git", "update-ref", "refs/heads/"+c.Repo.Branch, lone)
	require.Equal(t, 0, git.ExitCode, git.Output)

	requireRefused(t, st, c, branchBundle(t, st, c), lone, lone, c.Repo.BaseCommit)
}

func TestImportRefusesAnotherRef(t *testing.T) {
	st, c := importRun(t)
	taken(t, st, c)

	head := commit(t, st, c, "echo CAVU-HEAD >> notes.txt && git branch other")

	requireRefused(t, st, c, bundleOf(t, st, "^"+c.Repo.BaseCommit, "refs/heads/"+c.Repo.Branch, "refs/heads/other"), head, "refs/heads/other")
}

// overwriteObject overwrites the loose object of the change's a.txt, made
// writable first, with the bytes of b.txt's.
const overwriteObject = `o() { h=$(git rev-parse "HEAD:$1") && echo ".git/objects/$(echo "$h" | cut -c1-2)/$(echo "$h" | cut -c3-)"; }
a=$(o a.txt) && b=$(o b.txt) && chmod u+w "$a" && cat "$b" > "$a"`

func TestImportRefusesACorruptObject(t *testing.T) {
	st, c := importRun(t)
	taken(t, st, c)

	head := commit(t, st, c, "echo CAVU-A > a.txt && echo CAVU-B > b.txt")

	out := runIn(t, st, "sh", "-c", overwriteObject)
	require.Equal(t, 0, out.ExitCode, out.Output)

	requireRefused(t, st, c, branchBundle(t, st, c), head)
}

func TestImportTwiceChangesNothing(t *testing.T) {
	st, c := importRun(t)

	head := commit(t, st, c, "git mv notes.txt moved.txt && echo CAVU-NEW > 'new é.txt'")
	bundle := branchBundle(t, st, c)
	before := mirrorRefs(t, st)

	first, problem, err := st.change(t.Context(), c, bundle, head)
	require.NoError(t, err)
	require.Empty(t, problem)
	assert.Equal(t, []engine.ChangedFile{
		{Path: "moved.txt", Added: true, Size: 11}, {Path: "new é.txt", Added: true, Size: 9}, {Path: "notes.txt"},
	}, first.Files, "a rename is a delete and an add")

	refs := mirrorRefs(t, st)
	assert.ElementsMatch(t, append(before, head+" "+taskRef(c.TaskID)), refs, "the import writes the task's ref and no other")

	second, problem, err := st.change(t.Context(), c, bundle, head)
	require.NoError(t, err)
	require.Empty(t, problem)

	assert.Equal(t, first, second)
	assert.Equal(t, refs, mirrorRefs(t, st))
}
