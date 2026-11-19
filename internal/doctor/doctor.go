// Package doctor checks that an installation works: the container runtime,
// the sandbox image, the sandbox, the model endpoint and the forge credential.
package doctor

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/mhersson/cavu/internal/driver"
	"github.com/mhersson/cavu/internal/engine"
	"github.com/mhersson/cavu/internal/forge"
	"github.com/mhersson/cavu/internal/proc"
	"github.com/mhersson/cavu/internal/prompts"
	"github.com/mhersson/cavu/internal/redact"
	"github.com/mhersson/cavu/internal/runner"
	"github.com/mhersson/cavu/internal/sandbox"
	"github.com/mhersson/cavu/internal/sidecar"
)

const (
	buildFix = "cavu image build"
	// commandTimeout bounds a command in the scratch container, the probe included.
	commandTimeout  = time.Minute
	endpointTimeout = 5 * time.Minute
	// endpointTurns caps the check's session, which needs two.
	endpointTurns = 4
)

// Check is one line of the report; a Warning still passes.
type Check struct {
	Name                 string
	OK, Skipped, Warning bool
	Detail, Fix          string
}

// Deps is everything Run needs; the caller reads the store and the vault.
type Deps struct {
	Version      string            // this binary's; its image is sandbox.ImageName(Version)
	AgentVersion string            // the pinned agent release
	Settings     map[string]string // the settings table; runtime names the runtime to use
	Credential   string            // the secret endpoint_credential
	CABundle     []byte            // the configured CA bundle the image must carry, empty for none
	ForgeToken   string
	ForgeAppKey  string        // the secret forge_app_key
	Forge        *forge.Client // the client the forge check mints and reads with
	Projects     []Project     // the projects the forge check fetches, in the store's order
	Driver       driver.Driver

	host host // nil drives the runtime; the unit tests set a fake
}

// Project is what the forge check needs of one project.
type Project struct {
	ID                  int64
	Name                string
	RepoURL, BaseBranch string
	Publish             bool // the switch publish
}

// host is the doctor's seam over internal/sandbox.
type host interface {
	// detect finds the runtime and names it with its engine's version.
	detect(ctx context.Context, preferred string) (string, error)
	imageLabels(ctx context.Context) (sandbox.ImageLabels, bool, error)
	// open opens a sandbox of the image under the environment check's ids: run and task id 0.
	open(projectID int64) (box, error)
}

// box is what the doctor uses of a *sandbox.Sandbox, its scratch container
// as a target.
type box interface {
	EnsureVolumes(ctx context.Context) error
	Up(ctx context.Context, phase sandbox.Phase, p sidecar.ProxyRequest, lines func(sidecar.Line)) (sandbox.Container, error)
	Clone(ctx context.Context, r sidecar.CloneRequest) (string, error)
	Destroy(ctx context.Context, keepHome bool) error
	scratch(ctx context.Context) (target, error)
}

// target is a container to run commands in, as sandbox.Container is.
type target interface {
	proc.Target
	Run(ctx context.Context, cmd proc.Command, timeout time.Duration) (sandbox.Result, error)
}

type doctor struct {
	Deps

	labels  sandbox.ImageLabels
	found   bool
	box     box              // the doctor's own sandbox, under project 0
	scratch target           // its scratch container, when the image is there
	err     error            // of reading the image or starting the box
	config  engine.RunConfig // the settings and the credential, read as a run reads them
	tokens  []string         // the forge checks' clone tokens, which an App mints
	agent   proc.Target
	spec    driver.Spec // a session in the agent phase, but its prompt and id
	checks  []Check
	failed  map[string]bool // failed or skipped
}

// Run runs the checks in order. One whose prerequisite did not pass is
// skipped with the reason; settings is decided first, as sandbox needs it.
func Run(ctx context.Context, d Deps) []Check {
	r := &doctor{Deps: d, failed: map[string]bool{}}
	if r.host == nil {
		r.host = &runtimeHost{image: sandbox.ImageName(d.Version), instance: d.Settings["instance_id"]}
	}

	settings := r.settings()
	r.failed["settings"] = !settings.OK

	r.step(ctx, "runtime", r.runtime)

	if !r.failed["runtime"] {
		r.start(ctx)
	}

	r.step(ctx, "no_network", r.noNetwork, "runtime")
	r.step(ctx, "image", r.imageCheck, "runtime")
	r.step(ctx, "claude_code", r.claudeCode, "image")
	r.step(ctx, "sandbox", r.sandbox, "image", "settings")
	r.checks = append(r.checks, settings)
	r.step(ctx, "endpoint", r.endpoint, "claude_code", "sandbox")

	// What a failed removal leaves, the next run's opening Destroy removes.
	if r.box != nil {
		_ = r.box.Destroy(ctx, false)
	}

	r.forges(ctx)

	// In key mode the session's token is a stand-in only this mask knows, and
	// an App's tokens are the forge checks' own.
	mask := redact.New(append([]string{d.Credential, d.ForgeToken, d.ForgeAppKey, r.spec.Auth.Token}, r.tokens...)...)
	for i := range r.checks {
		r.checks[i].Detail, r.checks[i].Fix = mask(r.checks[i].Detail), mask(r.checks[i].Fix)
	}

	return r.checks
}

