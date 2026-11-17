package claudecode

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"path"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/cavu/internal/driver"
	"github.com/mhersson/cavu/internal/proc"
)

// testBase is the base commit the recording target's tree stands for.
const testBase = "0123456789abcdef0123456789abcdef01234567"

// fakeTree is a base commit's tree for the recording target, by path. A
// symlink's content is its target, and a directory is any path a file sits
// below.
type fakeTree map[string]fakeEntry

type fakeEntry struct{ mode, content string }

func regular(content string) fakeEntry { return fakeEntry{mode: "100644", content: content} }
func symlink(target string) fakeEntry  { return fakeEntry{mode: "120000", content: target} }

// oid is e's object id in git's SHA-256 form, so two blobs with one content
// share it, as in git.
func (e fakeEntry) oid() string {
	sum := sha256.Sum256(fmt.Appendf(nil, "blob %d\x00%s", len(e.content), e.content))

	return hex.EncodeToString(sum[:])
}

func (tr fakeTree) dir(p string) bool {
	return slices.ContainsFunc(slices.Collect(maps.Keys(tr)), func(q string) bool { return strings.HasPrefix(q, p+"/") })
}

// listing answers ls-tree -z for names.
func (tr fakeTree) listing(names []string) []byte {
	var b bytes.Buffer

	for _, name := range names {
		if e, ok := tr[name]; ok {
			fmt.Fprintf(&b, "%s blob %s\t%s\x00", e.mode, e.oid(), name)
		} else if tr.dir(name) {
			fmt.Fprintf(&b, "040000 tree %s\t%s\x00", regular(name).oid(), name)
		}
	}

	return b.Bytes()
}

// nested answers the nested listing: every path below the root named
// AGENTS.md or CLAUDE.md.
func (tr fakeTree) nested() []byte {
	var b bytes.Buffer

	for _, p := range slices.Sorted(maps.Keys(tr)) {
		if strings.HasSuffix(p, "/AGENTS.md") || strings.HasSuffix(p, "/CLAUDE.md") {
			b.WriteString(p + "\x00")
		}
	}

	return b.Bytes()
}

// batch answers cat-file --batch --follow-symlinks for <commit>:<path> lines.
// It follows one link, all these tests plant.
func (tr fakeTree) batch(stdin []byte) []byte {
	var b bytes.Buffer

	for line := range strings.Lines(string(stdin)) {
		_, p, _ := strings.Cut(strings.TrimSuffix(line, "\n"), ":")

		e := tr[p]
		if e.mode == "120000" {
			p = path.Join(path.Dir(p), e.content)
			e = tr[p]
		}

		switch {
		case strings.HasPrefix(p, "../"):
			fmt.Fprintf(&b, "symlink %d\n%s\n", len(p), p)
		case tr.dir(p):
			fmt.Fprintf(&b, "%s tree 4\ntree\n", regular(p).oid())
		case e.mode == "":
			fmt.Fprintf(&b, "dangling %d\n%s\n", len(p), p)
		default:
			fmt.Fprintf(&b, "%s blob %d\n%s\n", e.oid(), len(e.content), e.content)
		}
	}

	return b.Bytes()
}

// stageFake runs the fake claude with tree as the base commit's and returns
// the run; the staging's commands are recorded and run nowhere.
func stageFake(t *testing.T, tree fakeTree) *fakeRun {
	t.Helper()

	f := newFakeRun(t, fakeScenario{Replay: fixture(t, "real/t1.jsonl")})
	f.spec.BaseCommit = testBase
	f.target.rec.tree = tree

	runFake(t, f)

	return f
}

// requireWrite fails unless f's staging ran one write for its config dir, dir
// and base commit, and returns the CLAUDE.md it was given, empty for none,
// and the paths it unpacks from the commit.
func requireWrite(t *testing.T, f *fakeRun) (memory string, unpack []string) {
	t.Helper()

	files, unpack := requireStaged(t, f)

	return files["CLAUDE.md"].content, unpack
}

// stagedFile is one file of the stream the write unpacks.
type stagedFile struct {
	content string
	mode    int64
}

