package doctor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/cavu/internal/driver"
	"github.com/mhersson/cavu/internal/engine"
	"github.com/mhersson/cavu/internal/forge"
	"github.com/mhersson/cavu/internal/proc"
	"github.com/mhersson/cavu/internal/runner"
	"github.com/mhersson/cavu/internal/sandbox"
	"github.com/mhersson/cavu/internal/sidecar"
	"github.com/mhersson/cavu/internal/testkit/forgefake"
	"github.com/mhersson/cavu/internal/testkit/sandboxtest"
)

const (
	credential = "endpoint-credential-5d1f8a2c" //nolint:gosec // a dummy
	forgeToken = "forge-token-6b1e4a7f"         //nolint:gosec // a dummy
	pinned     = "2.1.325"
	version    = "1.4.0"
	routeHead  = "Iface\tDestination\tGateway\tFlags\tRefCnt\tUse\tMetric\tMask\tMTU\tWindow\tIRTT\n"
)

var order = []string{"runtime", "no_network", "image", "claude_code", "sandbox", "settings", "endpoint", "forge"}

// fakeHost answers from its fields and records what the doctor asks of it.
// It runs nothing.
type fakeHost struct {
	labels   sandbox.ImageLabels
	found    bool
	noMarker bool
	route    bool

	cloneErr func(sidecar.CloneRequest) error // the clone fails with the error it makes from the request

	opened   []int64
	destroys int
	ups      []sidecar.ProxyRequest
	clones   []sidecar.CloneRequest
}

func (h *fakeHost) detect(context.Context, string) (string, error) { return "docker 28.5.1", nil }

func (h *fakeHost) imageLabels(context.Context) (sandbox.ImageLabels, bool, error) {
	return h.labels, h.found, nil
}

func (h *fakeHost) open(projectID int64) (box, error) {
	h.opened = append(h.opened, projectID)

	return fakeBox{h}, nil
}

type fakeBox struct{ h *fakeHost }

func (b fakeBox) EnsureVolumes(context.Context) error { return nil }

func (b fakeBox) Up(_ context.Context, _ sandbox.Phase, p sidecar.ProxyRequest, _ func(sidecar.Line)) (sandbox.Container, error) {
	b.h.ups = append(b.h.ups, p)

	return sandbox.Container{}, nil
}

func (b fakeBox) Clone(_ context.Context, r sidecar.CloneRequest) (string, error) {
	b.h.clones = append(b.h.clones, r)

	if b.h.cloneErr != nil {
		return "", b.h.cloneErr(r)
	}

	return strings.Repeat("a", 40), nil
}

func (b fakeBox) Destroy(context.Context, bool) error {
	b.h.destroys++

	return nil
}

func (b fakeBox) scratch(context.Context) (target, error) { return fakeScratch(b), nil }

// fakeScratch answers the doctor's two commands; it starts nothing.
type fakeScratch struct{ h *fakeHost }

func (fakeScratch) Start(context.Context, proc.Command) (proc.Process, error) {
	return nil, errors.New("fake: no process")
}

func (s fakeScratch) Run(_ context.Context, cmd proc.Command, _ time.Duration) (sandbox.Result, error) {
	switch {
	case slices.Contains(cmd.Argv, proc.SandboxMarker) && s.h.noMarker:
		return sandbox.Result{ExitCode: 1}, nil
	case slices.Contains(cmd.Argv, "/proc/net/route") && s.h.route:
		return sandbox.Result{Output: routeHead + "eth0\t00000000\t010011AC\t0003\t0\t0\t0\t00000000\t0\t0\t0\n"}, nil
	case slices.Contains(cmd.Argv, "/proc/net/route"):
		return sandbox.Result{Output: routeHead}, nil
	}

	return sandbox.Result{}, nil
}

// fakeDriver streams one tool call per session and answers with its results
// in order.
type fakeDriver struct {
	version string
	results []driver.Result
	specs   []driver.Spec
}

func (d *fakeDriver) Probe(context.Context, proc.Target) (string, error) { return d.version, nil }

func (d *fakeDriver) Hosts(mode string) []string {
	if mode == "subscription" {
		return []string{"model.example"}
	}

	return nil
}

