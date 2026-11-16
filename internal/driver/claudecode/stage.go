package claudecode

import (
	"archive/tar"
	"bytes"
	"context"
	_ "embed" // the staged text
	"errors"
	"fmt"
	"io"
	"maps"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"text/template"
	"time"
	"unicode"

	"github.com/mhersson/cavu/internal/driver"
	"github.com/mhersson/cavu/internal/proc"
)

// The pinned release reads a repository's instruction files only with the
// project settings source, which also applies the repository's env, permission
// rules and hooks. So the driver stages them from the base commit into the
// config dir, which the user source reads. Everything else the user source
// would load from there is cleared before every session, so nothing a session
// writes into the task's home reaches the next one but its transcripts. The
// clear deletes and the write writes, so each is a command of its own behind
// proc.SandboxOnly and does nothing outside a sandbox container.
const (
	stagedMemory = "CLAUDE.md" // the memory files, then the list of nested ones
	stagedRules  = "rules"     // .claude/rules, its Markdown files
	stagedSkills = "skills"    // .claude/skills

	repoRules  = ".claude/rules/"
	repoSkills = ".claude/skills/"

	// keptEntry is the one config-dir entry a session keeps: the transcripts,
	// which a resume needs, and the auto memory beside them.
	keptEntry = "projects"

	stageTimeout = time.Minute
	// listLimit bounds the tree listing, stageLimit what the staging holds as
	// stageBudget counts it, and fileLimit each memory and rule file, which is
	// cut there.
	listLimit  = 64 << 20
	stageLimit = 16 << 20
	fileLimit  = 64 << 10
	// entryCost is what the budget counts for each archive entry and each link
	// read beyond its name and content: a tar header and its padding.
	entryCost = 1 << 10
	// linkLimit is the longest symlink target read.
	linkLimit = 4096

	// A nested file is listed only this many directories deep, and the list
	// holds at most nestedMaxFiles, so a monorepo stays short.
	nestedMaxDepth = 4
	nestedMaxFiles = 24
)

var (
	//go:embed staged-instructions.tmpl
	stagedText     string
	stagedTemplate = template.Must(template.New("staged").Parse(stagedText))
	cutText        = mustRender("cut", nil)
)

func mustRender(name string, data any) string {
	var b strings.Builder
	if err := stagedTemplate.ExecuteTemplate(&b, name, data); err != nil {
		panic(err)
	}

	return b.String()
}

// memoryNames are the files staged into one CLAUDE.md, in this order, and
// instructionNames the names a nested instruction file goes by.
var (
	memoryNames      = []string{"CLAUDE.md", ".claude/CLAUDE.md", "AGENTS.md"}
	instructionNames = []string{"CLAUDE.md", "AGENTS.md"}
)

var commitPattern = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)

// clearScript empties the config dir $1 but for keptEntry. It leaves a missing
// dir alone, refuses one that is a symlink or resolves elsewhere, and deletes
// only by relative name inside the dir it checked it is in. It runs only
// through proc.SandboxOnly, with the dir as its argument, never in its text.
const clearScript = `d=$1
[ -e "$d" ] || [ -L "$d" ] || exit 0
cd -P -- "$d" || exit 1
[ "$(pwd -P)" = "$d" ] || { echo "cavu: the config dir resolves elsewhere; nothing deleted" >&2; exit 1; }
find . -mindepth 1 -maxdepth 1 ! -name ` + keptEntry + ` -exec rm -rf -- {} +`

// writeScript unpacks the tar stream on its stdin into the config dir $1,
// under the same rules as clearScript.
const writeScript = `d=$1
mkdir -p -- "$d" && cd -P -- "$d" && [ "$(pwd -P)" = "$d" ] || exit 1
exec tar -x -f -`

// errNotInSandbox is a clear or write that proc.SandboxOnly refused: the agent
// container runs an image built before the marker.
var errNotInSandbox = errors.New("the sandbox image lacks " + proc.SandboxMarker + "; rebuild it with cavu image build")

