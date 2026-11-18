package runner

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/mhersson/cavu/internal/engine"
	"github.com/mhersson/cavu/internal/env"
	"github.com/mhersson/cavu/internal/forge"
	"github.com/mhersson/cavu/internal/proc"
	"github.com/mhersson/cavu/internal/sandbox"
	"github.com/mhersson/cavu/internal/sidecar"
	"github.com/mhersson/cavu/internal/verify"
)

// The steps a prepare reports as failed.
const (
	stepClone     = "clone"
	stepToolchain = "toolchain"
	stepSetup     = "setup"
	stepBaseline  = "baseline"
)

const (
	toolchainTimeout = 20 * time.Minute
	setupTimeout     = 30 * time.Minute
	statusTimeout    = time.Minute
	baselineReused   = "reused"
)

// reshimScript makes the shims for what the setup command added to a tool,
// such as a console script pip installed: every phase reaches a tool's
// commands only through them.
var reshimScript = proc.SandboxOnly("exec mise reshim")

// Prepare starts the run from empty volumes and leaves the agent phase up.
func (r *Runner) Prepare(ctx context.Context, c engine.RunConfig) (engine.PrepareResult, error) {
	res, _, err := r.prepare(ctx, c, false)

	return res, err
}

// CheckEnvironment prepares a project the way a run does, under run and task
// id 0 and never reusing a baseline, then releases it with its volumes. It
// also returns the verify command the repository's root files suggest, or "".
func (r *Runner) CheckEnvironment(ctx context.Context, c engine.RunConfig) (res engine.PrepareResult, verifySuggestion string, err error) {
	c.RunID, c.TaskID = 0, 0

	res, files, err := r.prepare(ctx, c, true)
	verifySuggestion, _ = verify.Suggest(files)

	relErr := r.Release(ctx, refOf(c), true)

	return res, verifySuggestion, errors.Join(err, maskErr(relErr, masker(c)))
}

// prepare runs a prepare and returns the root files it read. The task's home
// goes first: every project's check shares task id 0, and a check that died
// must not hand its home to the next.
func (r *Runner) prepare(ctx context.Context, c engine.RunConfig, check bool) (engine.PrepareResult, map[string][]byte, error) {
	// The run's earlier state goes: its volumes are about to be destroyed.
	r.setState(c.RunID, nil)

	sb, err := r.sandbox(refOf(c))
	if err != nil {
		return engine.PrepareResult{}, nil, maskErr(err, masker(c))
	}

	st := &run{sb: sb}
	ctx, end := st.begin(ctx)
	p := &preparation{r: r, c: c, st: st, check: check}

	res, err := p.run(ctx)
	if fault := end(); fault != nil {
		res, err = engine.PrepareResult{}, fault
	}

	// A token the App minted is in no config: it joins this action's mask alone.
	mask := masker(c, p.token)
	res.Output = proc.HeadTail(mask(res.Output), proc.CaptureCap)

	return res, p.files, maskErr(err, mask)
}

// preparation is one prepare in progress.
type preparation struct {
	r     *Runner
	c     engine.RunConfig
	st    *run
	check bool
	files map[string][]byte
	token string // the token the clone used
}

func (p *preparation) run(ctx context.Context) (engine.PrepareResult, error) {
	sb, c := p.st.sb, p.c

	if err := sb.Destroy(ctx, false); err != nil {
		return engine.PrepareResult{}, err
	}

	if err := sb.EnsureVolumes(ctx); err != nil {
		return engine.PrepareResult{}, err
	}

	base, err := p.clone(ctx)
	if err != nil {
		if sandboxFault(ctx, err) {
			return engine.PrepareResult{}, err
		}

		return failure(engine.PrepareResult{}, &stepFailure{stepClone, err.Error()}), nil
	}

	res := engine.PrepareResult{BaseCommit: base}
	// A first prepare's config has no base commit yet: the clone's is the run's.
	p.c.Repo.BaseCommit = base

	det, failed, err := p.detect(ctx, base)
	if err != nil || failed != nil {
		return failure(res, failed), err
	}

	res.Tools, res.IgnoredTools, res.Ecosystems = make([]string, len(det.Tools)), det.Ignored, det.Ecosystems
	for i, t := range det.Tools {
		res.Tools[i] = t.Name + "@" + t.Version
	}

	if failed, err := p.setup(ctx, det); err != nil || failed != nil {
		return failure(res, failed), err
	}

	if err := p.st.down(ctx); err != nil {
		return engine.PrepareResult{}, err
	}

	if err := p.r.ensureUp(ctx, p.c, sandbox.PhaseAgent); err != nil {
		return engine.PrepareResult{}, err
	}

	if strings.TrimSpace(c.Setup.Command) != "" {
		if failed, err := p.cleanTree(ctx, stepSetup); err != nil || failed != nil {
			return failure(res, failed), err
		}
	}

	return p.baseline(ctx, res, base)
}

