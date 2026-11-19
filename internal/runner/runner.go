// Package runner carries out a run's actions in the run's sandbox, over a
// container runtime and an agent driver, for the engine.
package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/mhersson/cavu/internal/driver"
	"github.com/mhersson/cavu/internal/engine"
	"github.com/mhersson/cavu/internal/env"
	"github.com/mhersson/cavu/internal/forge"
	"github.com/mhersson/cavu/internal/proc"
	"github.com/mhersson/cavu/internal/proxy"
	"github.com/mhersson/cavu/internal/redact"
	"github.com/mhersson/cavu/internal/sandbox"
	"github.com/mhersson/cavu/internal/sidecar"
)

const (
	loopback  = "localhost,127.0.0.1,::1"
	home      = "/home/agent"
	workspace = "/workspace"
	modeKey   = "key"

	// homeTimeout leaves room to copy a large project's crates on a slow volume.
	homeTimeout = 10 * time.Minute
)

// linkScript makes each link given as a path and its target, replacing what
// is at the path: the home is the agent's to change.
var linkScript = proc.SandboxOnly(`while [ "$#" -ge 2 ]; do
	mkdir -p -- "$(dirname -- "$1")" && rm -rf -- "$1" && ln -s -- "$2" "$1" || exit 1
	shift 2
done`)

// seedScript copies into each directory, given with its source, what the
// source holds that is missing or older there, so a later setup-phase fetch
// replaces a stale index entry in a kept home; -f replaces git's read-only files.
var seedScript = proc.SandboxOnly(`while [ "$#" -ge 2 ]; do
	if [ -d "$2" ]; then mkdir -p -- "$1" && cp -R -u -f -- "$2/." "$1" || exit 1; fi
	shift 2
done`)

// bridgeEnv sends a process's egress through the bridge, the one way out of
// every phase.
var bridgeEnv = []string{
	"HTTP_PROXY=" + sandbox.BridgeURL, "HTTPS_PROXY=" + sandbox.BridgeURL, "http_proxy=" + sandbox.BridgeURL, "https_proxy=" + sandbox.BridgeURL,
}

// noProxyEnv keeps a command's loopback traffic off the bridge; the driver
// does the same for a session.
var noProxyEnv = []string{"NO_PROXY=" + loopback, "no_proxy=" + loopback}

// Runner carries out the engine's actions over a container runtime.
type Runner struct {
	rt     sandbox.Runtime
	driver driver.Driver
	forge  *forge.Client
	labels map[string]string // added to every container and volume; tests set it
	open   func(sandbox.Options) (sandboxer, error)

	mu sync.Mutex
	// runs holds only runs whose volumes are in place and whose ecosystems
	// are known; only Prepare and ensureUp add one.
	runs map[int64]*run
	// turns holds each project's turns: a full channel is a turn taken. A
	// channel, not a mutex, so a waiter can give up with its context.
	turns map[turnKey]chan struct{}
}

// turnKind is one of the turns a project's runs take one at a time.
type turnKind int

const (
	setupTurn turnKind = iota
	// mirrorTurn is held over each write into the project's mirror: two fetches
	// into one mirror at once fail to lock a ref the other moved.
	mirrorTurn
)

type turnKey struct {
	projectID int64
	kind      turnKind
}

var _ engine.Runner = (*Runner)(nil)

// New returns a runner that runs sandboxes on rt and agents through d, and
// gets a GitHub App's tokens from f.
func New(rt sandbox.Runtime, d driver.Driver, f *forge.Client) *Runner {
	return &Runner{rt: rt, driver: d, forge: f, open: openSandbox, runs: map[int64]*run{}, turns: map[turnKey]chan struct{}{}}
}

// sandboxer is the part of *sandbox.Sandbox the runner uses.
type sandboxer interface {
	EnsureVolumes(ctx context.Context) error
	Clone(ctx context.Context, r sidecar.CloneRequest) (string, error)
	Push(ctx context.Context, r sidecar.PushRequest) (branch, rejected string, err error)
	Importer(ctx context.Context) (container, error)
	RemoveImporter(ctx context.Context) error
	Up(ctx context.Context, phase sandbox.Phase, p sidecar.ProxyRequest, lines func(sidecar.Line)) (container, error)
	Restart(ctx context.Context) (container, error)
	Down(ctx context.Context) error
	Destroy(ctx context.Context, keepHome bool) error
}

