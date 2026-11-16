//go:build integration

package sandbox_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/cavu/internal/driver/claudecode"
	"github.com/mhersson/cavu/internal/proc"
	"github.com/mhersson/cavu/internal/sandbox"
	"github.com/mhersson/cavu/internal/testkit/sandboxtest"
)

func TestMain(m *testing.M) { sandboxtest.Main(m) }

func sandboxImage(t *testing.T, r sandbox.Runtime, ca []byte) string {
	t.Helper()

	return sandboxtest.Image(t, r, claudecode.Version, claudecode.SHA512, ca)
}

func removeAfter(t *testing.T, fn func(ctx context.Context) error) {
	t.Helper()

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()

		assert.NoError(t, fn(ctx))
	})
}

func run(t *testing.T, c sandbox.Container, argv ...string) sandbox.Result {
	t.Helper()

	res, err := c.Run(t.Context(), proc.Command{Argv: argv}, 30*time.Second)
	require.NoError(t, err)
	require.False(t, res.TimedOut)

	return res
}

func startContainer(t *testing.T, r sandbox.Runtime, image string, mounts []sandbox.Mount) sandbox.Container {
	t.Helper()

	name := sandboxtest.UniqueName()

	removeAfter(t, func(ctx context.Context) error { return r.Remove(ctx, name) })
	require.NoError(t, r.Start(t.Context(), sandbox.ContainerSpec{
		Name:     name,
		Image:    image,
		Argv:     []string{"sleep", "3600"},
		Labels:   sandboxtest.Labels(),
		Mounts:   mounts,
		Tmpfs:    []string{"/run/cavu"},
		Network:  "none",
		Hardened: true,
		MemoryMB: 1024,
	}))

	return r.Container(name)
}

// TestImageRebuildIsCached rebuilds an image with unchanged inputs and wants it
// from the layer cache, in seconds; every integration test package builds the
// image, so a cache miss would make each of them pay for the downloads.
func TestImageRebuildIsCached(t *testing.T) {
	r := sandboxtest.Runtime(t)
	sandboxImage(t, r, nil)

	o := sandbox.BuildOptions{
		CavuBinary:   sandboxtest.CavuBinary(t),
		CavuVersion:  "test-" + sandboxtest.RunID + "-cached",
		AgentVersion: claudecode.Version,
		AgentSHA512:  claudecode.SHA512,
	}
	name := sandbox.ImageName(o.CavuVersion)

	removeAfter(t, func(ctx context.Context) error {
		return exec.CommandContext(ctx, r.Bin, "image", "rm", "--no-prune", name).Run() //nolint:gosec // our own image, by name
	})

	started := time.Now()

	require.NoError(t, r.BuildImage(t.Context(), o, io.Discard))

	took := time.Since(started)
	t.Logf("cached rebuild: %s", took.Round(time.Millisecond))
	assert.Less(t, took, 30*time.Second)
}

func TestImageTrustsCABundle(t *testing.T) {
	r := sandboxtest.Runtime(t)
	caPEM, _, _ := sandboxtest.CA(t)
	sha := strings.Repeat("ab", 20)

	mux := http.NewServeMux()
	mux.HandleFunc("/repo.git/info/refs", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintf(w, "%s\trefs/heads/main\n", sha)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "ok") })

	port := sandboxtest.StartTLSServer(t, sandboxtest.HostAddr(t, r), mux)
	url := "https://host.docker.internal:" + port

	trusting := sandboxImage(t, r, caPEM)
	plain := sandboxImage(t, r, nil)

	out, err := sandboxtest.RunInImage(t, r, trusting, "curl", "-fsS", url+"/")
	require.NoError(t, err, out)
	assert.Equal(t, "ok", out)

	out, err = sandboxtest.RunInImage(t, r, trusting, "git", "ls-remote", url+"/repo.git")
	require.NoError(t, err, out)
	assert.Contains(t, out, sha)

	// Every certificate of a bundle of several ends up in the system bundle.
	second, _, _ := sandboxtest.CAFromSeed(t, "cavu-test-ca-seed-second-0123456")
	multi := sandboxImage(t, r, append(append([]byte{}, caPEM...), second...))

	for _, c := range [][]byte{caPEM, second} {
		out, err = sandboxtest.RunInImage(t, r, multi, "grep", "-cF", sandboxtest.SignatureLine(c), "/etc/ssl/certs/ca-certificates.crt")
		require.NoError(t, err, out)
		assert.Equal(t, "1", strings.TrimSpace(out))
	}

	t.Run("without the bundle", func(t *testing.T) {
		out, err := sandboxtest.RunInImage(t, r, plain, "curl", "-fsS", url+"/")
		require.Error(t, err, out)

		out, err = sandboxtest.RunInImage(t, r, plain, "git", "ls-remote", url+"/repo.git")
		require.Error(t, err, out)
	})
}

