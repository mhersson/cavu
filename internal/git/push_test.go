package git

import (
	"context"
	"encoding/pem"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/cavu/internal/testkit/gitrepo"
)

const emptyTree = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

// pushFixture is a mirror and a remote, both bare. The mirror holds one more
// commit than the remote and a task ref at it.
type pushFixture struct {
	up, remote, mirror string
	commit             string
}

const taskRef = "refs/cavu/tasks/1"

func newPushFixture(t *testing.T) pushFixture {
	t.Helper()

	up := gitrepo.New(t, map[string]string{"a.txt": "one\n"})
	remote := strings.TrimPrefix(gitrepo.Bare(t, up), "file://")
	f := pushFixture{up: up, remote: remote}
	f.advance(t, "two")

	return f
}

// advance commits in the upstream, copies it into a fresh mirror and points the
// task ref at the commit.
func (f *pushFixture) advance(t *testing.T, content string) {
	t.Helper()

	f.commit = gitrepo.Commit(t, f.up, map[string]string{"a.txt": content + "\n"}, content)

	if f.mirror == "" {
		f.mirror = strings.TrimPrefix(gitrepo.Bare(t, f.up), "file://")
	}

	gitrepo.Git(t, f.mirror, "fetch", "-q", f.up, "+refs/heads/main:"+taskRef)
}

func (f pushFixture) spec() PushSpec {
	return PushSpec{URL: "file://" + f.remote, MirrorDir: f.mirror, Ref: taskRef, Commit: f.commit, Branch: "cavu/x"}
}

// foreign puts a commit the mirror lacks on the remote's branch.
func foreign(t *testing.T, remote, branch string) string {
	t.Helper()

	c := gitrepo.Git(t, remote, "commit-tree", emptyTree, "-m", "foreign "+branch)
	gitrepo.Git(t, remote, "update-ref", "refs/heads/"+branch, c)

	return c
}

func head(t *testing.T, remote, branch string) string {
	t.Helper()

	return gitrepo.Git(t, remote, "for-each-ref", "--format=%(objectname)", "refs/heads/"+branch)
}

func TestPushCreatesTheBranch(t *testing.T) {
	t.Parallel()

	f := newPushFixture(t)

	res, err := Push(context.Background(), f.spec())
	require.NoError(t, err)

	assert.Equal(t, PushResult{Branch: "cavu/x"}, res)
	assert.Equal(t, f.commit, head(t, f.remote, "cavu/x"))
}

func TestPushMovesTheBranchForward(t *testing.T) {
	t.Parallel()

	f := newPushFixture(t)
	first := f.commit

	_, err := Push(context.Background(), f.spec())
	require.NoError(t, err)

	f.advance(t, "three")

	s := f.spec()
	s.Expect = first

	res, err := Push(context.Background(), s)
	require.NoError(t, err)

	assert.Equal(t, PushResult{Branch: "cavu/x"}, res)
	assert.Equal(t, f.commit, head(t, f.remote, "cavu/x"))
}

func TestPushIsRepeatable(t *testing.T) {
	t.Parallel()

	f := newPushFixture(t)
	ctx, calls := wrapGit(t, "")

	for range 2 {
		res, err := Push(ctx, f.spec())
		require.NoError(t, err)
		assert.Equal(t, PushResult{Branch: "cavu/x"}, res)
	}

	// A later push of the same commit names the head it already published.
	s := f.spec()
	s.Expect = f.commit

	res, err := Push(ctx, s)
	require.NoError(t, err)
	assert.Equal(t, PushResult{Branch: "cavu/x"}, res)

	log, err := os.ReadFile(calls)
	require.NoError(t, err)
	assert.Equal(t, 1, strings.Count(string(log), " push "))
}

