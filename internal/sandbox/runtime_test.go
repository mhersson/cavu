package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/cavu/internal/proc"
)

// fakeScript stands in for the runtime client. Each call records its argv,
// its environment names, its HOME, file descriptor 3 (when open) and stdin,
// then prints the canned stdout and exits with the canned code.
const fakeScript = `#!/bin/sh
d='@DIR@'
n=$(mktemp -d "$d/call.XXXXXX") || exit 99
for a in "$@"; do printf '%s\000' "$a"; done > "$n/argv"
awk 'BEGIN { for (k in ENVIRON) print k }' > "$n/env"
printf '%s' "$HOME" > "$n/home"
if { true <&3; } 2>/dev/null; then cat <&3 > "$n/fd3"; fi
cat > "$n/stdin"
[ -f "$d/sleep" ] && sleep 30
[ -f "$d/stdout" ] && cat "$d/stdout"
exit "$(cat "$d/exit" 2>/dev/null || echo 0)"
`

type fake struct{ dir string }

type fakeCall struct {
	argv   []string
	env    []string
	home   string
	fd3    []byte
	hasFd3 bool
	stdin  []byte
}

// fakeRuntime writes the fake client as dir/name, so a name of "podman"
// makes the Runtime a Podman one.
func fakeRuntime(t *testing.T, name string) (Runtime, *fake) {
	t.Helper()

	dir := t.TempDir()
	bin := filepath.Join(dir, name)
	logs := filepath.Join(dir, "logs")
	require.NoError(t, os.Mkdir(logs, 0o700))
	require.NoError(t, os.WriteFile(bin, []byte(strings.ReplaceAll(fakeScript, "@DIR@", logs)), 0o700))

	return Runtime{Bin: bin}, &fake{dir: logs}
}

func (f *fake) set(t *testing.T, file string, content []byte) {
	t.Helper()

	require.NoError(t, os.WriteFile(filepath.Join(f.dir, file), content, 0o600))
}

func (f *fake) calls(t *testing.T) []fakeCall {
	t.Helper()

	dirs, err := filepath.Glob(filepath.Join(f.dir, "call.*"))
	require.NoError(t, err)

	out := make([]fakeCall, 0, len(dirs))

	for _, d := range dirs {
		read := func(name string) []byte {
			b, err := os.ReadFile(filepath.Join(d, name))
			if os.IsNotExist(err) {
				return nil
			}

			require.NoError(t, err)

			return b
		}

		argv := strings.Split(string(read("argv")), "\x00")
		c := fakeCall{
			argv:  argv[:len(argv)-1],
			env:   strings.Fields(string(read("env"))),
			home:  string(read("home")),
			fd3:   read("fd3"),
			stdin: read("stdin"),
		}
		_, err := os.Stat(filepath.Join(d, "fd3"))
		c.hasFd3 = err == nil
		out = append(out, c)
	}

	return out
}

func (f *fake) only(t *testing.T) fakeCall {
	t.Helper()

	calls := f.calls(t)
	require.Len(t, calls, 1)

	return calls[0]
}

func goodSpec() ContainerSpec {
	return ContainerSpec{
		Name:     "cavu-agent-r1",
		Image:    "cavu-sandbox:dev",
		Argv:     []string{"sleep", "60"},
		Labels:   map[string]string{"cavu.run": "r1", "cavu.instance": "i1"},
		Mounts:   []Mount{{Volume: "cavu-workspace-r1", Path: "/workspace"}, {Volume: "cavu-cache-p", Path: "/cache", ReadOnly: true}},
		Tmpfs:    []string{"/run/cavu"},
		Network:  "none",
		Hardened: true,
		MemoryMB: 512,
	}
}

var hardenedFlags = []string{
	"--cap-drop=ALL",
	"--security-opt=no-new-privileges",
	"--init",
	"--pids-limit=2048",
	"--memory=512m",
	"--user=1000:1000",
}