// requireStaged fails unless f's staging ran one write for its config dir,
// dir and base commit, and returns the files of the stream it unpacks, by
// name, and the paths it unpacks from the commit after them.
func requireStaged(t *testing.T, f *fakeRun) (map[string]stagedFile, []string) {
	t.Helper()

	writes := f.target.rec.ofKind(kindWrite)
	require.Len(t, writes, 1)

	argv := writes[0].Argv
	require.GreaterOrEqual(t, len(argv), 8)
	assert.Equal(t, []string{f.spec.ConfigDir, f.spec.Dir, f.spec.BaseCommit}, argv[4:7])

	if argv[7] == "" {
		assert.Empty(t, writes[0].stdin, "stdin carries files the write leaves unwritten")

		return nil, argv[8:]
	}

	files := map[string]stagedFile{}
	tr := tar.NewReader(bytes.NewReader(writes[0].stdin))

	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}

		require.NoError(t, err)

		data, err := io.ReadAll(tr)
		require.NoError(t, err)
		assert.Equal(t, byte(tar.TypeReg), h.Typeflag, h.Name)

		files[h.Name] = stagedFile{content: string(data), mode: h.Mode}
	}

	require.NotEmpty(t, files, "the write unpacks an empty stream")

	return files, argv[8:]
}

// startedKinds names each command f started, in order: a staging command by
// its kind, the fake claude as "claude".
func startedKinds(f *fakeRun) []string {
	var out []string

	for _, c := range f.target.started() {
		k := commandKind(c)
		if len(c.Argv) > 0 && c.Argv[0] == f.d.binary {
			k = "claude"
		}

		out = append(out, k)
	}

	return out
}

// rendered is the staged CLAUDE.md for m, as the template renders it.
func rendered(m memoryFile) string {
	return string(m.render())
}

func section(content string, names ...string) memorySection {
	return memorySection{Names: names, Content: content}
}

func TestStageInstructionFiles(t *testing.T) {
	f := newFakeRun(t, fakeScenario{Replay: fixture(t, "real/t1.jsonl")})
	f.spec.BaseCommit, f.spec.Instructions = testBase, testInstr
	f.target.rec.tree = fakeTree{
		"CLAUDE.md":                   regular("root claude\n"),
		".claude/CLAUDE.md":           regular("dot claude\n"),
		"AGENTS.md":                   regular("root agents"),
		"README.md":                   regular("not an instruction file\n"),
		"pkg/CLAUDE.md":               regular("nested claude\n"),
		"pkg/deep/AGENTS.md":          regular("deep agents\n"),
		".claude/settings.json":       regular("{}\n"),
		".claude/agents/helper.md":    regular("an agent\n"),
		".claude/rules/style.md":      regular("style rule\n"),
		".claude/skills/one/SKILL.md": regular("skill one\n"),
	}

	rec := runFake(t, f)

	// The clear, the reads and the write each run alone, all before the session.
	assert.Equal(t, []string{kindClear, kindListing, kindNested, kindBatch, kindWrite, "claude"}, startedKinds(f))

	memory, unpack := requireWrite(t, f)
	assert.Equal(t, rendered(memoryFile{
		Sections: []memorySection{
			section("root claude", "CLAUDE.md"), section("dot claude", ".claude/CLAUDE.md"), section("root agents", "AGENTS.md"),
		},
		Nested: []string{"pkg/CLAUDE.md", "pkg/deep/AGENTS.md"},
	}), memory)
	assert.Equal(t, archiveNames, unpack)

	// The repository's text never joins the caller's instructions.
	assert.Equal(t, testInstr, rec.RunFiles["--append-system-prompt-file"])
}

func TestStageSymlinkedMemoryFiles(t *testing.T) {
	for _, tc := range []struct {
		name, at, link string
		want           []memorySection
	}{
		{"to AGENTS.md, staged once", "CLAUDE.md", "AGENTS.md", []memorySection{section("agents", "CLAUDE.md", "AGENTS.md")}},
		{"from .claude, relative to it", ".claude/CLAUDE.md", "../AGENTS.md", []memorySection{section("agents", ".claude/CLAUDE.md", "AGENTS.md")}},
		{"to a file in the tree", "CLAUDE.md", "docs/rules.md", []memorySection{section("rules", "CLAUDE.md"), section("agents", "AGENTS.md")}},
		{"out of the tree", "CLAUDE.md", "../outside.md", []memorySection{section("agents", "AGENTS.md")}},
		{"to a directory", "CLAUDE.md", "docs", []memorySection{section("agents", "AGENTS.md")}},
		{"to nothing", "CLAUDE.md", "missing.md", []memorySection{section("agents", "AGENTS.md")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := stageFake(t, fakeTree{
				"AGENTS.md":     regular("agents\n"),
				"docs/rules.md": regular("rules\n"),
				tc.at:           symlink(tc.link),
			})

			memory, _ := requireWrite(t, f)
			assert.Equal(t, rendered(memoryFile{Sections: tc.want}), memory)
		})
	}
}

