package engine

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"
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
	p.Caps = withoutEmptyCaps(p.Caps)
	if err := checkProject(ctx, s, p); err != nil {
		return store.Project{}, err
	}

	return s.CreateProject(ctx, p)
}

// EditProject writes a person's edit of a project that holds only what a
// project may hold. It keeps the stored baseline only when the edit leaves
// alone everything the baseline rests on, and never writes one of its own.
func EditProject(ctx context.Context, s Store, p store.Project) error {
	p.Caps = withoutEmptyCaps(p.Caps)
	if err := checkProject(ctx, s, p); err != nil {
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

// withoutEmptyCaps copies caps without the empty ones: a stored empty cap
// would lift the instance's setting to its default.
func withoutEmptyCaps(caps map[string]string) map[string]string {
	kept := maps.Clone(caps)
	maps.DeleteFunc(kept, func(_, v string) bool { return v == "" })

	return kept
}

// checkProject refuses what a project may not hold, naming the field: a
// value a person typed never reaches git as an option, nor a shell as a blank
// command, which exits 0.
func checkProject(ctx context.Context, s Store, p store.Project) error {
	switch {
	case !projectName.MatchString(p.Name):
		return refuse("name %q: want 1 to 40 of a-z, 0-9 and -", p.Name)
	case !strings.HasPrefix(p.RepoURL, "https://") || !plainArg(p.RepoURL):
		return refuse("repo_url %q: want an https:// URL with no whitespace", p.RepoURL)
	case !plainArg(p.BaseBranch):
		return refuse("base_branch %q: want a branch name that does not start with - and holds no whitespace", p.BaseBranch)
	case blank(p.SetupCommand):
		return refuse("setup_command is blank")
	case blank(p.VerifyCommand):
		return refuse("verify_command is blank")
	case p.VerifyCommand != "" && p.VerifyWaived:
		return refuse("verify_command and verify_waived: give one")
	}

	for k := range p.Env {
		if !varName.MatchString(k) {
			return refuse("env %q: want a variable name of letters, digits and _, not starting with a digit", k)
		}
	}

	for _, v := range p.SecretVars {
		if !varName.MatchString(v) {
			return refuse("secret_vars %q: want a variable name of letters, digits and _, not starting with a digit", v)
		}
	}

	for k, v := range p.ToolOverrides {
		if k == "" || v == "" {
			return refuse("tool_overrides %q=%q: want a tool name and a version", k, v)
		}
	}

	known := switchFields(&Switches{})

	for k := range p.Switches {
		if known[k] == nil {
			return refuse("switches %q: want continuations, verify_fix, web_tools, readiness, review or publish", k)
		}
	}

	for k, v := range p.Caps {
		if !slices.Contains(capKeys, k) {
			return refuse("caps %q: want %s", k, strings.Join(capKeys, " or "))
		}

		if err := CheckSetting(k, v); err != nil {
			return refuse("caps: %v", err)
		}
	}

	return checkCredential(ctx, s, p.ForgeCredential)
}

// checkCredential refuses a name no credential has; the foreign key holds it in a race.
func checkCredential(ctx context.Context, s Store, name string) error {
	if name == "" {
		return nil
	}

	creds, err := s.Credentials(ctx)
	if err != nil {
		return err
	}

	if !slices.ContainsFunc(creds, func(c store.Credential) bool { return c.Name == name }) {
		return refuse("forge_credential %q: no such credential", name)
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
// Switches, caps and the forge policy change no command, and an edit of the
// egress hosts adds a host far more often than it removes one a command needed.
func sameBaselineGround(a, b store.Project) bool {
	return a.RepoURL == b.RepoURL && a.BaseBranch == b.BaseBranch && a.SetupCommand == b.SetupCommand &&
		a.VerifyCommand == b.VerifyCommand && a.VerifyWaived == b.VerifyWaived &&
		maps.Equal(a.ToolOverrides, b.ToolOverrides) && maps.Equal(a.Env, b.Env) && slices.Equal(a.SecretVars, b.SecretVars)
}

// Runnable reports whether a project's tasks may start: a passed baseline or a waived verify.
func Runnable(p store.Project) bool {
	return p.VerifyWaived || p.Baseline != nil && p.Baseline.Passed
}

// RequestCheck asks for an environment check of the project. Every request
// carries the time it was made, so a check that started before it leaves it.
func (l *Loop) RequestCheck(ctx context.Context, projectID int64) error {
	p, err := l.Store.Project(ctx, projectID)
	if err != nil {
		return fmt.Errorf("project %d: %w", projectID, err)
	}

	at := l.Now()
	p.CheckRequestedAt = &at

	return l.Store.UpdateProject(ctx, p)
}

// CheckProject prepares a project the way a run does, under run and task id 0,
// for a person who runs the check: it answers no request, so it clears none.
// Whatever the outcome, it stores the project's check result, with any error
// it met as the fault, and stores the baseline it found unless it met an error
// or the project has no verify command. It also returns the verify command the
// repository suggests, or "", even with an error.
func (l *Loop) CheckProject(ctx context.Context, projectID int64) (PrepareResult, string, error) {
	started, res, suggestion, err := l.checkEnvironment(ctx, projectID)

	return res, suggestion, errors.Join(err, l.storeCheck(ctx, started, time.Time{}, res, suggestion, err))
}

var errChangedDuringCheck = errors.New("the project changed during the check, run it again")

// checkEnvironment runs the check and returns the project it started on, the
// one its config was built from, for storeCheck to compare with.
func (l *Loop) checkEnvironment(ctx context.Context, projectID int64) (store.Project, PrepareResult, string, error) {
	started, err := l.Store.Project(ctx, projectID)
	if err != nil {
		return started, PrepareResult{}, "", fmt.Errorf("project %d: %w", projectID, err)
	}

	c, err := l.runConfig(ctx, started, store.Task{ProjectID: projectID}, store.Run{}, nil)
	if err != nil {
		return started, PrepareResult{}, "", err
	}

	res, suggestion, err := l.Runner.CheckEnvironment(ctx, c)

	return started, res, suggestion, err
}

// storeCheck stores what a check found, or the error it met. It clears the
// request made at answers, the one the check started on, unless a later
// request replaced it. A zero answers clears none.
// An error leaves the baseline as it is, and so does a project with no verify
// command, since nothing was verified.
// A project that changed under the check, in what a baseline rests on, gets
// nothing stored and no request cleared: the result is of the old ground, and
// the request makes the next check run on the new.
func (l *Loop) storeCheck(ctx context.Context, started store.Project, answers time.Time, res PrepareResult, suggestion string, cause error) error {
	projectID := started.ID

	p, err := l.Store.Project(ctx, projectID)
	if err == nil && !sameBaselineGround(started, p) {
		return fmt.Errorf("project %d: %w", projectID, errChangedDuringCheck)
	}

	if err == nil {
		if r := p.CheckRequestedAt; r != nil && r.Equal(answers) {
			p.CheckRequestedAt = nil
		}

		p.CheckResult = store.CheckResult{
			Tools: res.Tools, IgnoredTools: res.IgnoredTools, BaseCommit: res.BaseCommit, FailedStep: res.FailedStep,
			Output: res.Output, VerifySuggestion: suggestion,
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

// ErrVolumesNotRemoved matches the error of a removal whose volumes the
// runtime would not remove; the project is left as it was. The error's text
// is the runtime's own.
var ErrVolumesNotRemoved = errors.New("volumes not removed")

type volumesError struct{ error }

func (e volumesError) Is(target error) bool { return target == ErrVolumesNotRemoved }

func (e volumesError) Unwrap() error { return e.error }

// RemoveProject removes a project: its cache and mirror volumes, then in one
// write the project's mark and its secrets. The tasks it has had, their
// events and artifacts stay. A removed or unknown project returns an error
// wrapping store.ErrNotFound; a task that is not done or cancelled, a replay's
// shadow included, is a refusal; a volume that cannot be removed leaves the
// project untouched and returns an error wrapping ErrVolumesNotRemoved.
func (l *Loop) RemoveProject(ctx context.Context, projectID int64) error {
	p, err := l.Store.Project(ctx, projectID)
	if err != nil {
		return fmt.Errorf("project %d: %w", projectID, err)
	}

	if p.RemovedAt != nil {
		return fmt.Errorf("project %d: %w", projectID, store.ErrNotFound)
	}

	if err := l.refuseOpenTasks(ctx, p); err != nil {
		return err
	}

	set, err := l.Store.Settings(ctx)
	if err != nil {
		return fmt.Errorf("project %s: %w", p.Name, err)
	}

	ref := RunRef{InstanceID: set["instance_id"], Image: l.Image, ProjectID: p.ID}
	if err := l.Runner.RemoveProject(ctx, ref); err != nil {
		return fmt.Errorf("project %s was not removed: %w; remove it again", p.Name, volumesError{err})
	}

	return l.Store.RemoveProject(ctx, p.ID, l.Now(), ProjectSecretName(p.Name, ""))
}

// refuseOpenTasks refuses while a task of p, a replay's shadow included, is
// neither done nor cancelled.
func (l *Loop) refuseOpenTasks(ctx context.Context, p store.Project) error {
	tasks, err := l.Store.Tasks(ctx, true)
	if err != nil {
		return fmt.Errorf("project %s: %w", p.Name, err)
	}

	var open []string

	for _, t := range tasks {
		if t.ProjectID != p.ID {
			continue
		}

		events, err := l.Store.EventsOmitting(ctx, t.ID, ProgressTypes...)
		if err != nil {
			return fmt.Errorf("task %d: %w", t.ID, err)
		}

		if s := StatusOf(Reduce(events)); s != StatusDone && s != StatusCancelled {
			open = append(open, fmt.Sprintf("%d (%s)", t.ID, s))
		}
	}

	if len(open) > 0 {
		return refuse("project %s has open tasks: %s; cancel them with cavu task cancel ID, or let them finish", p.Name, strings.Join(open, ", "))
	}

	return nil
}
