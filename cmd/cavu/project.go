package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"regexp"
	"strings"
	"text/tabwriter"
	"unicode"

	"github.com/mhersson/cavu/internal/engine"
	"github.com/mhersson/cavu/internal/store"
)

func init() {
	commands["project"] = command{
		summary: "project add | set | list: add a project, change one, or list them",
		run:     runProject,
	}
}

const (
	projectAddUsage = "cavu project add NAME --repo URL [--base BRANCH, default main] [--setup CMD] [--verify CMD | --no-verify] [--data-dir DIR]"
	projectSetUsage = "cavu project set NAME [--base BRANCH] [--setup CMD] [--verify CMD | --no-verify] [--egress HOST]... [--env K=V]...\n" +
		"         [--secret VAR]... [--tool NAME=VERSION]... [--switch NAME=on|off]... [--data-dir DIR]"
	projectListUsage = "cavu project list [--data-dir DIR]"
)

var (
	projectName = regexp.MustCompile(`^[a-z0-9-]{1,40}$`)
	varName     = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

func runProject(ctx context.Context, args []string, _ io.Reader, stdout, stderr io.Writer) error {
	if len(args) > 0 {
		switch args[0] {
		case "add":
			return projectAdd(ctx, args[1:], stderr)
		case "set":
			return projectSet(ctx, args[1:], stderr)
		case "list":
			return projectList(ctx, args[1:], stdout, stderr)
		}
	}

	usage := strings.Join([]string{projectAddUsage, projectSetUsage, projectListUsage}, "\n       ")

	return newVerbFlags(usage, stderr).usageError()
}

// projectFlags are the flags of project add and project set; only set has the lists.
type projectFlags struct {
	verbFlags

	base, setup, verify                   string
	noVerify                              bool
	egress, env, secrets, tools, switches listFlag
}

func newProjectFlags(usage string, stderr io.Writer, lists bool) *projectFlags {
	f := &projectFlags{verbFlags: newVerbFlags(usage, stderr)}
	f.StringVar(&f.base, "base", "", "the branch tasks start from")
	f.StringVar(&f.setup, "setup", "", "the command that prepares the workspace, such as installing dependencies")
	f.StringVar(&f.verify, "verify", "", "the command whose exit code decides whether a change passes")
	f.BoolVar(&f.noVerify, "no-verify", false, "waive the verify command: a person checks every change")

	if lists {
		f.Var(&f.egress, "egress", "a host the setup command, the agent and verify may reach; repeat for more, '' alone for none")
		f.Var(&f.env, "env", "a variable K=V for the setup command; repeat for more, '' alone for none")
		f.Var(&f.secrets, "secret", "a variable for the setup command from the secret project.NAME.VAR; repeat for more, '' alone for none")
		f.Var(&f.tools, "tool", "a tool version NAME=VERSION over the repository's; repeat for more, '' alone for none")
		f.Var(&f.switches, "switch", "continuations, verify_fix or web_tools, =on or =off; repeat for more")
	}

	return f
}

// listFlag collects a repeated flag's values. An empty value adds none, so
// one given alone clears the field.
type listFlag []string

func (l *listFlag) String() string { return strings.Join(*l, " ") }

func (l *listFlag) Set(v string) error {
	if v != "" {
		*l = append(*l, v)
	}

	return nil
}

// apply writes the flags given over p, or refuses the first value p cannot
// use, naming its flag. A list flag replaces its field.
func (f *projectFlags) apply(p *store.Project) error {
	given := f.given()

	switch {
	case given["base"] && !plainArg(f.base):
		return fmt.Errorf("--base %q: want a branch name that does not start with - and holds no whitespace", f.base)
	case given["setup"] && strings.TrimSpace(f.setup) == "":
		return errors.New("--setup is blank")
	case given["verify"] && f.noVerify:
		return errors.New("--verify and --no-verify: give one")
	case given["verify"] && strings.TrimSpace(f.verify) == "":
		return errors.New("--verify is blank; to waive verify, give --no-verify")
	}

	env, err := pairs("--env", "NAME=VALUE, NAME of letters, digits and _, not starting with a digit", f.env,
		func(k, _ string) bool { return varName.MatchString(k) })
	if err != nil {
		return err
	}

	tools, err := pairs("--tool", "NAME=VERSION", f.tools, func(k, v string) bool { return k != "" && v != "" })
	if err != nil {
		return err
	}

	for _, v := range f.secrets {
		if !varName.MatchString(v) {
			return fmt.Errorf("--secret %q: want a variable name of letters, digits and _, not starting with a digit", v)
		}
	}

	switches := map[string]bool{}

	for _, s := range f.switches {
		k, v, _ := strings.Cut(s, "=")
		if !engine.IsSwitch(k) || v != "on" && v != "off" {
			return fmt.Errorf("--switch %q: want continuations, verify_fix or web_tools, then =on or =off", s)
		}

		switches[k] = v == "on"
	}

	if given["base"] {
		p.BaseBranch = f.base
	}

	if given["setup"] {
		p.SetupCommand = f.setup
	}

	if given["verify"] {
		p.VerifyCommand, p.VerifyWaived = f.verify, false
	}

	if f.noVerify {
		p.VerifyCommand, p.VerifyWaived = "", true
	}

	if given["egress"] {
		p.EgressHosts = f.egress
	}

	if given["env"] {
		p.Env = env
	}

	if given["secret"] {
		p.SecretVars = f.secrets
	}

	if given["tool"] {
		p.ToolOverrides = tools
	}

	if len(switches) > 0 && p.Switches == nil {
		p.Switches = map[string]bool{}
	}

	maps.Copy(p.Switches, switches)

	return nil
}

// pairs reads K=V values into a map, each pair checked by ok.
func pairs(flagName, want string, values []string, ok func(k, v string) bool) (map[string]string, error) {
	m := make(map[string]string, len(values))

	for _, s := range values {
		k, v, found := strings.Cut(s, "=")
		if !found || !ok(k, v) {
			return nil, fmt.Errorf("%s %q: want %s", flagName, s, want)
		}

		m[k] = v
	}

	return m, nil
}

// plainArg reports whether s reaches git as one plain argument: not empty,
// not an option, and free of whitespace and control characters.
func plainArg(s string) bool {
	return s != "" && !strings.HasPrefix(s, "-") &&
		!strings.ContainsFunc(s, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) })
}

