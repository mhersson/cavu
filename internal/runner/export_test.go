package runner

import (
	"bytes"
	"context"
	"errors"
	"slices"
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

// twoFilesListed is the import's list of twoFiles' paths.
const twoFilesListed = "6 :100644 100644 1111111111111111111111111111111111111111 2222222222222222222222222222222222222222 M\tlogin.go\n" +
	"3 :000000 100644 0000000000000000000000000000000000000000 3333333333333333333333333333333333333333 A\tlogo.png\n"

const exportedBundle = "# v2 git bundle\n"

// exportName is how the fake names the export of commit in the agent container.
func exportName(c engine.RunConfig, commit string) string {
	return stepName(scriptArgv(exportScript, c.Repo.BaseCommit, commit, c.Repo.Branch, bundleFile))
}

// importName is how the fake names the import of a bundle of the task branch at head.
func importName(c engine.RunConfig, head string) string {
	return stepName(scriptArgv(importScript, tmpfsBundle, c.Repo.Branch, head, c.Repo.BaseCommit, "refs/cavu/tasks/5"))
}

// changeName is how the fake names the read of the change to head from the mirror.
func changeName(c engine.RunConfig, head string) string {
	return stepName(scriptArgv(changeScript, c.Repo.BaseCommit, head, changeDiff, changeFiles))
}

// exported is a fake whose export goes through, its change two files.
func exported() *fakeSandbox {
	f := newFake()
	f.out = map[string][]byte{bundleFile: []byte(exportedBundle), changeDiff: []byte(twoFiles), changeFiles: []byte(twoFilesListed)}

	return f
}

// The agent container makes only the bundle, which is copied out through it.
// Everything the result holds but the bundle comes from the import of that
// bundle into the project's mirror, in the import container, which is removed
// at the end. Every script runs only in a sandbox, with every value as an argument.
func TestExportImportsTheBundle(t *testing.T) {
	f := exported()
	c := workConfig()

	// git's summary, then what a rootless Podman client prints after an exec's own output.
	f.results[changeName(c, headCommit)] = sandbox.Result{Output: " login.go | 2 +-\n logo.png | Bin 0 -> 3 bytes\n" +
		" 2 files changed, 1 insertion(+), 1 deletion(-)\n" +
		`time="2026-11-16T15:00:00Z" level=warning msg="The cgroupv2 manager is set to systemd but there is no systemd user session available"` + "\n"}

	res, err := f.runner().Export(t.Context(), c, headCommit)
	require.NoError(t, err)
	assert.Equal(t, engine.ExportResult{
		HeadCommit: headCommit, Diff: []byte(twoFiles), Bundle: []byte(exportedBundle), FilesChanged: 2,
		DiffStat: []string{"login.go | 2 +-", "logo.png | Bin 0 -> 3 bytes", "2 files changed, 1 insertion(+), 1 deletion(-)"},
		Files:    []engine.ChangedFile{{Path: "login.go", Size: 6}, {Path: "logo.png", Added: true, Size: 3}},
	}, res)

	assert.Equal(t, []string{
		"volumes", "up/agent", goSeed, "run/" + exportName(c, headCommit), "copyout " + bundleFile,
		"importer", "import/copy", "import/run/" + importName(c, headCommit), "import/run/" + changeName(c, headCommit),
		"import/copyout " + changeFiles, "import/copyout " + changeDiff, "importer removed",
	}, f.calls())

	agent := f.runsIn(sandbox.PhaseAgent)
	require.Len(t, agent, 2, "the agent container seeds its home, then computes no diff")
	assert.Equal(t, workspace, agent[1].cmd.Dir)
	assert.Equal(t, []string{baseCommit, headCommit, "cavu/fix-login", bundleFile}, agent[1].cmd.Argv[4:])
	assert.Equal(t, map[string][]byte{tmpfsBundle: []byte(exportedBundle)}, f.imported, "the import takes the bundle the agent container made")

	for _, r := range slices.Concat(agent, f.runsIn(phaseImport)) {
		assert.True(t, strings.HasPrefix(r.cmd.Argv[2], proc.SandboxOnly("")), "the script runs only in a sandbox: %s", r.name)
	}

	assertImportCommands(t, f)
}

// assertImportCommands wants every command of the import container run in
// the mirror, under no git configuration but the mirror's.
func assertImportCommands(t *testing.T, f *fakeSandbox) {
	t.Helper()

	runs := f.runsIn(phaseImport)
	require.NotEmpty(t, runs)

	for _, r := range runs {
		assert.Equal(t, "/mirror", r.cmd.Dir, r.name)
		assert.Equal(t, []string{"HOME=/run/cavu", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null"}, r.cmd.Env, "no configuration but the mirror's: %s", r.name)
	}
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
		f := exported()
		c := workConfig()
		f.results[exportName(c, headCommit)] = sandbox.Result{ExitCode: 1, Output: "not a descendant\n"}

		res, err := f.runner().Export(t.Context(), c, headCommit)
		require.NoError(t, err)
		assert.Equal(t, engine.ExportResult{Problem: "not a descendant"}, res)
		assert.NotContains(t, f.calls(), "importer")
	})

	for name, step := range map[string]func(engine.RunConfig) string{
		"a bundle the import refuses": func(c engine.RunConfig) string { return importName(c, headCommit) },
		"a change git cannot read":    func(c engine.RunConfig) string { return changeName(c, headCommit) },
	} {
		t.Run(name, func(t *testing.T) {
			f := exported()
			c := workConfig()
			f.results[step(c)] = sandbox.Result{ExitCode: 1, Output: "refused " + plantedSetup + "\n"}

			res, err := f.runner().Export(t.Context(), c, headCommit)
			require.NoError(t, err)
			assert.Equal(t, engine.ExportResult{Problem: masker(c)("refused " + plantedSetup)}, res)

			calls := f.calls()
			assert.Equal(t, "importer removed", calls[len(calls)-1])
			assert.NotContains(t, calls, "import/copyout "+changeDiff, "no diff comes with a refusal")
		})
	}

	// git refuses a quiet fetch without a word, and a killed command says nothing either.
	t.Run("a step that fails with no output", func(t *testing.T) {
		f := exported()
		c := workConfig()
		f.results[changeName(c, headCommit)] = sandbox.Result{ExitCode: 137}

		res, err := f.runner().Export(t.Context(), c, headCommit)
		require.NoError(t, err)
		assert.NotEmpty(t, res.Problem)
		assert.Nil(t, res.Diff)
	})

	for _, file := range []string{bundleFile, changeDiff, changeFiles} {
		t.Run(file+" over the bound", func(t *testing.T) {
			f := exported()
			f.out[file] = bytes.Repeat([]byte{'x'}, maxArtifact+1)

			res, err := f.runner().Export(t.Context(), workConfig(), headCommit)
			require.NoError(t, err)
			assert.NotEmpty(t, res.Problem)
			assert.Nil(t, res.Diff)
			assert.Nil(t, res.Bundle)
		})
	}
}

// The import container is removed when the import fails and when the
// action's context ends during it.
func TestExportRemovesTheImporter(t *testing.T) {
	t.Run("a runtime error", func(t *testing.T) {
		f := exported()
		c := workConfig()
		f.runErrs[importName(c, headCommit)] = errors.New("exec failed")

		_, err := f.runner().Export(t.Context(), c, headCommit)
		require.Error(t, err)

		calls := f.calls()
		assert.Equal(t, "importer removed", calls[len(calls)-1])
	})

	t.Run("a cancelled context", func(t *testing.T) {
		f := exported()
		c := workConfig()
		ctx, cancel := context.WithCancel(t.Context())

		f.onRun = func(name string) {
			if name == importName(c, headCommit) {
				cancel()
			}
		}

		_, err := f.runner().Export(ctx, c, headCommit)
		require.ErrorIs(t, err, context.Canceled)

		calls := f.calls()
		assert.Equal(t, "importer removed", calls[len(calls)-1])
	})
}

// The import lists each path once, a rename as a delete and an add, with the
// size of the new blob: 0 for a deleted path, which git calls missing.
func TestChangedPaths(t *testing.T) {
	const zero = "0000000000000000000000000000000000000000"

	list := "12 :100644 100644 1111111111111111111111111111111111111111 2222222222222222222222222222222222222222 M\tsrc/login.go\n" +
		zero + " missing :100644 000000 3333333333333333333333333333333333333333 " + zero + " D\told name.txt\n" +
		"2097152 :000000 100644 " + zero + " 3333333333333333333333333333333333333333 A\tnew name.txt\n" +
		"4 :000000 100644 " + zero + " 4444444444444444444444444444444444444444 A\t\"tab\\there \\303\\251.txt\"\n"

	files, err := changedPaths([]byte(list))
	require.NoError(t, err)
	assert.Equal(t, []engine.ChangedFile{
		{Path: "src/login.go", Size: 12},
		{Path: "old name.txt"},
		{Path: "new name.txt", Added: true, Size: 2 << 20},
		{Path: "tab\there é.txt", Added: true, Size: 4},
	}, files)

	files, err = changedPaths(nil)
	require.NoError(t, err)
	assert.Empty(t, files)
}
