package git

import (
	"context"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/cavu/internal/testkit/gitrepo"
)

func spec(t *testing.T, url string) CloneSpec {
	t.Helper()

	root := t.TempDir()

	return CloneSpec{
		URL: url, BaseBranch: "main", Branch: "cavu/x",
		MirrorDir: filepath.Join(root, "mirror"), WorkspaceDir: filepath.Join(root, "ws"),
		Name: "Cavu", Email: "cavu@example.com",
	}
}

// withMainMirror creates the mirror with HEAD on main, as a container whose
// init.defaultBranch is main would.
func withMainMirror(t *testing.T, s CloneSpec) {
	t.Helper()

	gitrepo.Git(t, filepath.Dir(s.MirrorDir), "init", "--bare", "--quiet", "-b", "main", s.MirrorDir)
}

func TestCloneFromMirror(t *testing.T) {
	t.Parallel()

	up := gitrepo.New(t, map[string]string{"a.txt": "one\n"})
	s := spec(t, "file://"+up)
	withMainMirror(t, s)

	base, err := Clone(context.Background(), s)
	require.NoError(t, err)

	assert.Equal(t, gitrepo.Git(t, up, "rev-parse", "HEAD"), base)
	assert.Equal(t, base, gitrepo.Git(t, s.WorkspaceDir, "rev-parse", "HEAD"))
	assert.Equal(t, "cavu/x", gitrepo.Git(t, s.WorkspaceDir, "branch", "--show-current"))
	assert.Empty(t, gitrepo.Git(t, s.WorkspaceDir, "remote"))
	assert.Equal(t, "refs/heads/cavu/x", gitrepo.Git(t, s.WorkspaceDir, "for-each-ref", "--format=%(refname)", "refs/heads"))
	assert.Equal(t, "Cavu", gitrepo.Git(t, s.WorkspaceDir, "config", "user.name"))
	assert.Equal(t, "cavu@example.com", gitrepo.Git(t, s.WorkspaceDir, "config", "user.email"))
	assert.FileExists(t, filepath.Join(s.WorkspaceDir, "a.txt"))
}

func TestCloneAtCommit(t *testing.T) {
	t.Parallel()

	up := gitrepo.New(t, map[string]string{"a.txt": "one\n"})
	older := gitrepo.Git(t, up, "rev-parse", "HEAD")
	gitrepo.Commit(t, up, map[string]string{"a.txt": "two\n"}, "second")

	newer := gitrepo.Git(t, up, "rev-parse", "HEAD")
	s := spec(t, "file://"+up)
	withMainMirror(t, s)
	s.Commit = older

	base, err := Clone(context.Background(), s)
	require.NoError(t, err)

	assert.Equal(t, older, base)
	assert.Equal(t, older, gitrepo.Git(t, s.WorkspaceDir, "rev-parse", "HEAD"))
	assert.Equal(t, "refs/heads/cavu/x", gitrepo.Git(t, s.WorkspaceDir, "for-each-ref", "--format=%(refname)", "refs/heads"))
	assert.Empty(t, gitrepo.Git(t, s.WorkspaceDir, "for-each-ref", "--points-at", newer))
}

func TestCloneBranchNamedLikeBase(t *testing.T) {
	t.Parallel()

	up := gitrepo.New(t, map[string]string{"a.txt": "one\n"})
	s := spec(t, "file://"+up)
	withMainMirror(t, s)
	s.Branch = "main"

	base, err := Clone(context.Background(), s)
	require.NoError(t, err)

	assert.Equal(t, base, gitrepo.Git(t, s.WorkspaceDir, "rev-parse", "HEAD"))
	assert.Equal(t, "refs/heads/main", gitrepo.Git(t, s.WorkspaceDir, "for-each-ref", "--format=%(refname)", "refs/heads"))
	assert.Empty(t, gitrepo.Git(t, s.WorkspaceDir, "remote"))
}

