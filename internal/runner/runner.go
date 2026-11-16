// Package runner carries out a run's actions in the run's sandbox, over a
// container runtime and an agent driver, for the engine.
package runner

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mhersson/cavu/internal/driver"
	"github.com/mhersson/cavu/internal/engine"
	"github.com/mhersson/cavu/internal/env"
	"github.com/mhersson/cavu/internal/proc"
	"github.com/mhersson/cavu/internal/proxy"
	"github.com/mhersson/cavu/internal/redact"
	"github.com/mhersson/cavu/internal/sandbox"
	"github.com/mhersson/cavu/internal/sidecar"
)

const (
	// bridgeURL is the sandbox's bridge to the proxy inside a phase container.
	bridgeURL = "http://127.0.0.1:3128"
	loopback  = "localhost,127.0.0.1,::1"
	home      = "/home/agent"
	cache     = "/cache"
	workspace = "/workspace"
	modeKey   = "key"

	linkTimeout = time.Minute
	// seedTimeout leaves room for a large project's crates on a slow volume.
	seedTimeout = 10 * time.Minute

	// elsewhere is a guarded script's exit status when a directory it would
	// change resolves elsewhere; its other failures exit 2.
	elsewhere = 1
)

// linkScript makes each link given as a path and its target, replacing what
// is at the path: the home is the agent's to change.
var linkScript = proc.SandboxOnly(`while [ "$#" -ge 2 ]; do
	mkdir -p -- "$(dirname -- "$1")" && rm -rf -- "$1" && ln -s -- "$2" "$1" || exit 1
	shift 2
done`)

// seedScript copies into each directory, given with its source, what the
// source holds that is missing or older there: a later setup-phase fetch then
// replaces a stale index entry in a kept home, and the agent's newer files
// stay. It writes only by relative name in a directory whose real path is the
// one seedCommand validated; -f replaces the read-only files git writes.
var seedScript = proc.SandboxOnly(`while [ "$#" -ge 2 ]; do
	if [ -d "$2" ]; then
		mkdir -p -- "$1" && cd -P -- "$1" || exit 2
		if [ "$(pwd -P)" != "$1" ]; then
			printf 'cavu: %s resolves to %s; nothing copied into it\n' "$1" "$(pwd -P)" >&2
			exit ` + strconv.Itoa(elsewhere) + `
		fi
		cp -R -u -f -- "$2/." . || exit 2
	fi
	shift 2
done
[ "$#" -eq 0 ] || exit 2`)

var (
	errOutOfBounds  = errors.New("runner: a guarded step's path is not a clean absolute path strictly under its root")
	errNotInSandbox = errors.New("the image lacks the sandbox marker " + proc.SandboxMarker +
		", so nothing ran; `cavu image build` builds an image with the marker")
	errElsewhere = errors.New("a directory resolves elsewhere, and nothing in it was changed")
)

// proxyEnv sends a command's egress through the bridge, the one way out of
// either phase, and keeps loopback traffic off it.
var proxyEnv = []string{
	"HTTP_PROXY=" + bridgeURL, "HTTPS_PROXY=" + bridgeURL, "http_proxy=" + bridgeURL, "https_proxy=" + bridgeURL,
	"NO_PROXY=" + loopback, "no_proxy=" + loopback,
}

// Runner carries out the engine's actions over a container runtime.
type Runner struct {
	rt     sandbox.Runtime
	driver driver.Driver
	labels map[string]string // added to every container and volume; tests set it
	open   func(sandbox.Options) (sandboxer, error)

	mu sync.Mutex
	// runs holds only runs whose volumes are in place and whose ecosystems
	// are known; only Prepare and ensureUp add one.
	runs map[int64]*run
}

// New returns a runner that runs sandboxes on rt and agents through d.
func New(rt sandbox.Runtime, d driver.Driver) *Runner {
	return &Runner{rt: rt, driver: d, open: openSandbox, runs: map[int64]*run{}}
}

// sandboxer is the part of *sandbox.Sandbox the runner uses.
type sandboxer interface {
	EnsureVolumes(ctx context.Context) error
	Clone(ctx context.Context, r sidecar.CloneRequest) (string, error)
	Up(ctx context.Context, phase sandbox.Phase, p sidecar.ProxyRequest, lines func(sidecar.Line)) (container, error)
	Down(ctx context.Context) error
	Destroy(ctx context.Context, keepHome bool) error
}

