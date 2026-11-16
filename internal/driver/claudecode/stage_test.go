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
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/cavu/internal/driver"
	"github.com/mhersson/cavu/internal/proc"
	"github.com/mhersson/cavu/internal/testkit/fakeclaude"
)

// testBase is the base commit the recording target's tree stands for.
const testBase = "0123456789abcdef0123456789abcdef01234567"

// fakeTree is a base commit's tree for the recording target, by path. A
// symlink's content is its target.
type fakeTree map[string]fakeEntry

type fakeEntry struct{ mode, content string }

func regular(content string) fakeEntry    { return fakeEntry{"100644", content} }
func executable(content string) fakeEntry { return fakeEntry{"100755", content} }
func symlink(target string) fakeEntry     { return fakeEntry{"120000", target} }

// submodule is a gitlink: no blob, so the listing gives it no size.
var submodule = fakeEntry{mode: "160000"}

// oid is e's object id in git's SHA-256 form, so two blobs with one content
// share it, as in git.
func (e fakeEntry) oid() string {
	kind := "blob"
	if e.mode == submodule.mode {
		kind = "commit"
	}

	sum := sha256.Sum256(fmt.Appendf(nil, "%s %d\x00%s", kind, len(e.content), e.content))

	return hex.EncodeToString(sum[:])
}

// listing answers ls-tree -r -l -z --full-tree.
func (tr fakeTree) listing() []byte {
	var b bytes.Buffer

	for _, p := range slices.Sorted(maps.Keys(tr)) {
		e := tr[p]

		typ, size := "blob", strconv.Itoa(len(e.content))
		if e.mode == submodule.mode {
			typ, size = "commit", "-"
		}

		fmt.Fprintf(&b, "%s %s %s %7s\t%s\x00", e.mode, typ, e.oid(), size, p)
	}

	return b.Bytes()
}

// batch answers cat-file --batch for the object ids on stdin.
func (tr fakeTree) batch(stdin []byte) []byte {
	var b bytes.Buffer

	for line := range strings.Lines(string(stdin)) {
		oid := strings.TrimSuffix(line, "\n")

		content, ok := tr.content(oid)
		if !ok {
			fmt.Fprintf(&b, "%s missing\n", oid)

			continue
		}

		fmt.Fprintf(&b, "%s blob %d\n%s\n", oid, len(content), content)
	}

	return b.Bytes()
}

// blob answers cat-file blob.
func (tr fakeTree) blob(oid string) []byte {
	content, _ := tr.content(oid)

	return []byte(content)
}

func (tr fakeTree) content(oid string) (string, bool) {
	for _, e := range tr {
		if e.mode != submodule.mode && e.oid() == oid {
			return e.content, true
		}
	}

	return "", false
}

// stagedEntry is one entry of the tar the write was given.
type stagedEntry struct {
	typeflag byte
	mode     int64
	content  string
}

func stagedDir() stagedEntry                   { return stagedEntry{tar.TypeDir, 0o755, ""} }
func stagedFile(content string) stagedEntry    { return stagedEntry{tar.TypeReg, 0o644, content} }
func stagedProgram(content string) stagedEntry { return stagedEntry{tar.TypeReg, 0o755, content} }

// staged reads back the tar on the write's stdin, nil when no write ran,
// checking every entry with requirePlainEntry.
func staged(t *testing.T, f *fakeRun) map[string]stagedEntry {
	t.Helper()

	writes := f.target.rec.ofKind(kindWrite)
	if len(writes) == 0 {
		return nil
	}

	require.Len(t, writes, 1)

	out := map[string]stagedEntry{}
	tr := tar.NewReader(bytes.NewReader(writes[0].stdin))

	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out
		}

		require.NoError(t, err)
		requirePlainEntry(t, h)

		data, err := io.ReadAll(tr)
		require.NoError(t, err)

		out[h.Name] = stagedEntry{typeflag: h.Typeflag, mode: h.Mode, content: string(data)}
	}
}

