package git

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// freeNames is how many names a first push tries: the branch and its -2 to -9.
const freeNames = 9

// PushSpec says what to push from the mirror and to which branch. Ref is the
// mirror's ref for the task and Commit the commit it must be at. Expect is the
// head CAVU published before, "" for a first push.
type PushSpec struct {
	URL, Token, MirrorDir string
	Ref, Commit           string
	Branch, Expect        string
}

// PushResult names the branch the commit is on. Rejected says why the forge
// did not take it, "" when it did.
type PushResult struct{ Branch, Rejected string }

// Push puts Commit on Branch at URL, never forced. A first push takes the first
// free name when Branch is someone else's. A forge that refuses the push is a
// rejection; an error means the forge could not be asked.
func Push(ctx context.Context, s PushSpec) (PushResult, error) {
	err := inputs{
		url:      s.URL,
		plain:    []field{{"branch", s.Branch}, {"mirror dir", s.MirrorDir}, {"ref", s.Ref}, {"commit", s.Commit}},
		commits:  []field{{"commit", s.Commit}, {"expect", s.Expect}},
		branches: []field{{"branch", s.Branch}},
	}.validate(ctx, filepath.Dir(s.MirrorDir))
	if err != nil {
		return PushResult{}, err
	}

	out, err := run(ctx, call{dir: s.MirrorDir, args: []string{"rev-parse", "--verify", "--end-of-options", s.Ref + "^{commit}"}, read: true})
	if err != nil {
		return PushResult{}, fmt.Errorf("resolve ref: %w", err)
	}

	if strings.TrimSpace(string(out)) != s.Commit {
		return PushResult{}, errors.New("the mirror's ref is not at the commit")
	}

	heads, err := forgeHeads(ctx, s)
	if err != nil {
		return PushResult{}, err
	}

	target, rejected := choose(s, heads)
	if rejected != "" {
		return PushResult{Branch: s.Branch, Rejected: rejected}, nil
	}

	if heads[target] == s.Commit {
		return PushResult{Branch: target}, nil
	}

	return push(ctx, s, target)
}

// forgeHeads asks the forge where the branch and its numbered names are. ls-remote
// matches patterns on the tail of a ref, so a name is looked up exactly.
func forgeHeads(ctx context.Context, s PushSpec) (map[string]string, error) {
	args := []string{"ls-remote", "--heads", "--", s.URL}

	for _, name := range names(s.Branch) {
		args = append(args, "refs/heads/"+name)
	}

	out, err := run(ctx, call{dir: s.MirrorDir, args: args, env: netEnv(s.URL, s.Token, allowedProtocols()), net: true, noHooks: true, read: true})
	if err != nil {
		return nil, fmt.Errorf("ask the forge for the branch: %w", err)
	}

	heads := map[string]string{}

	for line := range strings.SplitSeq(string(out), "\n") {
		if sha, ref, ok := strings.Cut(line, "\t"); ok {
			heads[strings.TrimPrefix(ref, "refs/heads/")] = sha
		}
	}

	return heads, nil
}

func names(branch string) []string {
	all := []string{branch}
	for n := 2; n <= freeNames; n++ {
		all = append(all, branch+"-"+strconv.Itoa(n))
	}

	return all
}

// choose picks the branch to push to, or says why there is none.
func choose(s PushSpec, heads map[string]string) (target, rejected string) {
	if s.Expect != "" {
		if h, ok := heads[s.Branch]; !ok || (h != s.Expect && h != s.Commit) {
			return "", "the branch moved since it was pushed"
		}

		return s.Branch, ""
	}

	for _, name := range names(s.Branch) {
		if h, ok := heads[name]; !ok || h == s.Commit {
			return name, ""
		}
	}

	return "", fmt.Sprintf("%s and %s-2 to %s-%d are all taken", s.Branch, s.Branch, s.Branch, freeNames)
}

// push never forces: no --force, no lease, no leading "+" in the refspec.
func push(ctx context.Context, s PushSpec, branch string) (PushResult, error) {
	ref := "refs/heads/" + branch
	out, err := run(ctx, call{
		dir:  s.MirrorDir,
		args: []string{"push", "--porcelain", "--", s.URL, s.Commit + ":" + ref},
		env:  netEnv(s.URL, s.Token, allowedProtocols()),
		net:  true, noHooks: true,
	})

	var exit *exec.ExitError

	switch {
	case err == nil:
		return PushResult{Branch: branch}, nil
	case ctx.Err() != nil || !errors.As(err, &exit):
		return PushResult{}, err
	}

	return PushResult{Branch: branch, Rejected: refStatus(string(out), ref, err, exit)}, nil
}

// refStatus is git's status for ref in --porcelain output, else the first line
// of git's stderr that run put after the exit status.
func refStatus(porcelain, ref string, err error, exit *exec.ExitError) string {
	for line := range strings.SplitSeq(porcelain, "\n") {
		if flag, rest, ok := strings.Cut(line, "\t"); ok && flag == "!" {
			if to, status, ok := strings.Cut(rest, "\t"); ok && strings.HasSuffix(to, ":"+ref) {
				return strings.TrimSpace(status)
			}
		}
	}

	if _, line, ok := strings.Cut(err.Error(), exit.Error()+": "); ok {
		return line
	}

	return exit.Error()
}
