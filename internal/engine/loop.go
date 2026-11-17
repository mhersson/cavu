package engine

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/mhersson/cavu/internal/auth"
	"github.com/mhersson/cavu/internal/redact"
	"github.com/mhersson/cavu/internal/store"
)

// Store is the part of *store.Store the engine uses; tests wrap it to inject a conflict.
type Store interface {
	CreateTask(ctx context.Context, t store.Task) (store.Task, error)
	Task(ctx context.Context, id int64) (store.Task, error)
	UpdateTask(ctx context.Context, t store.Task) error
	Project(ctx context.Context, id int64) (store.Project, error)
	CreateProject(ctx context.Context, p store.Project) (store.Project, error)
	UpdateProject(ctx context.Context, p store.Project) error
	Settings(ctx context.Context) (map[string]string, error)
	Events(ctx context.Context, taskID, afterID int64, limit int) ([]store.Event, error)
	EventsOmitting(ctx context.Context, taskID int64, omit ...string) ([]store.Event, error)
	LastEventID(ctx context.Context, taskID int64) (int64, error)
	Append(ctx context.Context, taskID, expectLast int64, events []store.Event, usage []store.Usage) (int64, error)
	CreateRun(ctx context.Context, r store.Run) (store.Run, error)
	UpdateRun(ctx context.Context, r store.Run) error
	OpenRun(ctx context.Context, taskID int64) (store.Run, bool, error)
	SetTaskDeps(ctx context.Context, taskID int64, dependsOn []int64) error
	TaskDeps(ctx context.Context, taskID int64) ([]int64, error)
	LastEventIDs(ctx context.Context) (map[int64]int64, error)
	Projects(ctx context.Context) ([]store.Project, error)
}

// Loop carries out the actions Decide chooses, through a Runner.
type Loop struct {
	Store          Store
	Vault          *auth.Vault
	Runner         Runner
	Now            func() time.Time
	DataDir, Image string
	Paused         func(taskID int64) Pause // what other tasks' open stops mean for the task; nil: nothing
}

// Run row outcomes besides the stop reasons that end a run.
const (
	RunExported  = "exported"
	RunAbandoned = "abandoned" // the row's run was never claimed
)

// The loop's caps, per RunTask call. Hitting one is an error, not a stop.
const (
	maxActions   = 100 // decisions
	maxConflicts = 5   // claims lost in a row
	maxRepairs   = 100 // stale run rows ended
)

// defaultCaps bounds the automatic continuations, fix rounds and fault retries Decide allows.
var defaultCaps = Caps{Continuations: 2, VerifyFixes: 2, FaultRetries: 2}

var (
	// ErrActionCap is returned when RunTask decides maxActions times and the task has neither rested nor waited.
	ErrActionCap = errors.New("engine: action cap reached")
	// ErrConflictCap is returned when maxConflicts claims in a row lose to another writer.
	ErrConflictCap = errors.New("engine: claim conflict cap reached")

	errNoVerifyResult = errors.New("the runner returned a verify result with no result")
)

const (
	actorEngine = "engine"
	actorRunner = "runner"
	actorAgent  = "agent"
	actorUser   = "user"
)

// progressActors are the progress types the loop records, with their actors.
// Any other type is dropped, so a runner cannot forge a claim or a result.
var progressActors = map[string]string{
	AgentText: actorAgent, AgentTool: actorAgent, RateLimit: actorAgent, EgressDenied: actorRunner,
}

const maxProgressText = 2 << 10 // bytes kept of agent_text.text and agent_tool.summary

// recordTimeout bounds recording a runner call's result after ctx has ended.
const recordTimeout = 10 * time.Second

// endTimeout bounds ending a run, its Release with volumes removed and its row,
// on a context that outlives the caller's.
const endTimeout = time.Minute

