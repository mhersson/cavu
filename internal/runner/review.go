package runner

import (
	"context"
	"encoding/json"
	"slices"

	"github.com/mhersson/cavu/internal/driver"
	"github.com/mhersson/cavu/internal/engine"
	"github.com/mhersson/cavu/internal/proc"
	"github.com/mhersson/cavu/internal/prompts"
	"github.com/mhersson/cavu/internal/sandbox"
)

// The diffs a review reads, on the tmpfs of the phase container: the task's
// change, and a scoped review's fix since the head the previous review saw.
const (
	reviewPatch = "/run/cavu/review.patch"
	fixPatch    = "/run/cavu/fix.patch"
)

// diffOptions keep git from running a program the repository names to render
// a file, and show a submodule's change in full.
const diffOptions = "--no-ext-diff --no-textconv --ignore-submodules=none"

// diffScript writes, for each commit $1, commit $2 and file $3 it is given,
// git's summary and the diff from $1 to $2 into $3, over what is there.
var diffScript = proc.SandboxOnly(gitFunc + `while [ "$#" -ge 3 ]; do
	g diff ` + diffOptions + ` --stat --patch "$1" "$2" >"$3" || exit 1
	shift 3
done`)

// bundleScript writes a bundle of the task branch $2 from the base commit $1
// to $3. It writes nothing in the workspace.
var bundleScript = proc.SandboxOnly(gitFunc + `g bundle create -q "$3" "$1..refs/heads/$2"`)

// Review puts the diffs a reviewer reads in the read-only phase, then runs one
// review session there. It changes nothing in the workspace and is never frozen.
func (r *Runner) Review(ctx context.Context, c engine.RunConfig, rq engine.ReviewRequest, progress func(engine.Progress)) (engine.ReviewResult, error) {
	return act(ctx, r, c, sandbox.PhaseReadOnly, func(ctx context.Context, st *run, mask func(string) string) (engine.ReviewResult, error) {
		// Without its diff the review gives no verdict, which never blocks the task.
		problem, err := st.reviewDiffs(ctx, c, rq)
		if err != nil || problem != "" {
			return engine.ReviewResult{Outcome: engine.ReviewFailed, Detail: mask(problem)}, err
		}

		res, usage, err := runSession(ctx, st, c, mask, progress, func(s *session) (engine.ReviewResult, []driver.Usage, error) {
			return s.review(ctx, r.driver, rq)
		})
		res.Usage = usage

		return res, err
	})
}

// scoped reports a review of a fix: one that rules on the findings the
// previous review left open.
func scoped(rq engine.ReviewRequest) bool { return rq.FixBase != "" }

// reviewDiffs makes the task branch's bundle in the read-only container,
// imports it, and copies into the read-only container the diffs git writes
// from the mirror. A bundle the import refuses, or a diff git cannot write, is
// a problem, not an error.
func (st *run) reviewDiffs(ctx context.Context, c engine.RunConfig, rq engine.ReviewRequest) (string, error) {
	cmd := proc.Command{Argv: scriptArgv(bundleScript, c.Repo.BaseCommit, c.Repo.Branch, tmpfsBundle), Env: agentEnv(st.ecosystems, c.Repo.BaseCommit), Dir: workspace}

	_, problem, err := gitStep(ctx, st.agent, cmd)
	if err != nil || problem != "" {
		return problem, err
	}

	bundle, problem, err := copyOut(ctx, st.agent, tmpfsBundle)
	if err != nil || problem != "" {
		return problem, err
	}

	diffs := []string{changeBase(c), rq.Head, reviewPatch}
	if scoped(rq) {
		diffs = append(diffs, rq.FixBase, rq.Head, fixPatch)
	}

	return st.imported(ctx, c, bundle, rq.Head, c.Repo.BaseCommit, func(imp container) (string, error) {
		if _, problem, err := gitStep(ctx, imp, importCommand(diffScript, diffs...)); err != nil || problem != "" {
			return problem, err
		}

		files := map[string][]byte{}

		for i := 2; i < len(diffs); i += 3 {
			b, problem, err := copyOut(ctx, imp, diffs[i])
			if err != nil || problem != "" {
				return problem, err
			}

			files[diffs[i]] = b
		}

		return "", st.agent.CopyIn(ctx, files)
	})
}

// writeDiffs runs diffScript with args in the agent phase's workspace, where
// the agent's configuration applies. A diff git cannot write is a problem,
// not an error.
func writeDiffs(ctx context.Context, st *run, c engine.RunConfig, args ...string) (string, error) {
	_, problem, err := gitStep(ctx, st.agent, proc.Command{Argv: scriptArgv(diffScript, args...), Env: agentEnv(st.ecosystems, c.Repo.BaseCommit), Dir: workspace})

	return problem, err
}

