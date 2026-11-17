package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mhersson/cavu/internal/proc"
	"github.com/mhersson/cavu/internal/sidecar"
)

const (
	instanceLabel = "cavu.instance"
	runLabel      = "cavu.run"
	taskLabel     = "cavu.task"
	projectLabel  = "cavu.project"

	bridgeAddr  = "127.0.0.1:3128"
	proxySocket = "/handoff/proxy.sock"
	// probeHost is a reserved name no allowlist holds, so the refusal it
	// yields is Up's own and its denied line can be told apart.
	probeHost = "cavu-probe.invalid:80"
	probeURL  = "http://cavu-probe.invalid/"

	// Memory limits, each capped at what the runtime reports in total.
	phaseMemoryMB   = 8192
	sidecarMemoryMB = 1024
	cloneMemoryMB   = 2048

	readyTimeout = 20 * time.Second
	// probeTimeout covers a bridge that does not listen yet.
	probeTimeout     = 10 * time.Second
	probeInterval    = 200 * time.Millisecond
	probeCallTimeout = 15 * time.Second
	inspectTimeout   = 15 * time.Second
	// cloneTimeout keeps a hung fetch from hanging the run.
	cloneTimeout = 10 * time.Minute
	// sidecarStopGrace outlasts the sidecar's own drain of in-flight responses.
	sidecarStopGrace = 10 * time.Second
	cleanupTimeout   = time.Minute
)

// BridgeURL is the proxy as a phase container reaches it, through its bridge.
const BridgeURL = "http://" + bridgeAddr

var (
	envArgv   = []string{"env", "-0"}
	probeArgv = []string{"curl", "--silent", "--output", "/dev/null", "--max-time", "5", "--proxy", BridgeURL, probeURL, "--write-out", "%{http_code}"}

	instanceIDText = regexp.MustCompile(`^[0-9a-f]{16}$`)
	commitID       = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)

	// runtimeEnv are the names a container holds without CAVU passing them:
	// what the runtimes set themselves, beside the proxy names Docker's run
	// blanks.
	runtimeEnv = []string{"PATH", "HOME", "HOSTNAME", "TERM", "container"}

	volumeKinds = []string{"workspace", "handoff", "home", "cache", "mirror"}
)

var (
	// ErrNotIsolated is a phase container that is not running, or whose
	// network, mounts or environment are not what the sandbox started it with.
	ErrNotIsolated = errors.New("sandbox: the container is not isolated")
	// ErrSidecarDown is a sidecar that did not get ready, or a proxy the phase
	// container cannot reach through the bridge.
	ErrSidecarDown = errors.New("sandbox: the sidecar does not answer")
	// ErrOutOfMemory is a container the kernel killed at its memory limit.
	ErrOutOfMemory = errors.New("sandbox: killed for memory")

	errNoVolumes = errors.New("sandbox: the volumes are not in place: run EnsureVolumes first")
)

// Options name one run's sandbox. The ids name and label its containers and
// volumes; Labels are added to every one of them.
type Options struct {
	Runtime                  Runtime
	Image                    string
	InstanceID               string // 16 lower-case hex characters
	RunID, TaskID, ProjectID int64
	Labels                   map[string]string
}

// Phase is what the phase container is for. Every phase is hardened alike,
// because the setup command runs with the project's secrets.
type Phase string

const (
	// PhaseSetup prepares the project: the cache is writable and there is no
	// agent home.
	PhaseSetup Phase = "setup"
	// PhaseAgent runs the agent: the cache read-only, the home mounted.
	PhaseAgent Phase = "agent"
	// PhaseReadOnly runs a session that may change nothing: the agent phase with
	// the workspace mounted read-only.
	PhaseReadOnly Phase = "readonly"
)

// phases are every Phase; one phase container runs at a time.
var phases = []Phase{PhaseSetup, PhaseAgent, PhaseReadOnly}

// Sandbox is the two containers one run lives in: a phase container with no
// network, whose one way out is its bridge to the proxy socket, and the
// sidecar on the bridge network that serves the proxy. It is not safe for
// concurrent use.
type Sandbox struct {
	o        Options
	timeouts struct{ ready, probe, clone time.Duration }
	volumes  bool // EnsureVolumes succeeded
	totalMB  int  // the runtime's memory, read once
	side     *sidecarProc
	phase    Phase // of the phase container this Sandbox started, "" for none
}

