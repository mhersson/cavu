// Package doctor checks that an installation works: the container runtime,
// the sandbox image, the sandbox, the model endpoint and the forge credential.
package doctor

import (
	"context"
	_ "embed" // the endpoint check's prompt
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"slices"
	"strings"
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

//go:embed prompt.txt
var prompt string

// schema is the structured result the endpoint check asks for.
const schema = `{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"]}`

const (
	buildFix = "cavu image build"
	home     = "/home/agent"
	// commandTimeout bounds a command in the scratch container, the probe included.
	commandTimeout  = time.Minute
	endpointTimeout = 5 * time.Minute
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
	CABundle     []byte            // the configured CA bundle, empty for none
	ForgeToken   string
	Project      *Project // the project the forge check fetches; nil for none
	Driver       driver.Driver

	host host // nil drives the runtime; the unit tests set a fake
}

// Project is what the forge check needs of one project.
type Project struct {
	ID                  int64
	RepoURL, BaseBranch string
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

	image   string
	labels  sandbox.ImageLabels
	found   bool
	box     box    // the doctor's own sandbox, under project 0
	scratch target // its scratch container, when the image is there
	err     error  // of reading the image or starting the box
	agent   proc.Target
	standIn string
	checks  []Check
	failed  map[string]bool // failed or skipped
}

// Run runs the checks in order. One whose prerequisite did not pass is
// skipped with the reason; settings is decided first, as sandbox needs it.
func Run(ctx context.Context, d Deps) []Check {
	r := &doctor{Deps: d, image: sandbox.ImageName(d.Version), failed: map[string]bool{}}
	if r.host == nil {
		r.host = &runtimeHost{image: r.image, instance: d.Settings["instance_id"]}
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

	r.step(ctx, "forge", r.forge, "image", "settings")

	mask := redact.New(d.Credential, d.ForgeToken, r.standIn)
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
	c := Check{Skipped: true}

	if i := slices.IndexFunc(needs, func(n string) bool { return r.failed[n] }); i >= 0 {
		c.Detail = "skipped: the " + needs[i] + " check did not pass"
	} else {
		c = check(ctx)
	}

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
		return Check{Skipped: true, Detail: "skipped: the image is not built"}
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
	fail := func(detail string) Check { return Check{Detail: detail, Fix: buildFix} }
	bundleSum, err := sandbox.BundleSum(r.CABundle)

	switch {
	case r.err != nil:
		return failed(r.err, "")
	case !r.found:
		return fail("image " + r.image + " is not built")
	case r.labels.CavuVersion != r.Version:
		return fail(fmt.Sprintf("image %s is from cavu %s, this is %s", r.image, r.labels.CavuVersion, r.Version))
	case err != nil:
		return failed(err, "set ca_bundle_path to a PEM file of CA certificates")
	case r.labels.CABundle != bundleSum:
		return fail("the image's CA bundle is not the configured one")
	}

	if _, err := r.run(ctx, "test", "-f", proc.SandboxMarker); err != nil {
		return fail("the image has no sandbox marker " + proc.SandboxMarker + ": " + err.Error())
	}

	return Check{OK: true, Detail: r.image}
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

// settings names every key a run requires that the operator has not set;
// instance_id is generated at first use.
func (r *doctor) settings() Check {
	keys := []string{"endpoint_mode", "model_work", "commit_name", "commit_email"}
	if r.Settings["endpoint_mode"] == "key" {
		keys = append(keys, "endpoint_url", "endpoint_auth_header")
	}

	missing := slices.DeleteFunc(keys, func(k string) bool { return r.Settings[k] != "" })
	if r.Credential == "" {
		missing = append(missing, "endpoint_credential")
	}

	if len(missing) > 0 {
		return Check{
			Name: "settings", Detail: "not set: " + strings.Join(missing, ", "),
			Fix: "cavu settings set KEY VALUE, and cavu secret set endpoint_credential for the credential",
		}
	}

	return Check{Name: "settings", OK: true}
}

// sandbox brings up the agent phase with the allowlist a run's agent phase
// has, and in key mode the model route with a stand-in token.
func (r *doctor) sandbox(ctx context.Context) Check {
	mode := r.Settings["endpoint_mode"]
	req := sidecar.ProxyRequest{AllowHosts: r.Driver.Hosts(mode), CABundle: string(r.CABundle)}

	if mode == "key" {
		r.standIn, _ = proxy.NewStandIn() // crypto/rand does not fail
		req.Model = &proxy.ModelRoute{
			BaseURL: r.Settings["endpoint_url"], Credential: r.Credential,
			AuthHeader: r.Settings["endpoint_auth_header"], StandIn: r.standIn,
		}
	}

	err := r.box.EnsureVolumes(ctx)
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

	auth := driver.Auth{Mode: r.Settings["endpoint_mode"], Token: r.Credential}
	if auth.Mode == "key" {
		auth.Token, auth.BaseURL = r.standIn, sandbox.BridgeURL
	}

	s := driver.Spec{
		Auth: auth, Model: r.Settings["model_work"], MaxTurns: 4, SessionID: engine.NewSessionID(),
		Prompt: prompt, OutputSchema: json.RawMessage(schema),
		Dir: "/workspace", Home: home, ConfigDir: home + "/.claude-cavu",
		Env: append(env.AgentEnv(nil, home), "HTTPS_PROXY="+sandbox.BridgeURL, "HTTP_PROXY="+sandbox.BridgeURL),
	}

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

// forge fetches the project into its own mirror, with run and task id 0.
func (r *doctor) forge(ctx context.Context) Check {
	if r.ForgeToken == "" || r.Project == nil {
		return Check{Skipped: true, Detail: "skipped: no forge token or no project"}
	}

	b, err := r.host.open(r.Project.ID)
	if err == nil {
		err = b.Destroy(ctx, false)
	}

	if err == nil {
		if err = b.EnsureVolumes(ctx); err == nil {
			_, err = b.Clone(ctx, sidecar.CloneRequest{
				URL: r.Project.RepoURL, Token: r.ForgeToken, BaseBranch: r.Project.BaseBranch, Branch: "cavu/doctor",
				CABundle: string(r.CABundle), Name: r.Settings["commit_name"], Email: r.Settings["commit_email"],
			})
		}

		err = errors.Join(err, b.Destroy(ctx, false))
	}

	if err != nil {
		return failed(err, "check the secret forge_token and the project's repository address")
	}

	return Check{OK: true}
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
