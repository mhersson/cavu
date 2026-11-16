package runner

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/mhersson/cavu/internal/driver"
	"github.com/mhersson/cavu/internal/engine"
	"github.com/mhersson/cavu/internal/proc"
	"github.com/mhersson/cavu/internal/prompts"
	"github.com/mhersson/cavu/internal/sidecar"
	"github.com/mhersson/cavu/internal/store"
	"github.com/mhersson/cavu/internal/verify"
)

const (
	configDir = home + "/.claude-cavu"
	// gitTimeout bounds the freeze and the export, each a few git commands.
	gitTimeout = 5 * time.Minute
)

// gitFunc is git for CAVU's own commands in the workspace: a hook the
// repository or the agent set must not run, and no commit waits on a signer.
const gitFunc = "g() { git -c core.hooksPath=/dev/null -c commit.gpgSign=false \"$@\"; }\n"

// freezeScript commits what the session left uncommitted with message $3,
// points the task branch $2 at HEAD and prints HEAD. It refuses a workspace
// without .git, or whose HEAD does not descend from the base commit $1.
var freezeScript = proc.SandboxOnly(gitFunc + `[ -d .git ] || { echo "The session removed the repository's .git directory."; exit 1; }
g merge-base --is-ancestor "$1" HEAD || { echo "The session left HEAD on a commit that does not descend from the base commit."; exit 1; }
if [ -n "$(g status --porcelain)" ]; then g add -A && g commit -q -m "$3" || exit 1; fi
g update-ref "refs/heads/$2" HEAD && g rev-parse HEAD`)

var errSessionTimeout = errors.New("runner: the session ran past its time limit")

// Work runs one session of the agent in the agent phase and, when it ends
// done, freezes its work into a commit on the task branch.
func (r *Runner) Work(ctx context.Context, c engine.RunConfig, w engine.WorkRequest, progress func(engine.Progress)) (engine.WorkResult, error) {
	return act(ctx, r, c, func(ctx context.Context, st *run, mask func(string) string) (engine.WorkResult, error) {
		s := &session{c: c, st: st, mask: mask, progress: progress, proxied: map[string]store.Usage{}}

		st.watching(s.line)
		res, totals, err := s.run(ctx, r.driver, w)

		st.watching(nil)

		if err != nil {
			return engine.WorkResult{}, err
		}

		res.Usage = s.usage(totals)

		return res, nil
	})
}

// session is one Work call, and what the sidecar reported during it.
type session struct {
	c        engine.RunConfig
	st       *run
	mask     func(string) string
	progress func(engine.Progress)
	proxied  map[string]store.Usage // key mode: the usage lines' sums per model
}

// run runs the session under the agent's time limit, maps how it ended and
// freezes the work of one that ended done. It also returns the agent's usage.
func (s *session) run(ctx context.Context, d driver.Driver, w engine.WorkRequest) (engine.WorkResult, []driver.Usage, error) {
	spec, err := s.spec(w)
	if err != nil {
		return engine.WorkResult{}, nil, err
	}

	sctx, cancel := context.WithTimeoutCause(ctx, s.c.Agent.Timeout, errSessionTimeout)
	defer cancel()

	dr, err := d.Run(sctx, s.st.agent, spec, s.event)
	if err == nil && dr.Reason == driver.ReasonResumeRefused {
		// A new session under the engine's id, which every later step resumes.
		if spec, err = s.spec(engine.WorkRequest{Mode: engine.ModeStart, SessionID: w.SessionID}); err == nil {
			dr, err = d.Run(sctx, s.st.agent, spec, s.event)
		}
	}

	switch {
	case ctx.Err() != nil:
		return engine.WorkResult{}, nil, ctx.Err()
	case err != nil:
		return engine.WorkResult{}, nil, err
	}

	res := s.result(dr)
	if sctx.Err() != nil {
		res.Outcome = engine.OutcomeTimeout
	}

	if res.Outcome == engine.OutcomeDone {
		if err := s.freeze(ctx, &res); err != nil {
			return engine.WorkResult{}, nil, err
		}
	}

	return res, dr.Usage, nil
}