func (d *fakeDriver) Run(_ context.Context, _ proc.Target, s driver.Spec, sink func(driver.Event)) (driver.Result, error) {
	d.specs = append(d.specs, s)

	sink(driver.Event{Kind: driver.KindToolUse, ToolName: "shell"})

	return d.results[len(d.specs)-1], nil
}

func sum(b []byte) string {
	s := sha256.Sum256(b)

	return hex.EncodeToString(s[:])
}

// green is an installation where every check passes.
func green(t *testing.T, mode string) (Deps, *fakeHost, *fakeDriver) {
	t.Helper()

	bundle, _, _ := sandboxtest.CA(t)
	h := &fakeHost{labels: sandbox.ImageLabels{CavuVersion: version, AgentVersion: pinned, CABundle: sum(bundle)}, found: true}
	done := driver.Result{Reason: driver.ReasonDone, Structured: json.RawMessage(`{"status":"done"}`)}
	cached := done
	cached.Usage = []driver.Usage{{Model: "model-a", CacheRead: 4096}}
	drv := &fakeDriver{version: pinned, results: []driver.Result{done, cached}}

	return Deps{
		Version:      version,
		AgentVersion: pinned,
		Settings: map[string]string{
			"instance_id": "0123456789abcdef", "endpoint_mode": mode, "endpoint_url": "https://gateway.example",
			"endpoint_auth_header": "bearer", "model_work": "model-a", "effort_work": "low",
			"commit_name": "CAVU", "commit_email": "cavu@example.com",
		},
		Credential: credential,
		CABundle:   bundle,
		ForgeToken: forgeToken,
		Project:    &Project{ID: 4, RepoURL: "https://forge.example/team/repo.git", BaseBranch: "main"},
		Driver:     drv,
		host:       h,
	}, h, drv
}

func byName(t *testing.T, checks []Check) map[string]Check {
	t.Helper()

	m := map[string]Check{}
	names := make([]string, 0, len(checks))

	for _, c := range checks {
		m[c.Name] = c
		names = append(names, c.Name)
	}

	require.Equal(t, order, names)

	return m
}

func TestDoctorAllGreen(t *testing.T) {
	for _, mode := range []string{"key", "subscription"} {
		t.Run(mode, func(t *testing.T) {
			d, h, drv := green(t, mode)

			checks := Run(t.Context(), d)
			for _, c := range byName(t, checks) {
				assert.True(t, c.OK && !c.Skipped && !c.Warning, "%+v", c)
			}

			assert.Equal(t, 0, ExitCode(checks))
			assert.Equal(t, []int64{0, 4}, h.opened, "the doctor's own sandbox, then the project's for the fetch")
			assert.Equal(t, 4, h.destroys, "each sandbox starts and ends with a Destroy")

			// The sandbox and the session are a run's, but the session's prompt, id and turns.
			c, err := engine.ReadSettings(d.Settings)
			require.NoError(t, err)

			c.Endpoint.Credential = credential

			req, spec, err := runner.AgentPhase(c, drv)
			require.NoError(t, err)
			require.Len(t, h.ups, 1)
			require.Len(t, drv.specs, 2)

			up, first, second := h.ups[0], drv.specs[0], drv.specs[1]
			if mode == "key" {
				req.Model.StandIn, spec.Auth.Token = up.Model.StandIn, up.Model.StandIn
			}

			spec.Prompt, spec.SessionID, spec.MaxTurns = first.Prompt, first.SessionID, first.MaxTurns

			assert.Equal(t, req, up)
			assert.Equal(t, spec, first)
			assert.Equal(t, first.SessionID, second.Resume, "the second turn resumes the session")
			assert.Empty(t, second.SessionID)

			require.Len(t, h.clones, 1)
			assert.Equal(t, d.Project.RepoURL, h.clones[0].URL)
			assert.Equal(t, forgeToken, h.clones[0].Token)
			assert.Equal(t, "main", h.clones[0].BaseBranch)
		})
	}
}