// RunTask decides and acts until the task rests (action none) or waits, or ctx
// ends. It returns the final status. The caller keeps one live RunTask per
// task by holding the data-directory lock or a per-task lock: the
// compare-and-swap guards claims, not actions in flight.
func (l *Loop) RunTask(ctx context.Context, taskID int64) (Status, error) {
	p, err := l.newPass(ctx, taskID)
	if err != nil {
		return "", err
	}

	var status Status

	// Held errors never stop the loop; every return carries them.
	done := func(err error) (Status, error) { return status, errors.Join(err, p.held) }

	if err := p.repair(ctx); err != nil {
		return done(err)
	}

	conflicts := 0

	for range maxActions {
		v, err := p.view(ctx)
		if err != nil {
			return done(err)
		}

		status = StatusOf(v.Facts)
		a := Decide(v.Snapshot)

		switch a.Kind {
		case KindNone:
			return done(nil)
		case KindWait:
			// A waiting task holds no container. A stop's halt already released its run.
			if v.Facts.RunOpen && v.Facts.OpenStop == nil {
				return done(p.free(ctx, v.Task, v.run, false, ""))
			}

			return done(nil)
		}

		err = p.act(ctx, v, a)
		if errors.Is(err, store.ErrConflict) {
			if conflicts++; conflicts == maxConflicts {
				return done(fmt.Errorf("task %d: %w after %d lost claims", taskID, ErrConflictCap, conflicts))
			}

			continue
		}

		if err != nil {
			return done(err)
		}

		conflicts = 0
	}

	return done(fmt.Errorf("task %d: %w after %d actions", taskID, ErrActionCap, maxActions))
}

// pass is one RunTask call.
type pass struct {
	*Loop

	taskID int64
	mask   func(string) string
	held   error // an error that must not stop the loop, such as a failed Release
}

// newPass masks with the vault's secrets as they are now.
func (l *Loop) newPass(ctx context.Context, taskID int64) (*pass, error) {
	secrets, err := l.Vault.Values(ctx)
	if err != nil {
		return nil, fmt.Errorf("task %d: %w", taskID, err)
	}

	return &pass{Loop: l, taskID: taskID, mask: redact.New(secrets...)}, nil
}

// view is a snapshot with what the loop needs beside it.
type view struct {
	Snapshot

	run    store.Run // the open run's row, while Facts.RunOpen
	hadRun bool      // a run was started before
}

// openRun is a run row with the task and the config its runner calls use.
type openRun struct {
	task store.Task
	row  store.Run
	cfg  RunConfig
}

// draft is an event before the loop stamps, encodes and masks it.
type draft struct {
	typ, actor string
	payload    any
}

// Snapshot reads the task as Decide sees it now.
func (l *Loop) Snapshot(ctx context.Context, taskID int64) (Snapshot, error) {
	s, _, err := l.snapshot(ctx, taskID)

	return s, err
}

// snapshot also returns the events it reduced. The newest event id is read
// first, so an event that lands after it makes the claim lose and be decided again.
func (l *Loop) snapshot(ctx context.Context, taskID int64) (Snapshot, []store.Event, error) {
	last, err := l.Store.LastEventID(ctx, taskID)
	if err != nil {
		return Snapshot{}, nil, fmt.Errorf("task %d: %w", taskID, err)
	}

	events, err := l.Store.EventsOmitting(ctx, taskID, ProgressTypes...)
	if err != nil {
		return Snapshot{}, nil, fmt.Errorf("task %d: %w", taskID, err)
	}

	t, err := l.Store.Task(ctx, taskID)
	if err != nil {
		return Snapshot{}, nil, fmt.Errorf("task %d: %w", taskID, err)
	}

	pr, err := l.Store.Project(ctx, t.ProjectID)
	if err != nil {
		return Snapshot{}, nil, fmt.Errorf("task %d: project %d: %w", taskID, t.ProjectID, err)
	}

	sw, err := resolveSwitches(pr, t.Overrides)
	if err != nil {
		return Snapshot{}, nil, fmt.Errorf("task %d: %w", taskID, err)
	}

	ids, err := l.Store.TaskDeps(ctx, taskID)
	if err != nil {
		return Snapshot{}, nil, fmt.Errorf("task %d: %w", taskID, err)
	}

	deps := make([]Dep, len(ids))

	for i, id := range ids {
		evs, err := l.Store.EventsOmitting(ctx, id, ProgressTypes...)
		if err != nil {
			return Snapshot{}, nil, fmt.Errorf("task %d: dependency %d: %w", taskID, id, err)
		}

		deps[i] = Dep{TaskID: id, Status: StatusOf(Reduce(evs))}
	}

	s := Snapshot{
		Task: t, Facts: Reduce(events), Deps: deps, Now: l.Now(), Switches: sw, Caps: defaultCaps, LastEventID: last,
	}

	if l.Paused != nil {
		s.Pause = l.Paused(taskID)
	}

	return s, events, nil
}

