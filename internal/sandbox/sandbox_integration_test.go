//go:build integration

package sandbox_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/cavu/internal/proxy"
	"github.com/mhersson/cavu/internal/sandbox"
	"github.com/mhersson/cavu/internal/sidecar"
	"github.com/mhersson/cavu/internal/testkit/gitrepo"
	"github.com/mhersson/cavu/internal/testkit/sandboxtest"
)

const viaBridge = "http://127.0.0.1:3128"

func randomInstance(t *testing.T) string {
	t.Helper()

	b := make([]byte, 8)
	_, err := rand.Read(b)
	require.NoError(t, err)

	return hex.EncodeToString(b)
}

func randomID(t *testing.T) int64 {
	t.Helper()

	n, err := rand.Int(rand.Reader, big.NewInt(1<<40))
	require.NoError(t, err)

	return n.Int64() + 1
}

// instancePrefix starts every container and volume name of the instance.
func instancePrefix(instance string) string { return "cavu-" + instance[:8] + "-" }

// schemeName is a name from the documented scheme, for checking what the
// runtime lists.
func schemeName(instance, kind string, id int64) string {
	return instancePrefix(instance) + kind + "-" + strconv.FormatInt(id, 10)
}

func openSandbox(t *testing.T, r sandbox.Runtime, image, instance string, run, task, project int64) *sandbox.Sandbox {
	t.Helper()

	s, err := sandbox.Open(sandbox.Options{
		Runtime: r, Image: image, InstanceID: instance,
		RunID: run, TaskID: task, ProjectID: project,
		Labels: sandboxtest.Labels(),
	})
	require.NoError(t, err)

	removeAfter(t, s.Down)

	return s
}

// newSandbox opens a sandbox of a fresh instance with its volumes in place.
func newSandbox(t *testing.T, r sandbox.Runtime, image string) *sandbox.Sandbox {
	t.Helper()

	instance := randomInstance(t)
	sandboxtest.RemoveInstanceAfter(t, r, instance)

	s := openSandbox(t, r, image, instance, randomID(t), randomID(t), randomID(t))
	require.NoError(t, s.EnsureVolumes(t.Context()))

	return s
}

// serveText serves body over plain HTTP on addr with a random port.
func serveText(t *testing.T, addr, body string) (port string) {
	t.Helper()

	ln, err := net.Listen("tcp", net.JoinHostPort(addr, "0"))
	require.NoError(t, err)

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, body) }))
	_ = srv.Listener.Close()
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)

	_, port, err = net.SplitHostPort(ln.Addr().String())
	require.NoError(t, err)

	return port
}

type lineLog struct {
	mu    sync.Mutex
	lines []sidecar.Line
}

func (l *lineLog) add(line sidecar.Line) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.lines = append(l.lines, line)
}

func (l *lineLog) all() []sidecar.Line {
	l.mu.Lock()
	defer l.mu.Unlock()

	return slices.Clone(l.lines)
}

func processes(t *testing.T, c sandbox.Container) string {
	t.Helper()

	res := run(t, c, "ps", "-eo", "args")
	require.Equal(t, 0, res.ExitCode, res.Output)

	return res.Output
}

