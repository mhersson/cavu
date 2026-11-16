//go:build integration

package sandbox

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/cavu/internal/proc"
)

const testImage = "docker.io/library/busybox:1.37"

// testRun labels everything this test binary creates.
var testRun = strings.ToLower(rand.Text()[:12])

func testRuntime(t *testing.T) Runtime {
	t.Helper()

	name := os.Getenv("CAVU_TEST_RUNTIME")
	if name == "" {
		name = "docker"
	}

	r, err := Detect(t.Context(), name)
	require.NoError(t, err)

	return r
}

func uniqueName() string { return "cavu-test-" + strings.ToLower(rand.Text()[:12]) }

func testLabels() map[string]string { return map[string]string{"cavu.test": testRun} }

// cleanup runs fn after the test with a context of its own, because
// t.Context is cancelled before cleanups run.
func cleanup(t *testing.T, fn func(ctx context.Context) error) {
	t.Helper()

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()

		assert.NoError(t, fn(ctx))
	})
}

func createVolume(t *testing.T, r Runtime) string {
	t.Helper()

	name := uniqueName()

	cleanup(t, func(ctx context.Context) error { return r.VolumeRemove(ctx, name) })
	require.NoError(t, r.VolumeCreate(t.Context(), name, testLabels()))

	return name
}

func startContainer(t *testing.T, r Runtime, mutate ...func(*ContainerSpec)) (string, Container) {
	t.Helper()

	s := ContainerSpec{
		Name:     uniqueName(),
		Image:    testImage,
		Argv:     []string{"sleep", "3600"},
		Labels:   testLabels(),
		Tmpfs:    []string{"/run/cavu"},
		Network:  "none",
		Hardened: true,
		MemoryMB: 64,
	}
	for _, m := range mutate {
		m(&s)
	}

	cleanup(t, func(ctx context.Context) error { return r.Remove(ctx, s.Name) })
	require.NoError(t, r.Start(t.Context(), s))

	return s.Name, r.Container(s.Name)
}

func run(t *testing.T, c Container, argv ...string) Result {
	t.Helper()

	res, err := c.Run(t.Context(), proc.Command{Argv: argv}, 30*time.Second)
	require.NoError(t, err)
	require.False(t, res.TimedOut)

	return res
}

func requireNoProcess(t *testing.T, c Container, args string) {
	t.Helper()

	require.Eventually(t, func() bool {
		for line := range strings.Lines(run(t, c, "ps", "-o", "args").Output) {
			if strings.TrimSpace(line) == args {
				return false
			}
		}

		return true
	}, 3*time.Second, 100*time.Millisecond, "%q is still running", args)
}

// TestDetectPodmanBehindDocker puts a docker that is the real Podman first on
// PATH, as the podman-docker shim script and as a symlink.
func TestDetectPodmanBehindDocker(t *testing.T) {
	podman, err := exec.LookPath("podman")
	if err != nil {
		t.Skip("podman is not installed")
	}

	shim := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(shim, "docker"),
		[]byte("#!/bin/sh\necho 'Emulate Docker CLI using podman.' >&2\nexec '"+podman+"' \"$@\"\n"), 0o700))

	link := t.TempDir()
	require.NoError(t, os.Symlink(podman, filepath.Join(link, "docker")))

	for name, dir := range map[string]string{"shim": shim, "symlink": link} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

			r, err := Detect(t.Context(), "")
			require.NoError(t, err)
			assert.Equal(t, filepath.Join(dir, "docker"), r.Bin)
			assert.Equal(t, "podman", r.Kind)
		})
	}
}

func TestExecStreamsLines(t *testing.T) {
	t.Parallel()

	r := testRuntime(t)
	_, c := startContainer(t, r)

	p, err := c.Start(t.Context(), proc.Command{Argv: []string{"cat"}})
	require.NoError(t, err)

	for _, line := range []string{"one", "two", "three"} {
		_, err := io.WriteString(p.Stdin(), line+"\n")
		require.NoError(t, err)

		select {
		case got := <-p.Lines():
			assert.Equal(t, line, string(got))
		case <-time.After(10 * time.Second):
			require.FailNow(t, "no echo", line)
		}
	}

	require.NoError(t, p.Stdin().Close())

	code, err := p.Wait()
	require.NoError(t, err)
	assert.Equal(t, 0, code)
}