func (p *pass) view(ctx context.Context) (view, error) {
	s, events, err := p.snapshot(ctx, p.taskID)
	if err != nil {
		return view{}, err
	}

	v := view{Snapshot: s, hadRun: hadRun(events)}

	if v.Facts.RunOpen {
		r, ok, err := p.Store.OpenRun(ctx, p.taskID)
		if err != nil {
			return view{}, fmt.Errorf("task %d: %w", p.taskID, err)
		}

		if !ok {
			return view{}, fmt.Errorf("task %d: the events hold a run open but no run row is open", p.taskID)
		}

		v.run = r
	}

	return v, nil
}

func hadRun(events []store.Event) bool {
	return slices.ContainsFunc(events, func(e store.Event) bool { return e.Type == RunStarted })
}

// repair makes the run rows follow the events. It ends the newest open row
// until that row is the run the events hold open, or no open row is left.
func (p *pass) repair(ctx context.Context) error {
	t, err := p.Store.Task(ctx, p.taskID)
	if err != nil {
		return fmt.Errorf("task %d: %w", p.taskID, err)
	}

	events, err := p.Store.Events(ctx, p.taskID, 0, 0)
	if err != nil {
		return fmt.Errorf("task %d: %w", p.taskID, err)
	}

	started, ended := map[int64]bool{}, map[int64]string{}

	for _, e := range events {
		switch e.Type {
		case RunStarted:
			started[e.RunID] = true
		case RunEnded:
			ended[e.RunID] = decode[RunEndedPayload](e.Payload).Outcome
		}
	}

	for range maxRepairs {
		r, ok, err := p.Store.OpenRun(ctx, p.taskID)
		if err != nil {
			return fmt.Errorf("task %d: %w", p.taskID, err)
		}

		outcome, done := ended[r.ID]

		switch {
		case !ok || started[r.ID] && !done:
			return nil
		case done:
			// The run ended but was never released.
			err = p.free(ctx, t, r, true, outcome)
		default:
			err = p.endRun(ctx, r, RunAbandoned)
		}

		if err != nil {
			return err
		}
	}

	return fmt.Errorf("task %d: more than %d stale run rows", p.taskID, maxRepairs)
}

