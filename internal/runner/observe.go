package runner

import (
	"context"
	"errors"

	"github.com/mhersson/cavu/internal/engine"
	"github.com/mhersson/cavu/internal/forge"
)

// Observe reads pull request number: its state, its head's checks but those
// the project ignores, and the requests of the reviewers it allows.
func (r *Runner) Observe(ctx context.Context, c engine.RunConfig, number int) (engine.Observation, error) {
	var o forge.Observation

	err := forgeCall(c, func(repo forge.Repo, a forge.Access) (err error) {
		o, err = r.forge.Observe(ctx, repo, a, forge.Query{Number: number, IgnoredChecks: c.Repo.IgnoredChecks, ReviewBots: c.Repo.ReviewBots})

		return err
	})
	if err != nil {
		return engine.Observation{}, err
	}

	out := engine.Observation{State: o.State, Draft: o.Draft, Head: o.Head, Conflict: o.Conflict, Checks: o.Checks, Failed: o.Failed}
	for _, cm := range o.Comments {
		out.Comments = append(out.Comments, engine.Comment(cm))
	}

	return out, nil
}

// Ready marks the pull request with node id nodeID ready for review.
func (r *Runner) Ready(ctx context.Context, c engine.RunConfig, nodeID string) error {
	return forgeCall(c, func(repo forge.Repo, a forge.Access) error { return r.forge.Ready(ctx, repo, a, nodeID) })
}

// Reply posts body, its secrets masked, as one comment on pull request number.
func (r *Runner) Reply(ctx context.Context, c engine.RunConfig, number int, body string) error {
	body = masker(c)(body)

	return forgeCall(c, func(repo forge.Repo, a forge.Access) error { return r.forge.Comment(ctx, repo, a, number, body) })
}

// forgeCall calls the forge for the run's repository with its access. A rate
// limit becomes the engine's, and every error is masked.
func forgeCall(c engine.RunConfig, call func(forge.Repo, forge.Access) error) error {
	repo, err := forge.ParseRepo(c.Repo.URL)
	if err == nil {
		err = call(repo, forge.Access(c.Repo.Access))
	}

	if limit, ok := errors.AsType[*forge.RateLimitError](err); ok {
		err = &engine.RateLimited{Until: limit.Reset}
	}

	return maskErr(err, masker(c))
}
