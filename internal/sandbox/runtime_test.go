package sandbox

import (
	"context"
	"fmt"
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
// then prints the canned stdout (the lines given for its exact argv in
// "outs", then "stdout") and stderr, and exits with the canned code (the one
// given for its exact argv in "exits", else the one in "exit").
// "sleep" holds a call whose last argument it names for 30 seconds before
// any output; "linger" keeps stdout open for 30 seconds after it. It finds
// its own tools when a test narrows PATH to fakes only.
const fakeScript = `#!/bin/sh
PATH=/usr/bin:/bin:$PATH
d='@DIR@'
n=$(mktemp -d "$d/call.XXXXXX") || exit 99
for a in "$@"; do printf '%s\000' "$a"; done > "$n/argv"
awk 'BEGIN { for (k in ENVIRON) print k }' > "$n/env"
printf '%s' "$HOME" > "$n/home"
if { true <&3; } 2>/dev/null; then cat <&3 > "$n/fd3"; fi
cat > "$n/stdin"
for last; do :; done
[ -f "$d/sleep" ] && [ "$(cat "$d/sleep")" = "$last" ] && sleep 30
tab=$(printf '\t')
if [ -f "$d/outs" ]; then
	while IFS=$tab read -r k v; do [ "$k" = "$*" ] && printf '%s\n' "$v"; done < "$d/outs"
fi
[ -f "$d/stdout" ] && cat "$d/stdout"
[ -f "$d/stderr" ] && cat "$d/stderr" >&2
[ -f "$d/linger" ] && sleep 30
code=$(cat "$d/exit" 2>/dev/null || echo 0)
if [ -f "$d/exits" ]; then
	while IFS=$tab read -r k v; do [ "$k" = "$*" ] && code=$v; done < "$d/exits"
fi
exit "$code"
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

// fakeRuntime writes the fake client as dir/name and returns it as a Runtime
// of that kind.
func fakeRuntime(t *testing.T, name string) (Runtime, *fake) {
	t.Helper()

	dir := t.TempDir()
	bin := filepath.Join(dir, name)
	logs := filepath.Join(dir, "logs")
	require.NoError(t, os.Mkdir(logs, 0o700))
	require.NoError(t, os.WriteFile(bin, []byte(strings.ReplaceAll(fakeScript, "@DIR@", logs)), 0o700))

	return Runtime{Bin: bin, Kind: name}, &fake{dir: logs}
}

func (f *fake) set(t *testing.T, file string, content []byte) {
	t.Helper()

	require.NoError(t, os.WriteFile(filepath.Join(f.dir, file), content, 0o600))
}

// outFor makes the call with exactly argv print line on stdout.
func (f *fake) outFor(t *testing.T, line string, argv ...string) {
	t.Helper()

	f.appendRule(t, "outs", strings.Join(argv, " ")+"\t"+line)
}

// exitFor makes the call with exactly argv exit with code.
func (f *fake) exitFor(t *testing.T, code int, argv ...string) {
	t.Helper()

	f.appendRule(t, "exits", strings.Join(argv, " ")+"\t"+strconv.Itoa(code))
}

func (f *fake) appendRule(t *testing.T, file, rule string) {
	t.Helper()

	fh, err := os.OpenFile(filepath.Join(f.dir, file), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	require.NoError(t, err)

	_, err = fmt.Fprintln(fh, rule)
	require.NoError(t, err)
	require.NoError(t, fh.Close())
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
		"value not UTF-8":      "A=" + secret + "\xff",
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
			assert.NotContains(t, err.Error(), secret)

			_, err = r.Container("cavu-agent-r1").Start(t.Context(), proc.Command{Argv: []string{"true"}, Env: env})
			require.Error(t, err)
			assert.NotContains(t, err.Error(), secret)

			_, err = r.Container("cavu-agent-r1").Run(t.Context(), proc.Command{Argv: []string{"true"}, Env: env}, time.Minute)
			require.Error(t, err)
			assert.NotContains(t, err.Error(), secret)

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

const (
	dockerVersion = "Docker version 29.9.3, build 5457199aa5"
	podmanVersion = "podman version 6.2.0"
	shimNotice    = "Emulate Docker CLI using podman. Create /etc/containers/nodocker to quiet msg."
)

var (
	askVersion = []string{"--version"}
	askRemote  = []string{"info", "--format={{.Host.ServiceIsRemote}}"}
)

// onPath puts the fakes' directories, and nothing else, on PATH.
func onPath(t *testing.T, fakes ...Runtime) {
	t.Helper()

	dirs := make([]string, 0, len(fakes))
	for _, r := range fakes {
		dirs = append(dirs, filepath.Dir(r.Bin))
	}

	t.Setenv("PATH", strings.Join(dirs, string(os.PathListSeparator)))
}

func argvs(t *testing.T, f *fake) [][]string {
	t.Helper()

	var out [][]string
	for _, c := range f.calls(t) {
		out = append(out, c.argv)
	}

	return out
}

func TestDetect(t *testing.T) {
	for answer, remote := range map[string]bool{"false": false, "true": true} {
		t.Run("only podman on PATH, remote "+answer, func(t *testing.T) {
			podman, f := fakeRuntime(t, "podman")
			f.outFor(t, podmanVersion, askVersion...)
			f.outFor(t, answer, askRemote...)
			onPath(t, podman)

			r, err := Detect(t.Context(), "")
			require.NoError(t, err)
			assert.Equal(t, Runtime{Bin: podman.Bin, Kind: "podman", Remote: remote}, r)
			assert.ElementsMatch(t, [][]string{askVersion, askRemote}, argvs(t, f), "asked for its version, and once whether it is remote")
		})
	}

	t.Run("docker printing Docker's version", func(t *testing.T) {
		docker, fd := fakeRuntime(t, "docker")
		fd.outFor(t, dockerVersion, askVersion...)
		podman, fp := fakeRuntime(t, "podman")
		onPath(t, docker, podman)

		r, err := Detect(t.Context(), "")
		require.NoError(t, err)
		assert.Equal(t, Runtime{Bin: docker.Bin, Kind: "docker"}, r)
		assert.Equal(t, [][]string{askVersion}, argvs(t, fd))
		assert.Empty(t, fp.calls(t))
	})

	// A shim script prints Podman's line with a notice on stderr; a symlink
	// makes Podman name itself after the link.
	for name, line := range map[string]string{"shim": podmanVersion, "symlink": "docker version 6.2.0"} {
		t.Run("docker that is Podman, as a "+name, func(t *testing.T) {
			docker, f := fakeRuntime(t, "docker")
			f.outFor(t, line, askVersion...)
			f.outFor(t, "false", askRemote...)
			f.set(t, "stderr", []byte(shimNotice+"\n"))
			onPath(t, docker)

			r, err := Detect(t.Context(), "")
			require.NoError(t, err)
			assert.Equal(t, Runtime{Bin: docker.Bin, Kind: "podman"}, r)

			require.NoError(t, r.Start(t.Context(), goodSpec()))

			var run []string
			for _, c := range f.calls(t) {
				if c.argv[0] == "run" {
					run = c.argv
				}
			}

			assert.Subset(t, run, []string{"--http-proxy=false", "--env-host=false"})
			assert.NotContains(t, run, "--env=HTTP_PROXY=")
		})
	}

	for name, out := range map[string]string{
		"another CLI":      "nerdctl version 1.7.6",
		"no version":       "",
		"Docker, no build": "Docker version 29.9.3",
		"long output":      strings.Repeat("x", 10000),
	} {
		t.Run("unknown version output: "+name, func(t *testing.T) {
			docker, f := fakeRuntime(t, "docker")
			f.outFor(t, out, askVersion...)
			onPath(t, docker)

			_, err := Detect(t.Context(), "")
			require.Error(t, err)
			assert.Contains(t, err.Error(), docker.Bin)
			assert.Less(t, len(err.Error()), len(docker.Bin)+300, "the error quotes at most an excerpt")
		})
	}

	t.Run("a version call that fails", func(t *testing.T) {
		docker, f := fakeRuntime(t, "docker")
		f.exitFor(t, 1, askVersion...)
		onPath(t, docker)

		_, err := Detect(t.Context(), "")
		require.Error(t, err)
	})

	t.Run("podman that does not answer whether it is remote", func(t *testing.T) {
		podman, f := fakeRuntime(t, "podman")
		f.outFor(t, podmanVersion, askVersion...)
		f.exitFor(t, 125, askRemote...)
		onPath(t, podman)

		_, err := Detect(t.Context(), "")
		require.Error(t, err)
	})

	t.Run("a preference that is not on PATH", func(t *testing.T) {
		podman, _ := fakeRuntime(t, "podman")
		onPath(t, podman)

		_, err := Detect(t.Context(), "docker")
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

func TestUnknownKindIsRefused(t *testing.T) {
	for _, kind := range []string{"", "nerdctl"} {
		t.Run(strconv.Quote(kind), func(t *testing.T) {
			r, f := fakeRuntime(t, "docker")
			r.Kind = kind

			require.Error(t, r.Start(t.Context(), goodSpec()))

			_, err := r.StartAttached(t.Context(), goodSpec())
			require.Error(t, err)
			require.Error(t, r.VolumeCreate(t.Context(), "v1", nil))
			require.Error(t, r.VolumeRemove(t.Context(), "v1"))
			assert.Empty(t, f.calls(t), "the runtime was called")
		})
	}
}

// clientProxyNames spells the ten names out, so the test does not trust the
// package's own list.
var clientProxyNames = []string{
	"HTTP_PROXY", "HTTPS_PROXY", "FTP_PROXY", "NO_PROXY", "ALL_PROXY",
	"http_proxy", "https_proxy", "ftp_proxy", "no_proxy", "all_proxy",
}

func TestRunNeutralisesClientConfig(t *testing.T) {
	t.Run("docker empties every proxy variable", func(t *testing.T) {
		r, f := fakeRuntime(t, "docker")
		require.NoError(t, r.Start(t.Context(), goodSpec()))

		argv := f.only(t).argv
		for _, n := range clientProxyNames {
			assert.Contains(t, argv, "--env="+n+"=")
		}

		assert.NotContains(t, argv, "--env-host=false")
	})

	for _, remote := range []bool{false, true} {
		t.Run("podman remote "+strconv.FormatBool(remote), func(t *testing.T) {
			r, f := fakeRuntime(t, "podman")
			r.Remote = remote
			require.NoError(t, r.Start(t.Context(), goodSpec()))

			argv := f.only(t).argv
			assert.Contains(t, argv, "--http-proxy=false")
			assert.Equal(t, !remote, slices.Contains(argv, "--env-host=false"))

			for _, a := range argv {
				assert.False(t, strings.HasPrefix(a, "--env="), a)
			}
		})
	}

	for _, n := range append(slices.Clone(clientProxyNames), "Http_Proxy") {
		t.Run("refuses "+n, func(t *testing.T) {
			for _, bin := range []string{"docker", "podman"} {
				r, f := fakeRuntime(t, bin)
				s := goodSpec()
				s.Env = []string{n + "=http://proxy.invalid:3128"}

				err := r.Start(t.Context(), s)
				require.Error(t, err)
				assert.NotContains(t, err.Error(), "proxy.invalid")

				_, err = r.StartAttached(t.Context(), s)
				require.Error(t, err)
				assert.Empty(t, f.calls(t), "the runtime was called")
			}
		})
	}

	t.Run("an exec may carry a proxy", func(t *testing.T) {
		r, f := fakeRuntime(t, "docker")

		p, err := r.Container("cavu-agent-r1").Start(t.Context(), proc.Command{Argv: []string{"true"}, Env: []string{"HTTPS_PROXY=http://proxy.invalid:3128"}})
		require.NoError(t, err)
		require.NoError(t, p.Stdin().Close())

		_, err = p.Wait()
		require.NoError(t, err)
		assert.Equal(t, "HTTPS_PROXY=http://proxy.invalid:3128\n", string(f.only(t).fd3))
	})
}

func TestInspectParsesNetworkNone(t *testing.T) {
	// Trimmed from `container inspect` of the same spec on each runtime: a
	// named volume, a bind, a --mount tmpfs and a --tmpfs. Docker lists a
	// --tmpfs only under HostConfig.Tmpfs, Podman lists both tmpfs kinds there.
	cases := map[string]struct {
		out  string
		want Info
	}{
		"docker": {
			out: `[{"Id":"89d16867","Name":"/cavu-agent-r1","State":{"Status":"exited","Running":false,"OOMKilled":true,"ExitCode":137},` +
				`"HostConfig":{"NetworkMode":"none","Memory":67108864,"Tmpfs":{"/run/cavu":""}},` +
				`"Config":{"Labels":{"cavu.run":"r1"},"Env":["FROMFILE=1","HTTP_PROXY=from-file","HTTPS_PROXY=","HTTP_PROXY=","PATH=/usr/bin:/bin"]},` +
				`"Mounts":[{"Type":"volume","Name":"cavu-workspace-r1","Source":"/var/lib/docker/volumes/cavu-workspace-r1/_data","Destination":"/workspace","Driver":"local","Mode":"z","RW":true,"Propagation":""},` +
				`{"Type":"bind","Source":"/srv/host","Destination":"/host","Mode":"","RW":false,"Propagation":"rprivate"},` +
				`{"Type":"tmpfs","Source":"","Destination":"/scratch","Mode":"","RW":true,"Propagation":""}]}]`,
			want: Info{
				OOMKilled: true,
				Network:   "none",
				ExitCode:  137,
				Labels:    map[string]string{"cavu.run": "r1"},
				EnvNames:  []string{"FROMFILE", "HTTPS_PROXY", "HTTP_PROXY", "PATH"},
				Mounts: []MountInfo{
					{Type: "bind", Source: "/srv/host", Target: "/host"},
					{Type: "tmpfs", Target: "/run/cavu"},
					{Type: "tmpfs", Target: "/scratch"},
					{Type: "volume", Source: "cavu-workspace-r1", Target: "/workspace"},
				},
			},
		},
		"podman": {
			out: `[{"Id":"89ebe475","Name":"cavu-agent-r1","State":{"OciVersion":"1.2.1","Status":"running","Running":true,"OOMKilled":false,"ExitCode":0},` +
				`"HostConfig":{"NetworkMode":"none","Tmpfs":{"/run/cavu":"rprivate,nosuid,nodev,tmpcopyup","/scratch":"rprivate,nosuid,nodev,tmpcopyup"}},` +
				`"Config":{"Labels":{"cavu.run":"r1"},"Env":["PATH=/usr/bin:/bin","container=podman","FROMFILE=1","HOME=/","HOSTNAME=89ebe4753d56"]},` +
				`"Mounts":[{"Type":"volume","Name":"cavu-workspace-r1","Source":"/home/u/.local/share/containers/storage/volumes/cavu-workspace-r1/_data","Destination":"/workspace","Driver":"local","Mode":"","Options":["nosuid","nodev","rbind"],"RW":true,"Propagation":"rprivate"},` +
				`{"Type":"bind","Source":"/srv/host","Destination":"/host","Driver":"","Mode":"","Options":["nosuid","nodev","rbind"],"RW":false,"Propagation":"rprivate"}]}]`,
			want: Info{
				Running:  true,
				Network:  "none",
				Labels:   map[string]string{"cavu.run": "r1"},
				EnvNames: []string{"FROMFILE", "HOME", "HOSTNAME", "PATH", "container"},
				Mounts: []MountInfo{
					{Type: "bind", Source: "/srv/host", Target: "/host"},
					{Type: "tmpfs", Target: "/run/cavu"},
					{Type: "tmpfs", Target: "/scratch"},
					{Type: "volume", Source: "cavu-workspace-r1", Target: "/workspace"},
				},
			},
		},
		"a tmpfs listed twice": {
			out: `[{"State":{},"HostConfig":{"NetworkMode":"none","Tmpfs":{"/run/cavu":""}},"Config":{},` +
				`"Mounts":[{"Type":"tmpfs","Source":"","Destination":"/run/cavu"}]}]`,
			want: Info{Network: "none", Mounts: []MountInfo{{Type: "tmpfs", Target: "/run/cavu"}}},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r, f := fakeRuntime(t, "docker")
			f.set(t, "stdout", []byte(tc.out))

			info, err := r.Inspect(t.Context(), "cavu-agent-r1")
			require.NoError(t, err)
			assert.Equal(t, tc.want, info)
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
	f.set(t, "sleep", []byte("cavu-agent-r1"))

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

func TestVolumeRemove(t *testing.T) {
	t.Run("docker forces one call", func(t *testing.T) {
		r, f := fakeRuntime(t, "docker")
		require.NoError(t, r.VolumeRemove(t.Context(), "v1", "v2"))
		assert.Equal(t, []string{"volume", "rm", "--force", "v1", "v2"}, f.only(t).argv)
	})

	cases := map[string]struct {
		exits   map[string]int
		wantErr bool
		calls   [][]string
	}{
		"removed": {
			calls: [][]string{{"volume", "rm", "v1"}, {"volume", "rm", "v2"}},
		},
		"absent": {
			exits: map[string]int{"volume rm v1": 1, "volume exists v1": 1},
			calls: [][]string{{"volume", "rm", "v1"}, {"volume", "exists", "v1"}, {"volume", "rm", "v2"}},
		},
		"in use": {
			exits:   map[string]int{"volume rm v1": 2},
			wantErr: true,
			calls:   [][]string{{"volume", "rm", "v1"}, {"volume", "exists", "v1"}},
		},
		"existence unknown": {
			exits:   map[string]int{"volume rm v1": 1, "volume exists v1": 125},
			wantErr: true,
			calls:   [][]string{{"volume", "rm", "v1"}, {"volume", "exists", "v1"}},
		},
	}

	for name, tc := range cases {
		t.Run("podman "+name, func(t *testing.T) {
			r, f := fakeRuntime(t, "podman")
			for argv, code := range tc.exits {
				f.exitFor(t, code, strings.Fields(argv)...)
			}

			err := r.VolumeRemove(t.Context(), "v1", "v2")
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}

			var got [][]string
			for _, c := range f.calls(t) {
				got = append(got, c.argv)
			}

			assert.ElementsMatch(t, tc.calls, got)
		})
	}
}