func TestSandboxEgress(t *testing.T) {
	t.Parallel()

	r := sandboxtest.Runtime(t)
	image := sandboxImage(t, r, nil)
	addr := sandboxtest.HostAddr(t, r)

	allowed := "http://host.docker.internal:" + serveText(t, addr, "allowed") + "/"
	refusedPort := serveText(t, addr, "refused")
	refused := "http://host.docker.internal:" + refusedPort + "/"

	s := newSandbox(t, r, image)

	var lines lineLog

	c, err := s.Up(t.Context(), sandbox.PhaseAgent, sidecar.ProxyRequest{AllowHosts: []string{strings.TrimSuffix(strings.TrimPrefix(allowed, "http://"), "/")}}, lines.add)
	require.NoError(t, err)

	res := run(t, c, "curl", "--silent", "--show-error", "--max-time", "10", "--proxy", viaBridge, allowed)
	require.Equal(t, 0, res.ExitCode, res.Output)
	assert.Equal(t, "allowed", res.Output)

	res = run(t, c, "curl", "--silent", "--output", "/dev/null", "--max-time", "10", "--write-out", "%{http_code}", "--proxy", viaBridge, refused)
	assert.Equal(t, "403", res.Output)

	for _, u := range []string{allowed, refused} {
		res = run(t, c, "curl", "--silent", "--max-time", "5", "--noproxy", "*", u)
		assert.NotEqual(t, 0, res.ExitCode, "%s reached without the proxy", u)
	}

	want := []sidecar.Line{{Type: "denied", Host: "host.docker.internal:" + refusedPort}}

	require.Eventually(t, func() bool { return len(lines.all()) >= len(want) }, 10*time.Second, 50*time.Millisecond)
	assert.Equal(t, want, lines.all())
}

// TestPhaseRunsABinaryFromTmp runs a binary from /tmp, as go test does with the
// test binaries it builds under TMPDIR.
func TestPhaseRunsABinaryFromTmp(t *testing.T) {
	t.Parallel()

	r := sandboxtest.Runtime(t)
	s := newSandbox(t, r, sandboxImage(t, r, nil))

	for _, phase := range []sandbox.Phase{sandbox.PhaseSetup, sandbox.PhaseAgent} {
		c, err := s.Up(t.Context(), phase, sidecar.ProxyRequest{}, nil)
		require.NoError(t, err)

		res := run(t, c, "sh", "-c", "cp /bin/true /tmp/true && /tmp/true")
		assert.Equal(t, 0, res.ExitCode, "%s: %s", phase, res.Output)
	}
}

func TestSandboxSurvivesDownUp(t *testing.T) {
	t.Parallel()

	r := sandboxtest.Runtime(t)
	s := newSandbox(t, r, sandboxImage(t, r, nil))

	c, err := s.Up(t.Context(), sandbox.PhaseAgent, sidecar.ProxyRequest{}, nil)
	require.NoError(t, err)
	require.Equal(t, 0, run(t, c, "sh", "-c", "echo kept > /workspace/kept").ExitCode)

	require.NoError(t, s.Down(t.Context()))

	c, err = s.Up(t.Context(), sandbox.PhaseAgent, sidecar.ProxyRequest{}, nil)
	require.NoError(t, err)
	assert.Equal(t, "kept\n", run(t, c, "cat", "/workspace/kept").Output)
}

func TestRestartKillsLeftovers(t *testing.T) {
	t.Parallel()

	r := sandboxtest.Runtime(t)
	s := newSandbox(t, r, sandboxImage(t, r, nil))

	c, err := s.Up(t.Context(), sandbox.PhaseAgent, sidecar.ProxyRequest{}, nil)
	require.NoError(t, err)

	res := run(t, c, "sh", "-c", "echo kept > /workspace/kept; sleep 1000 > /dev/null 2>&1 &")
	require.Equal(t, 0, res.ExitCode, res.Output)
	require.Contains(t, processes(t, c), "sleep 1000")

	c, err = s.Restart(t.Context())
	require.NoError(t, err)
	assert.NotContains(t, processes(t, c), "sleep 1000")
	assert.Equal(t, "kept\n", run(t, c, "cat", "/workspace/kept").Output)
}

// containerID is the runtime's id of the container name, which a container
// started again under the same name does not keep.
func containerID(t *testing.T, r sandbox.Runtime, name string) string {
	t.Helper()

	out, err := exec.CommandContext(t.Context(), r.Bin, "container", "inspect", name).Output() //nolint:gosec // our runtime, our container
	require.NoError(t, err)

	var got []struct {
		ID string `json:"Id"`
	}

	require.NoError(t, json.Unmarshal(out, &got))
	require.Len(t, got, 1)
	require.NotEmpty(t, got[0].ID)

	return got[0].ID
}