// The write carries a CLAUDE.md only when there is one, and unpacks only
// .claude/rules and .claude/skills that are directories.
func TestStageWrites(t *testing.T) {
	for _, tc := range []struct {
		name   string
		tree   fakeTree
		memory memoryFile
		unpack []string
	}{
		{"skills alone", fakeTree{".claude/skills/one/SKILL.md": regular("skill\n")}, memoryFile{}, []string{".claude/skills"}},
		{
			"nested list alone",
			fakeTree{"svc/AGENTS.md": regular("svc agents\n"), ".claude/rules/r.md": regular("rule\n")},
			memoryFile{Nested: []string{"svc/AGENTS.md"}},
			[]string{".claude/rules"},
		},
		{
			"rules and skills as links",
			fakeTree{
				"CLAUDE.md": regular("root\n"), ".claude/rules": symlink("../shared/rules"), ".claude/skills": symlink("../shared/skills"),
				"shared/rules/r.md": regular("rule\n"), "shared/skills/one/SKILL.md": regular("skill\n"),
			},
			memoryFile{Sections: []memorySection{section("root", "CLAUDE.md")}},
			[]string{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			memory, unpack := requireWrite(t, stageFake(t, tc.tree))

			assert.Equal(t, rendered(tc.memory), memory)
			assert.Equal(t, tc.unpack, unpack)
		})
	}
}

// With no root file, no rules or skills directory and an empty nested list,
// the staging clears and reads but runs no write.
func TestStageNothingToStage(t *testing.T) {
	f := stageFake(t, fakeTree{
		"README.md":               regular("no instruction files\n"),
		"CLAUDE.md":               symlink("../outside.md"),
		".cache/CLAUDE.md":        regular("in a dot directory, so not listed\n"),
		"pkg/README.md":           regular("none here\n"),
		".claude/settings.json":   regular("{}\n"),
		".claude/agents/agent.md": regular("an agent\n"),
	})

	assert.Equal(t, []string{kindClear, kindListing, kindNested, kindBatch, "claude"}, startedKinds(f))
	assert.Empty(t, f.target.rec.ofKind(kindWrite))
}

// requireNoSession fails if f started claude.
func requireNoSession(t *testing.T, f *fakeRun) {
	t.Helper()

	assert.NotContains(t, startedKinds(f), "claude", "a session started after staging failed")
}

// A failed staging command fails the run before any session. Exit
// proc.NotInSandbox from a command behind proc.SandboxOnly is its refusal
// outside a sandbox container, and only there.
func TestStageFailureStartsNoSession(t *testing.T) {
	for _, tc := range []struct {
		kind         string
		code         int
		notInSandbox bool
	}{
		{kindClear, proc.NotInSandbox, true},
		{kindClear, 1, false},
		{kindWrite, proc.NotInSandbox, true},
		{kindWrite, 2, false},
		{kindListing, 128, false},
		{kindListing, proc.NotInSandbox, false},
		{kindNested, 128, false},
		{kindBatch, 128, false},
	} {
		t.Run(fmt.Sprintf("%s exits %d", tc.kind, tc.code), func(t *testing.T) {
			f := newFakeRun(t, fakeScenario{Replay: fixture(t, "real/t1.jsonl")})
			f.spec.BaseCommit = testBase
			f.target.rec.tree = fakeTree{"CLAUDE.md": regular("root\n")}
			f.target.rec.exits[tc.kind] = tc.code

			_, err := f.run(t.Context())
			require.Error(t, err)
			assert.Equal(t, tc.notInSandbox, errors.Is(err, errNotInSandbox))
			requireNoSession(t, f)

			if tc.kind == kindClear {
				assert.Equal(t, []string{kindClear}, startedKinds(f), "a command ran after the clear failed")
			}
		})
	}
}

