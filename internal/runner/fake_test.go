package runner

import (
	"context"
	"errors"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mhersson/cavu/internal/driver"
	"github.com/mhersson/cavu/internal/engine"
	"github.com/mhersson/cavu/internal/proc"
	"github.com/mhersson/cavu/internal/sandbox"
	"github.com/mhersson/cavu/internal/sidecar"
)

const baseCommit = "0123456789abcdef0123456789abcdef01234567"

// The secrets testConfig carries.
const (
	plantedSetup    = "npm-secret-value"
	plantedEndpoint = "sk-endpoint-secret"
	plantedForge    = "forge-token-secret"
	plantedAppKey   = "app-private-key-secret"
)

func testConfig() engine.RunConfig {
	var c engine.RunConfig

	c.TaskID, c.RunID, c.ProjectID = 5, 7, 3
	c.InstanceID, c.Image = "0123456789abcdef", "cavu-sandbox:test"
	c.Repo.URL, c.Repo.BaseBranch, c.Repo.Branch = "https://forge.example/web.git", "main", "cavu/fix-login"
	c.Repo.Access = engine.ForgeAccess{Token: plantedForge, AppKey: plantedAppKey}
	c.Setup.Command = "make setup"
	c.Setup.Env = map[string]string{"APP_ENV": "test"}
	c.Setup.Secrets = map[string]string{"NPM_TOKEN": plantedSetup}
	c.Setup.EgressHosts = []string{"registry.example"}
	c.Verify.Command = "make test"
	c.Verify.Timeout = 2 * time.Minute
	c.Endpoint.Mode, c.Endpoint.URL, c.Endpoint.AuthHeader, c.Endpoint.Credential = "key", "https://gateway.example", "bearer", plantedEndpoint
	c.Identity.Name, c.Identity.Email = "CAVU", "cavu@example.com"

	return c
}

// rootFiles are the root files of the fake repository at the base commit.
func rootFiles() map[string]string {
	return map[string]string{
		"go.mod":   "module example.com/web\n\ngo 1.25.0\n",
		"Makefile": "test:\n\tgo test ./...\n",
	}
}

// phaseImport and phaseFill stand for the import and fill containers in the fake's records.
const (
	phaseImport sandbox.Phase = "import"
	phaseFill   sandbox.Phase = "fill"
)

type upCall struct {
	phase sandbox.Phase
	req   sidecar.ProxyRequest
}

type runCall struct {
	name    string
	phase   sandbox.Phase
	cmd     proc.Command
	timeout time.Duration
}

// fakeSandbox is a sandbox and its containers that record every call in
// order and answer from what the test set.
type fakeSandbox struct {
	mu sync.Mutex

	log      []string
	opened   []sandbox.Options
	ups      []upCall
	runs     []runCall
	reads    []string // the commit of each read
	clones   []sidecar.CloneRequest
	pushes   []sidecar.PushRequest
	copied   map[string][]byte // into a phase container
	imported map[string][]byte // into the import container
	lines    func(sidecar.Line)
	phase    sandbox.Phase // of the last Up, "" after Down
	files    map[string]string
	cloneErr error
	onClone  func(sidecar.CloneRequest) error                               // runs as the clone starts; an error it makes from the request fails the clone
	onPush   func(sidecar.PushRequest) (branch, rejected string, err error) // else the push takes the branch asked for
	upErr    error
	fillErr  error
	readCode int
	results  map[string]sandbox.Result // by step name
	runErrs  map[string]error
	onRead   func()            // runs as the root files are read
	onRun    func(name string) // runs as a command starts, before its answer
	downErr  error
	volErr   error
	rmErr    error             // RemoveProjectVolumes's
	out      map[string][]byte // what CopyOut copies, by path, from any container
}

func newFake() *fakeSandbox {
	return &fakeSandbox{
		files: rootFiles(), results: map[string]sandbox.Result{}, runErrs: map[string]error{},
		copied: map[string][]byte{}, imported: map[string][]byte{},
	}
}

