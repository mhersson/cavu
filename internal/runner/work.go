package runner

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/mhersson/cavu/internal/driver"
	"github.com/mhersson/cavu/internal/engine"
	"github.com/mhersson/cavu/internal/env"
	"github.com/mhersson/cavu/internal/forge"
	"github.com/mhersson/cavu/internal/proc"
	"github.com/mhersson/cavu/internal/prompts"
	"github.com/mhersson/cavu/internal/proxy"
	"github.com/mhersson/cavu/internal/sandbox"
	"github.com/mhersson/cavu/internal/sidecar"
	"github.com/mhersson/cavu/internal/store"
)

const (
	configDir = home + "/.claude-cavu"
	// gitTimeout bounds each of CAVU's own git steps, such as the freeze, a bundle
	// or an import, each a few git commands.
	gitTimeout = 5 * time.Minute
)

// gitFunc is git for CAVU's own commands in the workspace: no hook or file
// system monitor runs, no replace ref or commit-graph stands in for a real
// object, and no commit waits on a signer.
const gitFunc = "g() { git --no-replace-objects -c core.hooksPath=/dev/null -c core.fsmonitor=false -c core.commitGraph=false -c commit.gpgSign=false \"$@\"; }\n"

// freezeScript commits what the session left uncommitted with message $3,
// points the task branch $2 at HEAD and prints HEAD. It refuses a workspace
// without .git, or whose HEAD does not descend from the commit $1 the run
// started at: a fix run's commits are on the pull request, never force-pushed.
var freezeScript = proc.SandboxOnly(gitFunc + `[ -d .git ] || { echo "The session removed the repository's .git directory."; exit 1; }
g merge-base --is-ancestor "$1" HEAD || { echo "The session left HEAD on a commit that does not descend from the commit the run started at."; exit 1; }
if [ -n "$(g status --porcelain)" ]; then g add -A && g commit -q -m "$3" || exit 1; fi
g update-ref "refs/heads/$2" HEAD && g rev-parse HEAD`)

var errSessionTimeout = errors.New("runner: the session ran past its time limit")

// budgetCut ends a session at the proxy's budget line: its cap is spent when
// reason is empty, else the proxy could not count a model call's spend.
type budgetCut struct{ reason string }

func (budgetCut) Error() string { return "runner: the proxy stopped the session's model calls" }

// Work runs one session of the agent, a readiness turn in the read-only phase
// and any other in the agent phase, and, when it ends done, freezes its work
// into a commit on the task branch. A fix of failed checks first reads from
// the forge what they printed.
func (r *Runner) Work(ctx context.Context, c engine.RunConfig, w engine.WorkRequest, progress func(engine.Progress)) (engine.WorkResult, error) {
	phase := sandbox.PhaseAgent
	if w.Mode == engine.ModeReadiness {
		phase = sandbox.PhaseReadOnly
	}

	return act(ctx, r, c, phase, func(ctx context.Context, st *run, mask func(string) string) (engine.WorkResult, error) {
		var logs string

		if w.Mode == engine.ModeFixCI {
			var err error
			if logs, err = r.failedLogs(ctx, c); err != nil {
				return engine.WorkResult{}, err
			}

			// A job may print a secret the project also gives its setup.
			logs = mask(logs)
		}

		res, usage, err := runSession(st, c, mask, progress, func(s *session) (engine.WorkResult, []driver.Usage, error) {
			return s.run(ctx, r.driver, w, logs)
		})
		res.Usage = usage

		return res, err
	})
}

// failedLogs is what the failed checks of the run's head printed, but those
// the project ignores.
func (r *Runner) failedLogs(ctx context.Context, c engine.RunConfig) (string, error) {
	repo, err := forge.ParseRepo(c.Repo.URL)
	if err != nil {
		return "", err
	}

	return r.forge.FailedLogs(ctx, repo, forge.Access(c.Repo.Access), c.Repo.Head, c.Repo.IgnoredChecks)
}

