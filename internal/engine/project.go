package engine

import (
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/mhersson/cavu/internal/store"
)

// EditProject writes a person's edit of a project. It keeps the stored
// baseline only when the edit leaves alone everything the baseline rests on,
// and never writes one of its own.
func EditProject(ctx context.Context, s Store, p store.Project) error {
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

// sameBaselineGround reports whether a and b agree on what a baseline rests on.
// Switches change no command, and an edit of the egress hosts adds a host far
// more often than it removes one a command needed.
func sameBaselineGround(a, b store.Project) bool {
	return a.RepoURL == b.RepoURL && a.BaseBranch == b.BaseBranch && a.SetupCommand == b.SetupCommand &&
		a.VerifyCommand == b.VerifyCommand && a.VerifyWaived == b.VerifyWaived &&
		maps.Equal(a.ToolOverrides, b.ToolOverrides) && maps.Equal(a.Env, b.Env) && slices.Equal(a.SecretVars, b.SecretVars)
}

// CheckProject prepares a project the way a run does, under run and task id 0,
// and stores what it found as the project's baseline. It also returns the
// verify command the repository suggests, or "", even with a runner error,
// which stores nothing.
func (l *Loop) CheckProject(ctx context.Context, projectID int64) (PrepareResult, string, error) {
	c, err := l.BuildRunConfig(ctx, store.Task{ProjectID: projectID}, store.Run{}, nil)
	if err != nil {
		return PrepareResult{}, "", err
	}

	res, suggestion, err := l.Runner.CheckEnvironment(ctx, c)
	if err != nil {
		return res, suggestion, err
	}

	p, err := l.Store.Project(ctx, projectID)
	if err == nil {
		p.Baseline = &store.Baseline{Commit: res.BaseCommit, Passed: res.Baseline == ResultPassed, At: l.Now(), Output: res.Output}
		if res.Baseline == ResultWaived {
			p.Baseline = nil // a waived verify ran no baseline
		}

		err = l.Store.UpdateProject(ctx, p)
	}

	if err != nil {
		return res, suggestion, fmt.Errorf("project %d: baseline: %w", projectID, err)
	}

	return res, suggestion, nil
}