func (p *pass) act(ctx context.Context, v view, a Action) error {
	// A stop or finish is claimed before its config is built, so a broken
	// setting cannot keep a person's cancel out of the record.
	switch a.Kind {
	case KindStop:
		var run *store.Run
		if v.Facts.RunOpen {
			run = &v.run
		}

		return p.halt(ctx, v.LastEventID, v.Task, run, stopPayload(a))
	case KindFinish:
		if err := p.write(ctx, v.LastEventID, v.run.ID, nil,
			draft{RunEnded, actorEngine, RunEndedPayload{Outcome: RunExported, Why: a.Why}}); err != nil {
			return err
		}

		return p.free(ctx, v.Task, v.run, true, RunExported)
	}

	row := v.run

	if a.NewRun {
		reason := "initial"
		if v.hadRun {
			reason = "retry"
		}

		var err error
		if row, err = p.Store.CreateRun(ctx, store.Run{TaskID: p.taskID, Reason: reason, StartedAt: p.Now()}); err != nil {
			return fmt.Errorf("task %d: %w", p.taskID, err)
		}
	}

	o, err := p.open(ctx, v.Task, row)
	if err != nil {
		return p.unconfigured(ctx, v, a, row, err)
	}

	if a.Kind == KindPrepare {
		return p.prepare(ctx, v, a, o)
	}

	// From the record, so a runner that restarted never reads the workspace
	// an agent may have changed to learn them again.
	o.cfg.Ecosystems = v.Facts.Ecosystems

	switch a.Kind {
	case KindWork:
		return p.work(ctx, v, a, o)
	case KindVerify:
		return p.verify(ctx, v, a, o)
	case KindExport:
		return p.export(ctx, v, a, o)
	}

	return fmt.Errorf("task %d: no handler for action %q", p.taskID, a.Kind)
}

func (p *pass) open(ctx context.Context, t store.Task, row store.Run) (openRun, error) {
	cfg, err := p.BuildRunConfig(ctx, t, row, t.Overrides)

	return openRun{task: t, row: row, cfg: cfg}, err
}

// unconfigured stops a task whose run config cannot be built, as a runner
// fault does, so a person fixes the cause and retries. The stop is claimed
// like an action. An open run keeps its volumes; a new run's row is abandoned
// and the stop carries no run.
func (p *pass) unconfigured(ctx context.Context, v view, a Action, row store.Run, cause error) error {
	run := &row

	if a.NewRun {
		if err := p.endRun(ctx, row, RunAbandoned); err != nil {
			return fmt.Errorf("%w (abandoning after: %v)", err, cause)
		}

		run = nil
	}

	return p.halt(ctx, v.LastEventID, v.Task, run, StoppedPayload{
		Reason: StopSandbox, Explanation: line(p.mask("The run cannot be configured: " + cause.Error())), Action: ClearRetry,
		Detail: cause.Error(),
	})
}

// abandon ends a new run row whose claim was never written, then returns cause.
// A failed end replaces cause, so a lost claim is not retried over an open row.
func (p *pass) abandon(ctx context.Context, a Action, row store.Run, cause error) error {
	if !a.NewRun {
		return cause
	}

	if err := p.endRun(ctx, row, RunAbandoned); err != nil {
		return fmt.Errorf("%w (abandoning after: %v)", err, cause)
	}

	return cause
}

func (p *pass) prepare(ctx context.Context, v view, a Action, o openRun) error {
	claim := []draft{{PrepareStarted, actorEngine, PrepareStartedPayload{Why: a.Why}}}
	if a.NewRun {
		claim = slices.Insert(claim, 0, draft{RunStarted, actorEngine, RunStartedPayload{Reason: o.row.Reason}})
	}

	if err := p.claim(ctx, v, o, claim...); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return p.abandon(ctx, a, o.row, err)
		}

		return err
	}

	res, err := p.Runner.Prepare(ctx, o.cfg)
	if err != nil {
		return p.fault(ctx, o, err)
	}

	// A failed baseline is an environment stop even when the runner names no step.
	if res.FailedStep == "" && res.Baseline == ResultFailed {
		res.FailedStep = "baseline"
	}

	if res.FailedStep != "" {
		return p.result(ctx, o, nil, draft{PrepareFailed, actorRunner, PrepareFailedPayload{Step: res.FailedStep, Output: res.Output}})
	}

	// Set before prepared is written, so a crash in between redoes the prepare.
	if v.Task.BaseCommit == "" && res.BaseCommit != "" {
		if err := p.setBaseCommit(ctx, res.BaseCommit); err != nil {
			return err
		}
	}

	// A verified baseline is stored before prepared is written, so the next task
	// at this commit reuses it. A replay's shadow verified an old commit.
	if res.Baseline == ResultPassed && o.task.ReplayID == 0 {
		if err := p.setBaseline(ctx, o.task.ProjectID, res.BaseCommit); err != nil {
			return err
		}
	}

	return p.result(ctx, o, nil, draft{Prepared, actorRunner, PreparedPayload{
		BaseCommit: res.BaseCommit, Tools: res.Tools, IgnoredTools: res.IgnoredTools, Ecosystems: res.Ecosystems,
		Baseline: res.Baseline,
	}})
}

