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

func allowedProtocols() string {
	if allowFileURLs {
		return "https:file"
	}

	return "https"
}

// CloneSpec says what to clone and where. The token is not stored anywhere
// under MirrorDir or WorkspaceDir.
type CloneSpec struct {
	URL, Token, BaseBranch, Commit, Branch string // Commit "" = tip of BaseBranch
	MirrorDir, WorkspaceDir                string // absolute
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
		env:  netEnv(s.URL, s.Token, allowedProtocols()),
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

type field struct{ name, v string }

// inputs are the values of a spec that reach git or the network.
type inputs struct {
	url      string
	plain    []field // required
	commits  []field // full object ids, empty allowed
	branches []field
}

// validate refuses an input that could act as a git option or smuggle a
// control character before any git command that
// touches the network runs. It names the field and never echoes the value.
func validate(ctx context.Context, s CloneSpec) error {
	return inputs{
		url: s.URL,
		plain: []field{
			{"base branch", s.BaseBranch},
			{"branch", s.Branch},
			{"mirror dir", s.MirrorDir},
			{"workspace dir", s.WorkspaceDir},
			{"name", s.Name},
			{"email", s.Email},
		},
		commits:  []field{{"commit", s.Commit}},
		branches: []field{{"base branch", s.BaseBranch}, {"branch", s.Branch}},
	}.validate(ctx, filepath.Dir(s.MirrorDir))
}

// validate runs the checks every spec shares; dir exists, for check-ref-format.
func (in inputs) validate(ctx context.Context, dir string) error {
	for _, f := range append([]field{{"url", in.url}}, in.plain...) {
		if err := plain(f.name, f.v, true); err != nil {
			return err
		}
	}

	for _, f := range in.commits {
		if err := plain(f.name, f.v, false); err != nil {
			return err
		}

		if f.v != "" && !commitRE.MatchString(f.v) {
			return fmt.Errorf("%s: not a hexadecimal object id", f.name)
		}
	}

	if err := checkURL(in.url); err != nil {
		return err
	}

	for _, f := range in.branches {
		out, err := run(ctx, call{dir: dir, args: []string{"check-ref-format", "--branch", f.v}, read: true})
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