// container is what the runner does in a running phase container.
type container interface {
	proc.Target
	Run(ctx context.Context, cmd proc.Command, timeout time.Duration) (sandbox.Result, error)
	CopyIn(ctx context.Context, files map[string][]byte) error
	CopyOut(ctx context.Context, p string, w io.Writer) error
}

// realSandbox returns Up's, Restart's and Importer's container as a container.
type realSandbox struct{ *sandbox.Sandbox }

func (s realSandbox) Importer(ctx context.Context) (container, error) {
	c, err := s.Sandbox.Importer(ctx)
	if err != nil {
		return nil, err
	}

	return c, nil
}

func (s realSandbox) Up(ctx context.Context, phase sandbox.Phase, p sidecar.ProxyRequest, lines func(sidecar.Line)) (container, error) {
	c, err := s.Sandbox.Up(ctx, phase, p, lines)
	if err != nil {
		return nil, err
	}

	return c, nil
}

func (s realSandbox) Restart(ctx context.Context) (container, error) {
	c, err := s.Sandbox.Restart(ctx)
	if err != nil {
		return nil, err
	}

	return c, nil
}

func openSandbox(o sandbox.Options) (sandboxer, error) {
	s, err := sandbox.Open(o)
	if err != nil {
		return nil, err
	}

	return realSandbox{s}, nil
}

// run is what the runner keeps of one run between calls. A sandbox is not
// safe for concurrent use; the engine makes one call on a run at a time.
type run struct {
	r          *Runner // for the import's turn on the project's mirror
	sb         sandboxer
	agent      container     // the agent's container while a session phase is up
	phase      sandbox.Phase // that session phase: the agent phase or the read-only one
	kept       bool          // the action in flight found that phase's container up and kept it
	ecosystems []string
	req        sidecar.ProxyRequest // the session phases' proxy request, kept while their sidecar lives
	standIn    string               // req's stand-in token in key mode

	mu       sync.Mutex // guards the fields below, which the sidecar's reader sets
	lost     bool       // the sidecar of the phase that is up has ended
	refusing bool       // that sidecar's proxy sent its budget line, so it refuses every model call
	fault    error      // the sidecar's error during the action in flight
	cancel   context.CancelCauseFunc
	watch    func(sidecar.Line) // takes the other lines while a session runs
	denied   []string           // hosts the proxy refused while no session watched, in order, once each
}

// lines takes the sidecar's lines. An error line means the phase lost its
// egress: it ends the action in flight, and that action's call fails with it.
// Other lines go to the running session's watch; a budget line also marks the
// sidecar as refusing. It never calls the sandbox, which would stall the
// sidecar's reader.
func (st *run) lines(l sidecar.Line) {
	st.mu.Lock()
	defer st.mu.Unlock()

	if l.Type != "error" {
		if l.Type == "budget" {
			st.refusing = true
		}

		switch {
		case st.watch != nil:
			st.watch(l)
		case l.Type == "denied" && l.Host != "" && !slices.Contains(st.denied, l.Host):
			st.denied = append(st.denied, l.Host)
		}

		return
	}

	st.lost = true

	if st.cancel != nil && st.fault == nil {
		st.fault = errors.New(l.Error)
		st.cancel(st.fault)
	}
}

// begin starts an action on the run, whose ctx ends at a sidecar error; end
// returns that error, if one came.
func (st *run) begin(ctx context.Context) (context.Context, func() error) {
	ctx, cancel := context.WithCancelCause(ctx)

	st.mu.Lock()
	st.fault, st.cancel = nil, cancel
	st.mu.Unlock()

	return ctx, func() error {
		st.mu.Lock()
		defer st.mu.Unlock()

		fault := st.fault
		st.fault, st.cancel = nil, nil

		cancel(nil)

		return fault
	}
}

// resetDenied starts a new collection of denied hosts for the step or verify
// that follows.
func (st *run) resetDenied() {
	st.mu.Lock()
	defer st.mu.Unlock()

	st.denied = nil
}

// withDenied ends a failed step's output with a line naming the hosts the
// proxy refused since resetDenied, so a person sees why the tool failed.
func (st *run) withDenied(out string) string {
	st.mu.Lock()
	defer st.mu.Unlock()

	if len(st.denied) == 0 {
		return out
	}

	if out != "" && !strings.HasSuffix(out, "\n") {
		out += "\n"
	}

	return out + "The egress proxy refused these hosts: " + strings.Join(st.denied, ", ") + "\n"
}