// Open checks o and returns its sandbox. It has no side effects.
func Open(o Options) (*Sandbox, error) {
	if !instanceIDText.MatchString(o.InstanceID) {
		return nil, errors.New("sandbox: instance id: want 16 lower-case hex characters")
	}

	// Run id 0 with task id 0 is reserved for a project's environment check,
	// and with project id 0 as well for the doctor.
	check := o.RunID == 0 && o.TaskID == 0
	if check && o.ProjectID < 0 || !check && (o.RunID < 1 || o.TaskID < 1 || o.ProjectID < 1) {
		return nil, errors.New("sandbox: run, task and project ids must be positive, or run and task both 0 with a project id of 0 or more")
	}

	if err := checkArg("image", o.Image); err != nil {
		return nil, fmt.Errorf("sandbox: %w", err)
	}

	for _, k := range []string{instanceLabel, runLabel, taskLabel, projectLabel} {
		if _, ok := o.Labels[k]; ok {
			return nil, fmt.Errorf("sandbox: label %s is the sandbox's own", k)
		}
	}

	// A copy, so a later change to the caller's map cannot add the sandbox's
	// own keys.
	o.Labels = maps.Clone(o.Labels)

	s := &Sandbox{o: o}
	s.timeouts.ready, s.timeouts.probe, s.timeouts.clone = readyTimeout, probeTimeout, cloneTimeout

	return s, nil
}

// owner is the label key and id of what a volume or container of kind
// belongs to: the task for the home, the project for the cache and the
// mirror, the run for the rest.
func (s *Sandbox) owner(kind string) (string, int64) {
	switch kind {
	case "home":
		return taskLabel, s.o.TaskID
	case "cache", "mirror":
		return projectLabel, s.o.ProjectID
	}

	return runLabel, s.o.RunID
}

// name is cavu-<the instance id's first 8 characters>-<kind>-<owner's id>.
func (s *Sandbox) name(kind string) string {
	_, id := s.owner(kind)

	return namePrefix(s.o.InstanceID) + kind + "-" + strconv.FormatInt(id, 10)
}

func namePrefix(instanceID string) string { return "cavu-" + instanceID[:8] + "-" }

func (s *Sandbox) labels(kind string) map[string]string {
	key, id := s.owner(kind)

	l := maps.Clone(s.o.Labels)
	if l == nil {
		l = map[string]string{}
	}

	l[instanceLabel] = s.o.InstanceID
	l[key] = strconv.FormatInt(id, 10)

	return l
}

func (s *Sandbox) mount(kind, path string, readOnly bool) Mount {
	return Mount{Volume: s.name(kind), Path: path, ReadOnly: readOnly}
}

// EnsureVolumes creates the run's volumes, and the task's and the project's
// where missing. A volume of one of these names that another instance owns,
// or none, is refused.
func (s *Sandbox) EnsureVolumes(ctx context.Context) error {
	s.volumes = false

	for _, kind := range volumeKinds {
		name := s.name(kind)

		if err := s.o.Runtime.VolumeCreate(ctx, name, s.labels(kind)); err != nil {
			return fmt.Errorf("sandbox: %w", err)
		}

		labels, err := s.o.Runtime.VolumeLabels(ctx, name)
		if err != nil {
			return fmt.Errorf("sandbox: %w", err)
		}

		if labels[instanceLabel] != s.o.InstanceID {
			return fmt.Errorf("sandbox: volume %s belongs to another instance or to none", name)
		}
	}

	s.volumes = true

	return nil
}

// readMemory reads the runtime's total memory, once, for the limits.
func (s *Sandbox) readMemory(ctx context.Context) error {
	if s.totalMB > 0 {
		return nil
	}

	total, err := s.o.Runtime.MemoryMB(ctx)
	if err != nil {
		return fmt.Errorf("sandbox: %w", err)
	}

	s.totalMB = total

	return nil
}

