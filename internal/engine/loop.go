package engine

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	UsageSince(ctx context.Context, taskID, afterEventID int64) ([]store.Usage, error)
	UsageBetween(ctx context.Context, from, to time.Time) ([]store.Usage, error)
	UsageTotals(ctx context.Context, taskID int64) ([]store.Usage, error)
	CreateRun(ctx context.Context, r store.Run) (store.Run, error)
	UpdateRun(ctx context.Context, r store.Run) error
	OpenRun(ctx context.Context, taskID int64) (store.Run, bool, error)
	SetTaskDeps(ctx context.Context, taskID int64, dependsOn []int64) error
	TaskDeps(ctx context.Context, taskID int64) ([]int64, error)
	LastEventIDs(ctx context.Context) (map[int64]int64, error)
	Projects(ctx context.Context) ([]store.Project, error)
	ProjectNames(ctx context.Context) (map[int64]string, error)
	Tasks(ctx context.Context, includeReplays bool) ([]store.Task, error)
	RemoveProject(ctx context.Context, id int64, at time.Time, secretPrefix string) error
	Credentials(ctx context.Context) ([]store.Credential, error)
	PutCredential(ctx context.Context, c store.Credential) error
	RemoveCredential(ctx context.Context, name, secretPrefix string) error
	ReceiveImage(ctx context.Context, img store.Image, r io.Reader, most int64) (*store.Upload, error)
	KeepImage(ctx context.Context, u *store.Upload, expectLast int64) (store.Image, error)
	TaskImages(ctx context.Context, taskID int64) ([]store.Image, error)
	OpenImage(ctx context.Context, taskID, id int64) (store.Image, *os.File, error)
	RemoveImages(ctx context.Context, taskID int64, ids []int64, expectLast int64) error
}

// Loop carries out the actions Decide chooses, through a Runner.
type Loop struct {
	Store          Store
	Vault          *auth.Vault
	Runner         Runner
	Now            func() time.Time
	DataDir, Image string
	Paused         func(taskID int64) Pause // what other tasks' open stops mean for the task; nil: nothing

	finished sync.Map // task id to finishedRow
}

// Run row outcomes besides the stop reasons that end a run.
const (
	RunExported  = "exported"
	RunFinished  = "finished"  // the run ended with its work over and no export: a merge came first, or a fix run changed nothing
	RunAbandoned = "abandoned" // the row's run was never claimed
)

// The loop's caps, per RunTask call. Hitting one is an error, not a stop.
const (
	maxActions   = 100 // decisions
	maxConflicts = 5   // claims lost in a row
	maxRepairs   = 100 // stale run rows ended
)

// defaultCaps bounds what Decide starts by itself: the continuations, the fix
// rounds after verify and after the review, the retries of a fault, the fix
// runs on a pull request's failed checks, and the runs on its reviewers' requests.
var defaultCaps = Caps{Continuations: 2, VerifyFixes: 2, ReviewFixes: 5, FaultRetries: 2, CIFixes: 2, CommentRuns: 3}

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

// endTimeout bounds a Release that removes volumes, and the end of the run's
// row, on a context that outlives the caller's.
const endTimeout = time.Minute

