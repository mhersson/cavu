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
