package claudecode

import (
	"bytes"
	"context"
	_ "embed" // the staged text
	"errors"
	"fmt"
	"io"
	"path"
	"slices"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/mhersson/cavu/internal/driver"
	"github.com/mhersson/cavu/internal/proc"
)

// The pinned release reads a repository's instruction files only with the
// project settings source, which also applies the repository's env, permission
// rules and hooks. So the driver stages them from the base commit into the
// config dir, which the user source reads. Everything else the user source
// would load from there is cleared before every session, so nothing a session
// writes into the task's home reaches the next one but its transcripts. The
// clear and the write each run behind proc.SandboxOnly and do nothing outside
// a sandbox container.
const (
	// keptEntry is the one config-dir entry a session keeps: the transcripts,
	// which a resume needs, and the auto memory beside them.
	keptEntry = "projects"

	stageTimeout = time.Minute
	// stageLimit caps the root files read and the rules and skills unpacked.
	stageLimit = 16 << 20

	// A nested file is listed only this many directories deep, and the list
	// holds at most nestedMaxFiles, so a monorepo stays short.
	nestedMaxDepth = 4
	nestedMaxFiles = 24
)

var (
	//go:embed staged-instructions.tmpl
	stagedText     string
	stagedTemplate = template.Must(template.New("staged").Parse(stagedText))
)

// memoryNames are the root files staged into one CLAUDE.md, in this order;
// archiveNames are unpacked into the config dir without their .claude/.
var (
	memoryNames  = []string{"CLAUDE.md", ".claude/CLAUDE.md", "AGENTS.md"}
	archiveNames = []string{".claude/rules", ".claude/skills"}
)

// clearScript empties the config dir $1 but for keptEntry. It leaves a missing
// dir alone, refuses one that is a symlink or resolves elsewhere, and deletes
// only by relative name inside the dir it checked it is in. It runs only
// through proc.SandboxOnly, with the dir as its argument, never in its text.
const clearScript = `d=$1
[ -e "$d" ] || [ -L "$d" ] || exit 0
cd -P -- "$d" || exit 1
[ "$(pwd -P)" = "$d" ] || { echo "cavu: the config dir resolves elsewhere; nothing deleted" >&2; exit 1; }
find . -mindepth 1 -maxdepth 1 ! -name ` + keptEntry + ` -exec rm -rf -- {} +`

// writeScript writes its stdin to CLAUDE.md when $4 is set, and unpacks the
// paths after $4 from commit $3 of the repository $2 without their .claude/,
// into the config dir $1 under the clear's checks. It runs under bash for
// pipefail: tar takes an empty or cut stream from a failed git archive as whole.
var writeScript = `set -o pipefail
d=$1 repo=$2 commit=$3 memory=$4
shift 4
mkdir -p -- "$d" && cd -P -- "$d" || exit 1
[ "$(pwd -P)" = "$d" ] || { echo "cavu: the config dir resolves elsewhere; nothing written" >&2; exit 1; }
if [ -n "$memory" ]; then cat > CLAUDE.md || exit 1; fi
[ $# -eq 0 ] || git -C "$repo" archive "$commit" -- "$@" | head -c ` + strconv.Itoa(stageLimit) + ` | tar -x -f - --strip-components=1 ||
	{ echo "cavu: .claude/rules and .claude/skills did not unpack; together they may hold at most ` + strconv.Itoa(stageLimit) + ` bytes" >&2; exit 1; }`

// nestedScript lists the files of commit $1 named AGENTS.md or CLAUDE.md below
// the root; grep exits 1 when there is none.
const nestedScript = `set -o pipefail
git ls-tree -r -z --name-only "$1" | { grep -z -E '/(AGENTS|CLAUDE)\.md$' || [ $? -eq 1 ]; }`

// errNotInSandbox is a clear or write that proc.SandboxOnly refused: the agent
// container runs an image built before the marker.
var errNotInSandbox = errors.New("the sandbox image lacks " + proc.SandboxMarker + "; rebuild it with cavu image build")

// stageEnv keeps git to the stored objects: no replace objects, no global or
// system config and no fetch from a promisor remote. The repository's own
// config and object store stay the agent's.
func stageEnv(s driver.Spec) []string {
	return proc.BuildEnv(nil, nil, map[string]string{
		"PATH": basePath, "HOME": s.Home, "LC_ALL": "C",
		"GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": "/dev/null", "GIT_NO_REPLACE_OBJECTS": "1", "GIT_NO_LAZY_FETCH": "1",
	})
}

// memorySection is one blob of the staged CLAUDE.md and the names it goes by.
type memorySection struct {
	Names   []string
	Content string
}

