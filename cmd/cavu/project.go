package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/mhersson/cavu/internal/engine"
	"github.com/mhersson/cavu/internal/store"
)

func init() {
	commands["project"] = command{
		summary: "project add | set | list | show | check: add a project, change one, list them, show one, or check one's environment",
		run:     runProject,
	}
}

const (
	projectAddUsage = "cavu project add NAME --repo URL [--base BRANCH, default main] [--setup CMD] [--verify CMD | --no-verify] [--data-dir DIR]"
	projectSetUsage = "cavu project set NAME [--base BRANCH] [--setup CMD] [--verify CMD | --no-verify] [--egress HOST]... [--env K=V]...\n" +
		"         [--secret VAR]... [--tool NAME=VERSION]... [--switch NAME=on|off]... [--cap KEY=VALUE]... [--data-dir DIR]"
	projectListUsage  = "cavu project list [--data-dir DIR]"
	projectShowUsage  = "cavu project show NAME [--data-dir DIR]"
	projectCheckUsage = "cavu project check NAME [--data-dir DIR]"
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
		case "show":
			return projectShow(ctx, args[1:], stdout, stderr)
		case "check":
			return projectCheck(ctx, args[1:], stdout, stderr)
		}
	}

	usage := strings.Join([]string{projectAddUsage, projectSetUsage, projectListUsage, projectShowUsage, projectCheckUsage}, "\n       ")

	return newVerbFlags(usage, stderr).helpOr(args)
}

