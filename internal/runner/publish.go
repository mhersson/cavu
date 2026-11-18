package runner

import (
	"cmp"
	"context"
	"errors"

	"github.com/mhersson/cavu/internal/engine"
	"github.com/mhersson/cavu/internal/forge"
	"github.com/mhersson/cavu/internal/sidecar"
)

// Publish imports the change's bundle into the project's mirror, pushes its
// head to the task's branch and, for a first publish, opens a draft pull
// request. It works through the run's sandbox and brings no phase up. Each part
// is safe to repeat, so a retry finishes a publish that failed half way.
func (r *Runner) Publish(ctx context.Context, c engine.RunConfig, p engine.PublishRequest) (engine.PublishResult, error) {
	st := r.state(c.RunID)
	if st == nil {
		sb, err := r.sandbox(refOf(c))
		if err == nil {
			err = sb.EnsureVolumes(ctx)
		}

		if err != nil {
			return engine.PublishResult{}, maskErr(err, masker(c))
		}

		// Not kept: only Prepare and ensureUp add a run, with its ecosystems.
		st = &run{sb: sb}
	}

	res, token, err := r.publish(ctx, st, c, p)

	// A token the App minted is in no config: it joins this action's mask alone.
	mask := masker(c, token)
	res.Rejected = mask(res.Rejected)

	return res, maskErr(err, mask)
}

// publish carries out Publish and returns the push token it used.
func (r *Runner) publish(ctx context.Context, st *run, c engine.RunConfig, p engine.PublishRequest) (engine.PublishResult, string, error) {
	var files []engine.ChangedFile

	problem, err := st.imported(ctx, c, p.Bundle, p.Head, cmp.Or(p.Prev, c.Repo.BaseCommit), func(imp container) (problem string, err error) {
		files, _, problem, err = changeList(ctx, imp, c, p.Head)

		return problem, err
	})
	if err != nil || problem != "" {
		return engine.PublishResult{Rejected: problem}, "", err
	}

	use := forge.ForPush
	if len(engine.Flag(files).Workflows) > 0 {
		use = forge.ForWorkflowPush
	}

	token, err := repoToken(ctx, r.forge, c.Repo.URL, c.Repo.Access, use)
	if refused, ok := errors.AsType[*forge.TokenRefusedError](err); ok {
		return engine.PublishResult{Rejected: refused.Error()}, "", nil
	}

	if err != nil {
		return engine.PublishResult{}, "", err
	}

	branch, rejected, err := st.sb.Push(ctx, sidecar.PushRequest{
		URL: c.Repo.URL, Token: token, Ref: taskRef(c.TaskID), Commit: p.Head, Branch: p.Branch, Expect: p.Prev,
	})
	if err != nil || rejected != "" {
		return engine.PublishResult{Rejected: rejected}, token, err
	}

	res := engine.PublishResult{Branch: branch, Head: p.Head, Number: p.Number}
	if p.Number != 0 {
		return res, token, nil
	}

	repo, err := forge.ParseRepo(c.Repo.URL)
	if err != nil {
		return engine.PublishResult{}, token, err
	}

	pr, err := r.forge.OpenPR(ctx, repo, forge.Access(c.Repo.Access), forge.NewPR{
		Title: p.Title, Body: p.Body, Base: c.Repo.BaseBranch, Head: branch,
	})
	if err != nil {
		return engine.PublishResult{}, token, err
	}

	res.Number, res.URL, res.NodeID = pr.Number, pr.URL, pr.NodeID

	return res, token, nil
}