// The sidecar and the clone container are hardened too: one holds the real
// credential and parses the agent's HTTP, the other runs git on forge content.
func (s *Sandbox) sidecarSpec(kind string, memoryMB int, mounts ...Mount) ContainerSpec {
	return ContainerSpec{
		Name:        s.name(kind),
		Image:       s.o.Image,
		Argv:        []string{"cavu", "sidecar"},
		Labels:      s.labels(kind),
		Mounts:      mounts,
		Network:     "bridge",
		HostGateway: true,
		Hardened:    true,
		MemoryMB:    min(memoryMB, s.totalMB),
		Stdin:       true,
	}
}

func (s *Sandbox) phaseSpec(p Phase) ContainerSpec {
	setup := p == PhaseSetup
	spec := ContainerSpec{
		Name:   s.name(string(p)),
		Image:  s.o.Image,
		Argv:   []string{"cavu", "bridge", "--listen", bridgeAddr, "--socket", proxySocket},
		Labels: s.labels(string(p)),
		Mounts: []Mount{
			s.mount("workspace", "/workspace", p == PhaseReadOnly),
			s.mount("handoff", "/handoff", false),
			s.mount("cache", "/cache", !setup),
		},
		// The image's /tmp stays: a tmpfs there would be noexec on Docker, and
		// go test runs the binaries it builds from TMPDIR.
		Tmpfs:    []string{pidDir},
		Network:  "none",
		Hardened: true,
		MemoryMB: min(phaseMemoryMB, s.totalMB),
	}

	if !setup {
		spec.Mounts = append(spec.Mounts, s.mount("home", "/home/agent", false))
	}

	return spec
}

func (s *Sandbox) phaseNames() []string {
	names := make([]string, 0, len(phases))
	for _, p := range phases {
		names = append(names, s.name(string(p)))
	}

	return names
}

// Clone runs the sidecar's clone operation in a short-lived container that
// mounts the mirror and the workspace and nothing else, and returns the base
// commit. It sets r's directories to those mounts. The token travels only on
// the container's stdin, and the whole call is bounded. A container the
// kernel killed at its memory limit is ErrOutOfMemory.
func (s *Sandbox) Clone(ctx context.Context, r sidecar.CloneRequest) (baseCommit string, err error) {
	if !s.volumes {
		return "", errNoVolumes
	}

	r.MirrorDir, r.WorkspaceDir = "/mirror", "/workspace"

	ctx, cancel := context.WithTimeout(ctx, s.timeouts.clone)
	defer cancel()

	if err := s.readMemory(ctx); err != nil {
		return "", err
	}

	spec := s.sidecarSpec("clone", cloneMemoryMB, s.mount("mirror", "/mirror", false), s.mount("workspace", "/workspace", false))
	rt := s.o.Runtime

	if err := rt.Remove(ctx, spec.Name); err != nil {
		return "", fmt.Errorf("sandbox: clone: %w", err)
	}

	p, err := rt.StartAttached(ctx, spec)
	if err != nil {
		return "", fmt.Errorf("sandbox: clone: %w", err)
	}

	baseCommit, err = readClone(ctx, p, r)

	cleanup, done := cleanupContext(ctx)
	defer done()

	// git killed by the kernel reports only a signal, so the runtime is asked.
	if err != nil && ctx.Err() == nil {
		if info, ierr := rt.Inspect(cleanup, spec.Name); ierr == nil && info.OOMKilled {
			err = fmt.Errorf("%w: the clone container hit its %d MB limit", ErrOutOfMemory, spec.MemoryMB)
		}
	}

	if rerr := rt.Remove(cleanup, spec.Name); rerr != nil {
		err = errors.Join(err, fmt.Errorf("sandbox: clone: %w", rerr))
	}

	if err != nil {
		return "", err
	}

	return baseCommit, nil
}