// runSession builds a session, runs it with the sidecar's lines watched, and
// returns its result with the usage it reported, also with an error, so what
// a cut session used is recorded.
func runSession[T any](st *run, c engine.RunConfig, mask func(string) string, progress func(engine.Progress), do func(*session) (T, []driver.Usage, error)) (T, []store.Usage, error) {
	s := &session{c: c, st: st, mask: mask, progress: progress, proxied: map[string]store.Usage{}}

	st.watching(s.line)
	res, totals, err := do(s)

	st.watching(nil)

	return res, s.usage(totals), err
}

// session is one agent session of a Work or Review call, and what the sidecar reported during it.
type session struct {
	c        engine.RunConfig
	st       *run
	mask     func(string) string
	progress func(engine.Progress)
	proxied  map[string]store.Usage // key mode: the usage lines' sums per model

	mu  sync.Mutex
	cut context.CancelCauseFunc // ends the driver's run in flight; nil while none runs
}

// run runs the session under the agent's time limit, maps how it ended and
// freezes the work of one that ended done. It also returns the agent's usage,
// and with an error the turns and usage so far. A fresh fix of a review's
// findings first gets the diff the review read; logs are what a fix of failed
// checks opens with.
func (s *session) run(ctx context.Context, d driver.Driver, w engine.WorkRequest, logs string) (engine.WorkResult, []driver.Usage, error) {
	if w.Mode == engine.ModeFixReview && !w.Resume {
		problem, err := writeDiffs(ctx, s.st, s.c, s.c.Repo.BaseCommit, "refs/heads/"+s.c.Repo.Branch, reviewPatch)
		if err != nil || problem != "" {
			problem = s.mask(problem)

			return engine.WorkResult{Outcome: engine.OutcomeFailed, Summary: problem, Detail: problem}, nil, err
		}
	}

	spec, err := s.spec(w, logs)
	if err != nil {
		return engine.WorkResult{}, nil, err
	}

	dr, limit, err := s.drive(ctx, d, spec)
	if err == nil && dr.Reason == driver.ReasonResumeRefused {
		// A new session under the engine's id, which every later step resumes.
		if spec, err = s.spec(engine.WorkRequest{Mode: engine.ModeStart, SessionID: w.SessionID, Role: w.Role}, ""); err == nil {
			dr, limit, err = s.drive(ctx, d, spec)
		}
	}

	if err != nil {
		return engine.WorkResult{Turns: dr.Turns}, dr.Usage, err
	}

	res := s.result(dr, w.Mode)
	res.Outcome, res.Detail = s.limited(limit, res.Outcome, res.Detail)

	if res.Outcome == engine.OutcomeDone {
		if err := s.freeze(ctx, &res); err != nil {
			return engine.WorkResult{Turns: dr.Turns}, dr.Usage, err
		}
	}

	return res, dr.Usage, nil
}

// drive runs one session of spec under the agent's time limit, until the
// proxy's budget line if one comes, and returns the limit that ended it, or
// nil. An error is the sandbox's or the call's; when ctx ended, the result is
// what the interrupted driver reported.
func (s *session) drive(ctx context.Context, d driver.Driver, spec driver.Spec) (dr driver.Result, limit, err error) {
	sctx, cancel := context.WithTimeoutCause(ctx, s.c.Agent.Timeout, errSessionTimeout)
	defer cancel()

	sctx, cut := context.WithCancelCause(sctx)
	defer cut(nil)

	s.mu.Lock()
	s.cut = cut
	s.mu.Unlock()

	dr, err = d.Run(sctx, s.st.agent, spec, s.event)

	// A budget line from here on leaves the result as it is.
	s.mu.Lock()
	s.cut = nil
	limit = context.Cause(sctx)
	s.mu.Unlock()

	switch {
	case ctx.Err() != nil:
		return dr, nil, ctx.Err()
	case err != nil:
		return driver.Result{}, nil, err
	}

	return dr, limit, nil
}