func TestImageBuilds(t *testing.T) {
	r := sandboxtest.Runtime(t)
	caPEM, _, _ := sandboxtest.CA(t)
	image := sandboxImage(t, r, caPEM)

	sum := sha256.Sum256(caPEM)

	cv, av, found, err := r.ImageInfo(t.Context(), image)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, strings.TrimPrefix(image, "cavu-sandbox:"), cv)
	assert.Equal(t, claudecode.Version, av)

	label, err := exec.CommandContext(t.Context(), r.Bin, "image", "inspect", `--format={{index .Config.Labels "cavu.ca_bundle"}}`, image).Output() //nolint:gosec // our own image
	require.NoError(t, err)
	assert.Equal(t, hex.EncodeToString(sum[:]), strings.TrimSpace(string(label)))

	_, _, found, err = r.ImageInfo(t.Context(), "cavu-sandbox:absent-"+sandboxtest.RunID)
	require.NoError(t, err)
	assert.False(t, found)

	paths := []string{"/workspace", "/home/agent", "/handoff", "/cache", "/mirror"}
	mounts := make([]sandbox.Mount, len(paths))

	for i, p := range paths {
		vol := sandboxtest.UniqueName()

		removeAfter(t, func(ctx context.Context) error { return r.VolumeRemove(ctx, vol) })
		require.NoError(t, r.VolumeCreate(t.Context(), vol, sandboxtest.Labels()))

		mounts[i] = sandbox.Mount{Volume: vol, Path: p}
	}

	c := startContainer(t, r, image, mounts)

	assert.Contains(t, run(t, c, "claude", "--version").Output, claudecode.Version)

	for _, tool := range [][]string{{"git", "--version"}, {"mise", "--version"}, {"curl", "--version"}, {"cavu", "version"}} {
		assert.Equal(t, 0, run(t, c, tool...).ExitCode, tool[0])
	}

	assert.Equal(t, "1000", strings.TrimSpace(run(t, c, "id", "-u").Output))

	for _, p := range paths {
		assert.Equal(t, "1000", strings.TrimSpace(run(t, c, "stat", "-c", "%u", p).Output), p)
		assert.Equal(t, 0, run(t, c, "touch", p+"/probe").ExitCode, "%s is not writable by uid 1000", p)
	}

	// Without a volume, the image's own mount points are owned by uid 1000.
	bare := startContainer(t, r, image, nil)

	for _, p := range paths {
		assert.Equal(t, "1000", strings.TrimSpace(run(t, bare, "stat", "-c", "%u", p).Output), p)
	}

	// Copy-out relies on the image's tar not following links.
	var sink strings.Builder

	require.Error(t, bare.CopyOut(t.Context(), "/etc/os-release", &sink))
}

func TestImageCarriesSandboxMarker(t *testing.T) {
	r := sandboxtest.Runtime(t)
	image := sandboxImage(t, r, nil)

	out, err := sandboxtest.RunInImage(t, r, image, "sh", "-c", proc.SandboxOnly("echo payload-ran"))
	require.NoError(t, err, out)
	assert.Contains(t, out, "payload-ran")

	// The rm and the test share one container, so the test sees what the rm left.
	out, err = sandboxtest.RunInImage(t, r, image, "sh", "-c", `rm -f "$1" 2>/dev/null; test -f "$1"`, "sh", proc.SandboxMarker)
	require.NoError(t, err, out)
}