// watching hands the sidecar's lines other than errors to watch until
// watching(nil); a line that arrives after that is dropped.
func (st *run) watching(watch func(sidecar.Line)) {
	st.mu.Lock()
	defer st.mu.Unlock()

	st.watch = watch
}

// act brings the session phase up and runs do as the run's action in flight,
// with the run's mask, which also holds its stand-in token: a sidecar error
// during it fails the call, with what do returned. The error is masked.
func act[T any](ctx context.Context, r *Runner, c engine.RunConfig, phase sandbox.Phase,
	do func(context.Context, *run, func(string) string) (T, error),
) (T, error) {
	var zero T

	err := r.ensureUp(ctx, c, phase)

	// Read after ensureUp, which makes key mode's stand-in: no vault holds it.
	st := r.state(c.RunID)

	var standIn string
	if st != nil {
		standIn = st.standIn
	}

	mask := masker(c, standIn)
	if err != nil {
		return zero, maskErr(err, mask)
	}

	ctx, end := st.begin(ctx)

	res, err := do(ctx, st, mask)
	if fault := end(); fault != nil {
		return res, maskErr(fault, mask)
	}

	return res, maskErr(err, mask)
}

// up brings phase up with p. A session phase counts as up once the caller
// sets agent.
func (st *run) up(ctx context.Context, phase sandbox.Phase, p sidecar.ProxyRequest) (container, error) {
	st.agent = nil

	// Cleared first: a sidecar that ends or refuses once Up has returned sets them again.
	st.mu.Lock()
	st.lost, st.refusing = false, false
	st.mu.Unlock()

	return st.sb.Up(ctx, phase, p, st.lines)
}

func (st *run) down(ctx context.Context) error {
	st.agent = nil

	return st.sb.Down(ctx)
}

// serves reports whether a session phase is up with a sidecar that counts
// against budget and has not refused.
func (st *run) serves(budget *proxy.Budget) bool {
	st.mu.Lock()
	defer st.mu.Unlock()

	return st.agent != nil && !st.lost && !st.refusing && reflect.DeepEqual(st.req.Budget, budget)
}

func (r *Runner) state(runID int64) *run {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.runs[runID]
}

