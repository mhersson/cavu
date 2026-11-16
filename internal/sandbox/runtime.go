// Package sandbox drives the container runtime client, Docker or Podman: it
// starts, inspects and removes containers and volumes, runs commands in them,
// and moves files in and out as streams on an exec, never through a host path.
package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mhersson/cavu/internal/proc"
)

const (
	// envFileArg names the feed. The client keeps the host's environment,
	// which decides the runtime it talks to, and never sees a container's.
	envFileArg = "--env-file=/dev/fd/3"
	// maxEnvLine is the longest line both clients' env-file scanners accept.
	maxEnvLine = 65535
	// killGrace is the time between TERM and KILL when CAVU stops a client or
	// a command on its own account.
	killGrace = 2 * time.Second
)

var (
	volumeName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)
	envName    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// proxyNames are the variables the Docker client fills in from the proxies
// of its config.json on `run`, overriding an env-file value of the same name.
var proxyNames = []string{
	"HTTP_PROXY", "HTTPS_PROXY", "FTP_PROXY", "NO_PROXY", "ALL_PROXY",
	"http_proxy", "https_proxy", "ftp_proxy", "no_proxy", "all_proxy",
}

// Runtime is a container runtime client binary. Kind is what the binary says
// it is, never what it is called: a docker on PATH can be a shim that runs
// Podman.
type Runtime struct {
	Bin    string // a name on PATH or a path
	Kind   string // "docker" or "podman", from the output of `<bin> --version`
	Remote bool   // a Podman client that talks to a service
}

var (
	dockerVersionLine = regexp.MustCompile(`^Docker version \S+, build \S+$`)
	// Podman names itself after argv[0], so behind a docker symlink it says
	// "docker version ...".
	podmanVersionLine = regexp.MustCompile(`^(\S+) version [0-9]\S*$`)
)

// Detect finds the runtime client on PATH: preferred when set, otherwise
// docker, then podman. It asks the binary for its version, a local call that
// contacts no daemon, to set Kind, and asks a Podman once whether it is
// remote. Output that is neither Docker's nor Podman's is an error.
func Detect(ctx context.Context, preferred string) (Runtime, error) {
	names := []string{"docker", "podman"}

	if preferred != "" {
		if !slices.Contains(names, preferred) {
			return Runtime{}, fmt.Errorf("container runtime %q: want docker or podman", preferred)
		}

		names = []string{preferred}
	}

	for _, n := range names {
		bin, err := exec.LookPath(n)
		if err != nil {
			continue
		}

		r := Runtime{Bin: bin}

		version, err := r.output(ctx, "--version")
		if err != nil {
			return Runtime{}, fmt.Errorf("%s --version: %w", bin, err)
		}

		if r.Kind, err = kindOf(bin, version); err != nil {
			return Runtime{}, err
		}

		if r.Kind == "docker" {
			return r, nil
		}

		out, err := r.output(ctx, "info", "--format={{.Host.ServiceIsRemote}}")
		if err == nil {
			r.Remote, err = strconv.ParseBool(strings.TrimSpace(string(out)))
		}

		if err != nil {
			return Runtime{}, fmt.Errorf("podman found at %s but did not answer: %w", bin, err)
		}

		return r, nil
	}

	return Runtime{}, fmt.Errorf("no container runtime: %s not found on PATH", strings.Join(names, " or "))
}

// kindOf reads the first line `--version` printed on stdout.
func kindOf(bin string, version []byte) (string, error) {
	line, _, _ := strings.Cut(strings.TrimSpace(string(version)), "\n")

	if dockerVersionLine.MatchString(line) {
		return "docker", nil
	}

	if m := podmanVersionLine.FindStringSubmatch(line); m != nil && (m[1] == "podman" || m[1] == filepath.Base(bin)) {
		return "podman", nil
	}

	if len(line) > 80 {
		line = line[:80] + "..."
	}

	return "", fmt.Errorf("%s is neither Docker nor Podman: --version printed %q", bin, line)
}