// limited is the outcome and detail of a session that mapped to outcome and
// detail, once limit is known: a limit that ended the session gives its own
// outcome, and the proxy's reason is the detail when it could not count the spend.
func (s *session) limited(limit error, outcome, detail string) (string, string) {
	var b budgetCut

	switch {
	case errors.Is(limit, errSessionTimeout):
		return engine.OutcomeTimeout, detail
	case errors.As(limit, &b) && b.reason == "":
		return engine.OutcomeBudget, detail
	case errors.As(limit, &b):
		return engine.OutcomeUnmetered, s.mask(b.reason)
	}

	return outcome, detail
}

func (s *session) spec(w engine.WorkRequest, logs string) (driver.Spec, error) {
	text, err := prompt(s.c, w, logs)
	if err != nil {
		return driver.Spec{}, err
	}

	spec, err := s.newSpec(text, w.Role)
	if err != nil {
		return driver.Spec{}, err
	}

	if w.Mode == engine.ModeReadiness {
		readOnly(&spec, prompts.ReadinessSchema())
	}

	if w.Resume {
		spec.Resume = w.SessionID
	} else {
		spec.SessionID = w.SessionID
	}

	return spec, nil
}

// newSpec is the spec of a session in role with prompt text, but its id.
func (s *session) newSpec(text, role string) (driver.Spec, error) {
	spec, err := sessionSpec(s.c, s.st.ecosystems, s.st.standIn)
	if err != nil {
		return driver.Spec{}, err
	}

	spec.Prompt = text

	switch role {
	case engine.RoleReview:
		spec.Model, spec.Effort = s.c.Agent.Review.Model, s.c.Agent.Review.Effort
	case engine.RoleStrong:
		spec.Model, spec.Effort = s.c.Agent.Strong.Model, s.c.Agent.Strong.Effort
	}

	return spec, nil
}

// readOnly makes spec a read-only session's that ends with schema. It gets no
// instructions, which tell a session to commit its work and how to end: its
// prompt says what a session that changes nothing needs.
func readOnly(spec *driver.Spec, schema json.RawMessage) {
	spec.ReadOnly, spec.Instructions, spec.OutputSchema = true, "", schema
}

// AgentPhase is the proxy request a run starts c's agent phase with, and the
// spec of a session in it but its prompt and id, which the caller sets. In key
// mode both hold one fresh stand-in for the credential. A run builds the two
// apart: the request when the phase starts, a spec for each session.
func AgentPhase(c engine.RunConfig, d driver.Driver) (sidecar.ProxyRequest, driver.Spec, error) {
	req, standIn := agentProxy(c, c.Ecosystems, d)
	spec, err := sessionSpec(c, c.Ecosystems, standIn)

	return req, spec, err
}

// agentProxy is the agent phase's proxy request and, in key mode, the
// stand-in its model route takes in place of the credential.
func agentProxy(c engine.RunConfig, ecosystems []string, d driver.Driver) (sidecar.ProxyRequest, string) {
	req := sidecar.ProxyRequest{AllowHosts: allowlist(env.AgentHosts(ecosystems), c.Setup.EgressHosts, d.Hosts(c.Endpoint.Mode))}
	if c.Endpoint.Mode != modeKey {
		return req, ""
	}

	standIn := proxy.NewStandIn()
	req.Model = &proxy.ModelRoute{BaseURL: c.Endpoint.URL, Credential: c.Endpoint.Credential, AuthHeader: c.Endpoint.AuthHeader, StandIn: standIn}
	req.Budget = proxyBudget(c)

	return req, standIn
}