// TestRemoveHeldProjectVolumes: a container that mounts the project's cache keeps
// the removal from succeeding; once it is gone the cache and the mirror are
// gone and the run's and the task's volumes stay.
func TestRemoveHeldProjectVolumes(t *testing.T) {
	t.Parallel()

	r := sandboxtest.Runtime(t)
	instance, project := randomInstance(t), randomID(t)
	sandboxtest.RemoveInstanceAfter(t, r, instance)

	s := openSandbox(t, r, "cavu-sandbox:unused", instance, 0, 0, project)
	require.NoError(t, s.EnsureVolumes(t.Context()))

	cache, mirror := schemeName(instance, "cache", project), schemeName(instance, "mirror", project)
	others := []string{schemeName(instance, "workspace", 0), schemeName(instance, "handoff", 0), schemeName(instance, "home", 0)}

	volumes := func() []string {
		_, v, err := r.List(t.Context(), "cavu.instance="+instance)
		require.NoError(t, err)

		return v
	}

	require.ElementsMatch(t, append([]string{cache, mirror}, others...), volumes())

	holder := sandboxtest.UniqueName()

	removeAfter(t, func(ctx context.Context) error { return r.Remove(ctx, holder) })
	require.NoError(t, r.Start(t.Context(), sandbox.ContainerSpec{
		Name: holder, Image: "docker.io/library/busybox:1.37", Argv: []string{"sleep", "3600"},
		Labels: sandboxtest.Labels(), Network: "none", Mounts: []sandbox.Mount{{Volume: cache, Path: "/cache"}},
	}))

	err := s.RemoveProjectVolumes(t.Context())
	require.Error(t, err, "a volume a container holds is not removed")
	t.Log(err)
	assert.Contains(t, volumes(), cache)

	require.NoError(t, r.Remove(t.Context(), holder))
	require.NoError(t, s.RemoveProjectVolumes(t.Context()))
	assert.ElementsMatch(t, others, volumes())
}

// TestReadOnlyPhase: the read-only phase refuses a write to the workspace and
// takes one wherever else the agent phase writes, and a change of phase with
// the same proxy request replaces the phase container alone.
func TestReadOnlyPhase(t *testing.T) {
	t.Parallel()

	r := sandboxtest.Runtime(t)
	image := sandboxImage(t, r, nil)
	allowed := "host.docker.internal:" + serveText(t, sandboxtest.HostAddr(t, r), "allowed")

	instance, runID := randomInstance(t), randomID(t)
	sandboxtest.RemoveInstanceAfter(t, r, instance)

	s := openSandbox(t, r, image, instance, runID, randomID(t), randomID(t))
	require.NoError(t, s.EnsureVolumes(t.Context()))

	req := sidecar.ProxyRequest{AllowHosts: []string{allowed}}
	side := schemeName(instance, "sidecar", runID)

	var sideID string

	for i, phase := range []sandbox.Phase{sandbox.PhaseAgent, sandbox.PhaseReadOnly, sandbox.PhaseAgent} {
		c, err := s.Up(t.Context(), phase, req, nil)
		require.NoError(t, err, phase)

		containers, _, err := r.List(t.Context(), "cavu.instance="+instance)
		require.NoError(t, err)
		assert.ElementsMatch(t, []string{side, schemeName(instance, string(phase), runID)}, containers, "Up %d", i+1)

		if i == 0 {
			sideID = containerID(t, r, side)
		}

		assert.Equal(t, sideID, containerID(t, r, side), "Up %d replaced the sidecar", i+1)

		res := run(t, c, "curl", "--silent", "--show-error", "--max-time", "10", "--proxy", viaBridge, "http://"+allowed+"/")
		assert.Equal(t, "allowed", res.Output, "Up %d", i+1)

		if phase == sandbox.PhaseAgent {
			res := run(t, c, "sh", "-c", "echo kept > /workspace/kept")
			assert.Equal(t, 0, res.ExitCode, "Up %d: %s", i+1, res.Output)

			continue
		}

		assert.Equal(t, "kept\n", run(t, c, "cat", "/workspace/kept").Output)

		for dir, writable := range map[string]bool{"/workspace": false, "/home/agent": true, "/handoff": true, "/run/cavu": true} {
			res := run(t, c, "touch", dir+"/written")
			assert.Equal(t, writable, res.ExitCode == 0, "a write under %s: %s", dir, res.Output)
		}
	}
}