// Root files past stageLimit fail the staging with an error naming the file
// at which they pass it, and nothing is written.
func TestStageCap(t *testing.T) {
	f := newFakeRun(t, fakeScenario{Replay: fixture(t, "real/t1.jsonl")})
	f.spec.BaseCommit = testBase
	f.target.rec.tree = fakeTree{"CLAUDE.md": regular("root\n"), "AGENTS.md": regular(strings.Repeat("a", stageLimit))}

	_, err := f.run(t.Context())
	require.ErrorContains(t, err, "AGENTS.md")
	assert.Empty(t, f.target.rec.ofKind(kindWrite), "a write ran")
	requireNoSession(t, f)
}

// cancelOnListing cancels the run as the staging's listing starts.
type cancelOnListing struct {
	*routedTarget

	cancel context.CancelFunc
}

func (c cancelOnListing) Start(ctx context.Context, cmd proc.Command) (proc.Process, error) {
	if commandKind(cmd) == kindListing {
		c.cancel()
	}

	return c.routedTarget.Start(ctx, cmd)
}

func TestStageCancelled(t *testing.T) {
	f := newFakeRun(t, fakeScenario{Replay: fixture(t, "real/t1.jsonl")})
	f.spec.BaseCommit = testBase
	f.target.rec.tree = fakeTree{"CLAUDE.md": regular("root\n")}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	res, err := f.d.Run(ctx, cancelOnListing{routedTarget: f.target, cancel: cancel}, f.spec, func(driver.Event) {})
	require.NoError(t, err)
	assert.Equal(t, driver.ReasonInterrupted, res.Reason)
	assert.True(t, res.IsError)
	requireNoSession(t, f)
}

// Without a base commit or instance files nothing is staged, but the config
// dir is cleared all the same.
func TestClearWithoutBaseCommit(t *testing.T) {
	f := newFakeRun(t, fakeScenario{Replay: fixture(t, "real/t1.jsonl")})

	runFake(t, f)

	assert.Equal(t, []string{kindClear, "claude"}, startedKinds(f))
}

const testInstanceInstr = "INSTANCE-INSTRUCTIONS-7e2a"

// The instance's instructions open the staged CLAUDE.md, before the
// repository's, and its skills go into the stream the write unpacks, a script
// executable. The same write then unpacks the commit's skills, so a
// repository skill of the same name is written over the instance's.
func TestStageInstanceFiles(t *testing.T) {
	f := newFakeRun(t, fakeScenario{Replay: fixture(t, "real/t1.jsonl")})
	f.spec.BaseCommit, f.spec.InstanceInstructions = testBase, testInstanceInstr+"\n"
	f.spec.InstanceSkills = map[string]driver.File{
		"shared/SKILL.md":       {Data: []byte("shared skill\n")},
		"shared/scripts/run.sh": {Data: []byte("echo run\n"), Executable: true},
		"one/SKILL.md":          {Data: []byte("the instance's one\n")},
	}
	f.target.rec.tree = fakeTree{
		"CLAUDE.md":                   regular("root claude\n"),
		".claude/skills/one/SKILL.md": regular("the repository's one\n"),
	}

	runFake(t, f)

	assert.Equal(t, []string{kindClear, kindListing, kindNested, kindBatch, kindWrite, "claude"}, startedKinds(f))

	files, unpack := requireStaged(t, f)
	memory := files["CLAUDE.md"].content
	assert.Equal(t, rendered(memoryFile{Instance: testInstanceInstr, Sections: []memorySection{section("root claude", "CLAUDE.md")}}), memory)
	assert.Less(t, strings.Index(memory, testInstanceInstr), strings.Index(memory, "root claude"))
	assert.Equal(t, map[string]stagedFile{
		"CLAUDE.md":                    {memory, 0o644},
		"skills/shared/SKILL.md":       {"shared skill\n", 0o644},
		"skills/shared/scripts/run.sh": {"echo run\n", 0o755},
		"skills/one/SKILL.md":          {"the instance's one\n", 0o644},
	}, files)
	assert.Equal(t, []string{".claude/skills"}, unpack)

	var unpacks []string

	for l := range strings.Lines(writeScript) {
		if strings.Contains(l, "tar -x") {
			unpacks = append(unpacks, l)
		}
	}

	require.Len(t, unpacks, 2)
	assert.NotContains(t, unpacks[0], " archive ", "the write unpacks the commit's paths before the stream")
	assert.Contains(t, unpacks[1], " archive ")
}