// requirePlainEntry fails unless h is a directory or a regular file under a
// clean relative name, a directory 0755 and a file 0644 or 0755, with no
// owner, no link, no device and the time at zero.
func requirePlainEntry(t *testing.T, h *tar.Header) {
	t.Helper()

	name := strings.TrimSuffix(h.Name, "/")
	assert.True(t, name != "" && name != "." && name != ".." && !strings.HasPrefix(name, "../") &&
		!path.IsAbs(name) && path.Clean(name) == name, "%q is not a clean relative name", h.Name)

	switch h.Typeflag {
	case tar.TypeDir:
		assert.Equal(t, int64(0o755), h.Mode, h.Name)
	case tar.TypeReg:
		assert.Contains(t, []int64{0o644, 0o755}, h.Mode, h.Name)
	default:
		assert.Failf(t, "neither a directory nor a regular file", "%q has type %q", h.Name, h.Typeflag)
	}

	assert.Zero(t, h.Uid, h.Name)
	assert.Zero(t, h.Gid, h.Name)
	assert.Empty(t, h.Uname, h.Name)
	assert.Empty(t, h.Gname, h.Name)
	assert.Empty(t, h.Linkname, h.Name)
	assert.Zero(t, h.Devmajor, h.Name)
	assert.Zero(t, h.Devminor, h.Name)
	assert.Zero(t, h.ModTime.Unix(), h.Name)
}

// stagedFiles is the content of each file the write was given, by name.
func stagedFiles(t *testing.T, f *fakeRun) map[string]string {
	t.Helper()

	got := map[string]string{}

	for name, e := range staged(t, f) {
		if e.typeflag == tar.TypeReg {
			got[name] = e.content
		}
	}

	return got
}