func TestStartBuildsHardenedArgv(t *testing.T) {
	t.Run("docker", func(t *testing.T) {
		r, f := fakeRuntime(t, "docker")
		require.NoError(t, r.Start(t.Context(), goodSpec()))

		argv := f.only(t).argv
		assert.Equal(t, "run", argv[0])
		assert.Subset(t, argv, hardenedFlags)
		assert.Subset(t, argv, []string{
			"--detach",
			"--name=cavu-agent-r1",
			"--network=none",
			"--label=cavu.run=r1",
			"--label=cavu.instance=i1",
			"--mount=type=volume,source=cavu-workspace-r1,target=/workspace",
			"--mount=type=volume,source=cavu-cache-p,target=/cache,readonly",
			"--tmpfs=/run/cavu",
		})
		assert.NotContains(t, argv, "--http-proxy=false")
		assert.Equal(t, []string{"cavu-sandbox:dev", "sleep", "60"}, argv[len(argv)-3:])
	})

	t.Run("podman", func(t *testing.T) {
		r, f := fakeRuntime(t, "podman")
		require.NoError(t, r.Start(t.Context(), goodSpec()))

		argv := f.only(t).argv
		assert.Contains(t, argv, "--http-proxy=false")
		assert.Subset(t, argv, hardenedFlags)
	})

	t.Run("not hardened", func(t *testing.T) {
		r, f := fakeRuntime(t, "docker")
		s := goodSpec()
		s.Hardened = false
		s.MemoryMB = 0
		s.Network = "bridge"
		require.NoError(t, r.Start(t.Context(), s))

		argv := f.only(t).argv
		assert.Contains(t, argv, "--network=bridge")

		for _, flag := range hardenedFlags {
			assert.NotContains(t, argv, flag)
		}
	})
}

func TestEnvNeverInArgvOrClientEnv(t *testing.T) {
	const secret = "hunter2hunter2"

	env := []string{"SECRET=" + secret, "HOME=/home/agent"}

	starts := map[string]func(r Runtime) error{
		"Start": func(r Runtime) error {
			s := goodSpec()
			s.Env = env

			return r.Start(t.Context(), s)
		},
		"StartAttached": func(r Runtime) error {
			s := goodSpec()
			s.Env = env

			p, err := r.StartAttached(t.Context(), s)
			if err != nil {
				return err
			}

			_ = p.Stdin().Close()
			_, err = p.Wait()

			return err
		},
		"Container.Start": func(r Runtime) error {
			p, err := r.Container("cavu-agent-r1").Start(t.Context(), proc.Command{Argv: []string{"true"}, Env: env})
			if err != nil {
				return err
			}

			_ = p.Stdin().Close()
			_, err = p.Wait()

			return err
		},
	}

	for name, start := range starts {
		t.Run(name, func(t *testing.T) {
			r, f := fakeRuntime(t, "docker")
			require.NoError(t, start(r))

			c := f.only(t)

			for _, a := range c.argv {
				assert.NotContains(t, a, secret)
			}

			assert.NotContains(t, c.env, "SECRET")
			assert.Equal(t, os.Getenv("HOME"), c.home)
			assert.Contains(t, strings.Split(string(c.fd3), "\n"), "SECRET="+secret)
		})
	}
}

func TestEnvRefusesBadNamesAndValues(t *testing.T) {
	const secret = "hunter2"

	cases := map[string]string{
		"name starting with =": "=C:=" + secret,
		"name with a space":    "A B=" + secret,
		"name with a digit":    "1A=" + secret,
		"no =":                 "BARE",
		"value with newline":   "A=" + secret + "\n" + secret,
		"value with CR":        "A=" + secret + "\r",
		"value with NUL":       "A=" + secret + "\x00",
		"line over 65535":      "A=" + secret + strings.Repeat("x", 65536-2-len(secret)),
	}

	for name, entry := range cases {
		t.Run(name, func(t *testing.T) {
			r, f := fakeRuntime(t, "docker")
			env := []string{"GOOD=1", entry}

			s := goodSpec()
			s.Env = env
			err := r.Start(t.Context(), s)
			require.Error(t, err)
			assert.NotContains(t, err.Error(), secret)

			_, err = r.StartAttached(t.Context(), s)
			require.Error(t, err)

			_, err = r.Container("cavu-agent-r1").Start(t.Context(), proc.Command{Argv: []string{"true"}, Env: env})
			require.Error(t, err)

			_, err = r.Container("cavu-agent-r1").Run(t.Context(), proc.Command{Argv: []string{"true"}, Env: env}, time.Minute)
			require.Error(t, err)

			assert.Empty(t, f.calls(t), "the runtime was called")
		})
	}

	t.Run("a line of exactly 65535 bytes is accepted", func(t *testing.T) {
		r, f := fakeRuntime(t, "docker")
		line := "A=" + strings.Repeat("x", 65535-2)

		s := goodSpec()
		s.Env = []string{line}
		require.NoError(t, r.Start(t.Context(), s))
		assert.Equal(t, line+"\n", string(f.only(t).fd3))
	})
}

