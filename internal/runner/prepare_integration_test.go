//go:build integration

package runner

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/cavu/internal/driver/claudecode"
	"github.com/mhersson/cavu/internal/engine"
	"github.com/mhersson/cavu/internal/testkit/sandboxtest"
)

func TestPrepareGoRepository(t *testing.T) {
	if os.Getenv("CAVU_TEST_OFFLINE") == "1" {
		t.Skip("CAVU_TEST_OFFLINE=1: this test downloads a Go toolchain through the allowlist")
	}

	r, c, head := fixtureRun(t, map[string]string{
		"go.mod":   "module example.com/hello\n\ngo " + goVersion + "\n",
		"hello.go": "package hello\n\nfunc Hello() string { return \"hello\" }\n",
		"hello_test.go": "package hello\n\nimport \"testing\"\n\n" +
			"func TestHello(t *testing.T) {\n\tif Hello() != \"hello\" {\n\t\tt.Fatal(Hello())\n\t}\n}\n",
	})
	c.Setup.Command = "go mod download"
	c.Verify.Command, c.Verify.Timeout = "go test ./...", 5*time.Minute

	started := time.Now()
	res, err := r.Prepare(t.Context(), c)
	cold := time.Since(started)

	require.NoError(t, err)
	require.Empty(t, res.FailedStep, res.Output)
	assert.Equal(t, head, res.BaseCommit)
	assert.Equal(t, []string{"go@" + goVersion}, res.Tools)
	assert.Equal(t, []string{"go"}, res.Ecosystems)
	assert.Equal(t, engine.ResultPassed, res.Baseline)

	st := r.state(c.RunID)
	require.NotNil(t, st.agent)

	out := runIn(t, st, "go", "version")
	assert.Equal(t, 0, out.ExitCode, out.Output)
	assert.Contains(t, out.Output, "go"+goVersion+" ")

	out = runIn(t, st, "touch", "/cache/x")
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
	restarted := New(r.rt, claudecode.New())
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

	t.Logf("%s: cold prepare %s, warm prepare %s", r.rt.Kind, cold.Round(time.Millisecond), warm.Round(time.Millisecond))
}