// A full object id names the object even when a branch has the same name as
// the id's short form.
func TestCloneFullCommitBeatsSameNamedBranch(t *testing.T) {
	t.Parallel()

	up := gitrepo.New(t, map[string]string{"a.txt": "one\n"})
	older := gitrepo.Git(t, up, "rev-parse", "HEAD")
	gitrepo.Commit(t, up, map[string]string{"a.txt": "two\n"}, "second")
	gitrepo.Git(t, up, "branch", older[:7])

	s := spec(t, "file://"+up)
	s.Commit = older

	base, err := Clone(context.Background(), s)
	require.NoError(t, err)

	assert.Equal(t, older, base)
}

func TestMirrorHoldsOnlyHeads(t *testing.T) {
	t.Parallel()

	up := gitrepo.New(t, map[string]string{"a.txt": "one\n"})
	gitrepo.Git(t, up, "tag", "v1")

	s := spec(t, "file://"+up)

	_, err := Clone(context.Background(), s)
	require.NoError(t, err)

	assert.Empty(t, gitrepo.Git(t, s.MirrorDir, "tag", "--list"))
}

// The CA file git gets holds the system roots as well as the given bundle, so
// a bundle adds trust and never replaces it.
func TestCABundleAddsToSystemRoots(t *testing.T) {
	t.Parallel()

	system, ok := systemBundle()
	if !ok {
		t.Skip("no system CA bundle on this host")
	}

	up := gitrepo.New(t, map[string]string{"a.txt": "one\n"})
	bare := strings.TrimPrefix(gitrepo.Bare(t, up), "file://")
	srv, ca := tlsServer(t, gitrepo.Handler(t, bare))

	ctx, calls := wrapGit(t, `[ -n "$GIT_SSL_CAINFO" ] && cp "$GIT_SSL_CAINFO" "$LOG.ca"`)
	s := spec(t, srv.URL+"/"+filepath.Base(bare))
	s.CABundle = ca

	_, err := Clone(ctx, s)
	require.NoError(t, err)

	got, err := os.ReadFile(calls + ".ca")
	require.NoError(t, err)
	assert.Contains(t, string(got), strings.TrimSpace(string(system)))
	assert.Contains(t, string(got), strings.TrimSpace(ca))
}

// Every git call runs in a directory the clone owns or in the one that holds
// it, never in the caller's working directory.
func TestCloneRunsGitInItsOwnDirectories(t *testing.T) {
	t.Parallel()

	up := gitrepo.New(t, map[string]string{"a.txt": "one\n"})
	ctx, calls := wrapGit(t, `pwd -P >> "$LOG.dirs"`)
	s := spec(t, "file://"+up)

	_, err := Clone(ctx, s)
	require.NoError(t, err)

	root, err := filepath.EvalSymlinks(filepath.Dir(s.MirrorDir))
	require.NoError(t, err)

	mirror, ws := filepath.Join(root, filepath.Base(s.MirrorDir)), filepath.Join(root, filepath.Base(s.WorkspaceDir))
	want := map[string]string{"check-ref-format": root, "init": root, "clone": root, "fetch": mirror, "rev-parse": mirror}

	log, err := os.ReadFile(calls)
	require.NoError(t, err)

	dirs, err := os.ReadFile(calls + ".dirs")
	require.NoError(t, err)

	lines := strings.Split(strings.TrimSpace(string(log)), "\n")
	got := strings.Split(strings.TrimSpace(string(dirs)), "\n")
	require.Len(t, got, len(lines))

	ran := map[string]bool{}

	for i, line := range lines {
		sub := subcommand(line)
		ran[sub] = true

		dir, ok := want[sub]
		if !ok {
			dir = ws
		}

		assert.Equal(t, dir, got[i], line)
	}

	for _, sub := range []string{"check-ref-format", "init", "clone"} {
		assert.True(t, ran[sub], sub)
	}
}