// The remote branch moves between the check and the push: only a force would
// overwrite it.
func TestPushNeverForces(t *testing.T) {
	t.Parallel()

	f := newPushFixture(t)
	other := gitrepo.Git(t, f.remote, "commit-tree", emptyTree, "-m", "raced")
	ctx, _ := wrapGit(t, `case " $* " in *" push "*) "$(cat "${0%/*}/real-git")" -C '`+f.remote+`' update-ref refs/heads/cavu/x `+other+`;; esac`)

	res, err := Push(ctx, f.spec())
	require.NoError(t, err)

	assert.NotEmpty(t, res.Rejected)
	assert.Equal(t, other, head(t, f.remote, "cavu/x"))
}

func TestPushTakesAFreeNameWhenTheBranchIsSomeoneElses(t *testing.T) {
	t.Parallel()

	f := newPushFixture(t)
	other := foreign(t, f.remote, "cavu/x")

	res, err := Push(context.Background(), f.spec())
	require.NoError(t, err)

	assert.Equal(t, PushResult{Branch: "cavu/x-2"}, res)
	assert.Equal(t, f.commit, head(t, f.remote, "cavu/x-2"))
	assert.Equal(t, other, head(t, f.remote, "cavu/x"))

	for _, n := range []string{"3", "4", "5", "6", "7", "8", "9"} {
		foreign(t, f.remote, "cavu/x-"+n)
	}

	foreign(t, f.remote, "cavu/x-2")

	res, err = Push(context.Background(), f.spec())
	require.NoError(t, err)

	assert.Equal(t, "cavu/x", res.Branch)
	assert.NotEmpty(t, res.Rejected)
	assert.Empty(t, head(t, f.remote, "cavu/x-10"))
}

func TestPushRejectsABranchThatMovedOnTheForge(t *testing.T) {
	t.Parallel()

	f := newPushFixture(t)
	first := f.commit

	_, err := Push(context.Background(), f.spec())
	require.NoError(t, err)

	// An ancestor of the new commit: only the check refuses a push onto it.
	moved := gitrepo.Git(t, f.remote, "rev-parse", "refs/heads/main")
	gitrepo.Git(t, f.remote, "update-ref", "refs/heads/cavu/x", moved)

	f.advance(t, "three")

	s := f.spec()
	s.Expect = first

	res, err := Push(context.Background(), s)
	require.NoError(t, err)

	assert.Equal(t, "cavu/x", res.Branch)
	assert.NotEmpty(t, res.Rejected)
	assert.Equal(t, moved, head(t, f.remote, "cavu/x"))
}

func TestPushRefusesAMirrorThatIsNotAtTheCommit(t *testing.T) {
	t.Parallel()

	f := newPushFixture(t)
	ctx, calls := wrapGit(t, "")
	s := f.spec()
	s.Commit = gitrepo.Git(t, f.up, "rev-parse", "HEAD~1")

	_, err := Push(ctx, s)
	require.Error(t, err)

	log, _ := os.ReadFile(calls)
	assert.NotContains(t, string(log), "ls-remote")
	assert.NotContains(t, string(log), " push ")
	assert.Empty(t, head(t, f.remote, "cavu/x"))
}

func TestPushErrorsWhenTheForgeCannotBeAsked(t *testing.T) {
	t.Parallel()

	f := newPushFixture(t)
	s := f.spec()
	s.URL = "file://" + filepath.Join(t.TempDir(), "absent.git")

	_, err := Push(context.Background(), s)
	require.Error(t, err)
}