// proxyBudget is c's budget as the proxy counts it, nil when no money cap is in force.
func proxyBudget(c engine.RunConfig) *proxy.Budget {
	if !c.Budget.Capped {
		return nil
	}

	prices := make(map[string]proxy.Price, len(c.Budget.Prices))
	for model, p := range c.Budget.Prices {
		prices[model] = proxy.Price(p)
	}

	return &proxy.Budget{CapMicroUSD: c.Budget.MicroUSD, Prices: prices}
}

// sessionSpec is a writing session's spec in the work role but its prompt and id.
func sessionSpec(c engine.RunConfig, ecosystems []string, standIn string) (driver.Spec, error) {
	instructions, err := prompts.Render(prompts.Instructions, nil)
	if err != nil {
		return driver.Spec{}, err
	}

	spec := driver.Spec{
		Auth:  driver.Auth{Mode: c.Endpoint.Mode, Token: c.Endpoint.Credential},
		Model: c.Agent.Model, Effort: c.Agent.Effort, MaxTurns: c.Agent.MaxTurns,
		Instructions: instructions, OutputSchema: prompts.ResultSchema(),
		Dir: workspace, BaseCommit: c.Repo.BaseCommit, Home: home, ConfigDir: configDir,
		Env: withProjectVars(sessionEnv(ecosystems, c.Repo.BaseCommit), c), WebTools: c.Agent.WebTools,
		InstanceInstructions: c.Agent.Instance.Instructions,
	}

	if c.Agent.Instance.Skills != nil {
		spec.InstanceSkills = make(map[string]driver.File, len(c.Agent.Instance.Skills))
		for p, f := range c.Agent.Instance.Skills {
			spec.InstanceSkills[p] = driver.File(f)
		}
	}

	if c.Endpoint.Mode == modeKey {
		// The agent holds only the stand-in; the proxy swaps the credential in.
		spec.Auth.Token, spec.Auth.BaseURL = standIn, sandbox.BridgeURL
	}

	return spec, nil
}

// prompt is the user message that opens or resumes a session in w's mode;
// w's detail is the verify output of a fix and the text of an answer, and
// logs what the failed checks printed for a fix of them.
func prompt(c engine.RunConfig, w engine.WorkRequest, logs string) (string, error) {
	task := prompts.WorkData{Title: c.Task.Title, Body: c.Task.Body}

	switch w.Mode {
	case engine.ModeReadiness:
		return prompts.Render(prompts.Readiness, task)
	case engine.ModeStart:
		// Only a readiness turn that found nothing missing leaves a session to start by resuming.
		if w.Resume {
			return prompts.Render(prompts.Proceed, nil)
		}
	case engine.ModeContinue:
		return prompts.Render(prompts.Continue, nil)
	case engine.ModeFixVerify:
		return prompts.Render(prompts.FixVerify, prompts.FixVerifyData{Command: c.Verify.Command, Output: w.Detail})
	case engine.ModeAnswer:
		return prompts.Render(prompts.Answer, prompts.AnswerData{Answer: w.Detail})
	case engine.ModeFixReview:
		if w.Resume {
			return prompts.Render(prompts.FixReview, prompts.FixReviewData{Findings: numberFindings(w.Findings)})
		}

		return prompts.Render(prompts.FixReviewFresh, prompts.FixReviewFreshData{
			Title: c.Task.Title, Body: c.Task.Body, Findings: numberFindings(w.Findings), DiffFile: reviewPatch,
		})
	case engine.ModeFixCI:
		return prompts.Render(prompts.FixCI, prompts.FixCIData{Logs: logs})
	case engine.ModeComments:
		return prompts.Render(prompts.Comments, prompts.CommentsData{Comments: reviewComments(w.Comments)})
	}

	return prompts.Render(prompts.Work, task)
}

// reviewComments are a reviewer's requests as the prompt gives them; their ids
// stay with the engine, which knows which were answered.
func reviewComments(comments []engine.Comment) []prompts.ReviewComment {
	out := make([]prompts.ReviewComment, len(comments))
	for i, c := range comments {
		out[i] = prompts.ReviewComment{Author: c.Author, Path: c.Path, Line: c.Line, Body: c.Body}
	}

	return out
}