func (p *pass) setBaseline(ctx context.Context, projectID int64, commit string) error {
	ctx, cancel := recording(ctx)
	defer cancel()

	pr, err := p.Store.Project(ctx, projectID)
	if err == nil {
		pr.Baseline = &store.Baseline{Commit: commit, Passed: true, At: p.Now()}
		err = p.Store.UpdateProject(ctx, pr)
	}

	if err != nil {
		return fmt.Errorf("task %d: baseline: %w", p.taskID, err)
	}

	return nil
}

func (p *pass) setBaseCommit(ctx context.Context, commit string) error {
	ctx, cancel := recording(ctx)
	defer cancel()

	t, err := p.Store.Task(ctx, p.taskID)
	if err == nil && t.BaseCommit == "" {
		t.BaseCommit = commit
		err = p.Store.UpdateTask(ctx, t)
	}

	if err != nil {
		return fmt.Errorf("task %d: base commit: %w", p.taskID, err)
	}

	return nil
}

func (p *pass) work(ctx context.Context, v view, a Action, o openRun) error {
	w := WorkRequest{Mode: a.WorkMode, SessionID: v.Facts.SessionID, Resume: v.Facts.SessionID != ""}
	if !w.Resume {
		w.SessionID = NewSessionID()
	}

	switch a.WorkMode {
	case ModeFixVerify:
		w.Detail = v.Facts.LastVerifyOutput
	case ModeAnswer:
		w.Detail = a.Detail
	}

	if err := p.claim(ctx, v, o, draft{SessionStarted, actorEngine, SessionStartedPayload{
		Mode: w.Mode, SessionID: w.SessionID, Resume: w.Resume, Automatic: a.Automatic, Why: a.Why,
	}}); err != nil {
		return err
	}

	if o.row.SessionID != w.SessionID {
		o.row.SessionID = w.SessionID
		if err := p.Store.UpdateRun(ctx, o.row); err != nil {
			return fmt.Errorf("task %d: run %d session: %w", p.taskID, o.row.ID, err)
		}
	}

	add, stop := p.progress(ctx, o.row.ID)
	res, err := p.Runner.Work(ctx, o.cfg, w, add)
	p.hold(stop())

	if err != nil {
		return p.fault(ctx, o, err)
	}

	usage := make([]store.Usage, len(res.Usage))
	for i, u := range res.Usage {
		u.RunID = o.row.ID
		usage[i] = u
	}

	if err := p.result(ctx, o, usage, draft{SessionEnded, actorRunner, SessionEndedPayload{
		Outcome: res.Outcome, Title: res.Title, Summary: res.Summary, Assumptions: res.Assumptions,
		Questions: res.Questions, SuggestedAnswers: res.SuggestedAnswers, Changed: res.Changed, Turns: res.Turns,
		CostEstimateUSD: res.CostEstimateUSD, ResetsAt: res.ResetsAt, Detail: res.Detail, HeadCommit: res.HeadCommit,
	}}); err != nil {
		return err
	}

	return nil
}