// gitArgv reads objects only: no replace objects and no hooks, and stageEnv
// keeps the agent's global git config out and a promisor remote from
// fetching. The repository's own config and object store stay the agent's.
func gitArgv(dir string, args ...string) []string {
	return append([]string{"git", "-C", dir, "--no-replace-objects", "-c", "core.hooksPath=/dev/null"}, args...)
}

func stageEnv(s driver.Spec) []string {
	return proc.BuildEnv(nil, nil, map[string]string{
		"PATH": basePath, "HOME": s.Home, "LC_ALL": "C",
		"GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": "/dev/null", "GIT_NO_REPLACE_OBJECTS": "1",
		"GIT_NO_LAZY_FETCH": "1",
	})
}

type treeEntry struct {
	mode, oid, path string
	size            int // -1 for an entry that is no blob
}

func (e treeEntry) regular() bool    { return e.mode == "100644" || e.mode == "100755" }
func (e treeEntry) symlink() bool    { return e.mode == "120000" }
func (e treeEntry) executable() bool { return e.mode == "100755" }

// boundedFile is a memory or rule file and the regular blob it resolves to.
type boundedFile struct {
	path string
	blob treeEntry
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

// render returns nil for a file with nothing in it.
func (m memoryFile) render() ([]byte, error) {
	if len(m.Sections) == 0 && len(m.Nested) == 0 {
		return nil, nil
	}

	var b bytes.Buffer
	if err := stagedTemplate.ExecuteTemplate(&b, "memory", m); err != nil {
		return nil, fmt.Errorf("render the staged CLAUDE.md: %w", err)
	}

	return b.Bytes(), nil
}

// prepare clears s.ConfigDir but for keptEntry and, when s names a base
// commit, stages its instruction files there, all inside the target: the
// clear, the reads and the write are separate commands.
func (c claudeCode) prepare(ctx context.Context, t proc.Target, s driver.Spec) error {
	if err := validatePaths(s); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, stageTimeout)
	defer cancel()

	if err := c.sandboxed(ctx, t, s, clearScript, nil); err != nil {
		return fmt.Errorf("clear the config dir: %w", err)
	}

	if s.BaseCommit == "" {
		return nil
	}

	listing, err := c.capture(ctx, t, s, gitArgv(s.Dir, "ls-tree", "-r", "-l", "-z", "--full-tree", s.BaseCommit), nil, c.listLimit)
	if err != nil {
		return fmt.Errorf("list %s: %w", s.BaseCommit, err)
	}

	tree, err := parseListing(listing)
	if err != nil {
		return err
	}

	// The budget counts the link targets before they are read, then the
	// archive before the staged blobs are read.
	budget := &stageBudget{limit: c.stageLimit, dirs: map[string]bool{}}

	memory, rules, err := c.boundedFiles(ctx, t, s, tree, budget)
	if err != nil {
		return fmt.Errorf("stage %s: %w", s.BaseCommit, err)
	}

	skills, err := skillEntries(tree)
	if err != nil {
		return err
	}

	nested := nestedFiles(tree)

	if err := budget.archive(memory, rules, skills, nested); err != nil {
		return fmt.Errorf("stage %s: %w", s.BaseCommit, err)
	}

	whole, bounded, err := c.readStaged(ctx, t, s, append(slices.Clone(memory), rules...), skills)
	if err != nil {
		return err
	}

	// Names are relative to the config dir.
	files := map[string][]byte{}
	executable := map[string]bool{}

	for _, e := range skills {
		files[skillName(e)], executable[skillName(e)] = whole[e.oid], e.executable()
	}

	for _, f := range rules {
		files[ruleName(f)] = bounded[f.blob.oid]
	}

	staged, err := memorySections(memory, bounded, nested).render()
	if err != nil {
		return err
	}

	if staged != nil {
		files[stagedMemory] = staged
	}

	if len(files) == 0 {
		return nil
	}

	// The files travel as a tar on the write's stdin, never as the command's
	// Files, which a target writes before the marker check runs.
	archive, err := stagedTar(files, executable)
	if err != nil {
		return err
	}

	if err := c.sandboxed(ctx, t, s, writeScript, archive); err != nil {
		return fmt.Errorf("write the instruction files: %w", err)
	}

	return nil
}