// podman refuses a Kind that is neither, rather than read it as Docker.
func (r Runtime) podman() (bool, error) {
	switch r.Kind {
	case "docker":
		return false, nil
	case "podman":
		return true, nil
	}

	return false, fmt.Errorf("runtime %s: unknown kind %q, want docker or podman", r.Bin, r.Kind)
}

// ContainerSpec is one container to run.
type ContainerSpec struct {
	Name, Image string
	Argv        []string
	Env         []string
	Labels      map[string]string
	Mounts      []Mount  // named volumes only
	Tmpfs       []string // absolute paths, mounted with the runtime's default options
	Network     string   // "none" or "bridge"
	Hardened    bool     // cap-drop ALL, no-new-privileges, init, pids 2048, uid 1000, memory limit
	MemoryMB    int
	Stdin       bool // keep stdin open and attached (sidecar)
}

// Mount is a named volume at Path.
type Mount struct {
	Volume, Path string
	ReadOnly     bool
}

// Info is what Inspect reports about a container.
type Info struct {
	Running, OOMKilled bool
	Network            string
	ExitCode           int
	Labels             map[string]string
	EnvNames           []string    // names of the container-level variables, never values
	Mounts             []MountInfo // every mount the runtime reports, whatever added it
}

// MountInfo is one mount of a container. Source is the volume name for a
// volume, the host path for a bind and empty for a tmpfs.
type MountInfo struct{ Type, Source, Target string }

// clientError is a client call that exited non-zero. It carries the client's
// stderr tail, which never holds an environment value.
type clientError struct {
	code   int
	stderr string
}

func (e *clientError) Error() string { return fmt.Sprintf("exit %d: %s", e.code, e.stderr) }

func isClientError(err error) bool {
	_, ok := errors.AsType[*clientError](err)

	return ok
}

// call is one client invocation run to its end.
type call struct {
	args   []string
	feed   []byte
	stdin  []byte
	stdout io.Writer
}

// run runs the client to its end; when ctx is done first it stops the client
// and returns an error wrapping ctx's.
func (r Runtime) run(ctx context.Context, c call) error {
	stdout := c.stdout
	if stdout == nil {
		stdout = io.Discard
	}

	p, err := proc.Local{}.Start(ctx, proc.Command{
		Argv:   append([]string{r.Bin}, c.args...),
		Env:    os.Environ(),
		Feed:   c.feed,
		Stdout: stdout,
	})
	if err != nil {
		return err
	}

	go func() {
		if len(c.stdin) > 0 {
			_, _ = p.Stdin().Write(c.stdin)
		}

		_ = p.Stdin().Close()
	}()

	code, err := waitContext(ctx, p)
	if err != nil {
		return err
	}

	if code != 0 {
		return &clientError{code: code, stderr: strings.TrimSpace(p.StderrTail())}
	}

	return nil
}

func (r Runtime) output(ctx context.Context, args ...string) ([]byte, error) {
	var out bytes.Buffer

	err := r.run(ctx, call{args: args, stdout: &out})

	return out.Bytes(), err
}

// waitContext waits for p, stopping it when ctx is done first.
func waitContext(ctx context.Context, p proc.Process) (int, error) {
	type exit struct {
		code int
		err  error
	}

	done := make(chan exit, 1)

	go func() {
		code, err := p.Wait()
		done <- exit{code, err}
	}()

	select {
	case e := <-done:
		return e.code, e.err
	case <-ctx.Done():
		_ = p.Stop(killGrace)

		<-done

		return 0, fmt.Errorf("runtime client stopped: %w", ctx.Err())
	}
}