func TestPushRefusesWhatCloneRefuses(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*PushSpec)
	}{
		{"url dash", func(s *PushSpec) { s.URL = "--receive-pack=x" }},
		{"url newline", func(s *PushSpec) { s.URL = "https://example.com/a\nb" }},
		{"url userinfo", func(s *PushSpec) { s.URL = "https://user:pw@example.com/a.git" }},
		{"url ssh", func(s *PushSpec) { s.URL = "ssh://example.com/a.git" }},
		{"url http", func(s *PushSpec) { s.URL = "http://example.com/a.git" }},
		{"url ext", func(s *PushSpec) { s.URL = "ext::sh -c id" }},
		{"branch dash", func(s *PushSpec) { s.Branch = "-b" }},
		{"branch newline", func(s *PushSpec) { s.Branch = "a\nb" }},
		{"branch ref format", func(s *PushSpec) { s.Branch = "a..b" }},
		{"branch colon", func(s *PushSpec) { s.Branch = "a:b" }},
		{"commit short id", func(s *PushSpec) { s.Commit = s.Commit[:7] }},
		{"commit empty", func(s *PushSpec) { s.Commit = "" }},
		{"commit not an id", func(s *PushSpec) { s.Commit = "main" }},
		{"expect not an id", func(s *PushSpec) { s.Expect = "main" }},
		{"ref dash", func(s *PushSpec) { s.Ref = "--all" }},
		{"mirror dir empty", func(s *PushSpec) { s.MirrorDir = "" }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			f := newPushFixture(t)
			ctx, calls := wrapGit(t, "")
			s := f.spec()
			tt.mutate(&s)

			_, err := Push(ctx, s)
			require.Error(t, err)

			log, _ := os.ReadFile(calls)
			assert.NotContains(t, string(log), "ls-remote")
			assert.NotContains(t, string(log), " push ")
			assert.Empty(t, head(t, f.remote, "cavu/x"))
		})
	}
}

// The token reaches the forge as basic auth and appears in no argv and in no
// file under the mirror.
func TestPushTokenStaysOutOfArgvAndFiles(t *testing.T) {
	t.Parallel()

	const token = "tok-secret-1234567890"

	f := newPushFixture(t)
	gitrepo.AllowPush(t, f.remote)

	backend := gitrepo.Handler(t, f.remote)
	pushed := make(chan struct{}, 1)

	srv := tlsServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if user, pass, ok := r.BasicAuth(); !ok || user != "x-access-token" || pass != token {
			w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
			w.WriteHeader(http.StatusUnauthorized)

			return
		}

		if strings.HasSuffix(r.URL.Path, "git-receive-pack") {
			select {
			case pushed <- struct{}{}:
			default:
			}
		}

		backend.ServeHTTP(w, r)
	}))

	ctx, calls := wrapGit(t, `GIT_SSL_CAINFO="${0%/*}/ca.pem"; export GIT_SSL_CAINFO`)
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	require.NoError(t, os.WriteFile(filepath.Join(filepath.Dir(calls), "ca.pem"), ca, 0o600))

	s := f.spec()
	s.URL = srv.URL + "/" + filepath.Base(f.remote)
	s.Token = token

	res, err := Push(ctx, s)
	require.NoError(t, err)
	assert.Equal(t, PushResult{Branch: "cavu/x"}, res)
	assert.Len(t, pushed, 1)
	assert.Equal(t, f.commit, head(t, f.remote, "cavu/x"))

	log, err := os.ReadFile(calls)
	require.NoError(t, err)
	assert.NotContains(t, string(log), token)

	require.NoError(t, filepath.WalkDir(f.mirror, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}

		data, err := os.ReadFile(path) //nolint:gosec // a tree the test owns
		if err != nil {
			return err
		}

		assert.NotContains(t, string(data), token, path)

		return nil
	}))
}

// The rejection names the numbered branch the push was for.
func TestPushRejectionNamesTheNumberedBranch(t *testing.T) {
	t.Parallel()

	f := newPushFixture(t)
	foreign(t, f.remote, "cavu/x")

	other := gitrepo.Git(t, f.remote, "commit-tree", emptyTree, "-m", "raced")
	ctx, _ := wrapGit(t, `case " $* " in *" push "*) "$(cat "${0%/*}/real-git")" -C '`+f.remote+`' update-ref refs/heads/cavu/x-2 `+other+`;; esac`)

	res, err := Push(ctx, f.spec())
	require.NoError(t, err)

	assert.Equal(t, "cavu/x-2", res.Branch)
	assert.NotEmpty(t, res.Rejected)
}