// Without a base commit the instance's files are staged all the same, and no
// git read runs.
func TestStageInstanceFilesWithoutBaseCommit(t *testing.T) {
	skills := map[string]driver.File{"shared/SKILL.md": {Data: []byte("shared skill\n")}}

	for _, tc := range []struct {
		name         string
		instructions string
		skills       map[string]driver.File
		want         map[string]stagedFile
	}{
		{"instructions alone", testInstanceInstr, nil, map[string]stagedFile{
			"CLAUDE.md": {rendered(memoryFile{Instance: testInstanceInstr}), 0o644},
		}},
		{"skills alone", "", skills, map[string]stagedFile{"skills/shared/SKILL.md": {"shared skill\n", 0o644}}},
		{"both", testInstanceInstr, skills, map[string]stagedFile{
			"CLAUDE.md":              {rendered(memoryFile{Instance: testInstanceInstr}), 0o644},
			"skills/shared/SKILL.md": {"shared skill\n", 0o644},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeRun(t, fakeScenario{Replay: fixture(t, "real/t1.jsonl")})
			f.spec.InstanceInstructions, f.spec.InstanceSkills = tc.instructions, tc.skills

			runFake(t, f)

			assert.Equal(t, []string{kindClear, kindWrite, "claude"}, startedKinds(f))

			files, unpack := requireStaged(t, f)
			assert.Equal(t, tc.want, files)
			assert.Empty(t, unpack)
		})
	}
}

// The clear and the write each run alone behind the marker check, with the
// paths only as their arguments, and neither carries Files.
func TestSandboxedComposition(t *testing.T) {
	f := stageFake(t, fakeTree{"CLAUDE.md": regular("root\n"), ".claude/skills/one/SKILL.md": regular("skill\n")})
	guard := proc.SandboxOnly("")

	for _, tc := range []struct {
		kind  string
		shell string
		args  []string
	}{
		{kindClear, "sh", []string{f.spec.ConfigDir}},
		{kindWrite, "bash", []string{f.spec.ConfigDir, f.spec.Dir, testBase, "1", ".claude/skills"}},
	} {
		cmds := f.target.rec.ofKind(tc.kind)
		require.Len(t, cmds, 1, tc.kind)

		argv := cmds[0].Argv
		require.Greater(t, len(argv), 3, tc.kind)
		assert.Equal(t, []string{tc.shell, "-c"}, argv[:2], tc.kind)
		assert.True(t, strings.HasPrefix(argv[2], guard), "%s does not open with the marker check", tc.kind)
		assert.NotContains(t, argv[2], f.spec.ConfigDir, "%s holds the config dir in its text", tc.kind)
		assert.Equal(t, append([]string{tc.shell}, tc.args...), argv[3:], tc.kind)
		assert.Empty(t, cmds[0].Files, tc.kind)
	}

	assert.Empty(t, f.target.rec.ofKind(kindClear)[0].stdin)
}