func TestEnvRoundTrip(t *testing.T) {
	t.Parallel()

	values := map[string]string{
		"SPACES":    "  two  words  ",
		"QUOTES":    `"double" 'single'`,
		"HASH":      "# not a comment",
		"EQUALS":    "a=b=c",
		"DOLLAR":    "$HOME ${PATH} $(id)",
		"BACKSLASH": `back\slash\n`,
		"TAB":       "a\tb",
		"UNICODE":   "æøå ✓ 😀",
		"EMPTY":     "",
		"LONG":      strings.Repeat("x", 65535-len("LONG=")),
	}

	var env []string
	for k, v := range values {
		env = append(env, k+"="+v)
	}

	parse := func(environ string) map[string]string {
		got := map[string]string{}

		for kv := range strings.SplitSeq(strings.TrimSuffix(environ, "\x00"), "\x00") {
			k, v, _ := strings.Cut(kv, "=")
			got[k] = v
		}

		return got
	}

	r := testRuntime(t)

	t.Run("exec", func(t *testing.T) {
		t.Parallel()

		_, c := startContainer(t, r)

		var out bytes.Buffer

		p, err := c.Start(t.Context(), proc.Command{Argv: []string{"cat", "/proc/self/environ"}, Env: env, Stdout: &out})
		require.NoError(t, err)
		require.NoError(t, p.Stdin().Close())

		code, err := p.Wait()
		require.NoError(t, err)
		require.Equal(t, 0, code, p.StderrTail())

		got := parse(out.String())
		for k, v := range values {
			assert.Equal(t, v, got[k], k)
		}
	})

	t.Run("run", func(t *testing.T) {
		t.Parallel()

		s := ContainerSpec{
			Name:     uniqueName(),
			Image:    testImage,
			Argv:     []string{"cat", "/proc/self/environ"},
			Env:      env,
			Labels:   testLabels(),
			Network:  "none",
			Hardened: true,
			MemoryMB: 64,
		}

		cleanup(t, func(ctx context.Context) error { return r.Remove(ctx, s.Name) })

		p, err := r.StartAttached(t.Context(), s)
		require.NoError(t, err)
		require.NoError(t, p.Stdin().Close())

		var lines []string
		for l := range p.Lines() {
			lines = append(lines, string(l))
		}

		code, err := p.Wait()
		require.NoError(t, err)
		require.Equal(t, 0, code, p.StderrTail())

		got := parse(strings.Join(lines, "\n"))
		for k, v := range values {
			assert.Equal(t, v, got[k], k)
		}
	})
}

func TestRunKillsAtTimeout(t *testing.T) {
	t.Parallel()

	r := testRuntime(t)
	_, c := startContainer(t, r)

	begun := time.Now()
	res, err := c.Run(t.Context(), proc.Command{Argv: []string{"sh", "-c", "sleep 300 & sleep 300"}}, time.Second)
	require.NoError(t, err)
	assert.True(t, res.TimedOut)
	assert.Less(t, time.Since(begun), 5*time.Second)

	requireNoProcess(t, c, "sleep 300")
}

func TestStopBeforePidFile(t *testing.T) {
	t.Parallel()

	r := testRuntime(t)
	_, c := startContainer(t, r)

	p, err := c.Start(t.Context(), proc.Command{Argv: []string{"sleep", "300"}})
	require.NoError(t, err)
	require.NoError(t, p.Stop(5*time.Second))

	_, err = p.Wait()
	require.NoError(t, err)

	requireNoProcess(t, c, "sleep 300")
}