// event forwards what a person follows a session by. Every string is masked:
// the driver masks nothing.
func (s *session) event(ev driver.Event) {
	switch ev.Kind {
	case driver.KindText:
		s.progress(engine.Progress{Type: engine.AgentText, Payload: engine.AgentTextPayload{Text: s.mask(ev.Text)}})
	case driver.KindToolUse:
		s.progress(engine.Progress{Type: engine.AgentTool, Payload: engine.AgentToolPayload{
			Name: s.mask(ev.ToolName), Summary: s.mask(string(ev.ToolInput)),
		}})
	case driver.KindRateLimit:
		s.progress(engine.Progress{Type: engine.RateLimit, Payload: engine.RateLimitPayload{
			Window: s.mask(ev.RateLimit.Window), Status: s.mask(ev.RateLimit.Status), ResetsAt: ev.RateLimit.ResetsAt,
		}})
	}
}

// line takes the sidecar's lines during the session, on the sidecar reader's
// goroutine.
func (s *session) line(l sidecar.Line) {
	switch l.Type {
	case "denied":
		s.progress(engine.Progress{Type: engine.EgressDenied, Payload: engine.EgressDeniedPayload{Host: l.Host}})
	case "usage":
		u := s.proxied[l.Usage.Model]
		u.Source, u.Model = "proxy", l.Usage.Model
		u.Input, u.Output = u.Input+l.Usage.Input, u.Output+l.Usage.Output
		u.CacheRead, u.CacheWrite = u.CacheRead+l.Usage.CacheRead, u.CacheWrite+l.Usage.CacheWrite
		s.proxied[l.Usage.Model] = u
	case "budget":
		s.mu.Lock()
		if s.cut != nil {
			s.cut(budgetCut{reason: l.Error})
		}
		s.mu.Unlock()
	}
}

// stopped is the outcome of a session end that stops the task whatever the
// session was for, with a usage limit's reset time; "" for any other end.
func stopped(dr driver.Result) (string, time.Time) {
	switch dr.Reason {
	case driver.ReasonUsageLimit:
		if dr.RateLimit != nil {
			return engine.OutcomeUsageLimit, dr.RateLimit.ResetsAt
		}

		return engine.OutcomeUsageLimit, time.Time{}
	case driver.ReasonMaxTurns:
		return engine.OutcomeMaxTurns, time.Time{}
	case driver.ReasonAuth, driver.ReasonEndpoint:
		return engine.OutcomeEndpoint, time.Time{}
	case driver.ReasonEndpointFailed:
		return engine.OutcomeEndpointFailed, time.Time{}
	}

	return "", time.Time{}
}

// result maps how the session in mode ended.
func (s *session) result(dr driver.Result, mode string) engine.WorkResult {
	res := engine.WorkResult{Turns: dr.Turns, CostEstimateUSD: dr.CostEstimateUSD, CostMissing: dr.CostMissing, Detail: s.mask(dr.Detail)}

	if res.Outcome, res.ResetsAt = stopped(dr); res.Outcome != "" {
		return res
	}

	switch dr.Reason {
	case driver.ReasonDone:
		if mode == engine.ModeReadiness {
			s.readiness(dr.Structured, &res)

			break
		}

		var out struct {
			Status, Title, Summary string
			Assumptions, Questions []string
			WhereToLook            []string `json:"where_to_look"`
		}

		if json.Unmarshal(dr.Structured, &out) != nil ||
			!slices.Contains([]string{engine.OutcomeDone, engine.OutcomeBlocked, engine.OutcomeFailed}, out.Status) {
			res.Outcome = engine.OutcomeUnfinished

			break
		}

		res.Outcome, res.Title, res.Summary = out.Status, s.mask(out.Title), s.mask(out.Summary)
		res.Assumptions, res.Questions = maskEach(s.mask, out.Assumptions), maskEach(s.mask, out.Questions)
		res.WhereToLook = maskEach(s.mask, out.WhereToLook)
	default:
		res.Outcome = engine.OutcomeCrashed
	}

	return res
}