func TestRemoveOrphans(t *testing.T) {
	t.Parallel()

	r := sandboxtest.Runtime(t)
	image := sandboxImage(t, r, nil)

	instance, other := randomInstance(t), randomInstance(t)
	sandboxtest.RemoveInstanceAfter(t, r, instance)
	sandboxtest.RemoveInstanceAfter(t, r, other)

	run1, run2, task, project := randomID(t), randomID(t), randomID(t), randomID(t)

	for _, sb := range []*sandbox.Sandbox{
		openSandbox(t, r, image, instance, run1, task, project),
		openSandbox(t, r, image, instance, run2, task, project),
		openSandbox(t, r, image, other, run1, task, project),
	} {
		require.NoError(t, sb.EnsureVolumes(t.Context()))

		_, err := sb.Up(t.Context(), sandbox.PhaseSetup, sidecar.ProxyRequest{}, nil)
		require.NoError(t, err)
	}

	otherContainers, otherVolumes, err := r.List(t.Context(), "cavu.instance="+other)
	require.NoError(t, err)
	require.Len(t, otherContainers, 2)
	require.Len(t, otherVolumes, 5)

	require.NoError(t, sandbox.RemoveOrphans(t.Context(), r, instance, []int64{run2}))

	containers, volumes, err := r.List(t.Context(), "cavu.instance="+instance)
	require.NoError(t, err)
	assert.Empty(t, containers)
	assert.ElementsMatch(t, []string{
		schemeName(instance, "workspace", run2),
		schemeName(instance, "handoff", run2),
		schemeName(instance, "home", task),
		schemeName(instance, "cache", project),
		schemeName(instance, "mirror", project),
	}, volumes)

	containers, volumes, err = r.List(t.Context(), "cavu.instance="+other)
	require.NoError(t, err)
	assert.Equal(t, otherContainers, containers)
	assert.Equal(t, otherVolumes, volumes)

	info, err := r.Inspect(t.Context(), schemeName(other, "setup", run1))
	require.NoError(t, err)
	assert.True(t, info.Running, "the other instance's container stopped")
}

// TestModelRouteTrustsTheImageCA sends a model request through the bridge to
// an upstream whose certificate the test CA signs, with nothing about the CA
// passed at run time: the sidecar's own TLS trusts it only in an image built
// with the CA.
func TestModelRouteTrustsTheImageCA(t *testing.T) {
	t.Parallel()

	r := sandboxtest.Runtime(t)
	caPEM, _, _ := sandboxtest.CA(t)

	var reached atomic.Int32

	port := sandboxtest.StartTLSServer(t, sandboxtest.HostAddr(t, r), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached.Add(1)
		fmt.Fprint(w, "{}")
	}))

	for _, tc := range []struct {
		name    string
		ca      []byte
		status  string
		reached int32
	}{
		{"trusting image", caPEM, "200", 1},
		{"plain image", nil, "502", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reached.Store(0)

			s := newSandbox(t, r, sandboxImage(t, r, tc.ca))

			c, err := s.Up(t.Context(), sandbox.PhaseAgent, sidecar.ProxyRequest{Model: &proxy.ModelRoute{
				BaseURL: "https://host.docker.internal:" + port, Credential: "credential", AuthHeader: "bearer", StandIn: "stand-in",
			}}, nil)
			require.NoError(t, err)

			res := run(t, c, "curl", "--silent", "--output", "/dev/null", "--max-time", "10", "--write-out", "%{http_code}",
				"--header", "Authorization: Bearer stand-in", "--data", "{}", viaBridge+"/v1/messages")
			assert.Equal(t, tc.status, res.Output)
			assert.Equal(t, tc.reached, reached.Load())
		})
	}
}