// RunTask decides and acts until the task rests (action none or watch) or
// waits, or ctx ends. It returns the final status. The caller keeps one live
// RunTask per task by holding the data-directory lock or a per-task lock: the
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
		case KindNone, KindWatch:
			return done(nil)
		case KindWait:
			// A waiting task holds no container. A stop's halt already released its run.
			if v.Facts.RunOpen && v.Facts.OpenStop == nil {
				return done(p.free(ctx, v.Task, v.run, KeepVolumes, ""))
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

	run    store.Run     // the open run's row, while Facts.RunOpen
	events []store.Event // the task's events but the agent's progress
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

	set, err := l.Store.Settings(ctx)
	if err != nil {
		return Snapshot{}, nil, fmt.Errorf("task %d: %w", taskID, err)
	}

	limits, err := readLimits(set, pr.Caps)
	if err != nil {
		return Snapshot{}, nil, fmt.Errorf("task %d: %w", taskID, err)
	}

	ciTimeout, err := CITimeout(set)
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
		Task: t, Facts: Reduce(events), Deps: deps, Now: l.Now(), Switches: sw, Caps: defaultCaps, Limits: limits, LastEventID: last,
	}
	s.Watch.NoCI, s.Watch.CITimeout = pr.Forge.NoCI, ciTimeout

	if limits.capped() {
		if s.Spent, s.prices, err = l.spent(ctx, taskID, s.Facts.LastHumanEventID, set, s.Now); err != nil {
			return Snapshot{}, nil, fmt.Errorf("task %d: %w", taskID, err)
		}
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

	v := view{Snapshot: s, events: events}

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

	started, ended := map[int64]bool{}, map[int64]RunEndedPayload{}

	for _, e := range events {
		switch e.Type {
		case RunStarted:
			started[e.RunID] = true
		case RunEnded:
			ended[e.RunID] = decode[RunEndedPayload](e.Payload)
		}
	}

	for range maxRepairs {
		r, ok, err := p.Store.OpenRun(ctx, p.taskID)
		if err != nil {
			return fmt.Errorf("task %d: %w", p.taskID, err)
		}

		end, done := ended[r.ID]

		switch {
		case !ok || started[r.ID] && !done:
			return nil
		case done:
			// The run ended but was never released; an end that kept no home took it along.
			remove := RemoveAll
			if end.KeptHome {
				remove = RemoveRun
			}

			err = p.free(ctx, t, r, remove, end.Outcome)
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
	// A stop, a finish and a clean build no run config, so a broken setting
	// cannot keep a person's cancel, or the ends that follow it, out of the
	// record. A ready-marking and a reply run on a forge call's config.
	switch a.Kind {
	case KindStop:
		var run *store.Run
		if v.Facts.RunOpen {
			run = &v.run
		}

		return p.halt(ctx, v.LastEventID, v.Task, run, stopPayload(a), nil)
	case KindFinish:
		outcome := RunFinished
		if v.Facts.Exported {
			outcome = RunExported
		}

		if err := p.write(ctx, v.LastEventID, v.run.ID, nil,
			draft{RunEnded, actorEngine, RunEndedPayload{Outcome: outcome, Why: a.Why, KeptHome: true}}); err != nil {
			return err
		}

		return p.free(ctx, v.Task, v.run, RemoveRun, outcome)
	case KindClean:
		// The newest run's release removes the home before it is recorded: one that
		// fails records nothing, so the clean is done again, and a release of nothing succeeds.
		ref, err := p.ref(ctx, v.Task, store.Run{ID: v.Facts.LastRun})
		if err != nil {
			return err
		}

		if err := p.Runner.Release(ctx, ref, RemoveAll); err != nil {
			return fmt.Errorf("task %d: release run %d: %w", p.taskID, ref.RunID, err)
		}

		return p.write(ctx, v.LastEventID, ref.RunID, nil, draft{HomeRemoved, actorEngine, HomeRemovedPayload{Why: a.Why}})
	case KindReady:
		return p.ready(ctx, v, a)
	case KindReply:
		return p.reply(ctx, v, a)
	}

	row := v.run

	if a.NewRun {
		reason := a.Reason
		if reason == "" {
			reason = "initial"
			if hadRun(v.events) {
				reason = "retry"
			}
		}

		var err error
		if row, err = p.Store.CreateRun(ctx, store.Run{TaskID: p.taskID, Reason: reason, StartedAt: p.Now()}); err != nil {
			return fmt.Errorf("task %d: %w", p.taskID, err)
		}
	}

	o, err := p.open(ctx, v.Task, row)
	if err != nil {
		return p.unconfigured(ctx, v, a, &row, err)
	}

	// A fix run works on the pull request's branch from its head: the open
	// run's, or for a new run the head it is about to record.
	head := v.Facts.RunHead
	if a.NewRun && v.Facts.PR != nil {
		head = v.Facts.PR.Head
	}

	if head != "" {
		o.cfg.Repo.Head, o.cfg.Repo.Branch = head, v.Facts.PR.Branch
	}

	if v.Limits.capped() {
		b := &o.cfg.Budget
		b.Capped, b.MicroUSD, b.Prices = true, moneyLeft(v.Snapshot), v.prices
	}

	if err := p.images(ctx, a.Kind, &o.cfg); err != nil {
		return p.unconfigured(ctx, v, a, &row, err)
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
	case KindReview:
		return p.review(ctx, v, a, o)
	case KindExport:
		return p.export(ctx, v, a, o)
	case KindPublish:
		return p.publish(ctx, v, a, o)
	}

	return fmt.Errorf("task %d: no handler for action %q", p.taskID, a.Kind)
}

func (p *pass) open(ctx context.Context, t store.Task, row store.Run) (openRun, error) {
	cfg, err := p.BuildRunConfig(ctx, t, row, t.Overrides)

	return openRun{task: t, row: row, cfg: cfg}, err
}

// images gives a work or review action, whose sessions alone read the task's
// images, their bytes, and keeps a prepare, a work or a review from a task
// whose images hold more than TaskImagesMaxBytes, as no session of it could start.
func (p *pass) images(ctx context.Context, kind ActionKind, c *RunConfig) error {
	switch kind {
	case KindPrepare:
		held, err := imagesOver(ctx, p.Store, p.taskID)
		if err == nil && held != "" {
			err = imagesFull(held)
		}

		return err
	case KindWork, KindReview:
		images, err := p.taskImages(ctx, p.taskID)
		c.Task.Images = images

		return err
	}

	return nil
}

// unconfigured stops a task whose run config cannot be built, as a runner
// fault does, so a person fixes the cause and retries; one whose images hold
// more than TaskImagesMaxBytes stops as images, which no retry clears while
// they do. The stop is claimed like an action. An open run keeps its volumes.
// A new run's row is abandoned and the stop carries no run, as does the stop
// of an action that runs with no run open, whose run is nil.
func (p *pass) unconfigured(ctx context.Context, v view, a Action, run *store.Run, cause error) error {
	sp := StoppedPayload{
		Reason: StopSandbox, Explanation: line(p.mask("The run cannot be configured: " + cause.Error())), Action: ClearRetry,
		Detail: cause.Error(),
	}

	if full, ok := errors.AsType[imagesFull](cause); ok {
		sp = StoppedPayload{Reason: StopImages, Action: ClearRetry, Explanation: "The task's images " + full.hold() +
			". Cancel the task, restore it to a draft, remove images and start it again."}
	}

	if a.NewRun {
		if err := p.endRun(ctx, *run, RunAbandoned); err != nil {
			return fmt.Errorf("%w (abandoning after: %v)", err, cause)
		}

		run = nil
	}

	return p.halt(ctx, v.LastEventID, v.Task, run, sp, nil)
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
		claim = slices.Insert(claim, 0, draft{RunStarted, actorEngine, RunStartedPayload{
			Reason: o.row.Reason, Head: o.cfg.Repo.Head, Requests: a.Requests,
		}})
	}

	if err := p.claim(ctx, v, o, claim...); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return p.abandon(ctx, a, o.row, err)
		}

		return err
	}

	res, err := p.Runner.Prepare(ctx, o.cfg)
	if err != nil {
		return p.fault(ctx, o, 0, nil, err)
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

// work starts or resumes the task's session, which every later work action
// resumes in its role; a fresh action starts a new one in role strong instead.
func (p *pass) work(ctx context.Context, v view, a Action, o openRun) error {
	w := WorkRequest{
		Mode: a.WorkMode, SessionID: v.Facts.SessionID, Resume: v.Facts.SessionID != "" && !a.Fresh,
		Role: cmp.Or(v.Facts.Role, RoleWork),
	}

	if a.Fresh {
		w.Role = RoleStrong
	}

	if !w.Resume {
		w.SessionID = NewSessionID()
	}

	o.cfg.Agent.MaxTurns, o.cfg.Agent.Timeout = left(v.Snapshot)

	switch a.WorkMode {
	case ModeFixVerify:
		w.Detail = v.Facts.LastVerifyOutput
	case ModeAnswer:
		w.Detail = a.Detail
	case ModeFixReview:
		w.Findings = v.Facts.Review.Open
	case ModeComments:
		w.Comments = requested(v.Facts.PR.Seen, a.Requests)
	}

	if err := p.claim(ctx, v, o, draft{SessionStarted, actorEngine, SessionStartedPayload{
		Mode: w.Mode, SessionID: w.SessionID, Resume: w.Resume, Automatic: a.Automatic, Role: w.Role, Why: a.Why,
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
		return p.fault(ctx, o, res.Turns, runUsage(o.row.ID, res.Usage), err)
	}

	return p.result(ctx, o, runUsage(o.row.ID, res.Usage), draft{SessionEnded, actorRunner, SessionEndedPayload{
		Outcome: res.Outcome, Title: res.Title, Summary: res.Summary, Assumptions: res.Assumptions,
		Questions: res.Questions, SuggestedAnswers: res.SuggestedAnswers, WhereToLook: res.WhereToLook, Changed: res.Changed,
		Turns: res.Turns, CostEstimateUSD: res.CostEstimateUSD, CostMissing: res.CostMissing, ResetsAt: res.ResetsAt,
		Detail: res.Detail, HeadCommit: res.HeadCommit,
	}})
}

// requested is the requests seen holds that ids name, in seen's order, from
// the record and never from a fresh look: masked already, and not masked again.
func requested(seen *Observation, ids []string) []Comment {
	var out []Comment

	for _, c := range seen.Comments {
		if slices.Contains(ids, c.ID) {
			out = append(out, c)
		}
	}

	return out
}

// runUsage is a session's usage rows, each carrying the run's id.
func runUsage(runID int64, usage []store.Usage) []store.Usage {
	out := make([]store.Usage, len(usage))
	for i, u := range usage {
		u.RunID = runID
		out[i] = u
	}

	return out
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
		return p.fault(ctx, o, 0, nil, err)
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
		return p.fault(ctx, o, 0, nil, err)
	}

	if res.Problem != "" {
		return p.halt(ctx, -1, o.task, &o.row, StoppedPayload{
			Reason: StopAgentError, Explanation: lineOr(p.mask(res.Problem), "Exporting the change failed."), Action: ClearRetry,
			Detail: res.Problem,
		}, nil)
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
		Flagged: Flag(res.Files),
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

// fault records a runner error as a sandbox stop, with the usage of the
// session it cut. While ctx is done the error is not a fault: the usage is kept
// on a session_cut event, the claim stays pending and the next RunTask redoes it.
func (p *pass) fault(ctx context.Context, o openRun, turns int, usage []store.Usage, cause error) error {
	if err := ctx.Err(); err != nil {
		if len(usage) == 0 {
			return err
		}

		if werr := p.result(ctx, o, usage, draft{SessionCut, actorRunner, SessionCutPayload{Turns: turns}}); werr != nil {
			return errors.Join(err, werr)
		}

		return err
	}

	// An action that runs with no run open stops with no run to release.
	var run *store.Run
	if o.row.ID != 0 {
		run = &o.row
	}

	return p.halt(ctx, -1, o.task, run, StoppedPayload{
		Reason: StopSandbox, Explanation: line(p.mask("The runner failed: " + cause.Error())), Action: ClearRetry,
		Detail: cause.Error(),
	}, usage)
}

func stopPayload(a Action) StoppedPayload {
	return StoppedPayload{Reason: a.Stop, Explanation: a.Explain, Action: a.Clear, ResetsAt: a.ResetsAt, Fault: a.Fault, Why: a.Why}
}

// halt records a stop and usage tied to it, with the run's end when the stop
// ends it, then releases the run. run is nil when no run is open. A stop that
// is a runner call's result passes expectLast -1 and is recorded even after ctx ends.
func (p *pass) halt(ctx context.Context, expectLast int64, t store.Task, run *store.Run, sp StoppedPayload, usage []store.Usage) error {
	// A sandbox stop keeps the run, so a retry redoes the cut action on its
	// volumes. A takeover ends it: its export cannot descend from the head a retry takes back.
	ends := sp.Reason == StopCancelled || sp.Reason == StopEnvironment || sp.Reason == StopTakenOver
	drafts := []draft{{Stopped, actorEngine, sp}}

	var runID int64

	if run != nil {
		runID = run.ID

		if ends {
			drafts = append(drafts, draft{RunEnded, actorEngine, RunEndedPayload{Outcome: sp.Reason, KeptHome: true}})
		}
	}

	wctx := ctx

	if expectLast < 0 {
		var cancel context.CancelFunc

		wctx, cancel = recording(ctx)
		defer cancel()
	}

	if err := p.write(wctx, expectLast, runID, usage, drafts...); err != nil {
		return err
	}

	if run == nil {
		return nil
	}

	if !ends {
		return p.free(ctx, t, *run, KeepVolumes, "")
	}

	return p.free(ctx, t, *run, RemoveRun, sp.Reason)
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

	if err := l.Runner.Release(ctx, ref, KeepVolumes); err != nil {
		return fmt.Errorf("task %d: release run %d: %w", taskID, row.ID, err)
	}

	return nil
}

// free stops a run's containers and removes what remove names, then ends the
// run's row with outcome unless that is "". A Release error is held so the row
// is still ended. A run's end is recorded already, so what it removes is
// carried out past ctx's end, or the record would hold what never happened.
func (p *pass) free(ctx context.Context, t store.Task, row store.Run, remove Remove, outcome string) error {
	if remove != KeepVolumes {
		var cancel context.CancelFunc

		ctx, cancel = context.WithTimeout(context.WithoutCancel(ctx), endTimeout)
		defer cancel()
	}

	ref, err := p.ref(ctx, t, row)
	if err != nil {
		return err
	}

	relErr := p.Runner.Release(ctx, ref, remove)
	if relErr != nil {
		relErr = fmt.Errorf("task %d: release run %d: %w", p.taskID, row.ID, relErr)
	}

	if outcome != "" {
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