// envFile renders env in the env-file format, refusing what the format would
// alter or a client's line scanner would reject. Errors name a valid
// variable or give the entry's index, never a value.
func envFile(env []string) ([]byte, error) {
	var b bytes.Buffer

	for i, kv := range env {
		name, value, ok := strings.Cut(kv, "=")

		// A bare entry and an invalid name may be a raw value passed by
		// mistake (a forge token matches the name pattern), so they are
		// identified by index, never quoted.
		switch {
		case !ok:
			// A bare name would import the client's own variable of that name.
			return nil, fmt.Errorf("environment entry at index %d has no =", i)
		case !envName.MatchString(name):
			return nil, fmt.Errorf("environment entry at index %d: invalid variable name", i)
		case strings.ContainsAny(value, "\n\r\x00"):
			return nil, fmt.Errorf("environment variable %s: value holds a newline, carriage return or NUL", name)
		case !utf8.ValidString(value):
			// Docker refuses such a line and prints it, value included.
			return nil, fmt.Errorf("environment variable %s: value is not valid UTF-8", name)
		case len(kv) > maxEnvLine:
			return nil, fmt.Errorf("environment variable %s: longer than %d bytes", name, maxEnvLine)
		}

		b.WriteString(kv)
		b.WriteByte('\n')
	}

	return b.Bytes(), nil
}

// checkArg refuses a positional value the client would read as a flag.
func checkArg(what, v string) error {
	if v == "" || strings.HasPrefix(v, "-") {
		return fmt.Errorf("%s %q: empty or starts with -", what, v)
	}

	return nil
}

// checkVolume keeps a name from smuggling options into --mount, where a comma
// or an = can turn a named volume into a host bind.
func checkVolume(name string) error {
	if !volumeName.MatchString(name) {
		return fmt.Errorf("volume name %q: want %s", name, volumeName)
	}

	return nil
}

// checkMount also refuses a line break in the target: Docker reads --mount as
// a CSV record.
func checkMount(m Mount) error {
	if err := checkVolume(m.Volume); err != nil {
		return err
	}

	if !path.IsAbs(m.Path) || strings.ContainsAny(m.Path, ",\n\r") {
		return fmt.Errorf("mount target %q: want an absolute path without a comma or line break", m.Path)
	}

	return nil
}

func labelArgs(labels map[string]string) ([]string, error) {
	args := make([]string, 0, len(labels))

	for _, k := range slices.Sorted(maps.Keys(labels)) {
		if k == "" || strings.Contains(k, "=") {
			return nil, fmt.Errorf("label key %q: empty or holds =", k)
		}

		args = append(args, "--label="+k+"="+labels[k])
	}

	return args, nil
}

// runArgs validates s and renders its `run` arguments and env file.
func (r Runtime) runArgs(s ContainerSpec, detach bool) ([]string, []byte, error) {
	if err := checkArg("container name", s.Name); err != nil {
		return nil, nil, err
	}

	if err := checkArg("image", s.Image); err != nil {
		return nil, nil, err
	}

	if s.Network != "none" && s.Network != "bridge" {
		return nil, nil, fmt.Errorf("network %q: want none or bridge", s.Network)
	}

	if s.MemoryMB < 0 || s.Hardened && s.MemoryMB == 0 {
		return nil, nil, fmt.Errorf("memory %d MB: a hardened container needs a positive limit", s.MemoryMB)
	}

	feed, err := envFile(s.Env)
	if err != nil {
		return nil, nil, err
	}

	for _, kv := range s.Env {
		name, _, _ := strings.Cut(kv, "=")
		if slices.ContainsFunc(proxyNames, func(p string) bool { return strings.EqualFold(p, name) }) {
			return nil, nil, fmt.Errorf("environment variable %s: a proxy variable goes to a container per exec, not at run", name)
		}
	}

	// A pull would be a network request of CAVU's own; images are built locally.
	args := []string{"run", "--pull=never", "--name=" + s.Name, "--network=" + s.Network}

	if detach {
		args = append(args, "--detach")
	}

	if s.Stdin {
		args = append(args, "--interactive")
	}

	podman, err := r.podman()
	if err != nil {
		return nil, nil, err
	}

	// Host-side client configuration must not put variables into a container.
	switch {
	case !podman:
		// An empty value set here keeps the config's proxies out.
		for _, n := range proxyNames {
			args = append(args, "--env="+n+"=")
		}
	case r.Remote:
		// The remote client has no --env-host.
		args = append(args, "--http-proxy=false")
	default:
		// env_host in containers.conf would copy the client's whole environment in.
		args = append(args, "--http-proxy=false", "--env-host=false")
	}

	labels, err := labelArgs(s.Labels)
	if err != nil {
		return nil, nil, err
	}

	args = append(args, labels...)

	for _, m := range s.Mounts {
		if err := checkMount(m); err != nil {
			return nil, nil, err
		}

		opt := "--mount=type=volume,source=" + m.Volume + ",target=" + m.Path
		if m.ReadOnly {
			opt += ",readonly"
		}

		args = append(args, opt)
	}

	for _, t := range s.Tmpfs {
		// Options after a colon could drop the runtime's noexec, nosuid and
		// nodev defaults.
		if !path.IsAbs(t) || strings.ContainsAny(t, ":,\n\r") {
			return nil, nil, fmt.Errorf("tmpfs %q: want an absolute path without options", t)
		}

		args = append(args, "--tmpfs="+t)
	}

	if s.Hardened {
		args = append(args, "--cap-drop=ALL", "--security-opt=no-new-privileges", "--init", "--pids-limit=2048", "--user=1000:1000")
	}

	if s.MemoryMB > 0 {
		args = append(args, "--memory="+strconv.Itoa(s.MemoryMB)+"m")
	}

	if len(feed) > 0 {
		args = append(args, envFileArg)
	}

	args = append(args, s.Image)

	return append(args, s.Argv...), feed, nil
}