// projectFlags are the flags of project add and project set; only set has the lists.
type projectFlags struct {
	verbFlags

	base, setup, verify                         string
	noVerify                                    bool
	egress, env, secrets, tools, switches, caps listFlag
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
		f.Var(&f.switches, "switch", "continuations, verify_fix, web_tools, readiness or review, =on or =off; repeat for more")
		f.Var(&f.caps, "cap", "a cap KEY=VALUE over the setting for this project's tasks, task_max_turns or task_timeout_minutes; KEY= removes it; repeat for more")
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

// apply writes the flags given over p; a list flag replaces its field. What
// p may hold is the engine's to refuse.
func (f *projectFlags) apply(p *store.Project) error {
	given := f.given()

	// The engine reads an empty command as none, so an empty value would clear the field.
	switch {
	case given["setup"] && f.setup == "":
		return errors.New("--setup needs a command")
	case given["verify"] && f.verify == "":
		return errors.New("--verify needs a command; to waive verify, give --no-verify")
	}

	env, err := pairs("--env", f.env)
	if err != nil {
		return err
	}

	tools, err := pairs("--tool", f.tools)
	if err != nil {
		return err
	}

	caps, err := pairs("--cap", f.caps)
	if err != nil {
		return err
	}

	switches := map[string]bool{}

	for _, s := range f.switches {
		k, v, _ := strings.Cut(s, "=")
		if v != "on" && v != "off" {
			return fmt.Errorf("--switch %q: want NAME=on or NAME=off", s)
		}

		switches[k] = v == "on"
	}

	if given["base"] {
		p.BaseBranch = f.base
	}

	if given["setup"] {
		p.SetupCommand = f.setup
	}

	// Given together, the command and the waiver reach the engine, which refuses them.
	if given["verify"] || f.noVerify {
		p.VerifyCommand, p.VerifyWaived = f.verify, f.noVerify
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

	if len(caps) > 0 && p.Caps == nil {
		p.Caps = map[string]string{}
	}

	maps.Copy(p.Caps, caps)
	maps.DeleteFunc(p.Caps, func(_, v string) bool { return v == "" })

	return nil
}

// pairs splits NAME=VALUE values into a map.
func pairs(flagName string, values []string) (map[string]string, error) {
	m := make(map[string]string, len(values))

	for _, s := range values {
		k, v, found := strings.Cut(s, "=")
		if !found {
			return nil, fmt.Errorf("%s %q: want NAME=VALUE", flagName, s)
		}

		m[k] = v
	}

	return m, nil
}

func projectAdd(ctx context.Context, args []string, stderr io.Writer) error {
	f := newProjectFlags(projectAddUsage, stderr, false)
	repoURL := f.String("repo", "", "the repository's https:// URL")

	name, err := f.parse(args, 1)
	if err != nil {
		return err
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

	_, err = engine.AddProject(ctx, a.store, p)

	return err
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
		verify, baseline := "none", "none"

		switch {
		case p.VerifyWaived:
			verify = "waived"
		case p.VerifyCommand != "":
			verify = p.VerifyCommand
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

func projectShow(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	f := newVerbFlags(projectShowUsage, stderr)

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

	verify := "none"

	switch {
	case p.VerifyWaived:
		verify = "waived"
	case p.VerifyCommand != "":
		verify = p.VerifyCommand
	}

	baseline := "none"

	if b := p.Baseline; b != nil {
		baseline = "failed at " + b.Commit
		if b.Passed {
			baseline = "passed at " + b.Commit
		}
	}

	var switches []string

	env, tools, caps := assignments(p.Env), assignments(p.ToolOverrides), assignments(p.Caps)

	state := engine.ProjectSwitches(p)
	for _, k := range slices.Sorted(maps.Keys(state)) {
		switches = append(switches, k+"="+map[bool]string{true: "on", false: "off"}[state[k]])
	}

	list := func(vs []string) string {
		if len(vs) == 0 {
			return "none"
		}

		return strings.Join(vs, ", ")
	}

	fmt.Fprintf(stdout, "project:  %s\nrepo:     %s\nbase:     %s\nsetup:    %s\nverify:   %s\negress:   %s\nenv:      %s\nsecrets:  %s\ntools:    %s\nswitches: %s\ncaps:     %s\nbaseline: %s\n",
		p.Name, p.RepoURL, p.BaseBranch, cmp.Or(p.SetupCommand, "none"), verify, list(p.EgressHosts), list(env),
		list(p.SecretVars), list(tools), list(switches), list(caps), baseline)

	fmt.Fprintf(stdout, "detected: %s\nsuggests: %s\n", list(p.CheckResult.Tools), cmp.Or(p.CheckResult.VerifySuggestion, "none"))

	return nil
}

// assignments are m's entries as KEY=VALUE, sorted by key.
func assignments(m map[string]string) []string {
	var out []string

	for _, k := range slices.Sorted(maps.Keys(m)) {
		out = append(out, k+"="+m[k])
	}

	return out
}

func projectCheck(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	f := newVerbFlags(projectCheckUsage, stderr)

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

	// Every check runs under run and task id 0, and a task run removes the
	// containers it does not own: one process drives containers at a time.
	rt, err := a.drive(ctx)
	if a.serverHolds(err) {
		if p, err = a.checkedByServer(ctx, p); ctx.Err() != nil {
			fmt.Fprintf(stderr, "interrupted; the server carries on with the check of %s\n", name[0])

			return exitError{code: 130}
		}

		if err != nil {
			return err
		}

		res, err := storedCheck(p)

		return reportCheck(stdout, p, res, p.CheckResult.VerifySuggestion, err)
	}

	if err != nil {
		return err
	}

	// The runner masked the result and the error.
	res, suggestion, err := a.loop(rt).CheckProject(ctx, p.ID)

	return reportCheck(stdout, p, res, suggestion, err)
}

// checkedByServer asks the server for a check of p and returns p once the
// server has cleared the request, which it does at the end of a check that
// started after the request.
func (a *app) checkedByServer(ctx context.Context, p store.Project) (store.Project, error) {
	if err := a.actions().RequestCheck(ctx, p.ID); err != nil {
		return p, err
	}

	tick := time.NewTicker(pollEvery)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			return p, ctx.Err()
		case <-tick.C:
		}

		now, err := a.store.Project(ctx, p.ID)
		if err != nil || now.CheckRequestedAt == nil {
			return now, err
		}
	}
}

// storedCheck is the newest check's result as the project stores it, with its
// fault as the error and the baseline's result as the baseline's.
func storedCheck(p store.Project) (engine.PrepareResult, error) {
	c := p.CheckResult
	if c.Fault != "" {
		return engine.PrepareResult{}, errors.New(c.Fault)
	}

	res := engine.PrepareResult{
		BaseCommit: c.BaseCommit, Tools: c.Tools, IgnoredTools: c.IgnoredTools, FailedStep: c.FailedStep, Output: c.Output,
	}

	switch b := p.Baseline; {
	case b != nil && b.Passed:
		res.Baseline = engine.ResultPassed
	case b != nil:
		res.Baseline = engine.ResultFailed
	case p.VerifyWaived:
		res.Baseline = engine.ResultWaived
	}

	return res, nil
}

// reportCheck prints a check's result, and the suggestion a project without a
// verify command needs. It fails unless the baseline passed or verify is waived.
func reportCheck(w io.Writer, p store.Project, res engine.PrepareResult, suggestion string, err error) error {
	if err == nil {
		printCheck(w, res)
	}

	if p.VerifyCommand == "" && !p.VerifyWaived {
		printSuggestion(w, p.Name, suggestion)
	}

	switch {
	case err != nil:
		return err
	case res.Baseline != engine.ResultPassed && res.Baseline != engine.ResultWaived:
		return exitError{code: 1}
	}

	return nil
}

func printCheck(w io.Writer, res engine.PrepareResult) {
	fmt.Fprintf(w, "base commit:   %s\n", res.BaseCommit)
	fmt.Fprintf(w, "tools:         %s\n", strings.Join(res.Tools, " "))
	fmt.Fprintf(w, "ignored tools: %s\n", strings.Join(res.IgnoredTools, " "))

	if res.FailedStep != "" {
		fmt.Fprintf(w, "failed step:   %s\n%s\n", res.FailedStep, strings.TrimRight(res.Output, "\n"))

		return
	}

	if res.Baseline != "" {
		fmt.Fprintf(w, "baseline:      %s\n", res.Baseline)
	}
}

// printSuggestion prints the line that sets the suggested verify command, or
// waives verify when the repository suggests none.
func printSuggestion(w io.Writer, name, suggestion string) {
	if suggestion == "" {
		fmt.Fprintf(w, "no verify command to suggest; set one with --verify, or waive verify:\n  cavu project set %s --no-verify\n", name)

		return
	}

	fmt.Fprintf(w, "suggested verify command:\n  cavu project set %s --verify '%s'\n", name, suggestion)
}