func readClone(ctx context.Context, p proc.Process, r sidecar.CloneRequest) (string, error) {
	// A client that never reads its stdin must not block the call.
	go func() {
		_ = json.NewEncoder(p.Stdin()).Encode(sidecar.Request{Clone: &r})
		_ = p.Stdin().Close()
	}()

	var result sidecar.Line

	lines := p.Lines()

read:
	for {
		select {
		case raw, ok := <-lines:
			if !ok {
				break read
			}

			var l sidecar.Line
			if json.Unmarshal(raw, &l) == nil && (l.Type == "cloned" || l.Type == "error") {
				result = l

				break read
			}
		case <-ctx.Done():
			_ = p.Stop(killGrace)

			return "", fmt.Errorf("sandbox: clone: %w", ctx.Err())
		}
	}

	code, err := proc.Wait(ctx, p)
	if err != nil {
		return "", fmt.Errorf("sandbox: clone: %w", err)
	}

	switch {
	case result.Type == "error":
		return "", fmt.Errorf("sandbox: clone: %s", result.Error)
	case result.Type != "cloned" || code != 0:
		return "", fmt.Errorf("sandbox: clone: the container exited %d without a result: %s", code, lastLine(p.StderrTail()))
	case !commitID.MatchString(result.Commit):
		return "", errors.New("sandbox: clone: the result is not a full commit id")
	}

	return result.Commit, nil
}

// Scratch starts a throwaway container of the image beside the sandbox, for a
// command that needs the image and nothing else: network none, hardened, no
// volume, and the tmpfs a command runs with. Down removes it.
func (s *Sandbox) Scratch(ctx context.Context) (Container, error) {
	if err := s.readMemory(ctx); err != nil {
		return Container{}, err
	}

	spec := ContainerSpec{
		Name: s.name("scratch"), Image: s.o.Image, Argv: []string{"sleep", "infinity"}, Labels: s.labels("scratch"),
		Tmpfs: []string{pidDir}, Network: "none", Hardened: true, MemoryMB: min(sidecarMemoryMB, s.totalMB),
	}

	if err := s.o.Runtime.Remove(ctx, spec.Name); err != nil {
		return Container{}, fmt.Errorf("sandbox: %w", err)
	}

	if err := s.o.Runtime.Start(ctx, spec); err != nil {
		return Container{}, fmt.Errorf("sandbox: %w", err)
	}

	return s.o.Runtime.Container(spec.Name), nil
}

// Up starts the sidecar and the phase container where they are not running
// (a sidecar serving another request is replaced, and so is the phase
// container of another phase), then checks that the phase container has
// network none, no mount or variable beyond its spec, and reaches the proxy
// through its bridge. It fails closed: after any error neither container is
// left, and a failed check is ErrNotIsolated or ErrSidecarDown. lines receives
// the sidecar's denied, usage and budget lines on a goroutine of its own, so one that
// blocks holds back the proxy. If the sidecar ends after it got ready and the
// sandbox did not stop it, lines receives one error line with the reason: the
// phase container has lost its egress.
func (s *Sandbox) Up(ctx context.Context, phase Phase, p sidecar.ProxyRequest, lines func(sidecar.Line)) (Container, error) {
	if !slices.Contains(phases, phase) {
		return Container{}, fmt.Errorf("sandbox: phase %q: want one of %q", phase, phases)
	}

	if !s.volumes {
		return Container{}, errNoVolumes
	}

	p.Socket = proxySocket

	c, err := s.up(ctx, phase, p, lines)
	if err != nil {
		return Container{}, errors.Join(err, s.Down(ctx))
	}

	return c, nil
}

func (s *Sandbox) up(ctx context.Context, phase Phase, p sidecar.ProxyRequest, lines func(sidecar.Line)) (Container, error) {
	if err := s.readMemory(ctx); err != nil {
		return Container{}, err
	}

	if err := s.ensureSidecar(ctx, p, lines); err != nil {
		return Container{}, err
	}

	if s.phase != phase || !s.running(ctx, phase) {
		if err := s.startPhase(ctx, phase); err != nil {
			return Container{}, err
		}
	}

	if err := s.side.waitReady(ctx, s.timeouts.ready); err != nil {
		return Container{}, err
	}

	return s.check(ctx)
}

// Restart removes the phase container of the last Up and starts it again on
// the same volumes, so nothing an earlier command left running survives. The
// sidecar keeps running. The new container is checked, and the call fails
// closed, as in Up.
func (s *Sandbox) Restart(ctx context.Context) (Container, error) {
	if s.phase == "" || s.side == nil {
		return Container{}, errors.New("sandbox: nothing to restart: run Up first")
	}

	c, err := s.restart(ctx, s.phase)
	if err != nil {
		return Container{}, errors.Join(err, s.Down(ctx))
	}

	return c, nil
}