// ExitCode is 1 when a check failed and 0 otherwise: a warning or a skip
// fails nothing.
func ExitCode(checks []Check) int {
	if slices.ContainsFunc(checks, func(c Check) bool { return !c.OK && !c.Skipped }) {
		return 1
	}

	return 0
}

func (r *doctor) step(ctx context.Context, name string, check func(context.Context) Check, needs ...string) {
	c := r.skip(needs)
	if !c.Skipped {
		c = check(ctx)
	}

	r.add(name, c)
}

// skip is a skipped check naming the first of needs that did not pass, or the
// zero Check when all passed.
func (r *doctor) skip(needs []string) Check {
	if i := slices.IndexFunc(needs, func(n string) bool { return r.failed[n] }); i >= 0 {
		return Check{Skipped: true, Detail: "the " + needs[i] + " check did not pass"}
	}

	return Check{}
}

func (r *doctor) add(name string, c Check) {
	c.Name = name
	r.failed[name] = !c.OK
	r.checks = append(r.checks, c)
}

func failed(err error, fix string) Check { return Check{Detail: err.Error(), Fix: fix} }

func (r *doctor) runtime(ctx context.Context) Check {
	v, err := r.host.detect(ctx, r.Settings["runtime"])
	if err != nil {
		return failed(err, "install or start Docker Engine or Podman, or set runtime to the one installed")
	}

	return Check{OK: true, Detail: v}
}

// start reads the image's labels and starts the doctor's sandbox clean, with
// its scratch container when the image is there.
func (r *doctor) start(ctx context.Context) {
	if r.labels, r.found, r.err = r.host.imageLabels(ctx); r.err != nil {
		return
	}

	if r.box, r.err = r.host.open(0); r.err != nil {
		return
	}

	if r.err = r.box.Destroy(ctx, false); r.err == nil && r.found {
		r.scratch, r.err = r.box.scratch(ctx)
	}
}

// run runs argv in the scratch container; a non-zero exit is an error.
func (r *doctor) run(ctx context.Context, argv ...string) (string, error) {
	if r.err != nil {
		return "", r.err
	}

	res, err := r.scratch.Run(ctx, proc.Command{Argv: argv}, commandTimeout)
	if err == nil && res.ExitCode != 0 {
		err = fmt.Errorf("%s exited %d: %s", argv[0], res.ExitCode, strings.TrimSpace(res.Output))
	}

	return res.Output, err
}

// noNetwork wants no route out of a container with network none, which shows
// that it reaches nothing without sending a packet.
func (r *doctor) noNetwork(ctx context.Context) Check {
	if r.err == nil && !r.found {
		return Check{Skipped: true, Detail: "the image is not built"}
	}

	out, err := r.run(ctx, "cat", "/proc/net/route")

	switch {
	case err != nil:
		return failed(err, "")
	case strings.Contains(strings.TrimSpace(out), "\n"):
		return Check{Detail: "a container with network none has a route out", Fix: "use a runtime whose network none isolates a container"}
	}

	return Check{OK: true}
}

func (r *doctor) imageCheck(ctx context.Context) Check {
	if r.err != nil {
		return failed(r.err, "")
	}

	c := CheckImage(r.labels, r.found, r.Version, r.CABundle)
	if !c.OK {
		return c
	}

	if _, err := r.run(ctx, "test", "-f", proc.SandboxMarker); err != nil {
		return Check{Detail: "the image has no sandbox marker " + proc.SandboxMarker + ": " + err.Error(), Fix: buildFix}
	}

	return c
}

