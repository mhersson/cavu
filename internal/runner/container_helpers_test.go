//go:build integration || ecosystems

package runner

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"math/big"
	"net/http"
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

// goVersion is the toolchain the Go fixtures' go.mod asks for.
const goVersion = "1.25.0"

// fixtureRun serves a repository holding files to the sandbox over TLS and
// returns a runner and a run configuration that clones it under a fresh
// instance, whose containers and volumes are removed after the test.
func fixtureRun(t *testing.T, files map[string]string) (*Runner, engine.RunConfig, string) {
	t.Helper()

	// Under an owner, as on a forge, so the address names a repository there.
	return fixtureRunOn(t, files, func(bare string) (http.Handler, string) {
		return http.StripPrefix("/cavu", gitrepo.Handler(t, bare)), "/cavu/" + filepath.Base(bare)
	})
}

// fixtureRunOn is fixtureRun on the server serve returns for the bare
// repository, with the repository at the path it returns.
func fixtureRunOn(t *testing.T, files map[string]string, serve func(bare string) (h http.Handler, path string)) (*Runner, engine.RunConfig, string) {
	t.Helper()

	gitrepo.Isolate(t)

	rt := sandboxtest.Runtime(t)
	caPEM, _, _ := sandboxtest.CA(t)
	image := sandboxtest.Image(t, rt, claudecode.Version, claudecode.SHA512, caPEM)

	repo := gitrepo.New(t, files)
	head := gitrepo.Git(t, repo, "rev-parse", "HEAD")
	bare := strings.TrimPrefix(gitrepo.Bare(t, repo), "file://")
	h, path := serve(bare)
	port := sandboxtest.StartTLSServer(t, sandboxtest.HostAddr(t, rt), h)

	var c engine.RunConfig

	c.TaskID, c.RunID, c.ProjectID = randomID(t), randomID(t), randomID(t)
	c.InstanceID, c.Image = randomInstance(t), image
	c.Repo.URL = "https://host.docker.internal:" + port + path
	c.Repo.BaseBranch, c.Repo.Branch = "main", "cavu/fixture"
	c.Endpoint.Mode = "subscription"
	c.Identity.Name, c.Identity.Email = "CAVU Test", "cavu@example.com"

	removeInstanceAfter(t, rt, c.InstanceID)

	r := New(rt, claudecode.New(), nil)
	r.labels = sandboxtest.Labels()

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()

		assert.NoError(t, r.Release(ctx, refOf(c), true))
	})

	return r, c, head
}

// runIn runs argv in the run's agent phase with the agent phase's environment.
func runIn(t *testing.T, st *run, argv ...string) sandbox.Result {
	t.Helper()

	cmd := proc.Command{Argv: argv, Env: agentEnv(st.ecosystems, ""), Dir: workspace}

	res, err := st.agent.Run(t.Context(), cmd, 10*time.Minute)
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