func TestRunCapsOutput(t *testing.T) {
	t.Parallel()

	r := testRuntime(t)
	_, c := startContainer(t, r)

	t.Run("many lines", func(t *testing.T) {
		res := run(t, c, "sh", "-c", "yes | head -c 5000000")
		assert.Equal(t, 0, res.ExitCode)
		assert.LessOrEqual(t, len(res.Output), proc.CaptureCap)
		assert.True(t, strings.HasPrefix(res.Output, "y\n"))
		assert.True(t, strings.HasSuffix(res.Output, "\ny\n"))
	})

	t.Run("stderr only", func(t *testing.T) {
		res := run(t, c, "sh", "-c", "echo to-stderr >&2")
		assert.Equal(t, "to-stderr\n", res.Output)
	})

	t.Run("no newline at all", func(t *testing.T) {
		res := run(t, c, "sh", "-c", `head -c 3145728 /dev/zero | tr '\0' x`)
		assert.Equal(t, 0, res.ExitCode)
		assert.LessOrEqual(t, len(res.Output), proc.CaptureCap)
		assert.True(t, strings.HasPrefix(res.Output, "xxx"))
		assert.True(t, strings.HasSuffix(res.Output, "xxx"))
	})
}

func TestCopyRoundTrip(t *testing.T) {
	t.Parallel()

	r := testRuntime(t)
	vol := createVolume(t, r)
	// A fresh volume takes the mode of the image's directory it covers, and
	// /tmp is world-writable, so the hardened user can write to the volume.
	_, c := startContainer(t, r, func(s *ContainerSpec) { s.Mounts = []Mount{{Volume: vol, Path: "/tmp"}} })

	content := []byte{0, 'b', 'i', 'n', 0xff, '\n', 0}

	for _, path := range []string{"/tmp/blob", "/run/cavu/blob"} {
		require.NoError(t, c.CopyIn(t.Context(), map[string][]byte{path: content}))
		assert.Equal(t, "600 1000\n", run(t, c, "stat", "-c", "%a %u", path).Output)

		var out bytes.Buffer
		require.NoError(t, c.CopyOut(t.Context(), path, &out))
		assert.Equal(t, content, out.Bytes())
	}

	t.Run("files written before the command", func(t *testing.T) {
		var out bytes.Buffer

		p, err := c.Start(t.Context(), proc.Command{
			Argv:   []string{"cat", "/run/cavu/input"},
			Files:  map[string][]byte{"/run/cavu/input": content},
			Stdout: &out,
		})
		require.NoError(t, err)
		require.NoError(t, p.Stdin().Close())

		code, err := p.Wait()
		require.NoError(t, err)
		require.Equal(t, 0, code)
		assert.Equal(t, content, out.Bytes())
	})

	t.Run("only a regular file comes out", func(t *testing.T) {
		require.Equal(t, 0, run(t, c, "ln", "-s", "/run/cavu/blob", "/run/cavu/link").ExitCode)

		for _, path := range []string{"/run/cavu", "/run/cavu/link", "/run/cavu/missing"} {
			assert.Error(t, c.CopyOut(t.Context(), path, io.Discard), path)
		}
	})
}

func TestStopAttachedStopsContainer(t *testing.T) {
	t.Parallel()

	r := testRuntime(t)
	s := ContainerSpec{
		Name:     uniqueName(),
		Image:    testImage,
		Argv:     []string{"sleep", "300"},
		Labels:   testLabels(),
		Network:  "none",
		Hardened: true,
		MemoryMB: 64,
		Stdin:    true,
	}

	cleanup(t, func(ctx context.Context) error { return r.Remove(ctx, s.Name) })

	p, err := r.StartAttached(t.Context(), s)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		info, err := r.Inspect(t.Context(), s.Name)

		return err == nil && info.Running
	}, 30*time.Second, 100*time.Millisecond)

	require.NoError(t, p.Stop(10*time.Second))

	info, err := r.Inspect(t.Context(), s.Name)
	require.NoError(t, err)
	assert.False(t, info.Running)
}

func TestCommandNeedsPidDir(t *testing.T) {
	t.Parallel()

	r := testRuntime(t)
	_, c := startContainer(t, r, func(s *ContainerSpec) { s.Tmpfs = nil })

	res := run(t, c, "touch", "/tmp/ran")
	assert.NotEqual(t, 0, res.ExitCode)
	assert.NotEmpty(t, res.Output)
	assert.Error(t, c.CopyOut(t.Context(), "/tmp/ran", io.Discard), "the command ran")
}

func TestRunOnRemovedContainer(t *testing.T) {
	t.Parallel()

	r := testRuntime(t)
	name, c := startContainer(t, r)
	require.NoError(t, r.Remove(t.Context(), name))

	res := run(t, c, "true")
	assert.NotEqual(t, 0, res.ExitCode)
	assert.NotEmpty(t, res.Output)
}

