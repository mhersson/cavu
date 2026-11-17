package runner

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/cavu/internal/engine"
	"github.com/mhersson/cavu/internal/proc"
	"github.com/mhersson/cavu/internal/sandbox"
)

const twoFiles = "diff --git a/login.go b/login.go\nindex 1..2 100644\n--- a/login.go\n+++ b/login.go\n" +
	"@@ -1 +1 @@\n-diff --git a/x b/x\n+fixed\n" +
	"diff --git a/logo.png b/logo.png\nGIT binary patch\nliteral 3\nKcmZ?wbN~PV\n\n"

// exportName is how the fake names the export of commit.
func exportName(c engine.RunConfig, commit string) string {
	return stepName(scriptArgv(exportScript, c.Repo.BaseCommit, commit, c.Repo.Branch, diffFile, bundleFile))
}

// The export runs as one script in the agent container, wrapped so it runs
// only in a sandbox, with every value as an argument, then copies both files
// out through the agent container. The script prints git's summary of the change.
func TestExportScript(t *testing.T) {
	f := newFake()
	f.out = map[string][]byte{diffFile: []byte(twoFiles), bundleFile: []byte("# v2 git bundle\n")}
	c := workConfig()
	name := exportName(c, headCommit)

	// git's summary, then what a rootless Podman client prints after an exec's own output.
	f.results[name] = sandbox.Result{Output: " login.go | 2 +-\n logo.png | Bin 0 -> 3 bytes\n" +
		" 2 files changed, 1 insertion(+), 1 deletion(-)\n" +
		`time="2026-11-16T15:00:00Z" level=warning msg="The cgroupv2 manager is set to systemd but there is no systemd user session available"` + "\n"}

	res, err := f.runner().Export(t.Context(), c, headCommit)
	require.NoError(t, err)
	assert.Equal(t, engine.ExportResult{
		HeadCommit: headCommit, Diff: []byte(twoFiles), Bundle: []byte("# v2 git bundle\n"), FilesChanged: 2,
		DiffStat: []string{"login.go | 2 +-", "logo.png | Bin 0 -> 3 bytes", "2 files changed, 1 insertion(+), 1 deletion(-)"},
	}, res)

	assert.Equal(t, []string{"volumes", "up/agent", "run/" + name, "copyout " + diffFile, "copyout " + bundleFile}, f.calls())

	run := f.run(t, name)
	assert.Equal(t, sandbox.PhaseAgent, run.phase)
	assert.Equal(t, workspace, run.cmd.Dir)
	assert.True(t, strings.HasPrefix(run.cmd.Argv[2], proc.SandboxOnly("")), "the script runs only in a sandbox")
	assert.Equal(t, []string{baseCommit, headCommit, "cavu/fix-login", diffFile, bundleFile}, run.cmd.Argv[4:])
}

func TestExportProblems(t *testing.T) {
	t.Run("no frozen commit", func(t *testing.T) {
		f := newFake()

		res, err := f.runner().Export(t.Context(), workConfig(), "")
		require.NoError(t, err)
		assert.NotEmpty(t, res.Problem)
		assert.Empty(t, f.calls())
	})

	t.Run("a commit the script refuses", func(t *testing.T) {
		f := newFake()
		c := workConfig()
		f.results[exportName(c, headCommit)] = sandbox.Result{ExitCode: 1, Output: "not a descendant\n"}

		res, err := f.runner().Export(t.Context(), c, headCommit)
		require.NoError(t, err)
		assert.Equal(t, engine.ExportResult{Problem: "not a descendant"}, res)
	})

	for _, file := range []string{diffFile, bundleFile} {
		t.Run(file+" over the bound", func(t *testing.T) {
			f := newFake()
			f.out = map[string][]byte{diffFile: []byte(twoFiles), bundleFile: []byte("# v2 git bundle\n")}
			f.out[file] = bytes.Repeat([]byte{'x'}, maxArtifact+1)

			res, err := f.runner().Export(t.Context(), workConfig(), headCommit)
			require.NoError(t, err)
			assert.NotEmpty(t, res.Problem)
			assert.Nil(t, res.Diff)
			assert.Nil(t, res.Bundle)
		})
	}
}