func (s *Sandbox) restart(ctx context.Context, phase Phase) (Container, error) {
	if err := s.startPhase(ctx, phase); err != nil {
		return Container{}, err
	}

	return s.check(ctx)
}

// Down removes the containers, the scratch one too, and keeps the volumes. It
// does so on a cancelled ctx too, so a cancelled run leaves no container
// running.
func (s *Sandbox) Down(ctx context.Context) error {
	s.stopSidecar()
	s.phase = ""

	cleanup, cancel := cleanupContext(ctx)
	defer cancel()

	if err := s.o.Runtime.Remove(cleanup, append(s.phaseNames(), s.name("sidecar"), s.name("clone"), s.name("scratch"))...); err != nil {
		return fmt.Errorf("sandbox: %w", err)
	}

	return nil
}

// Destroy runs Down, then removes the run's workspace and hand-off volumes
// and, unless keepHome is set, the task's home, on a cancelled ctx too. The
// project's cache and mirror stay.
func (s *Sandbox) Destroy(ctx context.Context, keepHome bool) error {
	if err := s.Down(ctx); err != nil {
		return err
	}

	s.volumes = false

	names := []string{s.name("workspace"), s.name("handoff")}
	if !keepHome {
		names = append(names, s.name("home"))
	}

	cleanup, cancel := cleanupContext(ctx)
	defer cancel()

	if err := s.o.Runtime.VolumeRemove(cleanup, names...); err != nil {
		return fmt.Errorf("sandbox: %w", err)
	}

	return nil
}

// cleanupContext outlives ctx's cancellation, bounded on its own.
func cleanupContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
}

// ensureSidecar keeps a live sidecar that serves p and starts one otherwise.
func (s *Sandbox) ensureSidecar(ctx context.Context, p sidecar.ProxyRequest, lines func(sidecar.Line)) error {
	if s.side != nil && s.side.alive() && reflect.DeepEqual(s.side.req, p) {
		s.side.lines.Store(&lines)

		return nil
	}

	s.stopSidecar()

	spec := s.sidecarSpec("sidecar", sidecarMemoryMB, s.mount("handoff", "/handoff", false))

	if err := s.o.Runtime.Remove(ctx, spec.Name); err != nil {
		return fmt.Errorf("sandbox: %w", err)
	}

	pr, err := s.o.Runtime.StartAttached(ctx, spec)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrSidecarDown, err)
	}

	s.side = readSidecar(s.o.Runtime, spec, pr, p, lines)

	// Stdin stays open, because its end ends the proxy. A request that never
	// arrives shows as a sidecar that does not get ready.
	go func() { _ = json.NewEncoder(pr.Stdin()).Encode(sidecar.Request{Proxy: &p}) }()

	return nil
}

func (s *Sandbox) stopSidecar() {
	if s.side != nil {
		s.side.stop()
		s.side = nil
	}
}

func (s *Sandbox) running(ctx context.Context, phase Phase) bool {
	info, err := s.o.Runtime.Inspect(ctx, s.name(string(phase)))

	return err == nil && info.Running
}

// startPhase replaces whichever phase container exists with a fresh one.
func (s *Sandbox) startPhase(ctx context.Context, phase Phase) error {
	s.phase = ""

	if err := s.o.Runtime.Remove(ctx, s.phaseNames()...); err != nil {
		return fmt.Errorf("sandbox: %w", err)
	}

	if err := s.o.Runtime.Start(ctx, s.phaseSpec(phase)); err != nil {
		return fmt.Errorf("sandbox: %w", err)
	}

	s.phase = phase

	return nil
}

// check proves the phase container's isolation from what the runtime
// reports and what an exec sees, then probes the proxy through the bridge.
func (s *Sandbox) check(ctx context.Context) (Container, error) {
	spec := s.phaseSpec(s.phase)

	info, err := s.o.Runtime.Inspect(ctx, spec.Name)
	if err != nil {
		return Container{}, fmt.Errorf("%w: %w", ErrNotIsolated, err)
	}

	if info.Network != "none" {
		return Container{}, fmt.Errorf("%w: network %q, want none", ErrNotIsolated, info.Network)
	}

	if !info.Running {
		return Container{}, fmt.Errorf("%w: container %s is not running (exit %d)", ErrNotIsolated, spec.Name, info.ExitCode)
	}

	if err := checkMounts(spec, info.Mounts); err != nil {
		return Container{}, err
	}

	out, err := s.o.Runtime.output(ctx, append([]string{"exec", spec.Name}, envArgv...)...)
	if err != nil {
		return Container{}, fmt.Errorf("%w: read the environment: %w", ErrNotIsolated, err)
	}

	if err := checkExecEnv(out); err != nil {
		return Container{}, err
	}

	if err := s.probe(ctx, spec.Name); err != nil {
		return Container{}, err
	}

	return s.o.Runtime.Container(spec.Name), nil
}