func (s *session) spec(w engine.WorkRequest) (driver.Spec, error) {
	c := s.c

	text, err := prompt(c, w.Mode, w.Detail)
	instructions, ierr := prompts.Render(prompts.Instructions, nil)

	if err := errors.Join(err, ierr); err != nil {
		return driver.Spec{}, err
	}

	spec := driver.Spec{
		Auth:  driver.Auth{Mode: c.Endpoint.Mode, Token: c.Endpoint.Credential},
		Model: c.Agent.Model, Effort: c.Agent.Effort, MaxTurns: c.Agent.MaxTurns,
		Prompt: text, Instructions: instructions, OutputSchema: prompts.ResultSchema(),
		Dir: workspace, BaseCommit: c.Repo.BaseCommit, Home: home, ConfigDir: configDir,
		Env: agentEnv(s.st.ecosystems), WebTools: c.Agent.WebTools,
	}

	if c.Endpoint.Mode == modeKey {
		// The agent holds only the stand-in; the proxy swaps the credential in.
		spec.Auth.Token, spec.Auth.BaseURL = s.st.standIn, bridgeURL
	}

	if w.Resume {
		spec.Resume = w.SessionID
	} else {
		spec.SessionID = w.SessionID
	}

	return spec, nil
}

// prompt is the user message that opens a session in mode; detail is the
// verify output of a fix and the text of an answer.
func prompt(c engine.RunConfig, mode, detail string) (string, error) {
	switch mode {
	case engine.ModeContinue:
		return prompts.Render(prompts.Continue, nil)
	case engine.ModeFixVerify:
		return prompts.Render(prompts.FixVerify, prompts.FixVerifyData{Command: c.Verify.Command, Output: verify.Excerpt(detail)})
	case engine.ModeAnswer:
		return prompts.Render(prompts.Answer, prompts.AnswerData{Answer: detail})
	}

	return prompts.Render(prompts.Work, prompts.WorkData{Title: c.Task.Title, Body: c.Task.Body})
}

// event forwards what a person follows a session by.
func (s *session) event(ev driver.Event) {
	switch ev.Kind {
	case driver.KindText:
		s.progress(engine.Progress{Type: engine.AgentText, Payload: engine.AgentTextPayload{Text: s.mask(ev.Text)}})
	case driver.KindToolUse:
		s.progress(engine.Progress{Type: engine.AgentTool, Payload: engine.AgentToolPayload{
			Name: ev.ToolName, Summary: s.mask(string(ev.ToolInput)),
		}})
	case driver.KindRateLimit:
		s.progress(engine.Progress{Type: engine.RateLimit, Payload: engine.RateLimitPayload{
			Window: ev.RateLimit.Window, Status: ev.RateLimit.Status, ResetsAt: ev.RateLimit.ResetsAt,
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
	}
}

// result maps how the session ended.
func (s *session) result(dr driver.Result) engine.WorkResult {
	res := engine.WorkResult{SessionID: dr.SessionID, Turns: dr.Turns, CostEstimateUSD: dr.CostEstimateUSD, Detail: s.mask(dr.Detail)}

	switch dr.Reason {
	case driver.ReasonUsageLimit:
		res.Outcome = engine.OutcomeUsageLimit

		if dr.RateLimit != nil {
			res.ResetsAt = dr.RateLimit.ResetsAt
		}
	case driver.ReasonMaxTurns:
		res.Outcome = engine.OutcomeMaxTurns
	case driver.ReasonAuth, driver.ReasonEndpoint:
		res.Outcome = engine.OutcomeEndpoint
	case driver.ReasonDone:
		var out struct {
			Status, Title, Summary string
			Assumptions, Questions []string
		}

		if json.Unmarshal(dr.Structured, &out) != nil ||
			!slices.Contains([]string{engine.OutcomeDone, engine.OutcomeBlocked, engine.OutcomeFailed}, out.Status) {
			res.Outcome = engine.OutcomeUnfinished

			break
		}

		res.Outcome, res.Title, res.Summary = out.Status, s.mask(out.Title), s.mask(out.Summary)
		res.Assumptions, res.Questions = maskEach(s.mask, out.Assumptions), maskEach(s.mask, out.Questions)
	default:
		res.Outcome = engine.OutcomeCrashed
	}

	return res
}

// freeze commits the session's work and points the task branch at it. A
// workspace the freeze refuses fails the session with the reason.
func (s *session) freeze(ctx context.Context, res *engine.WorkResult) error {
	message := cmp.Or(res.Title, s.c.Task.Title)
	cmd := proc.Command{
		Argv: scriptArgv(freezeScript, s.c.Repo.BaseCommit, s.c.Repo.Branch, message),
		Env:  agentEnv(s.st.ecosystems), Dir: workspace,
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

	res.HeadCommit, res.Changed = head, head != s.c.Repo.BaseCommit

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
		for _, u := range totals {
			rows = append(rows, store.Usage{
				Source: "agent", Model: u.Model, Input: u.Input, Output: u.Output,
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