// TestCloneThenAgentSeesWorkspace clones from a server whose certificate the
// CA built into the image signs; nothing about the CA is passed at run time.
func TestCloneThenAgentSeesWorkspace(t *testing.T) {
	gitrepo.Isolate(t)

	r := sandboxtest.Runtime(t)
	caPEM, _, _ := sandboxtest.CA(t)

	repo := gitrepo.New(t, map[string]string{"README.md": "hello\n"})
	head := gitrepo.Git(t, repo, "rev-parse", "HEAD")
	bare := strings.TrimPrefix(gitrepo.Bare(t, repo), "file://")
	port := sandboxtest.StartTLSServer(t, sandboxtest.HostAddr(t, r), gitrepo.Handler(t, bare))

	req := sidecar.CloneRequest{
		URL:        "https://host.docker.internal:" + port + "/" + filepath.Base(bare),
		BaseBranch: "main",
		Branch:     "cavu/test",
		Name:       "CAVU Test",
		Email:      "cavu@example.com",
	}

	t.Run("plain image", func(t *testing.T) {
		_, err := newSandbox(t, r, sandboxImage(t, r, nil)).Clone(t.Context(), req)
		t.Log(err)
		require.Error(t, err)
	})

	s := newSandbox(t, r, sandboxImage(t, r, caPEM))

	base, err := s.Clone(t.Context(), req)
	require.NoError(t, err)
	assert.Equal(t, head, base)

	c, err := s.Up(t.Context(), sandbox.PhaseAgent, sidecar.ProxyRequest{}, nil)
	require.NoError(t, err)

	res := run(t, c, "git", "-C", "/workspace", "log", "-1", "--format=%H")
	require.Equal(t, 0, res.ExitCode, res.Output)
	assert.Equal(t, head+"\n", res.Output)

	// git refuses a repository another uid owns unless safe.directory names it.
	res = run(t, c, "git", "-C", "/workspace", "status", "--porcelain")
	require.Equal(t, 0, res.ExitCode, res.Output)
	assert.Empty(t, res.Output)

	// The image keeps an empty /mirror so the volume mounted there takes uid
	// 1000; in the agent container it must stay that empty directory.
	res = run(t, c, "ls", "-A", "/mirror")
	require.Equal(t, 0, res.ExitCode, res.Output)
	assert.Empty(t, res.Output, "the agent container sees the mirror")
}

const pushToken = "ghp_pushtoken0123456789abcdef" //nolint:gosec // a test token

// pushForge serves the bare repository over TLS and requires pushToken as basic
// auth. During every authenticated request it asks the runtime to inspect the
// clone and the push container, so the test sees the token in neither while
// they run. The returned map names the containers an inspect reached.
func pushForge(t *testing.T, r sandbox.Runtime, bare string, names ...string) (url string, inspected func() map[string]bool) {
	t.Helper()

	var (
		mu   sync.Mutex
		seen = map[string]bool{}
	)

	backend := gitrepo.Handler(t, bare)
	port := sandboxtest.StartTLSServer(t, sandboxtest.HostAddr(t, r), http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if user, pass, ok := req.BasicAuth(); !ok || user != "x-access-token" || pass != pushToken {
			w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
			w.WriteHeader(http.StatusUnauthorized)

			return
		}

		for _, name := range names {
			out, err := exec.CommandContext(req.Context(), r.Bin, "inspect", name).CombinedOutput() //nolint:gosec // our runtime
			if err != nil {
				continue
			}

			assert.NotContains(t, string(out), pushToken, name)

			mu.Lock()
			seen[name] = true
			mu.Unlock()
		}

		backend.ServeHTTP(w, req)
	}))

	return "https://host.docker.internal:" + port + "/" + filepath.Base(bare), func() map[string]bool {
		mu.Lock()
		defer mu.Unlock()

		return maps.Clone(seen)
	}
}