func TestMountValidation(t *testing.T) {
	withMount := func(m Mount) ContainerSpec {
		s := goodSpec()
		s.Mounts = []Mount{m}

		return s
	}

	badVolumes := []string{"a/b", "a,b", "a=b", ".a", "~a", "-a", "", "x,volume-opt=type=none", "x,volume-opt=device=/etc"}

	cases := map[string]func(r Runtime) error{
		"relative target": func(r Runtime) error {
			return r.Start(t.Context(), withMount(Mount{Volume: "v", Path: "workspace"}))
		},
		"target with a comma": func(r Runtime) error {
			return r.Start(t.Context(), withMount(Mount{Volume: "v", Path: "/w,readonly=false"}))
		},
		"container name starting with -": func(r Runtime) error {
			s := goodSpec()
			s.Name = "--privileged"

			return r.Start(t.Context(), s)
		},
		"image starting with -": func(r Runtime) error {
			s := goodSpec()
			s.Image = "--privileged"

			return r.Start(t.Context(), s)
		},
		"exec into a name starting with -": func(r Runtime) error {
			_, err := r.Container("--user=0").Run(t.Context(), proc.Command{Argv: []string{"id"}}, time.Minute)

			return err
		},
		"inspect a name starting with -": func(r Runtime) error {
			_, err := r.Inspect(t.Context(), "-f")

			return err
		},
		"remove a name starting with -": func(r Runtime) error {
			return r.Remove(t.Context(), "ok", "-f")
		},
	}

	for _, v := range badVolumes {
		cases["mount volume "+strconv.Quote(v)] = func(r Runtime) error {
			return r.Start(t.Context(), withMount(Mount{Volume: v, Path: "/workspace"}))
		}
		cases["create volume "+strconv.Quote(v)] = func(r Runtime) error {
			return r.VolumeCreate(t.Context(), v, nil)
		}
		cases["remove volume "+strconv.Quote(v)] = func(r Runtime) error {
			return r.VolumeRemove(t.Context(), "ok", v)
		}
	}

	for name, run := range cases {
		t.Run(name, func(t *testing.T) {
			r, f := fakeRuntime(t, "docker")
			require.Error(t, run(r))
			assert.Empty(t, f.calls(t), "the runtime was called")
		})
	}
}

func TestStartRefusesBadSpecs(t *testing.T) {
	cases := map[string]func(s *ContainerSpec){
		"empty network":           func(s *ContainerSpec) { s.Network = "" },
		"host network":            func(s *ContainerSpec) { s.Network = "host" },
		"hardened without memory": func(s *ContainerSpec) { s.MemoryMB = 0 },
		"negative memory":         func(s *ContainerSpec) { s.Hardened = false; s.MemoryMB = -1 },
		"relative tmpfs":          func(s *ContainerSpec) { s.Tmpfs = []string{"run/cavu"} },
		"label key with =":        func(s *ContainerSpec) { s.Labels = map[string]string{"a=b": "c"} },
		"empty name":              func(s *ContainerSpec) { s.Name = "" },
		"empty image":             func(s *ContainerSpec) { s.Image = "" },
	}

	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			r, f := fakeRuntime(t, "docker")
			s := goodSpec()
			mutate(&s)

			require.Error(t, r.Start(t.Context(), s))
			assert.Empty(t, f.calls(t), "the runtime was called")
		})
	}
}

func TestDetect(t *testing.T) {
	t.Run("only podman on PATH", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "podman"), []byte("#!/bin/sh\nexit 1\n"), 0o700))
		t.Setenv("PATH", dir)

		r, err := Detect(t.Context(), "")
		require.NoError(t, err)
		assert.Equal(t, filepath.Join(dir, "podman"), r.Bin)

		_, err = Detect(t.Context(), "docker")
		require.Error(t, err)
	})

	t.Run("neither on PATH", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())

		_, err := Detect(t.Context(), "")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "docker")
		assert.Contains(t, err.Error(), "podman")
	})

	t.Run("unknown preference", func(t *testing.T) {
		_, err := Detect(t.Context(), "nerdctl")
		require.Error(t, err)
	})
}