// runner returns a Runner whose every sandbox is f.
func (f *fakeSandbox) runner() *Runner {
	r := New(sandbox.Runtime{Bin: "docker", Kind: "docker"}, fakeDriver{}, nil)
	r.open = func(o sandbox.Options) (sandboxer, error) {
		f.mu.Lock()
		f.opened = append(f.opened, o)
		f.mu.Unlock()

		return &fakeHandle{fakeSandbox: f}, nil
	}

	return r
}

var errFakeNoVolumes = errors.New("fake: the volumes are not in place")

// fakeHandle is one opened sandbox on the shared fake. Like the real one, it
// refuses Clone and Up until EnsureVolumes has run since it was opened or
// last destroyed.
type fakeHandle struct {
	*fakeSandbox

	volumes bool
}

func (h *fakeHandle) EnsureVolumes(ctx context.Context) error {
	err := h.fakeSandbox.EnsureVolumes(ctx)
	h.volumes = err == nil

	return err
}

func (h *fakeHandle) Destroy(ctx context.Context, keepHome bool) error {
	h.volumes = false

	return h.fakeSandbox.Destroy(ctx, keepHome)
}

func (h *fakeHandle) Clone(ctx context.Context, r sidecar.CloneRequest) (string, error) {
	if !h.volumes {
		h.record("clone")

		return "", errFakeNoVolumes
	}

	return h.fakeSandbox.Clone(ctx, r)
}

func (h *fakeHandle) Push(ctx context.Context, r sidecar.PushRequest) (string, string, error) {
	if !h.volumes {
		h.record("push")

		return "", "", errFakeNoVolumes
	}

	return h.fakeSandbox.Push(ctx, r)
}

func (h *fakeHandle) Up(ctx context.Context, phase sandbox.Phase, p sidecar.ProxyRequest, lines func(sidecar.Line)) (container, error) {
	if !h.volumes {
		h.record("up/" + string(phase))

		return nil, errFakeNoVolumes
	}

	return h.fakeSandbox.Up(ctx, phase, p, lines)
}

func (h *fakeHandle) Importer(ctx context.Context) (container, error) {
	if !h.volumes {
		h.record("importer")

		return nil, errFakeNoVolumes
	}

	return h.fakeSandbox.Importer(ctx)
}

func (f *fakeSandbox) record(entry string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.log = append(f.log, entry)
}

func (f *fakeSandbox) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return slices.Clone(f.log)
}

// runsIn returns the commands run in phase.
func (f *fakeSandbox) runsIn(phase sandbox.Phase) []runCall {
	f.mu.Lock()
	defer f.mu.Unlock()

	var out []runCall

	for _, r := range f.runs {
		if r.phase == phase {
			out = append(out, r)
		}
	}

	return out
}

// run returns the command run as step name.
func (f *fakeSandbox) run(t *testing.T, name string) runCall {
	t.Helper()

	f.mu.Lock()
	defer f.mu.Unlock()

	for _, r := range f.runs {
		if r.name == name {
			return r
		}
	}

	t.Fatalf("no command %q ran", name)

	return runCall{}
}

func (f *fakeSandbox) EnsureVolumes(context.Context) error {
	f.record("volumes")

	return f.volErr
}

func (f *fakeSandbox) Clone(ctx context.Context, r sidecar.CloneRequest) (string, error) {
	f.record("clone")

	f.mu.Lock()
	f.clones = append(f.clones, r)
	f.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return "", err
	}

	if f.onClone != nil {
		if err := f.onClone(r); err != nil {
			return "", err
		}
	}

	if f.cloneErr != nil {
		return "", f.cloneErr
	}

	return baseCommit, nil
}

func (f *fakeSandbox) Push(_ context.Context, r sidecar.PushRequest) (string, string, error) {
	f.record("push")

	f.mu.Lock()
	f.pushes = append(f.pushes, r)
	hook := f.onPush
	f.mu.Unlock()

	if hook != nil {
		return hook(r)
	}

	return r.Branch, "", nil
}

