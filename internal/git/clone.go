package git

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// allowFileURLs lets file:// URLs through the scheme check. Only this
// package's tests turn it on, so no request can enable it.
var allowFileURLs bool

// Only a full object id names an object: git reads a shorter hex string as a
// branch name first, and a pushed branch could then pick the base commit.
var commitRE = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)

// CloneSpec says what to clone and where. The token and the CA bundle are not
// stored anywhere under MirrorDir or WorkspaceDir.
type CloneSpec struct {
	URL, Token, BaseBranch, Commit, Branch string // Commit "" = tip of BaseBranch
	MirrorDir, WorkspaceDir                string // absolute
	CABundle                               string // PEM, optional
	Name, Email                            string // commit identity set in the workspace
}

// Clone brings the bare mirror at MirrorDir up to date with URL, clones the
// workspace from it at the base commit, and checks out a new Branch there. The
// workspace keeps no remote and no local branch but Branch. It returns the base
// commit. Errors never contain the token; one from git may name the URL.
func Clone(ctx context.Context, s CloneSpec) (string, error) {
	if err := validate(ctx, s); err != nil {
		return "", err
	}

	caFile, cleanup, err := writeCA(s.CABundle)
	if err != nil {
		return "", err
	}
	defer cleanup()

	allowed := "https"
	if allowFileURLs {
		allowed += ":file"
	}

	// No call inherits the caller's working directory, which could be any
	// repository. The directories that hold the mirror and the workspace exist
	// before git creates them, and a refused spec must leave no mirror behind.
	if _, err := os.Stat(filepath.Join(s.MirrorDir, "HEAD")); err != nil {
		initMirror := call{dir: filepath.Dir(s.MirrorDir), args: []string{"init", "--bare", "--quiet", "--", s.MirrorDir}, noHooks: true}
		if _, err := run(ctx, initMirror); err != nil {
			return "", fmt.Errorf("create mirror: %w", err)
		}
	}

	fetch := call{
		dir:  s.MirrorDir,
		args: []string{"fetch", "--prune", "--no-tags", "--quiet", "--", s.URL, "+refs/heads/*:refs/heads/*"},
		env:  netEnv(s.URL, s.Token, caFile, allowed),
		net:  true, noHooks: true,
	}
	if _, err := run(ctx, fetch); err != nil {
		return "", fmt.Errorf("update mirror: %w", err)
	}

	rev := s.Commit
	if rev == "" {
		rev = "refs/heads/" + s.BaseBranch
	}

	out, err := run(ctx, call{dir: s.MirrorDir, args: []string{"rev-parse", "--verify", "--end-of-options", rev + "^{commit}"}, read: true})
	if err != nil {
		return "", fmt.Errorf("resolve base commit: %w", err)
	}

	base := strings.TrimSpace(string(out))

	ws := s.WorkspaceDir

	steps := []call{
		{dir: filepath.Dir(ws), args: []string{"clone", "--quiet", "--no-hardlinks", "--no-checkout", "--", s.MirrorDir, ws}},
		{dir: ws, args: []string{"checkout", "--quiet", "--detach", base}},
		{dir: ws, args: []string{"remote", "remove", "origin"}},
		{dir: ws, args: []string{"config", "user.name", s.Name}},
		{dir: ws, args: []string{"config", "user.email", s.Email}},
	}
	for _, c := range steps {
		c.noHooks = true
		if _, err := run(ctx, c); err != nil {
			return "", fmt.Errorf("clone workspace: %w", err)
		}
	}

	if err := freshBranch(ctx, s.WorkspaceDir, s.Branch); err != nil {
		return "", fmt.Errorf("clone workspace: %w", err)
	}

	return base, nil
}

// freshBranch deletes the branch the clone made from the mirror's HEAD, which
// depends on the container's init.defaultBranch and may sit at a newer commit,
// then checks out branch at the detached HEAD.
func freshBranch(ctx context.Context, ws, branch string) error {
	out, err := run(ctx, call{dir: ws, args: []string{"for-each-ref", "--format=%(refname)", "refs/heads"}, read: true})
	if err != nil {
		return err
	}

	for ref := range strings.FieldsSeq(string(out)) {
		if _, err := run(ctx, call{dir: ws, args: []string{"update-ref", "-d", ref}, noHooks: true}); err != nil {
			return err
		}
	}

	_, err = run(ctx, call{dir: ws, args: []string{"checkout", "--quiet", "-b", branch}, noHooks: true})

	return err
}

// validate refuses an input that could act as a git option or smuggle a
// control character, and a relative directory, before any git command that
// touches the network runs. It names the field and never echoes the value.
func validate(ctx context.Context, s CloneSpec) error {
	for _, f := range []struct{ name, v string }{
		{"url", s.URL},
		{"base branch", s.BaseBranch},
		{"branch", s.Branch},
		{"mirror dir", s.MirrorDir},
		{"workspace dir", s.WorkspaceDir},
		{"name", s.Name},
		{"email", s.Email},
	} {
		if err := plain(f.name, f.v, true); err != nil {
			return err
		}
	}

	if err := plain("commit", s.Commit, false); err != nil {
		return err
	}

	if s.Commit != "" && !commitRE.MatchString(s.Commit) {
		return errors.New("commit: not a hexadecimal object id")
	}

	if err := checkURL(s.URL); err != nil {
		return err
	}

	for _, f := range []struct{ name, v string }{{"base branch", s.BaseBranch}, {"branch", s.Branch}} {
		out, err := run(ctx, call{dir: filepath.Dir(s.MirrorDir), args: []string{"check-ref-format", "--branch", f.v}, read: true})
		if err != nil || strings.TrimSpace(string(out)) != f.v {
			return fmt.Errorf("%s: not a valid branch name", f.name)
		}
	}

	return nil
}

// plain refuses an empty value when required, a leading "-" and control
// characters (newline and NUL included).
func plain(name, v string, required bool) error {
	if v == "" && required {
		return fmt.Errorf("%s: required", name)
	}

	if strings.HasPrefix(v, "-") {
		return fmt.Errorf("%s: must not start with a dash", name)
	}

	for _, r := range v {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%s: contains a control character", name)
		}
	}

	return nil
}

func checkURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return errors.New("url: not a valid URL")
	}

	switch {
	case u.User != nil:
		return errors.New("url: must not carry credentials")
	case u.Scheme == "https" && u.Host != "":
		return nil
	case u.Scheme == "file" && allowFileURLs:
		return nil
	}

	return errors.New("url: only https is allowed")
}

// writeCA writes the system CA bundle, when one is readable, followed by pem to
// a private temporary file, and returns its path ("" when pem is empty) and a
// cleanup. git's CA file replaces curl's default, so the system roots are copied
// in and pem only adds trust.
func writeCA(pem string) (path string, cleanup func(), err error) {
	if pem == "" {
		return "", func() {}, nil
	}

	f, err := os.CreateTemp("", "cavu-ca-*.pem")
	if err != nil {
		return "", nil, fmt.Errorf("write ca bundle: %w", err)
	}

	cleanup = func() { _ = os.Remove(f.Name()) }

	system, _ := systemBundle()

	if _, err := f.Write(append(append(system, '\n'), pem...)); err != nil {
		_ = f.Close()

		cleanup()

		return "", nil, fmt.Errorf("write ca bundle: %w", err)
	}

	if err := f.Close(); err != nil {
		cleanup()

		return "", nil, fmt.Errorf("write ca bundle: %w", err)
	}

	return f.Name(), cleanup, nil
}