// stagedTar packs files, named relative to the config dir, with the
// directories they sit in: directories 0755, files 0644, and 0755 for a name
// executable marks. It holds directories and regular files only, with no
// owner, no link and the time at zero.
func stagedTar(files map[string][]byte, executable map[string]bool) ([]byte, error) {
	epoch := time.Unix(0, 0)

	dirs := map[string]bool{}

	for name := range files {
		if !cleanRepoPath(name) {
			return nil, fmt.Errorf("staged name %q is not a clean relative path", name)
		}

		for d := path.Dir(name); d != "."; d = path.Dir(d) {
			dirs[d] = true
		}
	}

	var b bytes.Buffer

	tw := tar.NewWriter(&b)

	for _, d := range slices.Sorted(maps.Keys(dirs)) {
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: d + "/", Mode: 0o755, ModTime: epoch}); err != nil {
			return nil, fmt.Errorf("pack %s: %w", d, err)
		}
	}

	for _, name := range slices.Sorted(maps.Keys(files)) {
		mode := int64(0o644)
		if executable[name] {
			mode = 0o755
		}

		h := &tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: mode, Size: int64(len(files[name])), ModTime: epoch}
		if err := tw.WriteHeader(h); err != nil {
			return nil, fmt.Errorf("pack %s: %w", name, err)
		}

		if _, err := tw.Write(files[name]); err != nil {
			return nil, fmt.Errorf("pack %s: %w", name, err)
		}
	}

	if err := tw.Close(); err != nil {
		return nil, fmt.Errorf("pack the instruction files: %w", err)
	}

	return b.Bytes(), nil
}

// parseListing reads an ls-tree -r -l -z listing, by path.
func parseListing(listing []byte) (map[string]treeEntry, error) {
	tree := map[string]treeEntry{}

	for rec := range bytes.SplitSeq(bytes.TrimSuffix(listing, []byte{0}), []byte{0}) {
		if len(rec) == 0 {
			continue
		}

		meta, p, ok := strings.Cut(string(rec), "\t")
		fields := strings.Fields(meta)

		if !ok || len(fields) != 4 {
			return nil, fmt.Errorf("unreadable tree entry %q", rec)
		}

		size, err := strconv.Atoi(fields[3])
		if err != nil {
			size = -1
		}

		tree[p] = treeEntry{mode: fields[0], oid: fields[2], path: p, size: size}
	}

	return tree, nil
}

// boundedFiles resolves the memory files and the Markdown files under
// .claude/rules: a regular file is taken, a symlink only when its target is
// a regular file inside the tree, anything else skipped.
func (c claudeCode) boundedFiles(ctx context.Context, t proc.Target, s driver.Spec, tree map[string]treeEntry, budget *stageBudget) (memory, rules []boundedFile, err error) {
	var candidates []string

	for _, name := range memoryNames {
		if _, ok := tree[name]; ok {
			candidates = append(candidates, name)
		}
	}

	for _, p := range slices.Sorted(maps.Keys(tree)) {
		if strings.HasPrefix(p, repoRules) && path.Ext(p) == ".md" {
			if !cleanRepoPath(p) {
				return nil, nil, fmt.Errorf("tree entry %q is not a clean path", p)
			}

			candidates = append(candidates, p)
		}
	}

	// Each link target is read once and counted before the read.
	var links []treeEntry

	read := map[string]bool{}

	for _, p := range candidates {
		if e := tree[p]; e.symlink() && e.size >= 0 && e.size <= linkLimit && !read[e.oid] {
			if err := budget.spend(p, entryCost+e.size); err != nil {
				return nil, nil, err
			}

			read[e.oid] = true
			links = append(links, e)
		}
	}

	targets, err := c.readBlobs(ctx, t, s, links)
	if err != nil {
		return nil, nil, err
	}

	for _, p := range candidates {
		e := tree[p]
		if e.symlink() {
			e = linkTarget(tree, p, string(targets[e.oid]))
		}

		if !e.regular() {
			continue
		}

		if strings.HasPrefix(p, repoRules) {
			rules = append(rules, boundedFile{path: p, blob: e})
		} else {
			memory = append(memory, boundedFile{path: p, blob: e})
		}
	}

	return memory, rules, nil
}