func TestNoNetwork(t *testing.T) {
	t.Parallel()

	r := testRuntime(t)
	name, c := startContainer(t, r)

	assert.NotEqual(t, 0, run(t, c, "nslookup", "example.com").ExitCode)
	assert.NotEqual(t, 0, run(t, c, "wget", "-q", "-T", "2", "-O", "/dev/null", "http://1.1.1.1/").ExitCode)

	info, err := r.Inspect(t.Context(), name)
	require.NoError(t, err)
	assert.Equal(t, "none", info.Network)
	assert.True(t, info.Running)
	assert.Equal(t, testRun, info.Labels["cavu.test"])
}

// environ parses a NUL-separated environment.
func environ(raw string) map[string]string {
	env := map[string]string{}

	for kv := range strings.SplitSeq(strings.TrimSuffix(raw, "\x00"), "\x00") {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env[k] = v
		}
	}

	return env
}

// controlEnv is the little a client needs, so a control run that copies its
// client's environment into a container copies none of the operator's.
func controlEnv() []string {
	keep := []string{
		"PATH", "HOME", "USER", "XDG_RUNTIME_DIR", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "DBUS_SESSION_BUS_ADDRESS",
		"DOCKER_CONFIG", "CONTAINERS_CONF_OVERRIDE", "CAVU_TEST_CANARY", "HTTP_PROXY", "HTTPS_PROXY",
	}

	var env []string

	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); slices.Contains(keep, k) {
			env = append(env, kv)
		}
	}

	return env
}

// TestHostConfigNeverReachesContainer gives the client a scratch Docker
// config with proxies, a scratch containers.conf with env_host, and a canary
// in its own environment, and shows none of it reaches a container.
func TestHostConfigNeverReachesContainer(t *testing.T) {
	const proxyHost = "proxy-canary.invalid"

	canary := "canary-" + strings.ToLower(rand.Text()[:12])
	dir := t.TempDir()
	dockerConfig := filepath.Join(dir, "docker")
	containersConf := filepath.Join(dir, "containers.conf")

	require.NoError(t, os.Mkdir(dockerConfig, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dockerConfig, "config.json"), []byte(`{"proxies":{"default":{`+
		`"httpProxy":"http://`+proxyHost+`:3128","httpsProxy":"http://`+proxyHost+`:3129","ftpProxy":"http://`+proxyHost+`:3130",`+
		`"noProxy":"`+proxyHost+`","allProxy":"socks5://`+proxyHost+`:1080"}}}`), 0o600))
	require.NoError(t, os.WriteFile(containersConf, []byte("[containers]\nenv_host = true\n"), 0o600))

	t.Setenv("DOCKER_CONFIG", dockerConfig)
	t.Setenv("CONTAINERS_CONF_OVERRIDE", containersConf)
	t.Setenv("CAVU_TEST_CANARY", canary)
	t.Setenv("HTTP_PROXY", "http://"+proxyHost+":3128")
	t.Setenv("HTTPS_PROXY", "http://"+proxyHost+":3129")

	r := testRuntime(t)

	// A plain run shows the scratch configuration reaches the runtime, so the
	// assertions below cannot pass for want of anything to keep out.
	control := uniqueName()

	cleanup(t, func(ctx context.Context) error { return r.Remove(ctx, control) })

	var out bytes.Buffer

	p, err := proc.Local{}.Start(t.Context(), proc.Command{
		Argv:   []string{r.Bin, "run", "--rm", "--pull=never", "--network=none", "--name=" + control, "--label=cavu.test=" + testRun, testImage, "env"},
		Env:    controlEnv(),
		Stdout: &out,
	})
	require.NoError(t, err)
	require.NoError(t, p.Stdin().Close())

	if code, err := p.Wait(); err != nil || code != 0 {
		t.Skipf("the runtime cannot be reached with a scratch configuration (exit %d, %v): %s", code, err, p.StderrTail())
	}

	injected := canary
	if r.Kind == "docker" {
		injected = proxyHost
	}

	if !strings.Contains(out.String(), injected) {
		t.Skipf("the scratch configuration did not reach %s: a plain run shows nothing injected", r.Bin)
	}

	vol := createVolume(t, r)
	name, c := startContainer(t, r, func(s *ContainerSpec) {
		s.Mounts = []Mount{{Volume: vol, Path: "/tmp"}}
		s.Env = []string{"MINE=1"}
	})

	mainEnv := environ(run(t, c, "cat", "/proc/1/environ").Output)
	execEnv := environ(run(t, c, "cat", "/proc/self/environ").Output)

	// Failures name variables only: with a leak, the values would be the
	// test process's own environment.
	for label, env := range map[string]map[string]string{"main process": mainEnv, "exec": execEnv} {
		assert.Equal(t, "1", env["MINE"], label)

		_, hasCanary := env["CAVU_TEST_CANARY"]
		assert.False(t, hasCanary, "%s has CAVU_TEST_CANARY", label)

		for k, v := range env {
			if strings.Contains(v, proxyHost) || strings.Contains(v, canary) {
				t.Errorf("%s: %s carries the scratch proxy or the canary", label, k)
			}
		}
	}

	info, err := r.Inspect(t.Context(), name)
	require.NoError(t, err)

	// Inspect invents no name and misses none but the two Docker sets on the
	// process without recording them.
	for _, n := range info.EnvNames {
		_, ok := mainEnv[n]
		assert.True(t, ok, "Inspect lists %s, which the container does not have", n)
	}

	for n := range mainEnv {
		if !slices.Contains(info.EnvNames, n) {
			assert.True(t, n == "HOME" || n == "HOSTNAME", "Inspect misses %s", n)
		}
	}

	assert.Equal(t, []MountInfo{{Type: "tmpfs", Target: "/run/cavu"}, {Type: "volume", Source: vol, Target: "/tmp"}}, info.Mounts)
}