// stageFake runs the fake claude with tree as the base commit's and returns
// the run; the staging's commands are recorded and run nowhere.
func stageFake(t *testing.T, tree fakeTree) *fakeRun {
	t.Helper()

	f := newFakeRun(t, fakeclaude.Scenario{Replay: fixture(t, "real/t1.jsonl")})
	f.spec.BaseCommit = testBase
	f.target.rec.tree = tree

	runFake(t, f)

	return f
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
func rendered(t *testing.T, m memoryFile) string {
	t.Helper()

	b, err := m.render()
	require.NoError(t, err)

	return string(b)
}

func section(content string, names ...string) memorySection {
	return memorySection{Names: names, Content: content}
}

func TestStageInstructionFiles(t *testing.T) {
	f := newFakeRun(t, fakeclaude.Scenario{Replay: fixture(t, "real/t1.jsonl")})
	f.spec.BaseCommit, f.spec.Instructions = testBase, testInstr
	f.target.rec.tree = fakeTree{
		"CLAUDE.md":                         regular("root claude\n"),
		".claude/CLAUDE.md":                 regular("dot claude\n"),
		"AGENTS.md":                         regular("root agents"),
		"README.md":                         regular("not an instruction file\n"),
		"docs/rule.md":                      regular("linked rule\n"),
		"pkg/CLAUDE.md":                     regular("nested claude\n"),
		"pkg/deep/AGENTS.md":                regular("deep agents\n"),
		".claude/settings.json":             regular("{}\n"),
		".claude/agents/helper.md":          regular("an agent\n"),
		".claude/commands/go.md":            regular("a command\n"),
		".claude/rules/style.md":            regular("style rule\n"),
		".claude/rules/sub/deep.md":         regular("deep rule\n"),
		".claude/rules/notes.txt":           regular("not Markdown\n"),
		".claude/rules/linked.md":           symlink("../../docs/rule.md"),
		".claude/rules/out.md":              symlink("../../../outside.md"),
		".claude/skills/one/SKILL.md":       regular("skill one\n"),
		".claude/skills/one/ref/notes.md":   regular("notes\n"),
		".claude/skills/one/link.md":        symlink("../../../../../etc/hostname"),
		".claude/skills/two/SKILL.md":       regular("skill two\n"),
		".claude/skills/two/scripts/run.sh": executable("echo run\n"),
		".claude/skills/sub":                submodule,
	}

	rec := runFake(t, f)

	assert.Equal(t, map[string]stagedEntry{
		"CLAUDE.md": stagedFile(rendered(t, memoryFile{
			Sections: []memorySection{
				section("root claude", "CLAUDE.md"), section("dot claude", ".claude/CLAUDE.md"), section("root agents", "AGENTS.md"),
			},
			Nested: []string{"pkg/CLAUDE.md", "pkg/deep/AGENTS.md"},
		})),
		"rules/":                    stagedDir(),
		"rules/sub/":                stagedDir(),
		"rules/style.md":            stagedFile("style rule\n"),
		"rules/sub/deep.md":         stagedFile("deep rule\n"),
		"rules/linked.md":           stagedFile("linked rule\n"),
		"skills/":                   stagedDir(),
		"skills/one/":               stagedDir(),
		"skills/one/ref/":           stagedDir(),
		"skills/two/":               stagedDir(),
		"skills/two/scripts/":       stagedDir(),
		"skills/one/SKILL.md":       stagedFile("skill one\n"),
		"skills/one/ref/notes.md":   stagedFile("notes\n"),
		"skills/two/SKILL.md":       stagedFile("skill two\n"),
		"skills/two/scripts/run.sh": stagedProgram("echo run\n"),
	}, staged(t, f))

	// The clear, the reads (the listing, the rule links, the staged blobs)
	// and the write each run alone, all before the session.
	assert.Equal(t, []string{kindClear, kindListing, kindBatch, kindBatch, kindWrite, "claude"}, startedKinds(f))

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
		{"unclean, inside the tree", "CLAUDE.md", "docs/../docs/./rules.md", []memorySection{section("rules", "CLAUDE.md"), section("agents", "AGENTS.md")}},
		{"out of the tree", "CLAUDE.md", "../outside.md", []memorySection{section("agents", "AGENTS.md")}},
		{"absolute", "CLAUDE.md", "/etc/hostname", []memorySection{section("agents", "AGENTS.md")}},
		{"to a directory", "CLAUDE.md", "docs", []memorySection{section("agents", "AGENTS.md")}},
		{"to a symlink", "CLAUDE.md", "chain.md", []memorySection{section("agents", "AGENTS.md")}},
		{"to nothing", "CLAUDE.md", "missing.md", []memorySection{section("agents", "AGENTS.md")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := stageFake(t, fakeTree{
				"AGENTS.md":     regular("agents\n"),
				"docs/rules.md": regular("rules\n"),
				"chain.md":      symlink("AGENTS.md"),
				tc.at:           symlink(tc.link),
			})

			assert.Equal(t, rendered(t, memoryFile{Sections: tc.want}), stagedFiles(t, f)["CLAUDE.md"])
		})
	}
}

func TestStageCutsLongFiles(t *testing.T) {
	// A three-byte rune straddles the cut.
	kept := strings.Repeat("a", fileLimit-1)
	long := kept + "…" + strings.Repeat("b", 10<<10)

	f := stageFake(t, fakeTree{"CLAUDE.md": regular(long), ".claude/rules/long.md": regular(long)})
	got := stagedFiles(t, f)

	cut := kept + cutText
	require.True(t, utf8.ValidString(cut))
	assert.Equal(t, cut, got["rules/long.md"])
	assert.Equal(t, rendered(t, memoryFile{Sections: []memorySection{section(strings.TrimRight(cut, "\n"), "CLAUDE.md")}}), got["CLAUDE.md"])

	// One blob behind two names is read once, past the limit through a
	// bounded read of its own.
	assert.Len(t, f.target.rec.ofKind(kindBlob), 1)
}

func TestStageNestedListAlone(t *testing.T) {
	f := stageFake(t, fakeTree{"svc/AGENTS.md": regular("svc agents\n"), ".claude/skills/one/SKILL.md": regular("skill\n")})

	got := stagedFiles(t, f)
	assert.Equal(t, rendered(t, memoryFile{Nested: []string{"svc/AGENTS.md"}}), got["CLAUDE.md"])
	assert.NotContains(t, got["CLAUDE.md"], "svc agents")
}