func TestDoctorCABundleChanged(t *testing.T) {
	d, h, _ := green(t, "key")
	configured := d.CABundle
	d.CABundle, _, _ = sandboxtest.CAFromSeed(t, "doctor-test-other-ca-seed-012345")

	checks := byName(t, Run(t.Context(), d))
	assert.False(t, checks["image"].OK)
	assert.Contains(t, checks["image"].Fix, "cavu image build")
	assert.True(t, checks["claude_code"].Skipped)

	t.Run("a bundle configured after an image without one", func(t *testing.T) {
		h.labels.CABundle = ""
		d.CABundle = configured

		assert.False(t, byName(t, Run(t.Context(), d))["image"].OK)
	})

	t.Run("a configured file that is no bundle", func(t *testing.T) {
		d.CABundle = []byte("not a certificate")

		image := byName(t, Run(t.Context(), d))["image"]
		assert.False(t, image.OK)
		assert.NotContains(t, image.Fix, "cavu image build", "a rebuild would not help")
	})
}

func TestDoctorImageVersionMismatch(t *testing.T) {
	d, h, drv := green(t, "key")
	h.labels.CavuVersion = "1.3.0"

	checks := Run(t.Context(), d)
	m := byName(t, checks)
	assert.False(t, m["image"].OK)
	assert.Contains(t, m["image"].Fix, "cavu image build")

	for _, name := range []string{"claude_code", "sandbox", "endpoint"} {
		assert.True(t, m[name].Skipped, name)
		assert.NotEmpty(t, m[name].Detail, "a skip gives its reason")
	}

	assert.Empty(t, h.ups)
	assert.Empty(t, drv.specs)
	assert.Equal(t, 1, ExitCode(checks))
}

// The image's other faults: absent, without the sandbox marker, or a runtime
// whose network none still routes.
func TestDoctorImageFaults(t *testing.T) {
	t.Run("not built", func(t *testing.T) {
		d, h, _ := green(t, "key")
		h.found = false

		m := byName(t, Run(t.Context(), d))
		assert.True(t, m["no_network"].Skipped)
		assert.False(t, m["image"].OK)
		assert.Contains(t, m["image"].Fix, "cavu image build")
	})

	t.Run("no sandbox marker", func(t *testing.T) {
		d, h, _ := green(t, "key")
		h.noMarker = true

		m := byName(t, Run(t.Context(), d))
		assert.False(t, m["image"].OK)
		assert.Contains(t, m["image"].Fix, "cavu image build")
	})

	t.Run("a route out", func(t *testing.T) {
		d, h, _ := green(t, "key")
		h.route = true

		m := byName(t, Run(t.Context(), d))
		assert.False(t, m["no_network"].OK)
		assert.True(t, m["image"].OK)
	})
}

func TestDoctorMissingSettingNamesKey(t *testing.T) {
	for _, key := range []string{"instance_id", "model_work", "endpoint_url", "endpoint_auth_header", "commit_email", "endpoint_credential"} {
		t.Run(key, func(t *testing.T) {
			d, h, drv := green(t, "key")
			delete(d.Settings, key)

			if key == "endpoint_credential" {
				d.Credential = ""
			}

			checks := Run(t.Context(), d)
			m := byName(t, checks)
			assert.False(t, m["settings"].OK)
			assert.Contains(t, m["settings"].Detail, key)

			for _, name := range []string{"sandbox", "endpoint", "forge"} {
				assert.True(t, m[name].Skipped, name)
			}

			assert.True(t, m["image"].OK, "a check that needs no setting still runs")
			assert.Empty(t, h.ups)
			assert.Empty(t, drv.specs)
			assert.Equal(t, 1, ExitCode(checks))
		})
	}

	t.Run("a value a run cannot read", func(t *testing.T) {
		d, _, _ := green(t, "key")
		d.Settings["task_max_turns"] = "many"

		settings := byName(t, Run(t.Context(), d))["settings"]
		assert.False(t, settings.OK)
		assert.Contains(t, settings.Detail, "task_max_turns")
	})

	t.Run("subscription mode needs no endpoint url", func(t *testing.T) {
		d, _, _ := green(t, "subscription")
		delete(d.Settings, "endpoint_url")

		assert.True(t, byName(t, Run(t.Context(), d))["settings"].OK)
	})
}