func projectAdd(ctx context.Context, args []string, stderr io.Writer) error {
	f := newProjectFlags(projectAddUsage, stderr, false)
	repoURL := f.String("repo", "", "the repository's https:// URL")

	name, err := f.parse(args, 1)
	if err != nil {
		return err
	}

	switch {
	case !projectName.MatchString(name[0]):
		return fmt.Errorf("NAME %q: want 1 to 40 of a-z, 0-9 and -", name[0])
	case !strings.HasPrefix(*repoURL, "https://") || !plainArg(*repoURL):
		return fmt.Errorf("--repo %q: want an https:// URL with no whitespace", *repoURL)
	}

	p := store.Project{Name: name[0], RepoURL: *repoURL, BaseBranch: "main"}
	if err := f.apply(&p); err != nil {
		return err
	}

	a, err := f.open(ctx)
	if err != nil {
		return err
	}
	defer a.close()

	if _, err := a.store.CreateProject(ctx, p); errors.Is(err, store.ErrConflict) {
		return fmt.Errorf("NAME %q: a project of that name exists", p.Name)
	} else if err != nil {
		return err
	}

	return nil
}

func projectSet(ctx context.Context, args []string, stderr io.Writer) error {
	f := newProjectFlags(projectSetUsage, stderr, true)

	name, err := f.parse(args, 1)
	if err != nil {
		return err
	}

	a, err := f.open(ctx)
	if err != nil {
		return err
	}
	defer a.close()

	p, err := a.store.ProjectByName(ctx, name[0])
	if err != nil {
		return fmt.Errorf("project %q: %w", name[0], err)
	}

	if err := f.apply(&p); err != nil {
		return err
	}

	return engine.EditProject(ctx, a.store, p)
}

func projectList(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	f := newVerbFlags(projectListUsage, stderr)
	if _, err := f.parse(args, 0); err != nil {
		return err
	}

	a, err := f.open(ctx)
	if err != nil {
		return err
	}
	defer a.close()

	ps, err := a.store.Projects(ctx)
	if err != nil {
		return err
	}

	w := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tREPOSITORY\tBASE\tVERIFY\tBASELINE")

	for _, p := range ps {
		verify, baseline := p.VerifyCommand, "none"

		switch {
		case p.VerifyWaived:
			verify = "waived"
		case verify == "":
			verify = "none"
		}

		if b := p.Baseline; b != nil {
			baseline = "failed"
			if b.Passed {
				baseline = "passed"
			}
		}

		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", p.Name, p.RepoURL, p.BaseBranch, verify, baseline)
	}

	return w.Flush()
}
