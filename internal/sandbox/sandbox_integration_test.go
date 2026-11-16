//go:build integration

package sandbox_test

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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

func TestCloneThenAgentSeesWorkspace(t *testing.T) {
	gitrepo.Isolate(t)

	r := sandboxtest.Runtime(t)
	image := sandboxImage(t, r, nil)
	caPEM, _, _ := sandboxtest.CA(t)

	repo := gitrepo.New(t, map[string]string{"README.md": "hello\n"})
	head := gitrepo.Git(t, repo, "rev-parse", "HEAD")
	bare := strings.TrimPrefix(gitrepo.Bare(t, repo), "file://")
	port := sandboxtest.StartTLSServer(t, sandboxtest.HostAddr(t, r), gitrepo.Handler(t, bare))

	s := newSandbox(t, r, image)

	base, err := s.Clone(t.Context(), sidecar.CloneRequest{
		URL:        "https://host.docker.internal:" + port + "/" + filepath.Base(bare),
		BaseBranch: "main",
		Branch:     "cavu/test",
		CABundle:   string(caPEM),
		Name:       "CAVU Test",
		Email:      "cavu@example.com",
	})
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
