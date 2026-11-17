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
)

const (
	diffFile   = "/handoff/diff.patch"
	bundleFile = "/handoff/task.bundle"
	// maxArtifact bounds each file the export copies out of the container.
	maxArtifact = 64 << 20
	// maxStatFiles bounds the files git names in the change's summary; its last line still counts them all.
	maxStatFiles = 50
)

var errArtifactTooLarge = errors.New("the file is over the export's bound")

// exportScript points the task branch $3 at the frozen commit $2, writes the
// diff from the base commit $1 to $4 and a bundle of the branch from the base
// to $5, and prints git's summary of the diff. It refuses a commit that is
// missing or does not descend from the base. The working tree and HEAD are not read.
var exportScript = proc.SandboxOnly(gitFunc + `g merge-base --is-ancestor "$1" "$2" 2>/dev/null ||
	{ echo "The frozen commit $2 is missing from the repository or does not descend from the base commit."; exit 1; }
g update-ref "refs/heads/$3" "$2" && g diff --binary "$1" "$2" >"$4" && g bundle create -q "$5" "$1..$3" &&
g diff --stat --stat-count=` + strconv.Itoa(maxStatFiles) + ` "$1" "$2"`)

// Export exports commit, the work a session froze: it points the task branch
// at it and returns the diff from the base commit and a bundle of the branch.
func (r *Runner) Export(ctx context.Context, c engine.RunConfig, commit string) (engine.ExportResult, error) {
	if !commitID.MatchString(commit) {
		return engine.ExportResult{Problem: "No session froze its work into a commit, so there is nothing to export."}, nil
	}

	return act(ctx, r, c, func(ctx context.Context, st *run, mask func(string) string) (engine.ExportResult, error) {
		cmd := proc.Command{
			Argv: scriptArgv(exportScript, c.Repo.BaseCommit, commit, c.Repo.Branch, diffFile, bundleFile),
			Env:  agentEnv(st.ecosystems), Dir: workspace,
		}

		out, err := st.agent.Run(ctx, cmd, gitTimeout)
		if err != nil {
			return engine.ExportResult{}, err
		}

		if out.ExitCode != 0 || out.TimedOut {
			return engine.ExportResult{Problem: mask(strings.TrimSpace(output(out, gitTimeout)))}, nil
		}

		diff, problem, err := copyOut(ctx, st.agent, diffFile)
		if err != nil || problem != "" {
			return engine.ExportResult{Problem: problem}, err
		}

		bundle, problem, err := copyOut(ctx, st.agent, bundleFile)
		if err != nil || problem != "" {
			return engine.ExportResult{Problem: problem}, err
		}

		return engine.ExportResult{
			HeadCommit: commit, Diff: diff, Bundle: bundle, FilesChanged: changedFiles(diff), DiffStat: diffStat(out.Output),
		}, nil
	})
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

// copyOut copies file out of the agent container through the agent container,
// never the sidecar, which shares the hand-off volume. A file over the bound
// is a problem, not an error.
func copyOut(ctx context.Context, a container, file string) ([]byte, string, error) {
	b := &boundedBuffer{}

	err := a.CopyOut(ctx, file, b)
	if errors.Is(err, errArtifactTooLarge) {
		return nil, fmt.Sprintf("The exported %s is larger than %d MiB.", path.Base(file), maxArtifact>>20), nil
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