// linkTarget is the entry the symlink at link names, when that stays inside
// the tree; the zero entry otherwise.
func linkTarget(tree map[string]treeEntry, link, target string) treeEntry {
	if target == "" || path.IsAbs(target) {
		return treeEntry{}
	}

	clean := path.Join(path.Dir(link), target)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return treeEntry{}
	}

	return tree[clean]
}

// memorySections groups the memory files by blob, so one blob is staged once
// under every name that resolves to it.
func memorySections(memory []boundedFile, contents map[string][]byte, nested []string) memoryFile {
	m := memoryFile{Nested: nested}

	var oids []string

	for _, f := range memory {
		if i := slices.Index(oids, f.blob.oid); i >= 0 {
			m.Sections[i].Names = append(m.Sections[i].Names, f.path)

			continue
		}

		oids = append(oids, f.blob.oid)
		m.Sections = append(m.Sections, memorySection{
			Names: []string{f.path}, Content: string(bytes.TrimRight(contents[f.blob.oid], "\n")),
		})
	}

	return m
}

func skillEntries(tree map[string]treeEntry) ([]treeEntry, error) {
	var out []treeEntry

	for _, p := range slices.Sorted(maps.Keys(tree)) {
		e := tree[p]
		if !strings.HasPrefix(p, repoSkills) || !e.regular() {
			continue
		}

		if !cleanRepoPath(p) {
			return nil, fmt.Errorf("tree entry %q is not a clean path", p)
		}

		out = append(out, e)
	}

	return out, nil
}

func cleanRepoPath(p string) bool {
	for part := range strings.SplitSeq(p, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}

	return true
}

// boundError is a staging that would hold more than its bound; path is the
// repository file at which it passes it.
type boundError struct {
	path  string
	bound int
}

func (e *boundError) Error() string {
	return fmt.Sprintf("the instruction files pass %d bytes at %s", e.bound, e.path)
}

// stageBudget counts what the staging holds against limit, in order: each
// link target it reads, then each archive entry (CLAUDE.md, every rule and
// skill file, and every directory they sit in) at entryCost plus its name and
// content, the rules and memory files as cut. Reading takes no more than it
// counts.
type stageBudget struct {
	limit, total int
	dirs         map[string]bool // directories already counted
}

// spend counts n bytes for the repository file p.
func (b *stageBudget) spend(p string, n int) error {
	if b.total += n; b.total > b.limit {
		return &boundError{path: p, bound: b.limit}
	}

	return nil
}

// archive counts the archive the write gets, before its blobs are read. The
// staged CLAUDE.md is its rendered skeleton (headings, fixed text, nested
// list) plus each distinct memory blob.
func (b *stageBudget) archive(memory, rules []boundedFile, skills []treeEntry, nested []string) error {
	skeleton, err := memorySections(memory, nil, nested).render()
	if err != nil {
		return err
	}

	if skeleton != nil {
		blame := stagedMemory
		if len(memory) > 0 {
			blame = memory[0].path
		} else if len(nested) > 0 {
			blame = nested[0]
		}

		if err := b.spend(blame, entryCost+len(stagedMemory)+len(skeleton)); err != nil {
			return err
		}
	}

	counted := map[string]bool{}

	for _, f := range memory {
		if !counted[f.blob.oid] {
			counted[f.blob.oid] = true

			if err := b.spend(f.path, cutSize(f.blob.size)); err != nil {
				return err
			}
		}
	}

	for _, f := range rules {
		if err := b.file(f.path, ruleName(f), cutSize(f.blob.size)); err != nil {
			return err
		}
	}

	for _, e := range skills {
		if err := b.file(e.path, skillName(e), e.size); err != nil {
			return err
		}
	}

	return nil
}