func TestExitCode(t *testing.T) {
	t.Parallel()

	r := testRuntime(t)
	_, c := startContainer(t, r)

	assert.Equal(t, 7, run(t, c, "sh", "-c", "exit 7").ExitCode)

	p, err := c.Start(t.Context(), proc.Command{Argv: []string{"sh", "-c", "exit 7"}})
	require.NoError(t, err)
	require.NoError(t, p.Stdin().Close())

	code, err := p.Wait()
	require.NoError(t, err)
	assert.Equal(t, 7, code)
}

func TestHardenedLimitsApply(t *testing.T) {
	t.Parallel()

	r := testRuntime(t)
	_, c := startContainer(t, r)

	res := run(t, c, "sh", "-c", `id -u; grep -E '^(CapEff|NoNewPrivs):' /proc/self/status | tr -d '\t'; cat /sys/fs/cgroup/pids.max /sys/fs/cgroup/memory.max`)
	assert.Equal(t, "1000\nCapEff:0000000000000000\nNoNewPrivs:1\n2048\n67108864\n", res.Output)
}

func TestVolumesAndListing(t *testing.T) {
	t.Parallel()

	r := testRuntime(t)
	vol := createVolume(t, r)
	require.NoError(t, r.VolumeCreate(t.Context(), vol, testLabels()), "a second create of the same name")

	name, _ := startContainer(t, r, func(s *ContainerSpec) { s.Mounts = []Mount{{Volume: vol, Path: "/tmp"}} })

	require.Error(t, r.VolumeRemove(t.Context(), vol), "removing a volume in use")

	info, err := r.Inspect(t.Context(), name)
	require.NoError(t, err)
	assert.True(t, info.Running, "removing a volume stopped or removed its container")

	containers, volumes, err := r.List(t.Context(), "cavu.test="+testRun)
	require.NoError(t, err)
	assert.Contains(t, containers, name)
	assert.Contains(t, volumes, vol)

	require.NoError(t, r.Remove(t.Context(), uniqueName()), "removing an absent container")
	require.NoError(t, r.VolumeRemove(t.Context(), uniqueName()), "removing an absent volume")

	require.NoError(t, r.Remove(t.Context(), name))
	require.NoError(t, r.VolumeRemove(t.Context(), vol))

	containers, volumes, err = r.List(t.Context(), "cavu.test="+testRun)
	require.NoError(t, err)
	assert.NotContains(t, containers, name)
	assert.NotContains(t, volumes, vol)
}