func TestDoctorEndpointAuthFailure(t *testing.T) {
	d, _, drv := green(t, "key")
	drv.results[0] = driver.Result{
		Reason: driver.ReasonAuth, IsError: true,
		Detail: "API error 401 authentication_error: the key " + credential + " and " + forgeToken + " are refused",
	}

	checks := Run(t.Context(), d)
	m := byName(t, checks)
	assert.False(t, m["endpoint"].OK)
	assert.Contains(t, m["endpoint"].Detail, "401")
	assert.Len(t, drv.specs, 1, "no second turn after a failed first")
	assert.Equal(t, 1, ExitCode(checks))

	for _, c := range checks {
		for _, field := range []string{c.Name, c.Detail, c.Fix} {
			assert.NotContains(t, field, credential)
			assert.NotContains(t, field, forgeToken)
		}
	}
}

// echoDriver fails each session with a detail that repeats the token it holds.
type echoDriver struct{ *fakeDriver }

func (echoDriver) Run(_ context.Context, _ proc.Target, s driver.Spec, _ func(driver.Event)) (driver.Result, error) {
	return driver.Result{Reason: driver.ReasonAuth, IsError: true, Detail: s.Auth.Token}, nil
}

// In key mode the session holds the doctor's stand-in, which only the
// doctor's mask knows.
func TestDoctorMasksTheStandIn(t *testing.T) {
	d, h, drv := green(t, "key")
	d.Driver = echoDriver{drv}

	checks := Run(t.Context(), d)
	require.False(t, byName(t, checks)["endpoint"].OK)

	require.Len(t, h.ups, 1)
	standIn := h.ups[0].Model.StandIn
	require.NotEmpty(t, standIn)

	for _, c := range checks {
		assert.NotContains(t, c.Detail, standIn)
		assert.NotContains(t, c.Fix, standIn)
	}
}

func TestDoctorEndpointWarnsWithoutCacheReads(t *testing.T) {
	d, _, drv := green(t, "key")
	drv.results[1].Usage = []driver.Usage{{Model: "model-a", Input: 900}}

	checks := Run(t.Context(), d)
	endpoint := byName(t, checks)["endpoint"]
	assert.True(t, endpoint.OK)
	assert.True(t, endpoint.Warning)
	assert.Equal(t, 0, ExitCode(checks))
}

// A loopback endpoint gets its own fix: the sidecar cannot reach this
// machine's loopback.
func TestDoctorLoopbackEndpointFix(t *testing.T) {
	fix := func(url string) string {
		d, _, drv := green(t, "key")
		d.Settings["endpoint_url"] = url
		drv.results[0] = driver.Result{Reason: driver.ReasonEndpoint, IsError: true, Detail: "connection refused"}

		return byName(t, Run(t.Context(), d))["endpoint"].Fix
	}

	other := fix("https://gateway.example")
	for _, url := range []string{"http://localhost:8080", "http://127.0.0.1:4000/v1", "http://[::1]:9000"} {
		assert.NotEqual(t, other, fix(url), url)
	}

	assert.Equal(t, other, fix("http://host.docker.internal:8080"))
}

func TestDoctorExitCode(t *testing.T) {
	for name, tc := range map[string]struct {
		checks []Check
		want   int
	}{
		"all pass":          {[]Check{{Name: "a", OK: true}, {Name: "b", OK: true}}, 0},
		"a warning passes":  {[]Check{{Name: "a", OK: true, Warning: true}}, 0},
		"a skip fails none": {[]Check{{Name: "a", OK: true}, {Name: "forge", Skipped: true}}, 0},
		"a failure":         {[]Check{{Name: "a", OK: true}, {Name: "b"}, {Name: "c", Skipped: true}}, 1},
		"no checks":         {nil, 0},
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, ExitCode(tc.checks))
		})
	}
}