// clone clones with a personal access token as it is, without reading the
// address, so another git host's repository clones too; with an App, with a token it mints.
func (p *preparation) clone(ctx context.Context) (string, error) {
	c := p.c
	p.token = c.Repo.Access.Token

	if c.Repo.Access.AppID != 0 {
		repo, err := forge.ParseRepo(c.Repo.URL)
		if err != nil {
			return "", err
		}

		if p.token, err = p.r.forge.Token(ctx, repo, forge.Access(c.Repo.Access), false); err != nil {
			return "", err
		}
	}

	return p.st.sb.Clone(ctx, sidecar.CloneRequest{
		URL: c.Repo.URL, Token: p.token, BaseBranch: c.Repo.BaseBranch, Commit: c.Repo.BaseCommit, Branch: c.Repo.Branch,
		Name: c.Identity.Name, Email: c.Identity.Email,
	})
}

// detect reads the root files under an empty allowlist, the one command that
// runs under it, and detects the tools. Root files that detection cannot
// parse, or that are too large to read, fail the toolchain step.
func (p *preparation) detect(ctx context.Context, base string) (env.Detection, *stepFailure, error) {
	sc, err := p.st.up(ctx, sandbox.PhaseSetup, sidecar.ProxyRequest{})
	if err != nil {
		return env.Detection{}, nil, err
	}

	p.files, err = readFiles(ctx, sc, base)

	switch {
	case errors.Is(err, errFilesTooLarge):
		return env.Detection{}, &stepFailure{stepToolchain, err.Error()}, nil
	case err != nil:
		return env.Detection{}, nil, err
	}

	det, err := env.Detect(p.files, p.c.Setup.ToolOverrides)
	if err != nil {
		return env.Detection{}, &stepFailure{stepToolchain, err.Error()}, nil
	}

	p.st.ecosystems = det.Ecosystems
	p.r.setState(p.c.RunID, p.st)

	return det, nil, nil
}

// stepFailure is a prepare step that failed, with its output.
type stepFailure struct{ step, output string }

// setup installs the tools and runs the setup command with the detected
// hosts, then makes the shims for what it added. Only the setup command gets
// the project's secrets. A project's setups take turns, so runs beside each
// other do not install into the shared caches at once.
func (p *preparation) setup(ctx context.Context, det env.Detection) (*stepFailure, error) {
	release, err := p.r.takeTurn(ctx, p.c.ProjectID)
	if err != nil {
		return nil, err
	}
	defer release()

	req := sidecar.ProxyRequest{
		AllowHosts: allowlist(env.SetupHosts(det.Ecosystems), p.c.Setup.EgressHosts),
	}

	sc, err := p.st.up(ctx, sandbox.PhaseSetup, req)
	if err != nil {
		return nil, err
	}

	// Written even with no tools, so a stale configuration cannot install any.
	if err := sc.CopyIn(ctx, map[string][]byte{env.MiseConfigFile: env.MiseConfig(det.Tools)}); err != nil {
		return nil, err
	}

	environ := setupEnv(det.Ecosystems, p.c.Repo.BaseCommit)

	if len(det.Tools) > 0 {
		cmd := proc.Command{Argv: []string{"mise", "install"}, Env: environ}
		if failed, err := runStep(ctx, p.st, sc, stepToolchain, cmd, toolchainTimeout); err != nil || failed != nil {
			return failed, err
		}
	}

	if strings.TrimSpace(p.c.Setup.Command) == "" {
		return nil, nil
	}

	vars := map[string]string{}
	maps.Copy(vars, p.c.Setup.Env)
	maps.Copy(vars, p.c.Setup.Secrets)

	cmd := proc.Command{
		Argv: verify.Argv(p.c.Setup.Command),
		Env:  proc.BuildEnv(environ, vars),
		Dir:  workspace,
	}

	if failed, err := runStep(ctx, p.st, sc, stepSetup, cmd, setupTimeout); err != nil || failed != nil || len(det.Tools) == 0 {
		return failed, err
	}

	reshim := proc.Command{Argv: []string{"sh", "-c", reshimScript}, Env: environ}

	return runStep(ctx, p.st, sc, stepSetup, reshim, toolchainTimeout)
}