// progress returns the callback a session reports through, which may be called
// from any goroutine, and a stop that closes it and returns its first append
// error. Holding the lock across each append keeps progress before the result.
func (p *pass) progress(ctx context.Context, runID int64) (add func(Progress), stop func() error) {
	var (
		mu     sync.Mutex
		closed bool
		first  error
	)

	add = func(pr Progress) {
		mu.Lock()
		defer mu.Unlock()

		if closed {
			return
		}

		if err := p.recordProgress(ctx, runID, pr); err != nil && first == nil {
			first = err
		}
	}

	stop = func() error {
		mu.Lock()
		defer mu.Unlock()

		closed = true

		return first
	}

	return add, stop
}

func (p *pass) recordProgress(ctx context.Context, runID int64, pr Progress) error {
	actor, ok := progressActors[pr.Type]
	if !ok {
		return nil
	}

	payload, err := progressPayload(pr, p.mask)
	if err != nil {
		return fmt.Errorf("task %d: progress %s: %w", p.taskID, pr.Type, err)
	}

	return p.write(ctx, -1, runID, nil, draft{pr.Type, actor, payload})
}

// progressPayload reads a progress value as its event's payload struct, so only
// that struct's fields are kept. Text is masked before it is cut, so a cut
// cannot leave the head of a secret behind.
func progressPayload(pr Progress, mask func(string) string) (any, error) {
	switch pr.Type {
	case AgentText:
		v, err := retype[AgentTextPayload](pr.Payload)
		v.Text = clip(mask(v.Text), maxProgressText)

		return v, err
	case AgentTool:
		v, err := retype[AgentToolPayload](pr.Payload)
		v.Summary = clip(mask(v.Summary), maxProgressText)

		return v, err
	case RateLimit:
		v, err := retype[RateLimitPayload](pr.Payload)

		return v, err
	default:
		v, err := retype[EgressDeniedPayload](pr.Payload)

		return v, err
	}
}

func retype[T any](v any) (T, error) {
	var out T

	raw, err := json.Marshal(v)
	if err != nil {
		return out, err
	}

	err = json.Unmarshal(raw, &out)

	return out, err
}

// clip cuts s to at most n bytes without splitting a UTF-8 sequence.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}

	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}

	return s[:n]
}

func (p *pass) verify(ctx context.Context, v view, a Action, o openRun) error {
	if err := p.claim(ctx, v, o, draft{VerifyStarted, actorEngine, VerifyStartedPayload{Why: a.Why}}); err != nil {
		return err
	}

	res, err := p.Runner.Verify(ctx, o.cfg)
	if err == nil && res.Result == "" {
		err = errNoVerifyResult
	}

	if err != nil {
		return p.fault(ctx, o, err)
	}

	return p.result(ctx, o, nil, draft{VerifyEnded, actorRunner, VerifyEndedPayload{
		Result: res.Result, Command: res.Command, ExitCode: res.ExitCode, Output: res.Output,
		DurationMS: res.Duration.Milliseconds(),
	}})
}

func (p *pass) export(ctx context.Context, v view, a Action, o openRun) error {
	// Decide exports only after an ended session, so Session is set.
	s := v.Facts.Session

	if err := p.claim(ctx, v, o, draft{ExportStarted, actorEngine, ExportStartedPayload{Why: a.Why}}); err != nil {
		return err
	}

	res, err := p.Runner.Export(ctx, o.cfg, s.HeadCommit)
	if err != nil {
		return p.fault(ctx, o, err)
	}

	if res.Problem != "" {
		return p.halt(ctx, -1, o.task, &o.row, StoppedPayload{
			Reason: StopAgentError, Explanation: lineOr(p.mask(res.Problem), "Exporting the change failed."), Action: ClearRetry,
			Detail: res.Problem,
		})
	}

	// A failed write is not a stop: the claim stays pending and the next RunTask exports again.
	dir := filepath.Join(p.DataDir, "artifacts", strconv.FormatInt(p.taskID, 10), strconv.FormatInt(o.row.ID, 10))

	diff, err := writeArtifact(dir, "diff.patch", res.Diff)
	if err != nil {
		return fmt.Errorf("task %d: export: %w", p.taskID, err)
	}

	bundle, err := writeArtifact(dir, "task.bundle", res.Bundle)
	if err != nil {
		return fmt.Errorf("task %d: export: %w", p.taskID, err)
	}

	return p.result(ctx, o, nil, draft{Exported, actorRunner, ExportedPayload{
		HeadCommit: res.HeadCommit, DiffPath: diff, BundlePath: bundle, FilesChanged: res.FilesChanged, DiffStat: res.DiffStat,
	}})
}