// checkMounts wants exactly the spec's volumes and tmpfs mounts. An error
// names the target, never the source, which for a bind is a host path.
func checkMounts(spec ContainerSpec, got []MountInfo) error {
	want := map[string]MountInfo{}

	for _, m := range spec.Mounts {
		want[m.Path] = MountInfo{Type: "volume", Source: m.Volume, Target: m.Path}
	}

	for _, t := range spec.Tmpfs {
		want[t] = MountInfo{Type: "tmpfs", Target: t}
	}

	for _, m := range got {
		if w, ok := want[m.Target]; !ok || w != m {
			return fmt.Errorf("%w: a %s mount at %s that the container was not started with", ErrNotIsolated, m.Type, m.Target)
		}

		delete(want, m.Target)
	}

	if len(want) > 0 {
		return fmt.Errorf("%w: no mount at %s", ErrNotIsolated, slices.Sorted(maps.Keys(want))[0])
	}

	return nil
}

// checkEnvName quotes a name it refuses only when the name is valid, since
// anything else may be a value.
func checkEnvName(name string) error {
	switch {
	case slices.Contains(runtimeEnv, name) || slices.Contains(proxyNames, name):
		return nil
	case !envName.MatchString(name):
		return fmt.Errorf("%w: a variable with an invalid name", ErrNotIsolated)
	}

	return fmt.Errorf("%w: variable %s", ErrNotIsolated, name)
}

// checkExecEnv reads `env -0`: every name from the allowlist, and every proxy
// name blank.
func checkExecEnv(out []byte) error {
	for kv := range strings.SplitSeq(strings.TrimSuffix(string(out), "\x00"), "\x00") {
		name, value, ok := strings.Cut(kv, "=")
		if !ok {
			return fmt.Errorf("%w: a variable with an invalid name", ErrNotIsolated)
		}

		if err := checkEnvName(name); err != nil {
			return err
		}

		if value != "" && slices.Contains(proxyNames, name) {
			return fmt.Errorf("%w: variable %s is set", ErrNotIsolated, name)
		}
	}

	return nil
}

// probe asks the proxy, from inside the container through the bridge, for a
// host it must refuse: a 403 proves the bridge, the socket and the proxy at
// once.
func (s *Sandbox) probe(ctx context.Context, name string) error {
	argv := append([]string{"exec", name}, probeArgv...)
	deadline := time.Now().Add(s.timeouts.probe)

	for {
		call, cancel := context.WithTimeout(ctx, probeCallTimeout)
		out, err := s.o.Runtime.output(call, argv...)

		cancel()

		if err == nil && strings.TrimSpace(string(out)) == "403" {
			return nil
		}

		switch {
		case ctx.Err() != nil:
			return fmt.Errorf("sandbox: %w", ctx.Err())
		case !s.side.alive():
			return fmt.Errorf("%w: %w", ErrSidecarDown, s.side.cause)
		case time.Now().After(deadline):
			return fmt.Errorf("%w: the proxy did not refuse a probe through the bridge within %s", ErrSidecarDown, s.timeouts.probe)
		}

		select {
		case <-ctx.Done():
		case <-time.After(probeInterval):
		}
	}
}

// sidecarProc is the attached client of a proxy sidecar and the reader of
// its lines.
type sidecarProc struct {
	p        proc.Process
	rt       Runtime
	spec     ContainerSpec
	req      sidecar.ProxyRequest
	started  time.Time
	lines    atomic.Pointer[func(sidecar.Line)]
	stopping atomic.Bool // set before the sandbox ends it, so its end is no news

	readyOnce sync.Once
	ready     chan struct{} // closed at the ready line
	downOnce  sync.Once
	down      chan struct{} // closed at an error line or the end of output
	cause     error         // why it is down; set before down is closed
	ended     chan struct{} // closed at the end of output
}