// review runs the review session under the agent's time limit and maps how it
// ended. It also returns the agent's usage, and with an error the turns and
// usage so far.
func (s *session) review(ctx context.Context, d driver.Driver, rq engine.ReviewRequest) (engine.ReviewResult, []driver.Usage, error) {
	text, err := reviewPrompt(s.c, rq)
	if err != nil {
		return engine.ReviewResult{}, nil, err
	}

	schema := prompts.ReviewSchema()
	if scoped(rq) {
		schema = prompts.RereviewSchema()
	}

	spec, err := s.newSpec(text, engine.RoleReview)
	if err != nil {
		return engine.ReviewResult{}, nil, err
	}

	spec.SessionID = rq.SessionID
	readOnly(&spec, schema)

	dr, limit, err := s.drive(ctx, d, spec)
	if err != nil {
		return engine.ReviewResult{Turns: dr.Turns}, dr.Usage, err
	}

	res := s.reviewResult(dr, rq)
	res.Outcome, res.Detail = s.limited(limit, res.Outcome, res.Detail)

	return res, dr.Usage, nil
}

// reviewPrompt is a full review's prompt, or a scoped one's on the findings
// it rules on.
func reviewPrompt(c engine.RunConfig, rq engine.ReviewRequest) (string, error) {
	if scoped(rq) {
		return prompts.Render(prompts.Rereview, prompts.RereviewData{
			Title: c.Task.Title, Body: c.Task.Body, Images: imagePaths(c), Findings: numberFindings(rq.Findings), DiffFile: reviewPatch,
			FixDiffFile: fixPatch, VerifyCommand: c.Verify.Command, VerifyWaived: c.Verify.Waived,
		})
	}

	return prompts.Render(prompts.Review, prompts.ReviewData{
		Title: c.Task.Title, Body: c.Task.Body, Images: imagePaths(c), Answers: rq.Answers, Assumptions: rq.Assumptions, Summary: rq.Summary,
		DiffFile: reviewPatch, VerifyCommand: c.Verify.Command, VerifyWaived: c.Verify.Waived,
	})
}

// numberFindings numbers findings from 1 in their order, the numbers a
// rereview's verdicts name them by.
func numberFindings(findings []engine.Finding) []prompts.ReviewFinding {
	out := make([]prompts.ReviewFinding, len(findings))
	for i, f := range findings {
		out[i] = prompts.ReviewFinding{Number: i + 1, Severity: f.Severity, Where: f.Where, What: f.What, Why: f.Why}
	}

	return out
}

// reviewResult maps how a review session ended: reviewed when its result fits
// the schema, a session outcome that stops the task, and otherwise failed
// with the session's detail, so no other outcome reaches the record.
func (s *session) reviewResult(dr driver.Result, rq engine.ReviewRequest) engine.ReviewResult {
	res := engine.ReviewResult{Turns: dr.Turns, CostEstimateUSD: dr.CostEstimateUSD, CostMissing: dr.CostMissing, Detail: s.mask(dr.Detail)}

	if res.Outcome, res.ResetsAt = stopped(dr); res.Outcome != "" {
		return res
	}

	res.Outcome = engine.ReviewFailed

	var out struct {
		Asked, Built *engine.Verdict
		Findings     []engine.Finding
		Verdicts     []struct {
			Finding   int
			Addressed bool
		}
		Breakage []engine.Finding
	}

	if dr.Reason != driver.ReasonDone || json.Unmarshal(dr.Structured, &out) != nil {
		return res
	}

	if scoped(rq) {
		if out.Verdicts == nil || !severities(out.Breakage) {
			return res
		}

		res.Addressed = make([]bool, len(rq.Findings))

		for _, v := range out.Verdicts {
			if v.Finding >= 1 && v.Finding <= len(res.Addressed) {
				res.Addressed[v.Finding-1] = v.Addressed
			}
		}

		res.Outcome, res.Findings = engine.ReviewReviewed, maskFindings(s.mask, out.Breakage)

		return res
	}

	if out.Asked == nil || out.Built == nil || !severities(out.Findings) {
		return res
	}

	res.Outcome, res.Findings = engine.ReviewReviewed, maskFindings(s.mask, out.Findings)
	res.Asked = engine.Verdict{Pass: out.Asked.Pass, Reason: s.mask(out.Asked.Reason)}
	res.Built = engine.Verdict{Pass: out.Built.Pass, Reason: s.mask(out.Built.Reason)}

	return res
}

// severities reports that every finding has one of the three severities.
func severities(findings []engine.Finding) bool {
	return !slices.ContainsFunc(findings, func(f engine.Finding) bool {
		return !slices.Contains([]string{engine.SeverityCritical, engine.SeverityImportant, engine.SeverityMinor}, f.Severity)
	})
}

func maskFindings(mask func(string) string, findings []engine.Finding) []engine.Finding {
	for i, f := range findings {
		findings[i] = engine.Finding{Severity: f.Severity, Where: mask(f.Where), What: mask(f.What), Why: mask(f.Why)}
	}

	return findings
}