// writeArtifact writes data to dir/name, replacing a file a repeated export
// left, and returns the file's path.
func writeArtifact(dir, name string, data []byte) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}

	path := filepath.Join(dir, name)

	return path, os.WriteFile(path, data, 0o600)
}

// fault records a runner error as a sandbox stop. While ctx is done the error
// is not a fault: the claim stays pending and the next RunTask redoes it.
func (p *pass) fault(ctx context.Context, o openRun, cause error) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	return p.halt(ctx, -1, o.task, &o.row, StoppedPayload{
		Reason: StopSandbox, Explanation: line(p.mask("The runner failed: " + cause.Error())), Action: ClearRetry,
		Detail: cause.Error(),
	})
}

func stopPayload(a Action) StoppedPayload {
	return StoppedPayload{Reason: a.Stop, Explanation: a.Explain, Action: a.Clear, ResetsAt: a.ResetsAt, Why: a.Why}
}

// halt records a stop, with the run's end when the stop ends it, then releases
// the run. run is nil when no run is open. A stop that is a runner call's
// result passes expectLast -1 and is recorded even after ctx ends.
func (p *pass) halt(ctx context.Context, expectLast int64, t store.Task, run *store.Run, sp StoppedPayload) error {
	// A sandbox stop keeps the run, so a retry redoes the cut action on its volumes.
	ends := sp.Reason == StopCancelled || sp.Reason == StopEnvironment
	drafts := []draft{{Stopped, actorEngine, sp}}

	var runID int64

	if run != nil {
		runID = run.ID

		if ends {
			drafts = append(drafts, draft{RunEnded, actorEngine, RunEndedPayload{Outcome: sp.Reason}})
		}
	}

	wctx := ctx

	if expectLast < 0 {
		var cancel context.CancelFunc

		wctx, cancel = recording(ctx)
		defer cancel()
	}

	if err := p.write(wctx, expectLast, runID, nil, drafts...); err != nil {
		return err
	}

	if run == nil {
		return nil
	}

	return p.free(ctx, t, *run, ends, sp.Reason)
}

// ref names a run for Release from the task, the row, the loop's image and the
// instance id, so a broken setting or secret cannot keep a run's containers.
func (l *Loop) ref(ctx context.Context, t store.Task, row store.Run) (RunRef, error) {
	set, err := l.Store.Settings(ctx)
	if err != nil {
		return RunRef{}, fmt.Errorf("task %d: release run %d: %w", t.ID, row.ID, err)
	}

	return RunRef{InstanceID: set["instance_id"], Image: l.Image, TaskID: t.ID, RunID: row.ID, ProjectID: t.ProjectID}, nil
}

// ReleaseOpenRun stops the containers of the task's open run and keeps its
// volumes, so the next RunTask carries on in them after a RunTask was cut short.
func (l *Loop) ReleaseOpenRun(ctx context.Context, taskID int64) error {
	t, err := l.Store.Task(ctx, taskID)
	if err != nil {
		return fmt.Errorf("task %d: %w", taskID, err)
	}

	row, ok, err := l.Store.OpenRun(ctx, taskID)
	if err != nil || !ok {
		return err
	}

	ref, err := l.ref(ctx, t, row)
	if err != nil {
		return err
	}

	if err := l.Runner.Release(ctx, ref, false); err != nil {
		return fmt.Errorf("task %d: release run %d: %w", taskID, row.ID, err)
	}

	return nil
}