func readSidecar(rt Runtime, spec ContainerSpec, p proc.Process, req sidecar.ProxyRequest, lines func(sidecar.Line)) *sidecarProc {
	sc := &sidecarProc{
		p: p, rt: rt, spec: spec, req: req, started: time.Now(),
		ready: make(chan struct{}), down: make(chan struct{}), ended: make(chan struct{}),
	}
	sc.lines.Store(&lines)

	go sc.read()

	return sc
}

func (sc *sidecarProc) read() {
	defer close(sc.ended)

	for raw := range sc.p.Lines() {
		var l sidecar.Line
		if json.Unmarshal(raw, &l) != nil {
			continue
		}

		switch l.Type {
		case "ready":
			sc.readyOnce.Do(func() { close(sc.ready) })
		case "error":
			sc.fail(errors.New("its error: " + l.Error))
		case "denied", "usage", "budget":
			if l.Type == "denied" && l.Host == probeHost {
				continue
			}

			sc.deliver(l)
		}
	}

	sc.fail(sc.endCause())
}

// endCause is why the output ended. The runtime is asked only about an end
// the sandbox did not cause, since a stopped container may be gone already.
func (sc *sidecarProc) endCause() error {
	if sc.alive() && !sc.stopping.Load() {
		ctx, cancel := context.WithTimeout(context.Background(), inspectTimeout)
		defer cancel()

		if info, err := sc.rt.Inspect(ctx, sc.spec.Name); err == nil && info.OOMKilled {
			return fmt.Errorf("%w: the sidecar hit its %d MB limit", ErrOutOfMemory, sc.spec.MemoryMB)
		}
	}

	return errors.New("it exited: " + lastLine(sc.p.StderrTail()))
}

func (sc *sidecarProc) fail(cause error) {
	sc.downOnce.Do(func() {
		sc.cause = cause
		close(sc.down)

		// Before ready, Up's error reports it.
		if sc.isReady() && !sc.stopping.Load() {
			sc.deliver(sidecar.Line{Type: "error", Error: fmt.Errorf("%w: %w", ErrSidecarDown, cause).Error()})
		}
	})
}

func (sc *sidecarProc) deliver(l sidecar.Line) {
	if f := *sc.lines.Load(); f != nil {
		f(l)
	}
}

func (sc *sidecarProc) alive() bool {
	select {
	case <-sc.down:
		return false
	default:
		return true
	}
}

func (sc *sidecarProc) isReady() bool {
	select {
	case <-sc.ready:
		return true
	default:
		return false
	}
}

// waitReady counts within from the sidecar's start.
func (sc *sidecarProc) waitReady(ctx context.Context, within time.Duration) error {
	if !sc.alive() {
		return fmt.Errorf("%w: %w", ErrSidecarDown, sc.cause)
	}

	// First, because for a sidecar older than within the timer below has
	// already fired, and a select picks among ready cases at random.
	select {
	case <-sc.ready:
		return nil
	default:
	}

	timer := time.NewTimer(time.Until(sc.started.Add(within)))
	defer timer.Stop()

	select {
	case <-sc.ready:
		return nil
	case <-sc.down:
		return fmt.Errorf("%w: %w", ErrSidecarDown, sc.cause)
	case <-timer.C:
		return fmt.Errorf("%w: no ready line within %s", ErrSidecarDown, within)
	case <-ctx.Done():
		return fmt.Errorf("sandbox: %w", ctx.Err())
	}
}

// stop closes stdin, which ends the proxy once in-flight responses are done,
// and stops the client if it outlives the grace.
func (sc *sidecarProc) stop() {
	sc.stopping.Store(true)
	_ = sc.p.Stdin().Close()

	timer := time.NewTimer(sidecarStopGrace)
	defer timer.Stop()

	select {
	case <-sc.ended:
	case <-timer.C:
	}

	_ = sc.p.Stop(killGrace)
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	line := strings.TrimSpace(lines[len(lines)-1])

	if len(line) > 200 {
		line = line[:200] + "..."
	}

	if line == "" {
		return "no output"
	}

	return line
}