// file counts the archive entry name for the repository file p, with size
// bytes of content, and each directory above it not counted yet.
func (b *stageBudget) file(p, name string, size int) error {
	for d := path.Dir(name); d != "." && !b.dirs[d]; d = path.Dir(d) {
		b.dirs[d] = true

		if err := b.spend(p, entryCost+len(d)); err != nil {
			return err
		}
	}

	return b.spend(p, entryCost+len(name)+size)
}

// cutSize is what a memory or rule file of size bytes holds once staged.
func cutSize(size int) int {
	if size <= fileLimit {
		return size
	}

	return fileLimit + len(cutText)
}

func ruleName(f boundedFile) string {
	return path.Join(stagedRules, strings.TrimPrefix(f.path, repoRules))
}

func skillName(e treeEntry) string {
	return path.Join(stagedSkills, strings.TrimPrefix(e.path, repoSkills))
}

// readStaged reads the skills whole, by blob, and the bounded files cut at
// fileLimit, by blob, a bounded blob past it that no skill shares through a
// bounded read of its own. A skill sharing a blob with a cut file stays whole.
func (c claudeCode) readStaged(ctx context.Context, t proc.Target, s driver.Spec, files []boundedFile, skills []treeEntry) (whole, bounded map[string][]byte, err error) {
	batch := []treeEntry{}
	inBatch := map[string]bool{}

	add := func(e treeEntry) {
		if !inBatch[e.oid] {
			inBatch[e.oid] = true
			batch = append(batch, e)
		}
	}

	for _, e := range skills {
		add(e)
	}

	var long []treeEntry

	for _, f := range files {
		switch {
		case inBatch[f.blob.oid] || slices.ContainsFunc(long, func(e treeEntry) bool { return e.oid == f.blob.oid }):
		case f.blob.size > fileLimit:
			long = append(long, f.blob)
		default:
			add(f.blob)
		}
	}

	whole, err = c.readBlobs(ctx, t, s, batch)
	if err != nil {
		return nil, nil, err
	}

	bounded = map[string][]byte{}

	for _, e := range long {
		out := &firstBytes{limit: fileLimit + 1}
		if err := c.runCommand(ctx, t, s, gitArgv(s.Dir, "cat-file", "blob", e.oid), nil, out); err != nil {
			return nil, nil, fmt.Errorf("read %s of %s: %w", e.path, s.BaseCommit, err)
		}

		bounded[e.oid] = cutUTF8(out.buf.Bytes(), fileLimit)
	}

	for _, f := range files {
		if _, ok := bounded[f.blob.oid]; !ok {
			bounded[f.blob.oid] = cutUTF8(whole[f.blob.oid], fileLimit)
		}
	}

	return whole, bounded, nil
}

