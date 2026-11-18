package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"

	"github.com/mhersson/cavu/internal/engine"
	"github.com/mhersson/cavu/internal/proc"
	"github.com/mhersson/cavu/internal/sandbox"
)

const (
	bundleFile = "/handoff/task.bundle"
	// The change the import container writes for the export.
	changeDiff  = "/run/cavu/diff.patch"
	changeFiles = "/run/cavu/files"
	// maxArtifact bounds each file copied out of a container.
	maxArtifact = 64 << 20
	// maxStatFiles bounds the files git names in the change's summary; its last line still counts them all.
	maxStatFiles = 50
)

var errArtifactTooLarge = errors.New("the file is over the bound")

// exportScript points the task branch $3 at the frozen commit $2 and writes a
// bundle of the branch from the base commit $1 to $4. It refuses a commit that
// is missing or does not descend from the base. The working tree and HEAD are not read.
var exportScript = proc.SandboxOnly(gitFunc + `g merge-base --is-ancestor "$1" "$2" 2>/dev/null ||
	{ echo "The frozen commit $2 is missing from the repository or does not descend from the base commit."; exit 1; }
g update-ref "refs/heads/$3" "$2" && g bundle create -q "$4" "$1..refs/heads/$3"`)

// changeScript writes the diff from the base commit $1 to the head $2 to $3,
// and to $4 a line for each path the change adds, modifies or deletes, a
// rename as both: the size of the path's new blob, or git's "missing" for a
// deleted one, then git's raw line for it. It prints git's summary of the diff.
var changeScript = proc.SandboxOnly(`git diff --binary "$1" "$2" >"$3" &&
git diff --no-renames --raw --no-abbrev "$1" "$2" >"$4.raw" &&
cut -d' ' -f4 "$4.raw" | git cat-file --batch-check='%(objectsize)' | paste -d' ' - "$4.raw" >"$4" &&
git diff --stat --stat-count=` + strconv.Itoa(maxStatFiles) + ` "$1" "$2"`)

// Export exports commit, the work a session froze: it points the task branch
// at it, makes a bundle of the branch, and returns the bundle and the change
// as the import of the bundle reads it from the project's mirror.
func (r *Runner) Export(ctx context.Context, c engine.RunConfig, commit string) (engine.ExportResult, error) {
	if !commitID.MatchString(commit) {
		return engine.ExportResult{Problem: "No session froze its work into a commit, so there is nothing to export."}, nil
	}

	return act(ctx, r, c, sandbox.PhaseAgent, func(ctx context.Context, st *run, mask func(string) string) (engine.ExportResult, error) {
		if err := st.restartAgent(ctx); err != nil {
			return engine.ExportResult{}, err
		}

		cmd := proc.Command{
			Argv: scriptArgv(exportScript, c.Repo.BaseCommit, commit, c.Repo.Branch, bundleFile),
			Env:  agentEnv(st.ecosystems, c.Repo.BaseCommit), Dir: workspace,
		}

		_, problem, err := gitStep(ctx, st.agent, cmd)
		if err != nil || problem != "" {
			return engine.ExportResult{Problem: mask(problem)}, err
		}

		bundle, problem, err := copyOut(ctx, st.agent, bundleFile)
		if err != nil || problem != "" {
			return engine.ExportResult{Problem: problem}, err
		}

		res, problem, err := st.change(ctx, c, bundle, commit)
		if err != nil || problem != "" {
			return engine.ExportResult{Problem: mask(problem)}, err
		}

		return res, nil
	})
}

// change imports bundle, whose task branch is at head, and reads from the
// mirror the change from the base commit to head.
func (st *run) change(ctx context.Context, c engine.RunConfig, bundle []byte, head string) (engine.ExportResult, string, error) {
	res := engine.ExportResult{HeadCommit: head, Bundle: bundle}

	problem, err := st.imported(ctx, c, bundle, head, c.Repo.BaseCommit, func(imp container) (string, error) {
		out, problem, err := gitStep(ctx, imp, importCommand(changeScript, c.Repo.BaseCommit, head, changeDiff, changeFiles))
		if err != nil || problem != "" {
			return problem, err
		}

		if res.Diff, problem, err = copyOut(ctx, imp, changeDiff); err != nil || problem != "" {
			return problem, err
		}

		list, problem, err := copyOut(ctx, imp, changeFiles)
		if err != nil || problem != "" {
			return problem, err
		}

		res.FilesChanged, res.DiffStat = changedFiles(res.Diff), diffStat(out)
		res.Files, err = changedPaths(list)

		return "", err
	})
	if err != nil || problem != "" {
		return engine.ExportResult{}, problem, err
	}

	return res, "", nil
}

// changedPaths reads changeScript's list of the paths a change touches.
func changedPaths(list []byte) ([]engine.ChangedFile, error) {
	var files []engine.ChangedFile

	for l := range strings.Lines(string(list)) {
		size, raw, ok := strings.Cut(strings.TrimSuffix(l, "\n"), " :")
		meta, name, tab := strings.Cut(raw, "\t")
		fields := strings.Fields(meta)

		if !ok || !tab || len(fields) != 5 {
			return nil, fmt.Errorf("runner: the import listed a changed path as %q", l)
		}

		// git quotes a name that holds a control character, a quote, a backslash or a
		// byte above 0x7f, with the escapes of a Go string literal.
		if strings.HasPrefix(name, `"`) {
			unquoted, err := strconv.Unquote(name)
			if err != nil {
				return nil, fmt.Errorf("runner: the import listed a changed path as %q: %w", l, err)
			}

			name = unquoted
		}

		n, _ := strconv.ParseInt(size, 10, 64)
		files = append(files, engine.ChangedFile{Path: name, Added: fields[4] == "A", Size: n})
	}

	return files, nil
}

// diffStat is the lines of git's summary in out, which git indents by one
// space: the runtime client's own warnings can follow a command's output.
func diffStat(out string) []string {
	var stat []string

	for l := range strings.Lines(out) {
		if s, ok := strings.CutPrefix(strings.TrimSuffix(l, "\n"), " "); ok {
			stat = append(stat, s)
		}
	}

	return stat
}

// copyOut copies file out of a container through that container: a file of
// the agent container never goes through the sidecar, which shares the
// hand-off volume. A file over the bound is a problem, not an error.
func copyOut(ctx context.Context, a container, file string) ([]byte, string, error) {
	b := &boundedBuffer{}

	err := a.CopyOut(ctx, file, b)
	if errors.Is(err, errArtifactTooLarge) {
		return nil, fmt.Sprintf("The file %s is larger than %d MiB.", path.Base(file), maxArtifact>>20), nil
	}

	return b.buf.Bytes(), "", err
}

// boundedBuffer fails a write that would take it past maxArtifact, which
// stops CopyOut's client at once rather than reading the rest.
type boundedBuffer struct{ buf bytes.Buffer }

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if b.buf.Len()+len(p) > maxArtifact {
		return 0, errArtifactTooLarge
	}

	return b.buf.Write(p)
}

// changedFiles counts a git diff's file headers: no other line of a diff
// starts with "diff --git ".
func changedFiles(diff []byte) int {
	n := bytes.Count(diff, []byte("\ndiff --git "))
	if bytes.HasPrefix(diff, []byte("diff --git ")) {
		n++
	}

	return n
}