// free stops a run's containers, removing its volumes and ending its row when
// the run ends. A Release error is held so the row is still ended. An end is
// already recorded, so it is carried out past ctx's end, or the row stays open.
func (p *pass) free(ctx context.Context, t store.Task, row store.Run, end bool, outcome string) error {
	if end {
		var cancel context.CancelFunc

		ctx, cancel = context.WithTimeout(context.WithoutCancel(ctx), endTimeout)
		defer cancel()
	}

	ref, err := p.ref(ctx, t, row)
	if err != nil {
		return err
	}

	relErr := p.Runner.Release(ctx, ref, end)
	if relErr != nil {
		relErr = fmt.Errorf("task %d: release run %d: %w", p.taskID, row.ID, relErr)
	}

	if end {
		if err := p.endRun(ctx, row, outcome); err != nil {
			return errors.Join(err, relErr)
		}
	}

	p.hold(relErr)

	return nil
}

// hold keeps an error RunTask returns without stopping for it.
func (p *pass) hold(err error) {
	if err != nil {
		p.held = errors.Join(p.held, err)
	}
}

func (p *pass) endRun(ctx context.Context, r store.Run, outcome string) error {
	at := p.Now()
	r.Outcome, r.EndedAt = outcome, &at

	if err := p.Store.UpdateRun(ctx, r); err != nil {
		return fmt.Errorf("task %d: end run %d: %w", p.taskID, r.ID, err)
	}

	return nil
}

// claim writes an action's first events if no event came since the snapshot.
// A lost claim returns an error wrapping store.ErrConflict.
func (p *pass) claim(ctx context.Context, v view, o openRun, drafts ...draft) error {
	return p.write(ctx, v.LastEventID, o.row.ID, nil, drafts...)
}

func (p *pass) result(ctx context.Context, o openRun, usage []store.Usage, d draft) error {
	ctx, cancel := recording(ctx)
	defer cancel()

	return p.write(ctx, -1, o.row.ID, usage, d)
}

// recording returns a context for recording a runner call's result. It ignores
// ctx's end, so a result that came back is kept and its action not redone.
func recording(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
}

// write appends drafts as one Append. Every event goes through here, so every
// payload is masked before it reaches the record.
func (p *pass) write(ctx context.Context, expectLast, runID int64, usage []store.Usage, drafts ...draft) error {
	events := make([]store.Event, len(drafts))

	for i, d := range drafts {
		payload, err := p.encode(d.payload)
		if err != nil {
			return fmt.Errorf("task %d: encode %s: %w", p.taskID, d.typ, err)
		}

		events[i] = store.Event{TaskID: p.taskID, RunID: runID, At: p.Now(), Type: d.typ, Actor: d.actor, Payload: payload}
	}

	if _, err := p.Store.Append(ctx, p.taskID, expectLast, events, usage); err != nil {
		return fmt.Errorf("task %d: append %s: %w", p.taskID, drafts[0].typ, err)
	}

	return nil
}

// encode marshals a payload and masks every string in it, map keys included.
// Masking decoded values means JSON escaping cannot hide a secret.
func (p *pass) encode(payload any) (json.RawMessage, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()

	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}

	return json.Marshal(maskValue(v, p.mask))
}

func maskValue(v any, mask func(string) string) any {
	switch x := v.(type) {
	case string:
		return mask(x)
	case []any:
		for i, e := range x {
			x[i] = maskValue(e, mask)
		}
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[mask(k)] = maskValue(e, mask)
		}

		return out
	}

	return v
}

// NewSessionID returns a version 4 UUID.
func NewSessionID() string {
	var b [16]byte

	_, _ = rand.Read(b[:]) // rand.Read never returns an error
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80

	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:])
}
