// Package gitrepo builds git repositories and serves them over HTTP for tests.
// It runs the host's git and is never part of the shipped binary.
package gitrepo

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/cgi" //nolint:gosec // the httpoxy flaw is fixed in every supported Go
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// Main isolates a test binary's git from the machine's configuration and from
// any repository a hook or `rebase --exec` names, then runs its tests. Call it
// from TestMain.
func Main(m *testing.M) {
	names, err := inheritedRepoVars()
	if err != nil {
		panic(err)
	}

	for _, name := range names {
		if err := os.Unsetenv(name); err != nil {
			panic(err)
		}
	}

	dir, err := os.MkdirTemp("", "cavu-git-")
	if err != nil {
		panic(err)
	}

	env, err := isolation(dir)
	if err != nil {
		panic(err)
	}

	for k, v := range env {
		if err := os.Setenv(k, v); err != nil {
			panic(err)
		}
	}

	code := m.Run()

	_ = os.RemoveAll(dir)

	os.Exit(code)
}

// Isolate is Main's isolation for one test of a package whose TestMain does
// something else. The test cannot run in parallel.
func Isolate(t *testing.T) {
	t.Helper()

	names, err := inheritedRepoVars()
	if err != nil {
		t.Fatal(err)
	}

	for _, name := range names {
		t.Setenv(name, "") // restores the inherited value after the test

		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	}

	env, err := isolation(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	for k, v := range env {
		t.Setenv(k, v)
	}
}

// isolation writes a git configuration into dir and returns the variables
// that make git use it and nothing of the machine's.
func isolation(dir string) (map[string]string, error) {
	cfg := filepath.Join(dir, "gitconfig")
	if err := os.WriteFile(cfg, []byte("[maintenance]\n\tauto = false\n"), 0o600); err != nil {
		return nil, err
	}

	return map[string]string{
		"GIT_CONFIG_NOSYSTEM": "1",
		"GIT_ATTR_NOSYSTEM":   "1",
		"GIT_CONFIG_GLOBAL":   cfg,
		"XDG_CONFIG_HOME":     dir,
		"GIT_AUTHOR_NAME":     "Test",
		"GIT_AUTHOR_EMAIL":    "test@example.com",
		"GIT_COMMITTER_NAME":  "Test",
		"GIT_COMMITTER_EMAIL": "test@example.com",
	}, nil
}

// repoVars asks git once for its repository-local variables. Git answers this
// before it looks for a repository, so no inherited variable can steer the call.
var repoVars = sync.OnceValues(func() (map[string]bool, error) {
	out, err := exec.CommandContext(context.Background(), "git", "rev-parse", "--local-env-vars").Output()
	if err != nil {
		return nil, fmt.Errorf("git rev-parse --local-env-vars: %w", err)
	}

	vars := map[string]bool{}
	for name := range strings.FieldsSeq(string(out)) {
		vars[name] = true
	}

	if !vars["GIT_DIR"] || !vars["GIT_WORK_TREE"] || !vars["GIT_INDEX_FILE"] {
		return nil, fmt.Errorf("git rev-parse --local-env-vars lacks a repository variable: %q", out)
	}

	return vars, nil
})

// repoVar reports whether name is one of git's repository-local variables or a
// GIT_CONFIG_KEY_<n> or GIT_CONFIG_VALUE_<n>. Under a hook, `rebase --exec` or
// `bisect run` they name the operator's repository.
func repoVar(vars map[string]bool, name string) bool {
	return vars[name] || strings.HasPrefix(name, "GIT_CONFIG_KEY_") || strings.HasPrefix(name, "GIT_CONFIG_VALUE_")
}

// inheritedRepoVars names the repository variables set in this process.
func inheritedRepoVars() ([]string, error) {
	vars, err := repoVars()
	if err != nil {
		return nil, err
	}

	var names []string

	for _, kv := range os.Environ() {
		if name, _, _ := strings.Cut(kv, "="); repoVar(vars, name) {
			names = append(names, name)
		}
	}

	return names, nil
}

// cleanEnv returns inherited without git's repository variables. It never
// returns nil, because a nil Cmd.Env passes on the whole inherited environment.
func cleanEnv(t *testing.T, inherited []string) []string {
	t.Helper()

	vars, err := repoVars()
	if err != nil {
		t.Fatal(err)
	}

	env := make([]string, 0, len(inherited))

	for _, kv := range inherited {
		if name, _, _ := strings.Cut(kv, "="); !repoVar(vars, name) {
			env = append(env, kv)
		}
	}

	return env
}

// Git runs git in dir and returns its trimmed output; it fails the test on error.
func Git(t *testing.T, dir string, args ...string) string {
	t.Helper()

	var out, stderr bytes.Buffer

	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	cmd.Env = cleanEnv(t, os.Environ())
	cmd.Stdout = &out
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, stderr.String())
	}

	return strings.TrimSpace(out.String())
}

// Write writes files (path to content) under dir.
func Write(t *testing.T, dir string, files map[string]string) {
	t.Helper()

	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// New returns a repository holding files and one commit on main.
func New(t *testing.T, files map[string]string) (dir string) {
	t.Helper()

	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	Git(t, dir, "init", "-q", "-b", "main")
	Commit(t, dir, files, "init")

	return dir
}

// Commit writes files, commits them all and returns the new commit.
func Commit(t *testing.T, dir string, files map[string]string, msg string) string {
	t.Helper()

	Write(t, dir, files)
	Git(t, dir, "add", "--all")
	Git(t, dir, "commit", "-q", "-m", msg)

	return Git(t, dir, "rev-parse", "HEAD")
}

// Bare returns the file:// URL of a bare copy of the repository at from.
func Bare(t *testing.T, from string) (url string) {
	t.Helper()

	tmp := t.TempDir()
	dst := filepath.Join(tmp, "repo.git")
	Git(t, tmp, "clone", "-q", "--bare", from, dst)

	return "file://" + dst
}

// Handler serves the bare repository at bareDir over git's smart HTTP protocol
// at the path /<base name of bareDir>/. It is plain HTTP; mount it on a TLS
// server and wrap it for authentication.
func Handler(t *testing.T, bareDir string) http.Handler {
	t.Helper()

	path, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not found")
	}

	return &cgi.Handler{
		Path: path,
		Args: []string{"http-backend"},
		Env:  []string{"GIT_PROJECT_ROOT=" + filepath.Dir(bareDir), "GIT_HTTP_EXPORT_ALL=1"},
		InheritEnv: []string{
			"GIT_CONFIG_NOSYSTEM", "GIT_CONFIG_GLOBAL", "XDG_CONFIG_HOME", "HOME",
		},
	}
}

// AllowPush lets the smart HTTP backend take pushes into the bare repository at
// bareDir. Without it, a client sees a token that may read and not write.
func AllowPush(t *testing.T, bareDir string) {
	t.Helper()

	Git(t, bareDir, "config", "http.receivepack", "true")
}