// With no root file, no rule, no skill and an empty nested list, the staging
// clears and reads but runs no write.
func TestStageNothingToStage(t *testing.T) {
	f := stageFake(t, fakeTree{
		"README.md":               regular("no instruction files\n"),
		"CLAUDE.md":               symlink("../outside.md"),
		".cache/CLAUDE.md":        regular("in a dot directory, so not listed\n"),
		"pkg/README.md":           regular("none here\n"),
		".claude/settings.json":   regular("{}\n"),
		".claude/agents/agent.md": regular("an agent\n"),
		".claude/rules":           symlink("../shared/rules"),
		".claude/skills":          symlink("../shared/skills"),
	})

	// The listing, then the link's target.
	assert.Equal(t, []string{kindClear, kindListing, kindBatch, "claude"}, startedKinds(f))
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
		{kindBatch, 128, false},
	} {
		t.Run(fmt.Sprintf("%s exits %d", tc.kind, tc.code), func(t *testing.T) {
			f := newFakeRun(t, fakeclaude.Scenario{Replay: fixture(t, "real/t1.jsonl")})
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

// A crafted tree whose path under .claude/skills or .claude/rules holds an
// empty, . or .. component could aim a write outside skills/ or rules/.
func TestStageRefusesUncleanPaths(t *testing.T) {
	for _, p := range []string{
		".claude/skills/../settings.md",
		".claude/skills/./settings.md",
		".claude/skills//settings.md",
		".claude/rules/../settings.md",
		".claude/rules/sub/../../settings.md",
	} {
		t.Run(p, func(t *testing.T) {
			f := newFakeRun(t, fakeclaude.Scenario{Replay: fixture(t, "real/t1.jsonl")})
			f.spec.BaseCommit = testBase
			f.target.rec.tree = fakeTree{"CLAUDE.md": regular("root\n"), p: regular("planted\n")}

			_, err := f.run(t.Context())
			require.Error(t, err)
			assert.Empty(t, f.target.rec.ofKind(kindWrite), "a write ran")
			requireNoSession(t, f)
		})
	}
}

// stagedTar takes clean relative names only, whatever reaches it.
func TestStagedTarRefusesUncleanNames(t *testing.T) {
	for _, name := range []string{"../settings.md", "/etc/settings.md", "skills/../../x", "skills//x", "./CLAUDE.md", ""} {
		_, err := stagedTar(map[string][]byte{name: []byte("x")}, nil)
		assert.Error(t, err, "%q", name)
	}
}

// The staging stops before any read once what it would write passes its
// bound, naming the file at which it does; a long rule counts as its cut. A
// listing past its bound names no file.
func TestStageLimits(t *testing.T) {
	tree := fakeTree{
		"CLAUDE.md":                   regular("root\n"),
		".claude/rules/long.md":       regular(strings.Repeat("r", 2*fileLimit)),
		".claude/skills/one/SKILL.md": regular(strings.Repeat("s", 1000)),
	}
	counted := len("root\n") + fileLimit + 1000

	for _, tc := range []struct {
		name   string
		mutate func(*claudeCode)
		fails  bool
		passAt string // the file the error names
	}{
		{"within both", func(c *claudeCode) { c.stageLimit = counted }, false, ""},
		{"listing past its limit", func(c *claudeCode) { c.listLimit = 64 }, true, ""},
		{"past the bound at a skill", func(c *claudeCode) { c.stageLimit = counted - 1 }, true, ".claude/skills/one/SKILL.md"},
		{"past the bound at a rule", func(c *claudeCode) { c.stageLimit = len("root\n") + fileLimit - 1 }, true, ".claude/rules/long.md"},
		{"past the bound at a memory file", func(c *claudeCode) { c.stageLimit = len("root\n") - 1 }, true, "CLAUDE.md"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeRun(t, fakeclaude.Scenario{Replay: fixture(t, "real/t1.jsonl")})
			f.spec.BaseCommit = testBase
			f.target.rec.tree = tree
			tc.mutate(&f.d)

			_, err := f.run(t.Context())
			if !tc.fails {
				require.NoError(t, err)

				return
			}

			require.Error(t, err)

			var be *boundError
			if assert.Equal(t, tc.passAt != "", errors.As(err, &be)) && be != nil {
				assert.Equal(t, tc.passAt, be.path)
			}

			assert.Empty(t, f.target.rec.ofKind(kindBatch), "a blob was read")
			assert.Empty(t, f.target.rec.ofKind(kindWrite), "a write ran")
			requireNoSession(t, f)
		})
	}
}

// The bound counts every staged name, so skills that share one blob, read
// once, count once each.
func TestStageBoundCountsEachName(t *testing.T) {
	shared := strings.Repeat("s", 600)

	f := newFakeRun(t, fakeclaude.Scenario{Replay: fixture(t, "real/t1.jsonl")})
	f.spec.BaseCommit = testBase
	f.target.rec.tree = fakeTree{".claude/skills/a/SKILL.md": regular(shared), ".claude/skills/b/SKILL.md": regular(shared)}
	f.d.stageLimit = 1000

	_, err := f.run(t.Context())

	var be *boundError
	require.ErrorAs(t, err, &be)
	assert.Equal(t, ".claude/skills/b/SKILL.md", be.path)
	requireNoSession(t, f)
}

// The archive holds names, contents and two modes only: 0755 for directories
// and for the skill files the commit marks executable, 0644 for every other
// file, whatever mode a memory or rule blob has.
func TestStagedArchiveEntries(t *testing.T) {
	f := stageFake(t, fakeTree{
		"CLAUDE.md":                        executable("root\n"),
		".claude/rules/run.md":             executable("rule\n"),
		".claude/rules/linked.md":          symlink("../../docs/rule.md"),
		"docs/rule.md":                     regular("linked rule\n"),
		".claude/skills/one/SKILL.md":      regular("skill\n"),
		".claude/skills/one/bin/run.sh":    executable("echo run\n"),
		".claude/skills/one/link.sh":       symlink("bin/run.sh"),
		".claude/skills/one/deep/a/b/c.md": regular("deep\n"),
		".claude/skills/one/vendor":        submodule,
		"pkg/AGENTS.md":                    regular("nested\n"),
	})

	entries := staged(t, f)
	require.NotEmpty(t, entries)

	var programs []string

	for name, e := range entries {
		if e.typeflag == tar.TypeReg && e.mode == 0o755 {
			programs = append(programs, name)
		}
	}

	assert.Equal(t, []string{"skills/one/bin/run.sh"}, programs)
	assert.NotContains(t, entries, "skills/one/link.sh")
	assert.NotContains(t, entries, "skills/one/vendor")
}

// A symlink target longer than linkLimit is not read, so the link is skipped.
func TestStageLinkLimit(t *testing.T) {
	for _, tc := range []struct {
		name, target string
		want         []memorySection
	}{
		{"within the limit", "./AGENTS.md", []memorySection{section("agents", "CLAUDE.md", "AGENTS.md")}},
		{"past the limit", strings.Repeat("./", linkLimit/2) + "AGENTS.md", []memorySection{section("agents", "AGENTS.md")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := stageFake(t, fakeTree{"AGENTS.md": regular("agents\n"), "CLAUDE.md": symlink(tc.target)})

			assert.Equal(t, rendered(t, memoryFile{Sections: tc.want}), stagedFiles(t, f)["CLAUDE.md"])
		})
	}
}

// A skill file whose blob a rule shares is staged whole, the rule cut.
func TestStageSkillKeepsSharedBlobWhole(t *testing.T) {
	long := strings.Repeat("r", fileLimit+100)

	got := stagedFiles(t, stageFake(t, fakeTree{".claude/skills/one/reference.md": regular(long), ".claude/rules/shared.md": regular(long)}))
	assert.Equal(t, long, got["skills/one/reference.md"])
	assert.Equal(t, strings.Repeat("r", fileLimit)+cutText, got["rules/shared.md"])
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
	f := newFakeRun(t, fakeclaude.Scenario{Replay: fixture(t, "real/t1.jsonl")})
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

// Without a base commit nothing is staged, but the config dir is cleared all
// the same.
func TestClearWithoutBaseCommit(t *testing.T) {
	f := newFakeRun(t, fakeclaude.Scenario{Replay: fixture(t, "real/t1.jsonl")})

	runFake(t, f)

	assert.Equal(t, []string{kindClear, "claude"}, startedKinds(f))
}

// The clear and the write each run alone behind the marker check, with the
// config dir only as their argument, and neither carries Files.
func TestSandboxedComposition(t *testing.T) {
	f := stageFake(t, fakeTree{"CLAUDE.md": regular("root\n"), ".claude/skills/one/SKILL.md": regular("skill\n")})
	guard := proc.SandboxOnly("")

	for _, kind := range []string{kindClear, kindWrite} {
		cmds := f.target.rec.ofKind(kind)
		require.Len(t, cmds, 1, kind)

		argv := cmds[0].Argv
		require.Len(t, argv, 5, kind)
		assert.Equal(t, []string{"sh", "-c"}, argv[:2], kind)
		assert.True(t, strings.HasPrefix(argv[2], guard), "%s does not open with the marker check", kind)
		assert.NotContains(t, argv[2], f.spec.ConfigDir, "%s holds the config dir in its text", kind)
		assert.Equal(t, []string{"sh", f.spec.ConfigDir}, argv[3:], kind)
		assert.Empty(t, cmds[0].Files, kind)
	}

	assert.Empty(t, f.target.rec.ofKind(kindClear)[0].stdin)
	assert.NotEmpty(t, staged(t, f))
}

// The git reads take stored objects only: replace objects, hooks, lazy
// fetching and the global and system config are off, and none writes a file.
func TestGitReadComposition(t *testing.T) {
	f := stageFake(t, fakeTree{
		"CLAUDE.md":             regular("root\n"),
		"AGENTS.md":             symlink("CLAUDE.md"),
		".claude/rules/long.md": regular(strings.Repeat("r", fileLimit+1)),
	})

	reads := slices.Concat(f.target.rec.ofKind(kindListing), f.target.rec.ofKind(kindBatch), f.target.rec.ofKind(kindBlob))
	require.Len(t, reads, 4)

	for _, c := range reads {
		assert.Equal(t, []string{"git", "-C", f.spec.Dir, "--no-replace-objects", "-c", "core.hooksPath=/dev/null"}, c.Argv[:6])
		assert.Subset(t, c.Env, []string{"GIT_NO_REPLACE_OBJECTS=1", "GIT_NO_LAZY_FETCH=1", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null"})
		assert.Empty(t, c.Files)
	}
}

// TestMemoryFileRender checks what the staged CLAUDE.md carries and in which
// order, never its fixed wording.
func TestMemoryFileRender(t *testing.T) {
	empty, err := memoryFile{}.render()
	require.NoError(t, err)
	assert.Nil(t, empty)

	got := rendered(t, memoryFile{
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

func TestFilterNested(t *testing.T) {
	t.Run("root files, dot directories and deep directories are left out", func(t *testing.T) {
		got := filterNested([]string{
			"CLAUDE.md", "AGENTS.md", "svc/CLAUDE.md", ".cache/CLAUDE.md", ".claude/CLAUDE.md", "a/.hidden/b/AGENTS.md",
			"a/b/c/d/CLAUDE.md", "a/b/c/d/e/CLAUDE.md", "svc/README.md", "svc/claude.md",
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

	t.Run("a path with a control character is left out", func(t *testing.T) {
		assert.Equal(t, []string{"ok/CLAUDE.md"}, filterNested([]string{"ok/CLAUDE.md", "bad\nline/CLAUDE.md", "tab\there/AGENTS.md"}))
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

func TestCutUTF8(t *testing.T) {
	assert.Equal(t, []byte("short"), cutUTF8([]byte("short"), 10))

	// "…" is three bytes; a cut at 2 or 3 would split it.
	for _, limit := range []int{2, 3} {
		got := cutUTF8([]byte("a…b"), limit)
		assert.True(t, utf8.Valid(got), "limit %d", limit)
		assert.Equal(t, "a"+cutText, string(got), "limit %d", limit)
	}
}