// container is what the runner does in a running phase container.
type container interface {
	proc.Target
	Run(ctx context.Context, cmd proc.Command, timeout time.Duration) (sandbox.Result, error)
	CopyIn(ctx context.Context, files map[string][]byte) error
}

// realSandbox returns Up's container as a container.
type realSandbox struct{ *sandbox.Sandbox }

func (s realSandbox) Up(ctx context.Context, phase sandbox.Phase, p sidecar.ProxyRequest, lines func(sidecar.Line)) (container, error) {
	c, err := s.Sandbox.Up(ctx, phase, p, lines)
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
	sb         sandboxer
	agent      container // the agent phase while it is up
	ecosystems []string
	standIn    string // the agent phase's stand-in token in key mode

	mu     sync.Mutex // guards the fields below, which the sidecar's reader sets
	lost   bool       // the sidecar of the phase that is up has ended
	fault  error      // the sidecar's error during the action in flight
	cancel context.CancelCauseFunc
}

// lines takes the sidecar's lines. An error line means the phase lost its
// egress: it ends the action in flight, and that action's call fails with it.
// It never calls the sandbox, which would stall the sidecar's reader.
func (st *run) lines(l sidecar.Line) {
	if l.Type != "error" {
		return
	}

	st.mu.Lock()
	defer st.mu.Unlock()

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

// up brings phase up with p. The agent phase counts as up once the caller
// sets agent.
func (st *run) up(ctx context.Context, phase sandbox.Phase, p sidecar.ProxyRequest) (container, error) {
	st.agent = nil

	// Cleared first: a sidecar that ends once Up has returned sets it again.
	st.mu.Lock()
	st.lost = false
	st.mu.Unlock()

	return st.sb.Up(ctx, phase, p, st.lines)
}

func (st *run) down(ctx context.Context) error {
	st.agent = nil

	return st.sb.Down(ctx)
}

func (st *run) agentUp() bool {
	st.mu.Lock()
	defer st.mu.Unlock()

	return st.agent != nil && !st.lost
}

func (r *Runner) state(runID int64) *run {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.runs[runID]
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

// ensureUp brings the run's agent phase up unless it is up already. After a
// restart of this process it opens the run's sandbox and takes the ecosystems
// from c, which carries what the prepare detected: once an agent has run, the
// workspace, its .git included, is the agent's and is never read for them.
func (r *Runner) ensureUp(ctx context.Context, c engine.RunConfig) error {
	st := r.state(c.RunID)
	if st == nil {
		sb, err := r.sandbox(refOf(c))
		if err != nil {
			return err
		}

		if err := sb.EnsureVolumes(ctx); err != nil {
			return err
		}

		st = &run{sb: sb, ecosystems: c.Ecosystems}
		r.setState(c.RunID, st)
	}

	if st.agentUp() {
		return nil
	}

	req := sidecar.ProxyRequest{
		AllowHosts: allowlist(env.AgentHosts(st.ecosystems), c.Setup.EgressHosts, r.driver.Hosts(c.Endpoint.Mode)),
		CABundle:   string(c.CABundle),
	}

	st.standIn = ""

	if c.Endpoint.Mode == modeKey {
		standIn, err := proxy.NewStandIn()
		if err != nil {
			return err
		}

		st.standIn = standIn
		req.Model = &proxy.ModelRoute{
			BaseURL: c.Endpoint.URL, Credential: c.Endpoint.Credential, AuthHeader: c.Endpoint.AuthHeader, StandIn: standIn,
		}
	}

	a, err := st.up(ctx, sandbox.PhaseAgent, req)
	if err != nil {
		return err
	}

	if err := linkHome(ctx, a, env.AgentLinks(st.ecosystems, home)); err != nil {
		return err
	}

	if err := seedHome(ctx, a, env.AgentSeeds(st.ecosystems, home)); err != nil {
		return err
	}

	st.agent = a

	return nil
}

// linkHome makes links, path to target, in the agent phase's home.
func linkHome(ctx context.Context, a container, links map[string]string) error {
	if len(links) == 0 {
		return nil
	}

	argv := []string{"sh", "-c", linkScript, "sh"}
	for _, link := range slices.Sorted(maps.Keys(links)) {
		argv = append(argv, link, links[link])
	}

	res, err := a.Run(ctx, proc.Command{Argv: argv}, linkTimeout)
	if err != nil {
		return err
	}

	if res.ExitCode != 0 || res.TimedOut {
		return fmt.Errorf("runner: link the agent's home: %s", strings.TrimSpace(output(res, linkTimeout)))
	}

	return nil
}

// seedHome fills directories in the agent phase's home from their sources in
// the cache.
func seedHome(ctx context.Context, a container, seeds map[string]string) error {
	if len(seeds) == 0 {
		return nil
	}

	cmd, err := seedCommand(seeds)
	if err != nil {
		return err
	}

	return homeStep(ctx, a, "seed the agent's home", cmd, seedTimeout)
}

// seedCommand refuses every directory unless each is strictly under the home
// and each source under the cache, so the script copies only into the home.
func seedCommand(seeds map[string]string) (proc.Command, error) {
	argv := []string{"sh", "-c", seedScript, "sh"}

	for _, dir := range slices.Sorted(maps.Keys(seeds)) {
		src := seeds[dir]
		if err := errors.Join(within(dir, home), within(src, cache)); err != nil {
			return proc.Command{}, err
		}

		argv = append(argv, dir, src)
	}

	return proc.Command{Argv: argv}, nil
}

// homeStep runs a guarded step on the agent phase's home, where every failure
// is a fault: the home is the task's, and only removing it mends it.
func homeStep(ctx context.Context, a container, what string, cmd proc.Command, timeout time.Duration) error {
	res, err := runGuarded(ctx, a, cmd, timeout)
	if err == nil && (res.ExitCode != 0 || res.TimedOut) {
		err = errors.New(strings.TrimSpace(output(res, timeout)))
	}

	if err != nil {
		return fmt.Errorf("runner: %s: %w", what, err)
	}

	return nil
}

// runGuarded runs a script proc.SandboxOnly wraps, which exits elsewhere when
// a directory it would change resolves elsewhere. Those two refusals are
// errors; any other failure comes back in the result.
func runGuarded(ctx context.Context, c container, cmd proc.Command, timeout time.Duration) (sandbox.Result, error) {
	res, err := c.Run(ctx, cmd, timeout)
	if err != nil || res.TimedOut {
		return res, err
	}

	switch res.ExitCode {
	case proc.NotInSandbox:
		return res, errNotInSandbox
	case elsewhere:
		return res, fmt.Errorf("%w: %s", errElsewhere, strings.TrimSpace(res.Output))
	}

	return res, nil
}

// within is nil when p is a clean absolute path strictly under root.
func within(p, root string) error {
	if path.IsAbs(p) && path.Clean(p) == p && strings.HasPrefix(p, root+"/") {
		return nil
	}

	return fmt.Errorf("%w: %q is not strictly under %s", errOutOfBounds, p, root)
}

// Release stops the run's containers and, when removeVolumes is set, removes
// its run volumes and the task's home. It succeeds when nothing exists.
// Without the run's state it releases through a sandbox it does not keep,
// so a later start still makes the run's volumes ready.
func (r *Runner) Release(ctx context.Context, ref engine.RunRef, removeVolumes bool) error {
	st := r.state(ref.RunID)
	if st == nil {
		sb, err := r.sandbox(ref)
		if err != nil {
			return err
		}

		st = &run{sb: sb}
	}

	if !removeVolumes {
		return st.down(ctx)
	}

	r.setState(ref.RunID, nil)

	return st.sb.Destroy(ctx, false)
}

// agentEnv is the one environment of the agent phase: the baseline verify,
// the verify gate and the session run with it.
func agentEnv(ecosystems []string) []string {
	return append(env.AgentEnv(ecosystems, home), proxyEnv...)
}

func setupEnv(ecosystems []string) []string {
	return append(env.SetupEnv(ecosystems), proxyEnv...)
}

func allowlist(lists ...[]string) []string {
	var out []string
	for _, l := range lists {
		out = append(out, l...)
	}

	slices.Sort(out)

	return slices.Compact(out)
}

// masker masks the secrets of a run's configuration.
func masker(c engine.RunConfig) func(string) string {
	secrets := []string{c.Endpoint.Credential, c.Repo.Token}
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