// baseline verifies the base commit unless the project waives it, or a
// prepare that is not a check finds it passed on this commit before.
func (p *preparation) baseline(ctx context.Context, res engine.PrepareResult, base string) (engine.PrepareResult, error) {
	v := p.c.Verify

	switch {
	case v.Waived:
		res.Baseline = engine.ResultWaived

		return res, nil
	case !p.check && v.BaselinePassed && v.BaselineCommit == base:
		res.Baseline = baselineReused

		return res, nil
	}

	// A check of a project with no verify command still reports the workspace;
	// a task prepare reaches runVerify and faults.
	if p.check && strings.TrimSpace(v.Command) == "" {
		return res, nil
	}

	vr, err := runVerify(ctx, p.st, p.c)
	if err != nil {
		return engine.PrepareResult{}, err
	}

	res.Baseline = engine.ResultFailed

	if vr.Result != verify.Passed {
		return failure(res, &stepFailure{stepBaseline, vr.Output}), nil
	}

	failed, err := p.cleanTree(ctx, stepBaseline)
	if err != nil || failed != nil {
		return failure(res, failed), err
	}

	res.Baseline = engine.ResultPassed

	return res, nil
}

// treeScript lists the changed files and exits 1 when there are any, 2 when
// git fails: the runtime's own stderr in the output decides nothing.
const treeScript = `s=$(git status --porcelain) || exit 2; [ -z "$s" ] || { printf '%s\n' "$s"; exit 1; }`

// cleanTree fails step when it left the working tree changed, so the agent's
// exported diff holds only what the agent did.
func (p *preparation) cleanTree(ctx context.Context, step string) (*stepFailure, error) {
	cmd := proc.Command{Argv: []string{"sh", "-c", treeScript}, Env: agentEnv(p.st.ecosystems, p.c.Repo.BaseCommit), Dir: workspace}

	res, err := p.st.agent.Run(ctx, cmd, statusTimeout)
	if err != nil {
		return nil, err
	}

	switch {
	case res.TimedOut || res.ExitCode > 1:
		return nil, fmt.Errorf("runner: git status failed: %s", res.Output)
	case res.ExitCode == 0:
		return nil, nil
	}

	return &stepFailure{step, res.Output + "The " + step + " step left these files changed or untracked. Add their paths to .gitignore, " +
		"or use a command that leaves tracked files alone: the baseline runs on the base commit and the exported diff holds only what the agent did.\n"}, nil
}

// runVerify runs the verify command in the agent phase, under the same
// network rule as the agent. A blank command is a fault, never a pass: an
// empty shell command exits 0.
func runVerify(ctx context.Context, st *run, c engine.RunConfig) (engine.VerifyResult, error) {
	if strings.TrimSpace(c.Verify.Command) == "" {
		return engine.VerifyResult{}, errors.New("runner: the verify command is blank and the project does not waive it")
	}

	cmd := proc.Command{Argv: verify.Argv(c.Verify.Command), Env: withProjectVars(agentEnv(st.ecosystems, c.Repo.BaseCommit), c), Dir: workspace}
	start := time.Now()

	st.resetDenied()

	res, err := st.agent.Run(ctx, cmd, c.Verify.Timeout)
	if err != nil {
		return engine.VerifyResult{}, err
	}

	vr := engine.VerifyResult{
		Result: verify.Classify(res.ExitCode, res.TimedOut), Command: c.Verify.Command,
		Output: output(res, c.Verify.Timeout), ExitCode: res.ExitCode, Duration: time.Since(start),
	}
	if vr.Result != verify.Passed {
		vr.Output = st.withDenied(vr.Output)
	}

	return vr, nil
}

// runStep runs a setup-phase command as step. A command that cannot start is
// an error; one that exits non-zero or times out is the step's failure.
func runStep(ctx context.Context, st *run, c container, step string, cmd proc.Command, timeout time.Duration) (*stepFailure, error) {
	st.resetDenied()

	res, err := c.Run(ctx, cmd, timeout)
	if err != nil {
		return nil, err
	}

	if res.ExitCode != 0 || res.TimedOut {
		return &stepFailure{step, st.withDenied(output(res, timeout))}, nil
	}

	return nil, nil
}

// output is a command's output, marked when its timeout ended it.
func output(res sandbox.Result, timeout time.Duration) string {
	if res.TimedOut {
		return res.Output + fmt.Sprintf("\n[stopped at its %s timeout]\n", timeout)
	}

	return res.Output
}

// failure is res failed at f, or an empty result when f is nil because an
// error goes back instead.
func failure(res engine.PrepareResult, f *stepFailure) engine.PrepareResult {
	if f == nil {
		return engine.PrepareResult{}
	}

	res.FailedStep, res.Output = f.step, f.output

	return res
}

// sandboxFault reports a clone error that is the sandbox's, or the end of the
// action's ctx. A clone that hits its own time limit is a failed clone.
func sandboxFault(ctx context.Context, err error) bool {
	return ctx.Err() != nil ||
		errors.Is(err, sandbox.ErrOutOfMemory) || errors.Is(err, sandbox.ErrNotIsolated) || errors.Is(err, sandbox.ErrSidecarDown)
}