func TestInspectParsesNetworkNone(t *testing.T) {
	// Trimmed from `container inspect` on Docker and on Podman.
	outputs := map[string]string{
		"docker": `[{"Id":"7a00de1e","Name":"/cavu-agent-r1","State":{"Status":"exited","Running":false,"OOMKilled":true,"ExitCode":137},` +
			`"HostConfig":{"NetworkMode":"none","Memory":67108864},"Config":{"Labels":{"cavu.run":"r1"}}}]`,
		"podman": `[{"Id":"0c7a21cb","Name":"cavu-agent-r1","State":{"OciVersion":"1.2.1","Status":"exited","Running":false,"OOMKilled":true,"ExitCode":137},` +
			`"HostConfig":{"NetworkMode":"none"},"Config":{"Labels":{"cavu.run":"r1"}}}]`,
	}

	for name, out := range outputs {
		t.Run(name, func(t *testing.T) {
			r, f := fakeRuntime(t, name)
			f.set(t, "stdout", []byte(out))

			info, err := r.Inspect(t.Context(), "cavu-agent-r1")
			require.NoError(t, err)
			assert.Equal(t, Info{
				Running:   false,
				OOMKilled: true,
				Network:   "none",
				ExitCode:  137,
				Labels:    map[string]string{"cavu.run": "r1"},
			}, info)
			assert.Contains(t, f.only(t).argv, "cavu-agent-r1")
		})
	}

	t.Run("no such container", func(t *testing.T) {
		r, f := fakeRuntime(t, "docker")
		f.set(t, "stdout", []byte("[]"))
		f.set(t, "exit", []byte("1"))

		_, err := r.Inspect(t.Context(), "cavu-agent-r1")
		require.Error(t, err)
	})
}

func TestListParsesNames(t *testing.T) {
	r, f := fakeRuntime(t, "docker")
	f.set(t, "stdout", []byte("b\n\na\n"))

	containers, volumes, err := r.List(t.Context(), "cavu.instance=i1")
	require.NoError(t, err)
	assert.Equal(t, []string{"a", "b"}, containers)
	assert.Equal(t, []string{"a", "b"}, volumes)

	for _, c := range f.calls(t) {
		assert.Contains(t, c.argv, "--filter=label=cavu.instance=i1")
	}
}

func TestClientFailureCarriesStderrNotEnv(t *testing.T) {
	r, f := fakeRuntime(t, "docker")
	f.set(t, "exit", []byte("125"))

	s := goodSpec()
	s.Env = []string{"SECRET=hunter2hunter2"}

	err := r.Start(t.Context(), s)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "hunter2hunter2")
}

func TestCallsHonourContext(t *testing.T) {
	r, f := fakeRuntime(t, "docker")
	f.set(t, "sleep", nil)

	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()

	begun := time.Now()
	_, err := r.Inspect(ctx, "cavu-agent-r1")
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, time.Since(begun), 10*time.Second)
}

func TestFeedIsTheEnvFile(t *testing.T) {
	r, f := fakeRuntime(t, "docker")

	s := goodSpec()
	s.Env = []string{"A=1", "B=two words", "C="}
	require.NoError(t, r.Start(t.Context(), s))

	c := f.only(t)
	assert.Contains(t, c.argv, "--env-file=/dev/fd/3")
	assert.Equal(t, "A=1\nB=two words\nC=\n", string(c.fd3))

	t.Run("no env, no env file", func(t *testing.T) {
		r, f := fakeRuntime(t, "docker")
		require.NoError(t, r.Start(t.Context(), goodSpec()))

		c := f.only(t)
		assert.NotContains(t, c.argv, "--env-file=/dev/fd/3")
		assert.False(t, c.hasFd3)
	})
}

func TestVolumeCreateIgnoresExistingOnPodman(t *testing.T) {
	for _, name := range []string{"docker", "podman"} {
		t.Run(name, func(t *testing.T) {
			r, f := fakeRuntime(t, name)
			require.NoError(t, r.VolumeCreate(t.Context(), "cavu-home-t1", map[string]string{"cavu.task": "t1"}))

			argv := f.only(t).argv
			assert.Equal(t, []string{"volume", "create"}, argv[:2])
			assert.Contains(t, argv, "--label=cavu.task=t1")
			assert.Equal(t, "cavu-home-t1", argv[len(argv)-1])
			assert.Equal(t, name == "podman", slices.Contains(argv, "--ignore"))
		})
	}
}
