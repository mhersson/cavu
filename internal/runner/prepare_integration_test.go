//go:build integration

package runner

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/cavu/internal/driver/claudecode"
	"github.com/mhersson/cavu/internal/engine"
	"github.com/mhersson/cavu/internal/proc"
	"github.com/mhersson/cavu/internal/sandbox"
	"github.com/mhersson/cavu/internal/testkit/gitrepo"
	"github.com/mhersson/cavu/internal/testkit/sandboxtest"
)

func TestMain(m *testing.M) { sandboxtest.Main(m) }

// goVersion is the toolchain the fixture's go.mod asks for.
const goVersion = "1.25.0"

func TestPrepareGoRepository(t *testing.T) {
	if os.Getenv("CAVU_TEST_OFFLINE") == "1" {
		t.Skip("CAVU_TEST_OFFLINE=1: this test downloads a Go toolchain through the allowlist")
	}

	gitrepo.Isolate(t)

	rt := sandboxtest.Runtime(t)
	caPEM, _, _ := sandboxtest.CA(t)
	image := sandboxtest.Image(t, rt, claudecode.Version, claudecode.SHA512, caPEM)

	repo := gitrepo.New(t, map[string]string{
		"go.mod":   "module example.com/hello\n\ngo " + goVersion + "\n",
		"hello.go": "package hello\n\nfunc Hello() string { return \"hello\" }\n",
		"hello_test.go": "package hello\n\nimport \"testing\"\n\n" +
			"func TestHello(t *testing.T) {\n\tif Hello() != \"hello\" {\n\t\tt.Fatal(Hello())\n\t}\n}\n",
	})
	head := gitrepo.Git(t, repo, "rev-parse", "HEAD")
	bare := strings.TrimPrefix(gitrepo.Bare(t, repo), "file://")
	port := sandboxtest.StartTLSServer(t, sandboxtest.HostAddr(t, rt), gitrepo.Handler(t, bare))

	var c engine.RunConfig

	c.TaskID, c.RunID, c.ProjectID = randomID(t), randomID(t), randomID(t)
	c.InstanceID, c.Image = randomInstance(t), image
	c.Repo.URL = "https://host.docker.internal:" + port + "/" + filepath.Base(bare)
	c.Repo.BaseBranch, c.Repo.Branch = "main", "cavu/hello"
	c.Setup.Command = "go mod download"
	c.Verify.Command, c.Verify.Timeout = "go test ./...", 5*time.Minute
	c.Endpoint.Mode = "subscription"
	c.Identity.Name, c.Identity.Email = "CAVU Test", "cavu@example.com"
	c.CABundle = caPEM

	removeInstanceAfter(t, rt, c.InstanceID)

	r := New(rt, claudecode.New())
	r.labels = sandboxtest.Labels()

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()

		assert.NoError(t, r.Release(ctx, refOf(c), true))
	})

	started := time.Now()
	res, err := r.Prepare(t.Context(), c)
	cold := time.Since(started)

	require.NoError(t, err)
	require.Empty(t, res.FailedStep, res.Output)
	assert.Equal(t, head, res.BaseCommit)
	assert.Equal(t, []string{"go@" + goVersion}, res.Tools)
	assert.Equal(t, []string{"go"}, res.Ecosystems)
	assert.Equal(t, engine.ResultPassed, res.Baseline)

	agent := r.state(c.RunID).agent
	require.NotNil(t, agent)

	out := runIn(t, agent, "go", "version")
	assert.Equal(t, 0, out.ExitCode, out.Output)
	assert.Contains(t, out.Output, "go"+goVersion+" ")

	out = runIn(t, agent, "touch", "/cache/x")
	assert.NotEqual(t, 0, out.ExitCode, "the agent phase writes the project cache")

	c.Verify.BaselinePassed, c.Verify.BaselineCommit = true, head

	started = time.Now()
	res, err = r.Prepare(t.Context(), c)
	warm := time.Since(started)

	require.NoError(t, err)
	require.Empty(t, res.FailedStep, res.Output)
	assert.Equal(t, "reused", res.Baseline)

	// A process that restarted brings the agent phase up from the config's
	// ecosystems and verifies with the toolchain the prepare installed.
	restarted := New(rt, claudecode.New())
	restarted.labels = sandboxtest.Labels()
	c.Ecosystems = res.Ecosystems

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()

		assert.NoError(t, restarted.Release(ctx, refOf(c), true))
	})

	require.NoError(t, restarted.ensureUp(t.Context(), c))

	vr, err := runVerify(t.Context(), restarted.state(c.RunID), c)
	require.NoError(t, err)
	assert.Equal(t, engine.ResultPassed, vr.Result, vr.Output)

	t.Logf("%s: cold prepare %s, warm prepare %s", rt.Kind, cold.Round(time.Millisecond), warm.Round(time.Millisecond))
}

func runIn(t *testing.T, c container, argv ...string) sandbox.Result {
	t.Helper()

	res, err := c.Run(t.Context(), proc.Command{Argv: argv, Env: agentEnv([]string{"go"}), Dir: workspace}, time.Minute)
	require.NoError(t, err)

	return res
}

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

// removeInstanceAfter removes every container and volume of the instance
// after the test; the instance id is random, so they are this test's own.
func removeInstanceAfter(t *testing.T, r sandbox.Runtime, instance string) {
	t.Helper()

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()

		containers, volumes, err := r.List(ctx, "cavu.instance="+instance)
		if assert.NoError(t, err) {
			assert.NoError(t, r.Remove(ctx, containers...))
			assert.NoError(t, r.VolumeRemove(ctx, volumes...))
		}
	})
}