// Start runs the container detached.
func (r Runtime) Start(ctx context.Context, s ContainerSpec) error {
	args, feed, err := r.runArgs(s, true)
	if err != nil {
		return err
	}

	if err := r.run(ctx, call{args: args, feed: feed}); err != nil {
		return fmt.Errorf("start container %s: %w", s.Name, err)
	}

	return nil
}

// StartAttached runs the container in the foreground with stdio attached to
// the returned client. Stopping the client passes TERM on to the container's
// main process; Remove is the backstop for one that outlives it.
func (r Runtime) StartAttached(ctx context.Context, s ContainerSpec) (proc.Process, error) {
	args, feed, err := r.runArgs(s, false)
	if err != nil {
		return nil, err
	}

	p, err := proc.Local{}.Start(ctx, proc.Command{Argv: append([]string{r.Bin}, args...), Env: os.Environ(), Feed: feed})
	if err != nil {
		return nil, fmt.Errorf("start container %s: %w", s.Name, err)
	}

	return p, nil
}

// Remove force-removes containers; an absent name is not an error.
func (r Runtime) Remove(ctx context.Context, names ...string) error {
	for _, n := range names {
		if err := checkArg("container name", n); err != nil {
			return err
		}
	}

	if len(names) == 0 {
		return nil
	}

	// --force skips absent names on both runtimes.
	if err := r.run(ctx, call{args: append([]string{"rm", "--force"}, names...)}); err != nil {
		return fmt.Errorf("remove containers: %w", err)
	}

	return nil
}

// VolumeCreate creates a named volume; an existing one is not an error.
func (r Runtime) VolumeCreate(ctx context.Context, name string, labels map[string]string) error {
	if err := checkVolume(name); err != nil {
		return err
	}

	podman, err := r.podman()
	if err != nil {
		return err
	}

	args := []string{"volume", "create"}

	if podman {
		// Docker returns an existing volume; Podman fails unless told not to.
		args = append(args, "--ignore")
	}

	labelFlags, err := labelArgs(labels)
	if err != nil {
		return err
	}

	args = append(args, labelFlags...)

	if err := r.run(ctx, call{args: append(args, name)}); err != nil {
		return fmt.Errorf("create volume %s: %w", name, err)
	}

	return nil
}