func (f *fakeSandbox) Up(_ context.Context, phase sandbox.Phase, p sidecar.ProxyRequest, lines func(sidecar.Line)) (container, error) {
	f.record("up/" + string(phase))

	f.mu.Lock()
	defer f.mu.Unlock()

	if f.upErr != nil {
		return nil, f.upErr
	}

	f.ups = append(f.ups, upCall{phase: phase, req: p})
	f.lines, f.phase = lines, phase

	return fakeContainer{f: f, phase: phase}, nil
}

func (f *fakeSandbox) Importer(context.Context) (container, error) {
	f.record("importer")

	return fakeContainer{f: f, phase: phaseImport}, nil
}

func (f *fakeSandbox) Filler(context.Context) (container, error) {
	f.record("filler")

	if f.fillErr != nil {
		return nil, f.fillErr
	}

	return fakeContainer{f: f, phase: phaseFill}, nil
}

func (f *fakeSandbox) RemoveImporter(context.Context) error {
	f.record("importer removed")

	return nil
}

func (f *fakeSandbox) Restart(context.Context) (container, error) {
	f.record("restart")

	f.mu.Lock()
	defer f.mu.Unlock()

	return fakeContainer{f: f, phase: f.phase}, nil
}

func (f *fakeSandbox) Down(context.Context) error {
	f.record("down")

	f.mu.Lock()
	f.phase = ""
	f.mu.Unlock()

	return f.downErr
}

func (f *fakeSandbox) Destroy(_ context.Context, keepHome bool) error {
	if keepHome {
		f.record("destroy/keep-home")
	} else {
		f.record("destroy")
	}

	return f.downErr
}

func (f *fakeSandbox) RemoveProjectVolumes(context.Context) error {
	f.record("remove-project-volumes")

	return f.rmErr
}

// fail sends the sidecar error line the sandbox sends when its sidecar ends.
func (f *fakeSandbox) fail(text string) { f.send(sidecar.Line{Type: "error", Error: text}) }

// send delivers a sidecar line to the phase that is up.
func (f *fakeSandbox) send(l sidecar.Line) {
	f.mu.Lock()
	lines := f.lines
	f.mu.Unlock()

	lines(l)
}

func (f *fakeSandbox) requests(phase sandbox.Phase) []sidecar.ProxyRequest {
	f.mu.Lock()
	defer f.mu.Unlock()

	var out []sidecar.ProxyRequest

	for _, u := range f.ups {
		if u.phase == phase {
			out = append(out, u.req)
		}
	}

	return out
}

type fakeContainer struct {
	f     *fakeSandbox
	phase sandbox.Phase
}

// at names a record of what c did: the import and fill containers' are marked.
func (c fakeContainer) at(entry string) string {
	if c.phase == phaseImport || c.phase == phaseFill {
		return string(c.phase) + "/" + entry
	}

	return entry
}

// stepName names a command by its argv: a wrapped command by what it wraps.
func stepName(argv []string) string {
	if len(argv) == 3 && argv[0] == "bash" {
		return strings.TrimPrefix(argv[2], "set -o pipefail; ")
	}

	return strings.Join(argv, " ")
}

// Start answers the root file read with the files the fake holds.
func (c fakeContainer) Start(_ context.Context, cmd proc.Command) (proc.Process, error) {
	c.f.record("read")

	c.f.mu.Lock()
	c.f.reads = append(c.f.reads, cmd.Argv[4])
	files, code, hook := maps.Clone(c.f.files), c.f.readCode, c.f.onRead
	c.f.mu.Unlock()

	if hook != nil {
		hook()
	}

	if cmd.Stdout != nil {
		_, _ = cmd.Stdout.Write(frame(files))
	}

	return fakeProcess{code: code}, nil
}