// pushFixture is a forge repository with a feature branch ahead of main, and
// the sandbox whose mirror a clone filled from it.
func pushFixture(t *testing.T, r sandbox.Runtime, allowPush bool) (s *sandbox.Sandbox, bare, feature string, url string, inspected func() map[string]bool) {
	t.Helper()

	gitrepo.Isolate(t)

	caPEM, _, _ := sandboxtest.CA(t)
	repo := gitrepo.New(t, map[string]string{"README.md": "hello\n"})
	gitrepo.Git(t, repo, "checkout", "-q", "-b", "feature")
	feature = gitrepo.Commit(t, repo, map[string]string{"README.md": "feature\n"}, "feature")
	gitrepo.Git(t, repo, "checkout", "-q", "main")

	bare = strings.TrimPrefix(gitrepo.Bare(t, repo), "file://")
	if allowPush {
		gitrepo.AllowPush(t, bare)
	}

	instance, run := randomInstance(t), randomID(t)
	url, inspected = pushForge(t, r, bare, schemeName(instance, "clone", run), schemeName(instance, "push", run))

	sandboxtest.RemoveInstanceAfter(t, r, instance)

	s = openSandbox(t, r, sandboxImage(t, r, caPEM), instance, run, randomID(t), randomID(t))
	require.NoError(t, s.EnsureVolumes(t.Context()))

	_, err := s.Clone(t.Context(), sidecar.CloneRequest{URL: url, Token: pushToken, BaseBranch: "main", Branch: "cavu/test", Name: "CAVU Test", Email: "cavu@example.com"})
	require.NoError(t, err)

	return s, bare, feature, url, inspected
}

// TestSandboxPushReachesTheForge pushes a commit of the mirror through a push
// container to a TLS forge that wants the token, and inspects both containers
// while they run.
func TestSandboxPushReachesTheForge(t *testing.T) {
	r := sandboxtest.Runtime(t)
	s, bare, feature, url, inspected := pushFixture(t, r, true)

	branch, rejected, err := s.Push(t.Context(), sidecar.PushRequest{
		URL: url, Token: pushToken, Ref: "refs/heads/feature", Commit: feature, Branch: "cavu/pushed",
	})
	require.NoError(t, err)
	assert.Empty(t, rejected)
	assert.Equal(t, "cavu/pushed", branch)
	assert.Equal(t, feature, gitrepo.Git(t, bare, "rev-parse", "refs/heads/cavu/pushed"))

	seen := inspected()
	assert.Len(t, seen, 2, "an inspect reached the clone and the push container")
}

// A token that may read and not write meets a forge that refuses the push: a
// rejection that carries git's line, not an error.
func TestSandboxPushRefusedByTheForgeIsARejection(t *testing.T) {
	r := sandboxtest.Runtime(t)
	s, bare, feature, url, _ := pushFixture(t, r, false)

	branch, rejected, err := s.Push(t.Context(), sidecar.PushRequest{
		URL: url, Token: pushToken, Ref: "refs/heads/feature", Commit: feature, Branch: "cavu/pushed",
	})
	require.NoError(t, err)
	assert.Equal(t, "cavu/pushed", branch)
	assert.NotEmpty(t, rejected)
	assert.NotContains(t, rejected, pushToken)
	assert.NotContains(t, rejected, "://")
	assert.Empty(t, gitrepo.Git(t, bare, "for-each-ref", "refs/heads/cavu"))
}