// CheckImage checks the labels of version's image, found or not: the image
// must be built by that version and carry the configured CA bundle, empty for
// none. A run starts only on an image it passes.
func CheckImage(labels sandbox.ImageLabels, found bool, version string, caBundle []byte) Check {
	image := sandbox.ImageName(version)
	fail := func(detail string) Check { return Check{Detail: detail, Fix: buildFix} }
	bundleSum, err := sandbox.BundleSum(caBundle)

	switch {
	case !found:
		return fail("image " + image + " is not built")
	case labels.CavuVersion != version:
		return fail(fmt.Sprintf("image %s is from cavu %s, this is %s", image, labels.CavuVersion, version))
	case err != nil:
		return failed(err, "set ca_bundle_path to a PEM file of CA certificates")
	case labels.CABundle != bundleSum:
		return fail("the image's CA bundle is not the configured one")
	}

	return Check{OK: true, Detail: image}
}

func (r *doctor) claudeCode(ctx context.Context) Check {
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()

	v, err := r.Driver.Probe(ctx, r.scratch)

	switch {
	case err != nil:
		return failed(err, buildFix)
	case v != r.AgentVersion:
		return Check{Detail: "the image has " + v + ", this cavu pins " + r.AgentVersion, Fix: buildFix}
	}

	return Check{OK: true, Detail: v}
}

// settings reads the settings as a run reads them, and wants the credential.
func (r *doctor) settings() Check {
	var err error

	r.config, err = engine.ReadSettings(r.Settings)
	r.config.Endpoint.Credential = r.Credential

	var problems []string
	if err != nil {
		problems = append(problems, err.Error())
	}

	if r.Credential == "" {
		problems = append(problems, "the secret endpoint_credential is not set")
	}

	if len(problems) > 0 {
		return Check{
			Name: "settings", Detail: strings.Join(problems, "; "),
			Fix: "cavu settings set KEY VALUE, and cavu secret set endpoint_credential for the credential",
		}
	}

	return Check{Name: "settings", OK: true}
}

// sandbox brings up the agent phase a run brings up.
func (r *doctor) sandbox(ctx context.Context) Check {
	req, spec, err := runner.AgentPhase(r.config, r.Driver)
	r.spec = spec

	if err == nil {
		err = r.box.EnsureVolumes(ctx)
	}

	if err == nil {
		r.agent, err = r.box.Up(ctx, sandbox.PhaseAgent, req, nil)
	}

	if err != nil {
		return failed(err, "")
	}

	return Check{OK: true}
}

// endpoint runs one session in the agent container, then resumes it once to
// see the prompt cache read.
func (r *doctor) endpoint(ctx context.Context) Check {
	ctx, cancel := context.WithTimeout(ctx, endpointTimeout)
	defer cancel()

	prompt, err := prompts.Render(prompts.Doctor, nil)
	if err != nil {
		return failed(err, "")
	}

	s := r.spec
	s.Prompt, s.SessionID, s.MaxTurns = prompt, engine.NewSessionID(), endpointTurns

	turn := func(s driver.Spec) (driver.Result, string) {
		streamed := false
		res, err := r.Driver.Run(ctx, r.agent, s, func(e driver.Event) { streamed = streamed || e.Kind == driver.KindToolUse })

		switch {
		case err != nil:
			return res, err.Error()
		case res.Reason != driver.ReasonDone:
			return res, res.Reason + ": " + res.Detail
		case !streamed || len(res.Structured) == 0:
			return res, "the session streamed no tool call or gave no structured result"
		}

		return res, ""
	}

	if _, problem := turn(s); problem != "" {
		return Check{Detail: problem, Fix: r.endpointFix()}
	}

	s.SessionID, s.Resume = "", s.SessionID

	res, problem := turn(s)
	if problem != "" {
		return Check{Detail: "resumed: " + problem, Fix: r.endpointFix()}
	}

	var cached int64
	for _, u := range res.Usage {
		cached += u.CacheRead
	}

	if cached == 0 {
		return Check{OK: true, Warning: true, Detail: "the second turn read no tokens from the prompt cache"}
	}

	return Check{OK: true}
}

// endpointFix explains a loopback endpoint, which the sidecar cannot reach.
func (r *doctor) endpointFix() string {
	host := ""
	if u, err := url.Parse(r.Settings["endpoint_url"]); err == nil {
		host = u.Hostname()
	}

	if ip := net.ParseIP(host); host == "localhost" || ip != nil && ip.IsLoopback() {
		return "the sidecar reaches this machine as host.docker.internal, which on rootless Podman is the machine's " +
			"network address and not loopback: point endpoint_url there and have the endpoint listen on it"
	}

	return "check endpoint_mode, endpoint_url, endpoint_auth_header, model_work and the secret endpoint_credential"
}