// The git reads and the write take the base commit's stored objects only:
// replace objects, lazy fetching and the global and system config are off,
// and none carries Files.
func TestGitComposition(t *testing.T) {
	f := stageFake(t, fakeTree{"CLAUDE.md": regular("root\n"), ".claude/rules/r.md": regular("rule\n")})

	cmds := slices.Concat(f.target.rec.ofKind(kindListing), f.target.rec.ofKind(kindNested), f.target.rec.ofKind(kindBatch), f.target.rec.ofKind(kindWrite))
	require.Len(t, cmds, 4)

	for _, c := range cmds {
		assert.Equal(t, f.spec.Dir, c.Dir)
		assert.Subset(t, c.Env, []string{"GIT_NO_REPLACE_OBJECTS=1", "GIT_NO_LAZY_FETCH=1", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null"})
		assert.Empty(t, c.Files)
	}

	assert.Contains(t, cmds[0].Argv, testBase)
	assert.Equal(t, testBase, cmds[1].Argv[len(cmds[1].Argv)-1])
	assert.Equal(t, testBase+":CLAUDE.md\n", string(cmds[2].stdin))
}

// TestMemoryFileRender checks what the staged CLAUDE.md carries and in which
// order, never its fixed wording.
func TestMemoryFileRender(t *testing.T) {
	assert.Nil(t, memoryFile{}.render())

	got := rendered(memoryFile{
		Sections: []memorySection{section("FIRST-CONTENT", "CLAUDE.md", "AGENTS.md"), section("SECOND-CONTENT", ".claude/CLAUDE.md")},
		Nested:   []string{"web/AGENTS.md", "internal/api/CLAUDE.md"},
	})

	order := []string{"CLAUDE.md", "AGENTS.md", "FIRST-CONTENT", ".claude/CLAUDE.md", "SECOND-CONTENT", "web/AGENTS.md", "internal/api/CLAUDE.md"}
	at := -1

	for _, s := range order {
		i := strings.Index(got[at+1:], s)
		require.GreaterOrEqual(t, i, 0, "%s missing or out of order", s)

		at += i + 1
	}

	assert.Equal(t, 1, strings.Count(got, "FIRST-CONTENT"))
	assert.True(t, strings.HasSuffix(got, "internal/api/CLAUDE.md\n"), "the list ends the file")
}

// The instance's instructions open the staged CLAUDE.md, and alone they are
// all of it.
func TestMemoryFileRenderInstance(t *testing.T) {
	assert.Equal(t, "INSTANCE-CONTENT\n", rendered(memoryFile{Instance: "INSTANCE-CONTENT"}))

	got := rendered(memoryFile{
		Instance: "INSTANCE-CONTENT", Sections: []memorySection{section("FIRST-CONTENT", "CLAUDE.md")}, Nested: []string{"web/AGENTS.md"},
	})
	assert.True(t, strings.HasPrefix(got, "INSTANCE-CONTENT\n"), "the instance's instructions do not open the file")

	at := -1

	for _, s := range []string{"INSTANCE-CONTENT", "CLAUDE.md", "FIRST-CONTENT", "web/AGENTS.md"} {
		i := strings.Index(got[at+1:], s)
		require.GreaterOrEqual(t, i, 0, "%s missing or out of order", s)

		at += i + 1
	}

	assert.Equal(t, 1, strings.Count(got, "INSTANCE-CONTENT"))
}

func TestFilterNested(t *testing.T) {
	t.Run("dot directories and deep directories are left out", func(t *testing.T) {
		got := filterNested([]string{
			"svc/CLAUDE.md", ".cache/CLAUDE.md", ".claude/CLAUDE.md", "a/.hidden/b/AGENTS.md",
			"a/b/c/d/CLAUDE.md", "a/b/c/d/e/CLAUDE.md",
		})
		assert.Equal(t, []string{"svc/CLAUDE.md", "a/b/c/d/CLAUDE.md"}, got)
	})

	t.Run("AGENTS.md wins over CLAUDE.md in a directory", func(t *testing.T) {
		assert.Equal(t, []string{"web/AGENTS.md"}, filterNested([]string{"web/CLAUDE.md", "web/AGENTS.md"}))
		assert.Equal(t, []string{"web/AGENTS.md"}, filterNested([]string{"web/AGENTS.md", "web/CLAUDE.md"}))
	})

	t.Run("shallow first, then by path", func(t *testing.T) {
		got := filterNested([]string{"internal/api/AGENTS.md", "web/CLAUDE.md", "cmd/CLAUDE.md", "internal/AGENTS.md"})
		assert.Equal(t, []string{"cmd/CLAUDE.md", "internal/AGENTS.md", "web/CLAUDE.md", "internal/api/AGENTS.md"}, got)
	})

	t.Run("the list is capped", func(t *testing.T) {
		var candidates []string
		for i := range nestedMaxFiles + 5 {
			candidates = append(candidates, fmt.Sprintf("pkg%02d/CLAUDE.md", i))
		}

		got := filterNested(candidates)
		require.Len(t, got, nestedMaxFiles)
		assert.Equal(t, "pkg00/CLAUDE.md", got[0])
	})

	t.Run("nothing to list", func(t *testing.T) {
		assert.Empty(t, filterNested(nil))
	})
}