func (c fakeContainer) Run(ctx context.Context, cmd proc.Command, timeout time.Duration) (sandbox.Result, error) {
	name := stepName(cmd.Argv)
	c.f.record(c.at("run/" + name))

	c.f.mu.Lock()
	c.f.runs = append(c.f.runs, runCall{name: name, phase: c.phase, cmd: cmd, timeout: timeout})
	hook := c.f.onRun
	c.f.mu.Unlock()

	if hook != nil {
		hook(name)
	}

	if err := ctx.Err(); err != nil {
		return sandbox.Result{}, err
	}

	c.f.mu.Lock()
	defer c.f.mu.Unlock()

	if err := c.f.runErrs[name]; err != nil {
		return sandbox.Result{}, err
	}

	if res, ok := c.f.results[name]; ok {
		return res, nil
	}

	return sandbox.Result{Output: "ok\n"}, nil
}

// CopyIn records nothing for no files, for which the real one runs no exec.
func (c fakeContainer) CopyIn(_ context.Context, files map[string][]byte) error {
	if len(files) == 0 {
		return nil
	}

	c.f.record(c.at("copy"))

	c.f.mu.Lock()
	defer c.f.mu.Unlock()

	if c.phase == phaseImport {
		maps.Copy(c.f.imported, files)
	} else {
		maps.Copy(c.f.copied, files)
	}

	return nil
}

func (c fakeContainer) CopyOut(_ context.Context, p string, w io.Writer) error {
	c.f.record(c.at("copyout " + p))

	c.f.mu.Lock()
	data := c.f.out[p]
	c.f.mu.Unlock()

	_, err := w.Write(data)

	return err
}

// frame encodes files the way the read command prints them.
func frame(files map[string]string) []byte {
	var b strings.Builder

	for _, name := range slices.Sorted(maps.Keys(files)) {
		b.WriteString(strconv.Itoa(len(files[name])) + " " + name + "\n" + files[name])
	}

	return []byte(b.String())
}

type fakeProcess struct{ code int }

func (fakeProcess) Stdin() io.WriteCloser { return nopCloser{} }

func (fakeProcess) Lines() <-chan []byte {
	ch := make(chan []byte)
	close(ch)

	return ch
}

func (fakeProcess) StderrTail() string       { return "" }
func (fakeProcess) Stop(time.Duration) error { return nil }
func (p fakeProcess) Wait() (int, error)     { return p.code, nil }

type nopCloser struct{}

func (nopCloser) Write(b []byte) (int, error) { return len(b), nil }
func (nopCloser) Close() error                { return nil }

// fakeAgent is a driver whose runs end with results in turn, the last
// repeating, and that records each spec; during runs inside each run.
type fakeAgent struct {
	fakeDriver

	mu      sync.Mutex
	specs   []driver.Spec
	results []driver.Result
	err     error
	during  func(ctx context.Context, sink func(driver.Event))
}

func (d *fakeAgent) Run(ctx context.Context, _ proc.Target, s driver.Spec, sink func(driver.Event)) (driver.Result, error) {
	d.mu.Lock()
	d.specs = append(d.specs, s)
	n := len(d.specs)
	d.mu.Unlock()

	if d.during != nil {
		d.during(ctx, sink)
	}

	if d.err != nil {
		return driver.Result{}, d.err
	}

	return d.results[min(n, len(d.results))-1], nil
}

// workRunner returns a Runner whose every sandbox is f and whose agent is d.
func (f *fakeSandbox) workRunner(d *fakeAgent) *Runner {
	r := f.runner()
	r.driver = d

	return r
}

// fakeDriver names hosts per mode the way a driver does.
type fakeDriver struct{}

var driverHosts = map[string][]string{"subscription": {"model.example"}}

func (fakeDriver) Hosts(mode string) []string                         { return driverHosts[mode] }
func (fakeDriver) Probe(context.Context, proc.Target) (string, error) { return "", nil }

func (fakeDriver) Run(context.Context, proc.Target, driver.Spec, func(driver.Event)) (driver.Result, error) {
	return driver.Result{}, nil
}