// VolumeRemove removes named volumes; an absent name is not an error, one in
// use is, and it never removes a container.
func (r Runtime) VolumeRemove(ctx context.Context, names ...string) error {
	for _, n := range names {
		if err := checkVolume(n); err != nil {
			return err
		}
	}

	if len(names) == 0 {
		return nil
	}

	podman, err := r.podman()
	if err != nil {
		return err
	}

	if !podman {
		// Docker's --force skips absent names and still refuses a volume a
		// container holds, running or stopped.
		if err := r.run(ctx, call{args: append([]string{"volume", "rm", "--force"}, names...)}); err != nil {
			return fmt.Errorf("remove volumes: %w", err)
		}

		return nil
	}

	// Podman's --force also removes the containers using the volume, so a
	// failed removal asks whether the volume exists instead.
	for _, n := range names {
		err := r.run(ctx, call{args: []string{"volume", "rm", n}})
		if err == nil {
			continue
		}

		exists := r.run(ctx, call{args: []string{"volume", "exists", n}})
		if ce, ok := errors.AsType[*clientError](exists); ok && ce.code == 1 {
			continue
		}

		return fmt.Errorf("remove volume %s: %w", n, err)
	}

	return nil
}

// Inspect reports a container's state.
func (r Runtime) Inspect(ctx context.Context, name string) (Info, error) {
	if err := checkArg("container name", name); err != nil {
		return Info{}, err
	}

	out, err := r.output(ctx, "container", "inspect", name)
	if err != nil {
		return Info{}, fmt.Errorf("inspect container %s: %w", name, err)
	}

	var got []struct {
		State struct {
			Running, OOMKilled bool
			ExitCode           int
		}
		HostConfig struct {
			NetworkMode string
			Tmpfs       map[string]string
		}
		Config struct {
			Labels map[string]string
			Env    []string
		}
		Mounts []inspectMount
	}

	if err := json.Unmarshal(out, &got); err != nil {
		return Info{}, fmt.Errorf("inspect container %s: %w", name, err)
	}

	if len(got) != 1 {
		return Info{}, fmt.Errorf("inspect container %s: %d results", name, len(got))
	}

	c := got[0]

	return Info{
		Running:   c.State.Running,
		OOMKilled: c.State.OOMKilled,
		Network:   c.HostConfig.NetworkMode,
		ExitCode:  c.State.ExitCode,
		Labels:    c.Config.Labels,
		EnvNames:  envNames(c.Config.Env),
		Mounts:    mountInfos(c.Mounts, c.HostConfig.Tmpfs),
	}, nil
}

type inspectMount struct{ Type, Name, Source, Destination string }

// envNames keeps each name once; Docker lists a name twice when a later flag
// overrides the env file.
func envNames(env []string) []string {
	var names []string

	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		names = append(names, name)
	}

	slices.Sort(names)

	return slices.Compact(names)
}

// mountInfos merges the tmpfs mounts both runtimes list only under
// HostConfig.Tmpfs, once per target.
func mountInfos(reported []inspectMount, tmpfs map[string]string) []MountInfo {
	var out []MountInfo

	seen := map[string]bool{}

	for _, m := range reported {
		info := MountInfo{Type: m.Type, Source: m.Source, Target: m.Destination}

		switch m.Type {
		case "volume":
			info.Source = m.Name
		case "tmpfs":
			info.Source = ""
		}

		out = append(out, info)
		seen[m.Destination] = true
	}

	for target := range tmpfs {
		if !seen[target] {
			out = append(out, MountInfo{Type: "tmpfs", Target: target})
		}
	}

	slices.SortFunc(out, func(a, b MountInfo) int { return strings.Compare(a.Target, b.Target) })

	return out
}

// List names the containers, running or not, and the volumes that carry
// label, given as a key or as key=value.
func (r Runtime) List(ctx context.Context, label string) (containers, volumes []string, err error) {
	filter := "--filter=label=" + label

	out, err := r.output(ctx, "ps", "--all", filter, "--format={{.Names}}")
	if err != nil {
		return nil, nil, fmt.Errorf("list containers: %w", err)
	}

	vols, err := r.output(ctx, "volume", "ls", filter, "--format={{.Name}}")
	if err != nil {
		return nil, nil, fmt.Errorf("list volumes: %w", err)
	}

	return names(out), names(vols), nil
}

func names(out []byte) []string {
	n := strings.Fields(string(out))
	slices.Sort(n)

	return n
}