// onForge puts d's project, with publish on, on a fake GitHub that d's client
// reaches on the real clock, since the fake's tokens expire by it. It returns
// the fake and its API address.
func onForge(t *testing.T, d *Deps) (*forgefake.Fake, string) {
	t.Helper()

	fake := forgefake.New(t, t.TempDir())
	srv := httptest.NewTLSServer(fake)
	t.Cleanup(srv.Close)

	client, err := forge.New(forge.Options{Transport: srv.Client().Transport, Version: version})
	require.NoError(t, err)

	d.Forge = client
	d.Project.RepoURL, d.Project.Publish = srv.URL+"/o/n", true

	return fake, srv.URL + "/api/v3"
}

// withApp makes d's forge credential a GitHub App's.
func withApp(d *Deps) {
	d.ForgeToken, d.ForgeAppKey = "", forgefake.AppKey()
	d.Settings["forge_app_id"] = "42"
}

func paths(fake *forgefake.Fake) []string {
	var out []string
	for _, r := range fake.Requests() {
		out = append(out, r.Path)
	}

	return out
}

func TestDoctorForgeReadsTheRepository(t *testing.T) {
	t.Run("an App's token, publish on", func(t *testing.T) {
		d, h, _ := green(t, "key")
		fake, _ := onForge(t, &d)
		withApp(&d)

		forgeCheck := byName(t, Run(t.Context(), d))["forge"]
		assert.True(t, forgeCheck.OK && !forgeCheck.Skipped, "%+v", forgeCheck)

		minted := fake.Tokens()
		require.Len(t, minted, 1, "the clone's token serves the read too")
		require.Len(t, h.clones, 1)
		assert.Equal(t, minted[0].Value, h.clones[0].Token)
		assert.Contains(t, paths(fake), "/api/v3/repos/o/n")
	})

	t.Run("a personal access token, publish off", func(t *testing.T) {
		d, h, _ := green(t, "key")
		fake, _ := onForge(t, &d)
		d.Project.Publish = false

		assert.True(t, byName(t, Run(t.Context(), d))["forge"].OK)
		require.Len(t, h.clones, 1)
		assert.Equal(t, forgeToken, h.clones[0].Token)
		assert.Empty(t, fake.Requests(), "no request reaches the forge")
	})

	for _, tc := range []struct {
		name       string
		credential func(*Deps)
		clones     int
	}{
		{"the repository read", func(*Deps) {}, 1},
		{"the App's mint", withApp, 0},
	} {
		t.Run("a failure of "+tc.name+" names the API address", func(t *testing.T) {
			d, h, _ := green(t, "key")
			fake, api := onForge(t, &d)
			tc.credential(&d)
			fake.Fail(1)

			checks := Run(t.Context(), d)
			forgeCheck := byName(t, checks)["forge"]
			assert.False(t, forgeCheck.OK)
			assert.Contains(t, forgeCheck.Detail, api)
			assert.Len(t, h.clones, tc.clones)
			assert.Equal(t, 1, ExitCode(checks))
		})
	}

	t.Run("skipped without a credential or a project", func(t *testing.T) {
		for name, edit := range map[string]func(*Deps){
			"no credential": func(d *Deps) { d.ForgeToken = "" },
			"no project":    func(d *Deps) { d.Project = nil },
		} {
			d, h, _ := green(t, "key")
			edit(&d)

			assert.True(t, byName(t, Run(t.Context(), d))["forge"].Skipped, name)
			assert.Empty(t, h.clones, name)
		}
	})
}

// The forge check's token, an App's minted one too, and the App's key are
// masked wherever a check's text holds them.
func TestDoctorMasksTheForgeCredential(t *testing.T) {
	d, h, _ := green(t, "key")
	fake, _ := onForge(t, &d)
	withApp(&d)

	h.cloneErr = func(r sidecar.CloneRequest) error {
		return fmt.Errorf("fatal: https://x-access-token:%s@forge.example/o/n refused; key %s", r.Token, d.ForgeAppKey)
	}

	checks := Run(t.Context(), d)
	require.False(t, byName(t, checks)["forge"].OK)

	minted := fake.Tokens()
	require.Len(t, minted, 1)

	for _, c := range checks {
		for _, secret := range []string{minted[0].Value, d.ForgeAppKey} {
			assert.NotContains(t, c.Detail, secret)
			assert.NotContains(t, c.Fix, secret)
		}
	}
}
