package engine

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"unicode"

	"github.com/mhersson/cavu/internal/store"
)

var (
	projectName = regexp.MustCompile(`^[a-z0-9-]{1,40}$`)
	varName     = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// AddProject stores a new project that holds only what a project may hold.
// A duplicate name returns an error wrapping store.ErrConflict.
func AddProject(ctx context.Context, s Store, p store.Project) (store.Project, error) {
	if err := checkProject(p); err != nil {
		return store.Project{}, err
	}

	return s.CreateProject(ctx, p)
}

// EditProject writes a person's edit of a project that holds only what a
// project may hold. It keeps the stored baseline only when the edit leaves
// alone everything the baseline rests on, and never writes one of its own.
func EditProject(ctx context.Context, s Store, p store.Project) error {
	if err := checkProject(p); err != nil {
		return err
	}

	old, err := s.Project(ctx, p.ID)
	if err != nil {
		return fmt.Errorf("project %d: %w", p.ID, err)
	}

	p.Baseline = old.Baseline
	if !sameBaselineGround(old, p) {
		p.Baseline = nil
	}

	return s.UpdateProject(ctx, p)
}

// checkProject refuses what a project may not hold, naming the field: a
// value a person typed never reaches git as an option, nor a shell as a blank
// command, which exits 0.
func checkProject(p store.Project) error {
	switch {
	case !projectName.MatchString(p.Name):
		return fmt.Errorf("name %q: want 1 to 40 of a-z, 0-9 and -", p.Name)
	case !strings.HasPrefix(p.RepoURL, "https://") || !plainArg(p.RepoURL):
		return fmt.Errorf("repo_url %q: want an https:// URL with no whitespace", p.RepoURL)
	case !plainArg(p.BaseBranch):
		return fmt.Errorf("base_branch %q: want a branch name that does not start with - and holds no whitespace", p.BaseBranch)
	case blank(p.SetupCommand):
		return errors.New("setup_command is blank")
	case blank(p.VerifyCommand):
		return errors.New("verify_command is blank")
	case p.VerifyCommand != "" && p.VerifyWaived:
		return errors.New("verify_command and verify_waived: give one")
	}

	for k := range p.Env {
		if !varName.MatchString(k) {
			return fmt.Errorf("env %q: want a variable name of letters, digits and _, not starting with a digit", k)
		}
	}

	for _, v := range p.SecretVars {
		if !varName.MatchString(v) {
			return fmt.Errorf("secret_vars %q: want a variable name of letters, digits and _, not starting with a digit", v)
		}
	}

	for k, v := range p.ToolOverrides {
		if k == "" || v == "" {
			return fmt.Errorf("tool_overrides %q=%q: want a tool name and a version", k, v)
		}
	}

	known := switchFields(&Switches{})

	for k := range p.Switches {
		if known[k] == nil {
			return fmt.Errorf("switches %q: want continuations, verify_fix or web_tools", k)
		}
	}

	return nil
}

// plainArg reports whether s reaches git as one plain argument: not empty,
// not an option, and free of whitespace and control characters.
func plainArg(s string) bool {
	return s != "" && !strings.HasPrefix(s, "-") &&
		!strings.ContainsFunc(s, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) })
}

// blank reports a command that is set but holds only whitespace; an empty one is none.
func blank(command string) bool {
	return command != "" && strings.TrimSpace(command) == ""
}

// sameBaselineGround reports whether a and b agree on what a baseline rests on.
// Switches change no command, and an edit of the egress hosts adds a host far
// more often than it removes one a command needed.
func sameBaselineGround(a, b store.Project) bool {
	return a.RepoURL == b.RepoURL && a.BaseBranch == b.BaseBranch && a.SetupCommand == b.SetupCommand &&
		a.VerifyCommand == b.VerifyCommand && a.VerifyWaived == b.VerifyWaived &&
		maps.Equal(a.ToolOverrides, b.ToolOverrides) && maps.Equal(a.Env, b.Env) && slices.Equal(a.SecretVars, b.SecretVars)
}

// Runnable reports whether a project's tasks may start: a passed baseline or a waived verify.
func Runnable(p store.Project) bool {
	return p.VerifyWaived || p.Baseline != nil && p.Baseline.Passed
}

// RequestCheck asks for an environment check of the project. A request still
// waiting keeps its time, so the oldest request runs first.
func (l *Loop) RequestCheck(ctx context.Context, projectID int64) error {
	p, err := l.Store.Project(ctx, projectID)
	if err != nil {
		return fmt.Errorf("project %d: %w", projectID, err)
	}

	if p.CheckRequestedAt != nil {
		return nil
	}

	at := l.Now()
	p.CheckRequestedAt = &at

	return l.Store.UpdateProject(ctx, p)
}

// CheckProject prepares a project the way a run does, under run and task id 0.
// Whatever the outcome, it stores the project's check result, with any error
// it met as the fault, clears the check request, and stores the baseline it
// found unless it met an error or the project has no verify command. It also
// returns the verify command the repository suggests, or "", even with an error.
func (l *Loop) CheckProject(ctx context.Context, projectID int64) (PrepareResult, string, error) {
	res, suggestion, err := l.checkEnvironment(ctx, projectID)

	return res, suggestion, errors.Join(err, l.storeCheck(ctx, projectID, res, suggestion, err))
}

func (l *Loop) checkEnvironment(ctx context.Context, projectID int64) (PrepareResult, string, error) {
	c, err := l.BuildRunConfig(ctx, store.Task{ProjectID: projectID}, store.Run{}, nil)
	if err != nil {
		return PrepareResult{}, "", err
	}

	return l.Runner.CheckEnvironment(ctx, c)
}

// storeCheck stores what a check found, or the error it met, and clears the
// project's check request. An error leaves the baseline as it is, and so does
// a project with no verify command, since nothing was verified.
func (l *Loop) storeCheck(ctx context.Context, projectID int64, res PrepareResult, suggestion string, cause error) error {
	p, err := l.Store.Project(ctx, projectID)
	if err == nil {
		p.CheckRequestedAt = nil
		p.CheckResult = store.CheckResult{
			Tools: res.Tools, IgnoredTools: res.IgnoredTools, FailedStep: res.FailedStep, VerifySuggestion: suggestion,
		}

		switch {
		case cause != nil:
			p.CheckResult.Fault = cause.Error()
		case p.VerifyWaived || strings.TrimSpace(p.VerifyCommand) != "":
			p.Baseline = nil // a waived verify runs none, and drops what an earlier check stored
			if res.Baseline != ResultWaived {
				p.Baseline = &store.Baseline{Commit: res.BaseCommit, Passed: res.Baseline == ResultPassed, At: l.Now(), Output: res.Output}
			}
		}

		err = l.Store.UpdateProject(ctx, p)
	}

	if err != nil {
		return fmt.Errorf("project %d: check result: %w", projectID, err)
	}

	return nil
}