// takeTurn waits for projectID's turn of kind and returns the function that
// gives it back, or ctx's error when the action ends first.
func (r *Runner) takeTurn(ctx context.Context, projectID int64, kind turnKind) (func(), error) {
	key := turnKey{projectID, kind}

	r.mu.Lock()

	turn, ok := r.turns[key]
	if !ok {
		turn = make(chan struct{}, 1)
		r.turns[key] = turn
	}

	r.mu.Unlock()

	select {
	case turn <- struct{}{}:
		return func() { <-turn }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (r *Runner) setState(runID int64, st *run) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if st == nil {
		delete(r.runs, runID)

		return
	}

	r.runs[runID] = st
}

// sandbox opens the run's sandbox.
func (r *Runner) sandbox(ref engine.RunRef) (sandboxer, error) {
	return r.open(sandbox.Options{
		Runtime: r.rt, Image: ref.Image, InstanceID: ref.InstanceID,
		RunID: ref.RunID, TaskID: ref.TaskID, ProjectID: ref.ProjectID, Labels: r.labels,
	})
}

func refOf(c engine.RunConfig) engine.RunRef {
	return engine.RunRef{InstanceID: c.InstanceID, Image: c.Image, TaskID: c.TaskID, RunID: c.RunID, ProjectID: c.ProjectID}
}

// ensureUp brings phase up, the agent phase or the read-only one, unless it
// is up already. After a restart of this process it opens the run's sandbox
// and takes the ecosystems from c, which carries what the prepare detected:
// once an agent has run, the workspace, its .git included, is the agent's and
// is never read for them.
func (r *Runner) ensureUp(ctx context.Context, c engine.RunConfig, phase sandbox.Phase) error {
	st := r.state(c.RunID)
	if st == nil {
		sb, err := r.sandbox(refOf(c))
		if err != nil {
			return err
		}

		if err := sb.EnsureVolumes(ctx); err != nil {
			return err
		}

		st = &run{r: r, sb: sb, ecosystems: c.Ecosystems}
		r.setState(c.RunID, st)
	}

	live := st.serves(proxyBudget(c))
	// The phase's container stays when it was up, also when only its sidecar
	// starts anew, and so does whatever ran in it.
	st.kept = st.agent != nil && st.phase == phase

	if live && st.phase == phase {
		return nil
	}

	// The sandbox keeps the sidecar for the same request only, so a switch
	// between the session phases keeps the one the live sidecar serves, and
	// its count; a new request's fresh stand-in starts a fresh count.
	if !live {
		st.req, st.standIn = agentProxy(c, st.ecosystems, r.driver)
	}

	a, err := st.up(ctx, phase, st.req)
	if err != nil {
		return err
	}

	if err := homeStep(ctx, a, linkScript, env.AgentLinks(st.ecosystems, home)); err != nil {
		return err
	}

	if err := homeStep(ctx, a, seedScript, env.AgentSeeds(st.ecosystems, home)); err != nil {
		return err
	}

	st.agent, st.phase = a, phase

	return nil
}

// scriptArgv runs script with args as its positional arguments.
func scriptArgv(script string, args ...string) []string {
	return append([]string{"sh", "-c", script, "sh"}, args...)
}

// homeStep runs script in a session phase, whose home is writable, with each
// pair, a path in the home and the cache path it comes from, as positional arguments.
func homeStep(ctx context.Context, a container, script string, pairs map[string]string) error {
	if len(pairs) == 0 {
		return nil
	}

	var args []string
	for _, p := range slices.Sorted(maps.Keys(pairs)) {
		args = append(args, p, pairs[p])
	}

	res, err := a.Run(ctx, proc.Command{Argv: scriptArgv(script, args...)}, homeTimeout)
	if err != nil {
		return err
	}

	if res.ExitCode != 0 || res.TimedOut {
		return fmt.Errorf("runner: prepare the agent's home: %s", strings.TrimSpace(output(res, homeTimeout)))
	}

	return nil
}

// Release stops the run's containers and removes what remove asks for: the
// run's volumes, or those and the task's home. It succeeds when nothing
// exists. Without the run's state it releases through a sandbox it does not
// keep, so a later start still makes the run's volumes ready.
func (r *Runner) Release(ctx context.Context, ref engine.RunRef, remove engine.Remove) error {
	st := r.state(ref.RunID)
	if st == nil {
		sb, err := r.sandbox(ref)
		if err != nil {
			return err
		}

		st = &run{sb: sb}
	}

	if remove == engine.KeepVolumes {
		return st.down(ctx)
	}

	r.setState(ref.RunID, nil)

	return st.sb.Destroy(ctx, remove == engine.RemoveRun)
}

// agentEnv is the environment of the commands the runner runs in the agent
// phase.
func agentEnv(ecosystems []string, base string) []string {
	return slices.Concat(env.AgentEnv(ecosystems, home), bridgeEnv, noProxyEnv, baseEnv(base))
}

// sessionEnv is agentEnv without noProxyEnv, which the driver adds itself.
func sessionEnv(ecosystems []string, base string) []string {
	return slices.Concat(env.AgentEnv(ecosystems, home), bridgeEnv, baseEnv(base))
}

// withProjectVars puts the project's plain variables, taken as written, over
// a phase's environment. Secrets stay with the setup command.
func withProjectVars(base []string, c engine.RunConfig) []string {
	return proc.BuildEnv(base, c.Setup.Env)
}

func setupEnv(ecosystems []string, base string) []string {
	return slices.Concat(env.SetupEnv(ecosystems), bridgeEnv, noProxyEnv, baseEnv(base))
}

// baseEnv names the run's base commit, which nothing in the workspace names:
// it keeps no remote and no branch but the task's.
func baseEnv(base string) []string {
	return []string{"CAVU_BASE_COMMIT=" + base}
}

func allowlist(lists ...[]string) []string {
	var out []string
	for _, l := range lists {
		out = append(out, l...)
	}

	slices.Sort(out)

	return slices.Compact(out)
}

// masker masks the secrets of a run's configuration and the extra ones.
func masker(c engine.RunConfig, extra ...string) func(string) string {
	secrets := append([]string{c.Endpoint.Credential, c.Repo.Access.Token, c.Repo.Access.AppKey}, extra...)
	for _, v := range c.Setup.Secrets {
		secrets = append(secrets, v)
	}

	return redact.New(secrets...)
}

// maskedError masks the text of an error and keeps its chain.
type maskedError struct {
	err  error
	text string
}

func (e *maskedError) Error() string { return e.text }
func (e *maskedError) Unwrap() error { return e.err }

func maskErr(err error, mask func(string) string) error {
	if err == nil {
		return nil
	}

	return &maskedError{err: err, text: mask(err.Error())}
}