// A relative directory would leave git in the caller's working directory, so
// Clone refuses one before any git runs. The test runs in a directory of its
// own, so that a regression would write nowhere else.
func TestCloneRefusesRelativeDirs(t *testing.T) {
	for _, tt := range []struct {
		name   string
		mutate func(*CloneSpec)
	}{
		{"mirror dir", func(s *CloneSpec) { s.MirrorDir = "mirror" }},
		{"workspace dir", func(s *CloneSpec) { s.WorkspaceDir = "ws" }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cwd := t.TempDir()
			t.Chdir(cwd)

			up := gitrepo.New(t, map[string]string{"a.txt": "one\n"})
			ctx, calls := wrapGit(t, "")
			s := spec(t, "file://"+up)
			tt.mutate(&s)

			_, err := Clone(ctx, s)
			require.Error(t, err)

			assert.NoFileExists(t, calls, "no git ran")
			assert.NoDirExists(t, filepath.Join(cwd, "mirror"))
			assert.NoDirExists(t, filepath.Join(cwd, "ws"))
		})
	}
}

// subcommand returns the git subcommand of a line of the wrapper's log.
func subcommand(line string) string {
	fields := strings.Fields(line)
	for i := 0; i < len(fields); i++ {
		if fields[i] == "-c" {
			i++

			continue
		}

		return fields[i]
	}

	return ""
}

func TestMirrorUpdates(t *testing.T) {
	t.Parallel()

	up := gitrepo.New(t, map[string]string{"a.txt": "one\n"})
	s := spec(t, "file://"+up)

	_, err := Clone(context.Background(), s)
	require.NoError(t, err)

	second := gitrepo.Commit(t, up, map[string]string{"a.txt": "two\n"}, "second")
	s.WorkspaceDir = filepath.Join(t.TempDir(), "ws2")

	base, err := Clone(context.Background(), s)
	require.NoError(t, err)

	assert.Equal(t, second, base)
	assert.Equal(t, second, gitrepo.Git(t, s.WorkspaceDir, "rev-parse", "HEAD"))
}

func tlsServer(t *testing.T, h http.Handler) (*httptest.Server, string) {
	t.Helper()

	srv := httptest.NewTLSServer(h)
	t.Cleanup(srv.Close)

	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})

	return srv, string(ca)
}

// A token reaches the host it is scoped to and nothing else: not argv, not a
// file under the mirror or the workspace, not a host a redirect points at.
func TestCredentialHelperScope(t *testing.T) {
	t.Parallel()

	const token = "tok-secret-1234567890"

	up := gitrepo.New(t, map[string]string{"a.txt": "one\n"})
	bare := strings.TrimPrefix(gitrepo.Bare(t, up), "file://")
	backend := gitrepo.Handler(t, bare)

	var (
		mu      sync.Mutex
		authed  int
		otherAu []string
	)

	main, ca := tlsServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != "x-access-token" || pass != token {
			w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
			w.WriteHeader(http.StatusUnauthorized)

			return
		}

		mu.Lock()
		authed++
		mu.Unlock()
		backend.ServeHTTP(w, r)
	}))

	other, _ := tlsServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()

		otherAu = append(otherAu, r.Header.Get("Authorization"))
		mu.Unlock()
		w.WriteHeader(http.StatusUnauthorized)
	}))

	repoPath := "/" + filepath.Base(bare)
	ctx, calls := wrapGit(t, "")
	s := spec(t, main.URL+repoPath)
	s.Token, s.CABundle = token, ca

	_, err := Clone(ctx, s)
	require.NoError(t, err)

	mu.Lock()
	assert.Positive(t, authed)
	mu.Unlock()

	log, err := os.ReadFile(calls)
	require.NoError(t, err)
	assert.NotContains(t, string(log), token)

	for _, dir := range []string{s.MirrorDir, s.WorkspaceDir} {
		require.NoError(t, filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
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

	// A server that redirects to another host never sees the token.
	redirect, rca := tlsServer(t, http.RedirectHandler(other.URL+repoPath+"/info/refs?service=git-upload-pack", http.StatusFound))
	r := spec(t, redirect.URL+repoPath)
	r.Token, r.CABundle = token, rca+ca+string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: other.Certificate().Raw}))

	_, err = Clone(context.Background(), r)
	require.Error(t, err)

	mu.Lock()
	defer mu.Unlock()

	assert.NotEmpty(t, otherAu)

	for _, h := range otherAu {
		assert.Empty(t, h)
	}
}

