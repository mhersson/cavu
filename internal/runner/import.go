package runner

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/mhersson/cavu/internal/engine"
	"github.com/mhersson/cavu/internal/proc"
)

// tmpfsBundle is a bundle on a container's tmpfs: the review's, made in the
// read-only container, and the one the import container takes.
const tmpfsBundle = "/run/cavu/task.bundle"

// importScript takes the bundle $1 into the mirror. It refuses a bundle that
// holds any ref but the task branch $2, or holds it at another commit than $3;
// fetches its objects with git's object checks and no ref to write; refuses a
// head that does not descend from $4; and only then points the task's ref $5
// at the head. A refused bundle leaves every ref as it was. The fetch starts
// no maintenance, which would die with the container and leave its locks.
var importScript = proc.SandboxOnly(`g() { git -c core.hooksPath=/dev/null "$@"; }
heads=$(g bundle list-heads "$1") || exit 1
at=${heads%% *}
[ "$heads" = "$at refs/heads/$2" ] || { printf 'The bundle holds refs other than the task branch refs/heads/%s:\n%s\n' "$2" "$heads"; exit 1; }
[ "$at" = "$3" ] || { echo "The bundle holds the task branch at $at, not at $3, the commit to import."; exit 1; }
g -c fetch.fsckObjects=true fetch -q --no-tags --no-write-fetch-head --no-auto-maintenance "$1" "refs/heads/$2" || exit 1
g merge-base --is-ancestor "$4" "$3" || { echo "The bundle's head $3 does not descend from $4."; exit 1; }
g update-ref "$5" "$3"`)

// taskRef is the ref of the project's mirror that holds a task's newest imported head.
func taskRef(taskID int64) string { return "refs/cavu/tasks/" + strconv.FormatInt(taskID, 10) }

// importCommand runs script with args in the import container, in the mirror,
// under no git configuration but the mirror's own, which only CAVU writes.
func importCommand(script string, args ...string) proc.Command {
	return proc.Command{
		Argv: scriptArgv(script, args...),
		Env:  []string{"HOME=/run/cavu", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null"},
		Dir:  "/mirror",
	}
}

// imported copies bundle into a fresh import container and takes it into the
// project's mirror as the task's newest head, head, which must descend from
// prev; then it runs read there. A bundle the import refuses is a problem, and
// read does not run. The container is removed at the end, also on an error and
// on a cancelled ctx.
func (st *run) imported(ctx context.Context, c engine.RunConfig, bundle []byte, head, prev string,
	read func(imp container) (string, error),
) (problem string, err error) {
	imp, err := st.sb.Importer(ctx)
	defer func() { err = errors.Join(err, st.sb.RemoveImporter(ctx)) }()

	if err != nil {
		return "", err
	}

	if err := imp.CopyIn(ctx, map[string][]byte{tmpfsBundle: bundle}); err != nil {
		return "", err
	}

	release, err := st.r.takeTurn(ctx, c.ProjectID, mirrorTurn)
	if err != nil {
		return "", err
	}

	_, problem, err = gitStep(ctx, imp, importCommand(importScript, tmpfsBundle, c.Repo.Branch, head, prev, taskRef(c.TaskID)))

	// read writes nothing into the mirror: the turn ends with the import.
	release()

	if err != nil || problem != "" {
		return problem, err
	}

	return read(imp)
}

// gitStep runs cmd in a and returns its output. A command that fails or runs
// past gitTimeout gives its output as a problem, not an error; one that fails
// silently, as a quiet fetch git refuses or a killed command does, gives its status.
func gitStep(ctx context.Context, a container, cmd proc.Command) (out, problem string, err error) {
	res, err := a.Run(ctx, cmd, gitTimeout)
	if err != nil {
		return "", "", err
	}

	if res.ExitCode != 0 || res.TimedOut {
		return "", cmp.Or(strings.TrimSpace(output(res, gitTimeout)), fmt.Sprintf("git exited %d with no output.", res.ExitCode)), nil
	}

	return res.Output, "", nil
}