// forges runs the forge check once for each project, a line each.
func (r *doctor) forges(ctx context.Context) {
	a := r.config.Repo.Access
	a.Token, a.AppKey = r.ForgeToken, r.ForgeAppKey

	if c := r.skip([]string{"image", "settings"}); c.Skipped {
		r.add("forge", c)

		return
	}

	if a.Token == "" && a.AppID == 0 || len(r.Projects) == 0 {
		r.add("forge", Check{Skipped: true, Detail: "no credential for GitHub, or no project"})

		return
	}

	for _, p := range r.Projects {
		c := r.forge(ctx, p, a)
		if c.Detail == "" {
			c.Detail = p.Name
		} else {
			c.Detail = p.Name + ": " + c.Detail
		}

		r.add("forge", c)
	}
}

// forge fetches the project into its own mirror, with run and task id 0 and
// the token a run's clone gets, then, when the project publishes, reads the
// repository and the checks of the base head it fetched as the watch does.
func (r *doctor) forge(ctx context.Context, p Project, a engine.ForgeAccess) Check {
	const fix = "check the secret forge_token, or the setting forge_app_id and the secret forge_app_key, and the project's repository address; " +
		"a GitHub App must be granted Contents and Pull requests read and write, and Checks, Commit statuses, Actions and Metadata read"

	token, err := runner.CloneToken(ctx, r.Forge, p.RepoURL, a)
	r.tokens = append(r.tokens, token)

	if err != nil {
		return failed(err, fix)
	}

	head, err := r.clone(ctx, p, token)
	if err != nil {
		return failed(err, fix)
	}

	if !p.Publish {
		return Check{OK: true}
	}

	repo, err := forge.ParseRepo(p.RepoURL)
	if err == nil {
		err = r.Forge.Check(ctx, repo, forge.Access(a))
	}

	if err != nil {
		return failed(err, fix+"; or turn the project's switch publish off")
	}

	jobs, err := r.Forge.ReadChecks(ctx, repo, forge.Access(a), head)

	switch {
	case err != nil:
		return failed(err, grantRead(a)+"; or turn the project's switch publish off")
	case jobs:
		return Check{OK: true, Detail: "with this credential CAVU reads the jobs of GitHub Actions workflow runs and the commit " +
			"statuses: a check reported only as a check run, by another app or by a workflow step, is not seen, which a GitHub App does see"}
	}

	return Check{OK: true}
}

// grantRead is what the credential the instance holds needs to read checks.
func grantRead(a engine.ForgeAccess) string {
	if a.AppID == 0 {
		return "grant a fine-grained personal access token read access to Actions and Commit statuses"
	}

	return "grant the GitHub App read access to Checks, Actions and Commit statuses"
}

// clone clones p with token in a sandbox of its own, which it destroys, and
// returns the base branch's head.
func (r *doctor) clone(ctx context.Context, p Project, token string) (string, error) {
	b, err := r.host.open(p.ID)
	if err == nil {
		err = b.Destroy(ctx, false)
	}

	if err != nil {
		return "", err
	}

	var head string

	if err = b.EnsureVolumes(ctx); err == nil {
		head, err = b.Clone(ctx, sidecar.CloneRequest{
			URL: p.RepoURL, Token: token, BaseBranch: p.BaseBranch, Branch: "cavu/doctor",
			Name: r.Settings["commit_name"], Email: r.Settings["commit_email"],
		})
	}

	return head, errors.Join(err, b.Destroy(ctx, false))
}

// runtimeHost drives the runtime through internal/sandbox.
type runtimeHost struct {
	rt              sandbox.Runtime
	image, instance string
	labels          map[string]string // the conformance test's
}

func (h *runtimeHost) detect(ctx context.Context, preferred string) (string, error) {
	rt, err := sandbox.Detect(ctx, preferred)
	if err != nil {
		return "", err
	}

	h.rt = rt
	v, err := rt.Version(ctx)

	return rt.Kind + " " + v, err
}

func (h *runtimeHost) imageLabels(ctx context.Context) (sandbox.ImageLabels, bool, error) {
	return h.rt.ImageInfo(ctx, h.image)
}

func (h *runtimeHost) open(projectID int64) (box, error) {
	sb, err := sandbox.Open(sandbox.Options{Runtime: h.rt, Image: h.image, InstanceID: h.instance, ProjectID: projectID, Labels: h.labels})
	if err != nil {
		return nil, err
	}

	return sandboxBox{sb}, nil
}

type sandboxBox struct{ *sandbox.Sandbox }

func (b sandboxBox) scratch(ctx context.Context) (target, error) { return b.Scratch(ctx) }