func TestCloneRefusesOptionLikeInput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*CloneSpec)
	}{
		{"url dash", func(s *CloneSpec) { s.URL = "--upload-pack=x" }},
		{"url newline", func(s *CloneSpec) { s.URL = "https://example.com/a\nb" }},
		{"url nul", func(s *CloneSpec) { s.URL = "https://example.com/a\x00b" }},
		{"url userinfo", func(s *CloneSpec) { s.URL = "https://user:pw@example.com/a.git" }},
		{"url ssh", func(s *CloneSpec) { s.URL = "ssh://example.com/a.git" }},
		{"url http", func(s *CloneSpec) { s.URL = "http://example.com/a.git" }},
		{"url ext", func(s *CloneSpec) { s.URL = "ext::sh -c id" }},
		{"url bare path", func(s *CloneSpec) { s.URL = "/srv/repo" }},
		{"base dash", func(s *CloneSpec) { s.BaseBranch = "-x" }},
		{"base newline", func(s *CloneSpec) { s.BaseBranch = "a\nb" }},
		{"base tilde", func(s *CloneSpec) { s.BaseBranch = "main~1" }},
		{"base colon", func(s *CloneSpec) { s.BaseBranch = "main:foo" }},
		{"base nul", func(s *CloneSpec) { s.BaseBranch = "a\x00b" }},
		{"commit dash", func(s *CloneSpec) { s.Commit = "-abcdef1" }},
		{"commit newline", func(s *CloneSpec) { s.Commit = "abcdef1\n" }},
		{"commit nul", func(s *CloneSpec) { s.Commit = "abcdef1\x00" }},
		{"commit short id", func(s *CloneSpec) { s.Commit = "abcdef1" }},
		{"commit uppercase", func(s *CloneSpec) { s.Commit = strings.Repeat("A", 40) }},
		{"commit not an id", func(s *CloneSpec) { s.Commit = "main" }},
		{"branch dash", func(s *CloneSpec) { s.Branch = "-b" }},
		{"branch newline", func(s *CloneSpec) { s.Branch = "a\nb" }},
		{"branch nul", func(s *CloneSpec) { s.Branch = "a\x00b" }},
		{"branch ref format", func(s *CloneSpec) { s.Branch = "a..b" }},
		{"branch revspec", func(s *CloneSpec) { s.Branch = "HEAD~1" }},
		{"branch previous", func(s *CloneSpec) { s.Branch = "@{-1}" }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			up := gitrepo.New(t, map[string]string{"a.txt": "one\n"})
			ctx, calls := wrapGit(t, "")
			s := spec(t, "file://"+up)
			tt.mutate(&s)

			_, err := Clone(ctx, s)
			require.Error(t, err)

			log, _ := os.ReadFile(calls)
			assert.NotContains(t, string(log), "fetch")
			assert.NotContains(t, string(log), "clone")
			assert.NoDirExists(t, s.MirrorDir)
		})
	}
}

// Not parallel: it mutates the package-level file switch.
func TestCloneRefusesFileURLsWhenSwitchIsOff(t *testing.T) {
	allowFileURLs = false

	t.Cleanup(func() { allowFileURLs = true })

	require.Error(t, checkURL("file:///srv/repo"))
	assert.NoError(t, checkURL("https://example.com/r.git"))
}

func TestNetEnvScopesCredentialToHost(t *testing.T) {
	t.Parallel()

	env := netEnv("https://forge.example:8443/o/r.git", "tok", "/ca.pem", "https")

	assert.Contains(t, env, "CAVU_GIT_TOKEN=tok")
	assert.Contains(t, env, "GIT_SSL_CAINFO=/ca.pem")
	assert.Contains(t, env, "GIT_CONFIG_KEY_0=credential.helper")
	assert.Contains(t, env, "GIT_CONFIG_VALUE_0=")
	assert.Contains(t, env, "GIT_CONFIG_KEY_1=credential.https://forge.example:8443.helper")
	assert.Contains(t, env, "GIT_CONFIG_COUNT=2")
	assert.NotContains(t, strings.Join(env, "\n"), "o/r.git")
}