// memoryFile is the staged CLAUDE.md.
type memoryFile struct {
	Sections []memorySection
	Nested   []string
}

// render returns nil for a file with nothing in it. The template and its data
// are fixed, so only a broken template fails, which the unit tests catch.
func (m memoryFile) render() []byte {
	if len(m.Sections) == 0 && len(m.Nested) == 0 {
		return nil
	}

	var b bytes.Buffer
	if err := stagedTemplate.ExecuteTemplate(&b, "memory", m); err != nil {
		panic(err)
	}

	return b.Bytes()
}

// prepare clears s.ConfigDir but for keptEntry and, when s names a base
// commit, stages its instruction files there. Every command runs in the
// target, and Run has validated the paths they take.
func (c claudeCode) prepare(ctx context.Context, t proc.Target, s driver.Spec) error {
	ctx, cancel := context.WithTimeout(ctx, stageTimeout)
	defer cancel()

	if err := c.sandboxed(ctx, t, s, "sh", clearScript, nil, s.ConfigDir); err != nil {
		return fmt.Errorf("clear the config dir: %w", err)
	}

	if s.BaseCommit == "" {
		return nil
	}

	listing, err := c.capture(ctx, t, s, slices.Concat([]string{"git", "ls-tree", "-z", s.BaseCommit, "--"}, memoryNames, archiveNames), nil)
	if err != nil {
		return fmt.Errorf("list %s: %w", s.BaseCommit, err)
	}

	types := map[string]string{}

	for rec := range strings.SplitSeq(string(listing), "\x00") {
		if meta, p, ok := strings.Cut(rec, "\t"); ok {
			if f := strings.Fields(meta); len(f) == 3 {
				types[p] = f[1]
			}
		}
	}

	nested, err := c.capture(ctx, t, s, []string{"bash", "-c", nestedScript, "bash", s.BaseCommit}, nil)
	if err != nil {
		return fmt.Errorf("list the nested instruction files of %s: %w", s.BaseCommit, err)
	}

	m := memoryFile{Nested: filterNested(strings.FieldsFunc(string(nested), func(r rune) bool { return r == 0 }))}

	var roots, trees []string

	for _, name := range memoryNames {
		if types[name] == "blob" {
			roots = append(roots, name)
		}
	}

	for _, name := range archiveNames {
		if types[name] == "tree" {
			trees = append(trees, name)
		}
	}

	if m.Sections, err = c.readRoots(ctx, t, s, roots); err != nil {
		return err
	}

	memory := m.render()
	if memory == nil && len(trees) == 0 {
		return nil
	}

	write := ""
	if memory != nil {
		write = "1"
	}

	if err := c.sandboxed(ctx, t, s, "bash", writeScript, memory, append([]string{s.ConfigDir, s.Dir, s.BaseCommit, write}, trees...)...); err != nil {
		return fmt.Errorf("write the instruction files: %w", err)
	}

	return nil
}

// readRoots reads the root files through one cat-file --batch, which follows
// a symlink inside the tree, and stages each blob once under all its names.
func (c claudeCode) readRoots(ctx context.Context, t proc.Target, s driver.Spec, roots []string) ([]memorySection, error) {
	if len(roots) == 0 {
		return nil, nil
	}

	var in strings.Builder
	for _, name := range roots {
		in.WriteString(s.BaseCommit + ":" + name + "\n")
	}

	out := &cappedBuffer{limit: stageLimit}
	if err := c.runCommand(ctx, t, s, []string{"git", "cat-file", "--batch", "--follow-symlinks"}, []byte(in.String()), out); err != nil {
		return nil, fmt.Errorf("read the instruction files of %s: %w", s.BaseCommit, err)
	}

	var (
		sections []memorySection
		oids     []string
	)

	rest := out.buf.Bytes()

	for _, name := range roots {
		header, body, _ := bytes.Cut(rest, []byte("\n"))

		// Each answer ends in the size of the body after it: the blob, or where
		// a link out of the tree, dangling or in a loop leads. The output stops
		// short only where the buffer dropped the rest.
		size, err := strconv.Atoi(string(header[bytes.LastIndexByte(header, ' ')+1:]))
		if err != nil || size >= len(body) {
			return nil, fmt.Errorf("the instruction files of %s pass %d bytes at %s", s.BaseCommit, stageLimit, name)
		}

		if fields := strings.Fields(string(header)); len(fields) == 3 && fields[1] == "blob" {
			if i := slices.Index(oids, fields[0]); i >= 0 {
				sections[i].Names = append(sections[i].Names, name)
			} else {
				oids = append(oids, fields[0])
				sections = append(sections, memorySection{Names: []string{name}, Content: string(bytes.TrimRight(body[:size], "\n"))})
			}
		}

		rest = body[size+1:]
	}

	return sections, nil
}