// readBlobs reads blobs through one cat-file --batch, by object id.
func (c claudeCode) readBlobs(ctx context.Context, t proc.Target, s driver.Spec, entries []treeEntry) (map[string][]byte, error) {
	contents := map[string][]byte{}
	if len(entries) == 0 {
		return contents, nil
	}

	var (
		oids  bytes.Buffer
		total int
	)

	for _, e := range entries {
		oids.WriteString(e.oid + "\n")

		total += e.size
	}

	out := &cappedBuffer{limit: total + len(entries)*200}
	if err := c.runCommand(ctx, t, s, gitArgv(s.Dir, "cat-file", "--batch"), oids.Bytes(), out); err != nil {
		return nil, fmt.Errorf("read the instruction files of %s: %w", s.BaseCommit, err)
	}

	rest := out.buf.Bytes()

	for _, e := range entries {
		header, body, ok := bytes.Cut(rest, []byte("\n"))
		fields := strings.Fields(string(header))

		if !ok || len(fields) != 3 || fields[0] != e.oid || fields[1] != "blob" {
			return nil, fmt.Errorf("unreadable object %s for %s: %q", e.oid, e.path, header)
		}

		size, err := strconv.Atoi(fields[2])
		if err != nil || size > len(body)-1 || body[size] != '\n' {
			return nil, fmt.Errorf("object %s for %s is cut short", e.oid, e.path)
		}

		contents[e.oid] = body[:size]
		rest = body[size+1:]
	}

	if len(rest) > 0 {
		return nil, errors.New("cat-file wrote more than was asked for")
	}

	return contents, nil
}

// cutUTF8 keeps at most limit bytes of data, backed off to a rune boundary,
// and marks a cut.
func cutUTF8(data []byte, limit int) []byte {
	if len(data) <= limit {
		return data
	}

	for limit > 0 && data[limit]&0xC0 == 0x80 {
		limit--
	}

	return append(slices.Clip(data[:limit]), cutText...)
}

// nestedFiles lists the nested instruction files of the tree, as filterNested
// keeps them.
func nestedFiles(tree map[string]treeEntry) []string {
	var candidates []string

	for p, e := range tree {
		if e.size >= 0 {
			candidates = append(candidates, p)
		}
	}

	return filterNested(candidates)
}

// filterNested keeps one instruction file per directory below the root,
// AGENTS.md over CLAUDE.md, and drops a path with a dot directory, one deeper
// than nestedMaxDepth, and one with a control character, which would break
// the list. It sorts shallow first, then by path, and keeps nestedMaxFiles.
func filterNested(candidates []string) []string {
	chosen := map[string]string{}

	for _, p := range candidates {
		name, dir := path.Base(p), path.Dir(p)

		if !slices.Contains(instructionNames, name) || dir == "." || dir == "" ||
			hasDotComponent(dir) || nestedDepth(dir) > nestedMaxDepth ||
			strings.ContainsFunc(p, unicode.IsControl) {
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

	slices.SortStableFunc(listed, func(a, b string) int {
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

// capture runs argv and returns its stdout, failing past limit bytes.
func (c claudeCode) capture(ctx context.Context, t proc.Target, s driver.Spec, argv []string, stdin []byte, limit int) ([]byte, error) {
	out := &cappedBuffer{limit: limit}
	if err := c.runCommand(ctx, t, s, argv, stdin, out); err != nil {
		return nil, err
	}

	return out.buf.Bytes(), nil
}

// sandboxed runs script through proc.SandboxOnly with the config dir as its
// one argument, and names a refusal by the marker check.
func (c claudeCode) sandboxed(ctx context.Context, t proc.Target, s driver.Spec, script string, stdin []byte) error {
	err := c.runCommand(ctx, t, s, []string{"sh", "-c", proc.SandboxOnly(script), "sh", s.ConfigDir}, stdin, io.Discard)
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

// cappedBuffer fails a write past its limit.
type cappedBuffer struct {
	buf   bytes.Buffer
	limit int
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if b.buf.Len()+len(p) > b.limit {
		return 0, fmt.Errorf("more than %d bytes of output", b.limit)
	}

	return b.buf.Write(p)
}

// firstBytes keeps the first limit bytes and drops the rest, so a long blob
// is read to its end without being held.
type firstBytes struct {
	buf   bytes.Buffer
	limit int
}

func (b *firstBytes) Write(p []byte) (int, error) {
	if room := b.limit - b.buf.Len(); room > 0 {
		b.buf.Write(p[:min(room, len(p))])
	}

	return len(p), nil
}