// readiness maps a readiness turn's structured result: ready, or blocked with
// at least one question and the answer the agent would assume for each.
// Anything else is unfinished.
func (s *session) readiness(structured json.RawMessage, res *engine.WorkResult) {
	var out struct {
		Status    string
		Questions []struct {
			Question        string
			SuggestedAnswer string `json:"suggested_answer"`
		}
	}

	res.Outcome = engine.OutcomeUnfinished

	if json.Unmarshal(structured, &out) != nil {
		return
	}

	switch {
	case out.Status == engine.OutcomeReady:
		res.Outcome = engine.OutcomeReady
	case out.Status == engine.OutcomeBlocked && len(out.Questions) > 0:
		res.Outcome = engine.OutcomeBlocked

		for _, q := range out.Questions {
			res.Questions = append(res.Questions, s.mask(q.Question))
			res.SuggestedAnswers = append(res.SuggestedAnswers, s.mask(q.SuggestedAnswer))
		}
	}
}

// freeze commits the session's work and points the task branch at it. A
// workspace the freeze refuses fails the session with the reason. The work
// changed when its head is not the commit the run started at.
func (s *session) freeze(ctx context.Context, res *engine.WorkResult) error {
	start := cmp.Or(s.c.Repo.Head, s.c.Repo.BaseCommit)
	message := cmp.Or(res.Title, s.c.Task.Title)
	cmd := proc.Command{
		Argv: scriptArgv(freezeScript, start, s.c.Repo.Branch, message),
		Env:  agentEnv(s.st.ecosystems, s.c.Repo.BaseCommit), Dir: workspace,
	}

	out, err := s.st.agent.Run(ctx, cmd, gitTimeout)
	if err != nil {
		return err
	}

	head := lastCommit(out.Output)
	if out.ExitCode != 0 || out.TimedOut || head == "" {
		// The summary is what the stop of a failed session explains.
		res.Outcome, res.Summary = engine.OutcomeFailed, s.mask(strings.TrimSpace(output(out, gitTimeout)))
		res.Detail = res.Summary

		return nil
	}

	res.HeadCommit, res.Changed = head, head != start

	return nil
}

// lastCommit is the last line of out that is a commit id: the runtime client's
// own warnings can follow a command's output.
func lastCommit(out string) string {
	for _, l := range slices.Backward(strings.Split(out, "\n")) {
		if l = strings.TrimSpace(l); commitID.MatchString(l) {
			return l
		}
	}

	return ""
}

// usage is the session's rows: in key mode the proxy's count of every model
// call, otherwise the agent's own totals; the costs are the agent's estimates.
func (s *session) usage(totals []driver.Usage) []store.Usage {
	var rows []store.Usage

	if s.c.Endpoint.Mode != modeKey {
		// The engine masks event payloads, not usage rows.
		for _, u := range totals {
			rows = append(rows, store.Usage{
				Source: "agent", Model: s.mask(u.Model), Input: u.Input, Output: u.Output,
				CacheRead: u.CacheRead, CacheWrite: u.CacheWrite, CostEstimateUSD: u.CostEstimateUSD,
			})
		}

		return rows
	}

	cost := map[string]float64{}
	for _, u := range totals {
		cost[u.Model] = u.CostEstimateUSD
	}

	for _, model := range slices.Sorted(maps.Keys(s.proxied)) {
		u := s.proxied[model]
		u.CostEstimateUSD = cost[model]
		rows = append(rows, u)
	}

	return rows
}

func maskEach(mask func(string) string, ss []string) []string {
	for i, s := range ss {
		ss[i] = mask(s)
	}

	return ss
}