// filterNested keeps one instruction file per directory, AGENTS.md over
// CLAUDE.md, and drops a path with a dot directory or deeper than
// nestedMaxDepth. It sorts shallow first, then by path, and keeps
// nestedMaxFiles.
func filterNested(candidates []string) []string {
	chosen := map[string]string{}

	for _, p := range candidates {
		dir, name := path.Dir(p), path.Base(p)
		if hasDotComponent(dir) || nestedDepth(dir) > nestedMaxDepth {
			continue
		}

		if cur, ok := chosen[dir]; !ok || (cur == "CLAUDE.md" && name == "AGENTS.md") {
			chosen[dir] = name
		}
	}

	listed := make([]string, 0, len(chosen))
	for dir, name := range chosen {
		listed = append(listed, dir+"/"+name)
	}

	slices.SortFunc(listed, func(a, b string) int {
		if da, db := nestedDepth(path.Dir(a)), nestedDepth(path.Dir(b)); da != db {
			return da - db
		}

		return strings.Compare(a, b)
	})

	return listed[:min(len(listed), nestedMaxFiles)]
}

func hasDotComponent(dir string) bool {
	for seg := range strings.SplitSeq(dir, "/") {
		if strings.HasPrefix(seg, ".") {
			return true
		}
	}

	return false
}

// nestedDepth counts the directory levels below the root: "svc" is 1.
func nestedDepth(dir string) int {
	return strings.Count(dir, "/") + 1
}

// capture runs argv and returns its stdout, failing past stageLimit bytes.
func (c claudeCode) capture(ctx context.Context, t proc.Target, s driver.Spec, argv []string, stdin []byte) ([]byte, error) {
	out := &cappedBuffer{limit: stageLimit}
	if err := c.runCommand(ctx, t, s, argv, stdin, out); err != nil {
		return nil, err
	}

	if out.over {
		return nil, fmt.Errorf("more than %d bytes of output", stageLimit)
	}

	return out.buf.Bytes(), nil
}

// sandboxed runs script under shell through proc.SandboxOnly with args as its
// positional parameters, and names a refusal by the marker check.
func (c claudeCode) sandboxed(ctx context.Context, t proc.Target, s driver.Spec, shell, script string, stdin []byte, args ...string) error {
	err := c.runCommand(ctx, t, s, append([]string{shell, "-c", proc.SandboxOnly(script), shell}, args...), stdin, io.Discard)
	if e, ok := errors.AsType[*exitError](err); ok && e.code == proc.NotInSandbox {
		return fmt.Errorf("%w (%w)", errNotInSandbox, err)
	}

	return err
}

// exitError is a staging command that ended with a non-zero status.
type exitError struct {
	program string
	code    int
	tail    string
}

func (e *exitError) Error() string {
	return fmt.Sprintf("%s exited %d: %s", e.program, e.code, e.tail)
}

// runCommand runs argv in s.Dir on t to its end, with stdout going to out;
// stdin is written and closed. A staging command never carries Files.
func (c claudeCode) runCommand(ctx context.Context, t proc.Target, s driver.Spec, argv []string, stdin []byte, out io.Writer) error {
	p, err := t.Start(ctx, proc.Command{Argv: argv, Env: stageEnv(s), Dir: s.Dir, Stdout: out})
	if err != nil {
		return err
	}

	go func() {
		_, _ = p.Stdin().Write(stdin)
		_ = p.Stdin().Close()
	}()

	type exit struct {
		code int
		err  error
	}

	done := make(chan exit, 1)

	go func() {
		code, err := p.Wait()
		done <- exit{code, err}
	}()

	var e exit

	select {
	case e = <-done:
	case <-ctx.Done():
		_ = p.Stop(c.grace)

		<-done

		return ctx.Err()
	}

	switch {
	case e.err != nil:
		return e.err
	case e.code != 0:
		return &exitError{program: argv[0], code: e.code, tail: strings.TrimSpace(p.StderrTail())}
	}

	return nil
}

// cappedBuffer keeps the first limit bytes written and drops the rest, so the
// command still runs to its end.
type cappedBuffer struct {
	buf   bytes.Buffer
	limit int
	over  bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if room := b.limit - b.buf.Len(); n > room {
		p, b.over = p[:room], true
	}

	b.buf.Write(p)

	return n, nil
}
