// Package sandbox is where a run's containers live: it drives the container
// runtime client, Docker or Podman, and assembles from it the two-container
// sandbox, an agent container with no network beside a proxy sidecar, that
// fails closed. Files move in and out as streams on an exec, never through a
// host path.
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

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// proxyNames are the variables the Docker client fills in on `run` from the
// proxies of its config.json.
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

		// The podman-docker shim prints a notice on stderr with every call,
		// which would land in every captured output.
		if filepath.Base(bin) != "podman" {
			if podman, ok := realPodman(ctx); ok {
				r.Bin = podman
			}
		}

		out, err := r.output(ctx, "info", "--format={{.Host.ServiceIsRemote}}")
		if err == nil {
			r.Remote, err = strconv.ParseBool(strings.TrimSpace(string(out)))
		}

		if err != nil {
			return Runtime{}, fmt.Errorf("podman found at %s but did not answer: %w", r.Bin, err)
		}

		return r, nil
	}

	return Runtime{}, fmt.Errorf("no container runtime: %s not found on PATH", strings.Join(names, " or "))
}

// realPodman finds a podman on PATH that says it is Podman.
func realPodman(ctx context.Context) (string, bool) {
	bin, err := exec.LookPath("podman")
	if err != nil {
		return "", false
	}

	version, err := Runtime{Bin: bin}.output(ctx, "--version")
	if err != nil {
		return "", false
	}

	kind, err := kindOf(bin, version)

	return bin, err == nil && kind == "podman"
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

func (r Runtime) podman() bool { return r.Kind == "podman" }

// ContainerSpec is one container to run.
type ContainerSpec struct {
	Name, Image string
	Argv        []string
	Labels      map[string]string
	Mounts      []Mount  // named volumes only
	Tmpfs       []string // absolute paths, mounted with the runtime's default options
	Network     string   // "none" or "bridge"
	HostGateway bool     // host.docker.internal resolves to the host; bridge only
	Hardened    bool     // cap-drop ALL, no-new-privileges, init, pids 2048, uid 1000
	MemoryMB    int      // a memory limit when positive
	Stdin       bool     // keep stdin open and attached (sidecar)
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
	Mounts             []MountInfo // every mount the runtime reports, whatever added it
}

// MountInfo is one mount of a container. Source is the volume name for a
// volume, the host path for a bind and empty for a tmpfs.
type MountInfo struct{ Type, Source, Target string }

// isClientError reports a client call that exited non-zero.
func isClientError(err error) bool {
	_, ok := errors.AsType[*proc.ExitError](err)

	return ok
}

// call is one client invocation run to its end.
type call struct {
	args   []string
	stdin  []byte
	stdout io.Writer
	stderr io.Writer // also receives stderr as it arrives
}

// run runs the client to its end; when ctx is done first it stops the client
// and returns an error wrapping ctx's. A non-zero exit is a *proc.ExitError,
// whose stderr tail never holds an environment value.
func (r Runtime) run(ctx context.Context, c call) error {
	stdout := c.stdout
	if stdout == nil {
		stdout = io.Discard
	}

	err := proc.Run(ctx, proc.Local{}, proc.Command{
		Argv:   append([]string{r.Bin}, c.args...),
		Env:    os.Environ(),
		Stdout: stdout,
		Stderr: c.stderr,
	}, c.stdin)
	if err != nil && errors.Is(err, ctx.Err()) {
		return fmt.Errorf("runtime client stopped: %w", err)
	}

	return err
}

func (r Runtime) output(ctx context.Context, args ...string) ([]byte, error) {
	var out bytes.Buffer

	err := r.run(ctx, call{args: args, stdout: &out})

	return out.Bytes(), err
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

// runArgs renders s as `run` arguments.
func (r Runtime) runArgs(s ContainerSpec, detach bool) ([]string, error) {
	// A pull would be a network request of CAVU's own; images are built locally.
	args := []string{"run", "--pull=never", "--name=" + s.Name, "--network=" + s.Network}

	if detach {
		args = append(args, "--detach")
	}

	if s.Stdin {
		args = append(args, "--interactive")
	}

	// Host-side client configuration must not put variables into a container.
	switch {
	case !r.podman():
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
		return nil, err
	}

	args = append(args, labels...)

	if s.HostGateway {
		args = append(args, "--add-host=host.docker.internal:host-gateway")
	}

	for _, m := range s.Mounts {
		opt := "--mount=type=volume,source=" + m.Volume + ",target=" + m.Path
		if m.ReadOnly {
			opt += ",readonly"
		}

		args = append(args, opt)
	}

	for _, t := range s.Tmpfs {
		args = append(args, "--tmpfs="+t)
	}

	if s.Hardened {
		args = append(args, "--cap-drop=ALL", "--security-opt=no-new-privileges", "--init", "--pids-limit=2048", "--user=1000:1000")
	}

	if s.MemoryMB > 0 {
		args = append(args, "--memory="+strconv.Itoa(s.MemoryMB)+"m")
	}

	args = append(args, s.Image)

	return append(args, s.Argv...), nil
}

// Start runs the container detached.
func (r Runtime) Start(ctx context.Context, s ContainerSpec) error {
	args, err := r.runArgs(s, true)
	if err != nil {
		return err
	}

	if err := r.run(ctx, call{args: args}); err != nil {
		return fmt.Errorf("start container %s: %w", s.Name, err)
	}

	return nil
}

// StartAttached runs the container in the foreground with stdio attached to
// the returned client. Stopping the client passes TERM on to the container's
// main process; Remove is the backstop for one that outlives it.
func (r Runtime) StartAttached(ctx context.Context, s ContainerSpec) (proc.Process, error) {
	args, err := r.runArgs(s, false)
	if err != nil {
		return nil, err
	}

	p, err := proc.Local{}.Start(ctx, proc.Command{Argv: append([]string{r.Bin}, args...), Env: os.Environ()})
	if err != nil {
		return nil, fmt.Errorf("start container %s: %w", s.Name, err)
	}

	return p, nil
}

// Remove force-removes containers; an absent name is not an error.
func (r Runtime) Remove(ctx context.Context, names ...string) error {
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
	args := []string{"volume", "create"}

	if r.podman() {
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
	if len(names) == 0 {
		return nil
	}

	if !r.podman() {
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
		if e, ok := errors.AsType[*proc.ExitError](exists); ok && e.Code == 1 {
			continue
		}

		return fmt.Errorf("remove volume %s: %w", n, err)
	}

	return nil
}

// VolumeLabels reads a volume's labels; an absent volume is an error.
func (r Runtime) VolumeLabels(ctx context.Context, name string) (map[string]string, error) {
	out, err := r.output(ctx, "volume", "inspect", "--format={{json .Labels}}", name)
	if err != nil {
		return nil, fmt.Errorf("inspect volume %s: %w", name, err)
	}

	var labels map[string]string
	if err := json.Unmarshal(bytes.TrimSpace(out), &labels); err != nil {
		return nil, fmt.Errorf("inspect volume %s: %w", name, err)
	}

	return labels, nil
}

// MemoryMB is the memory the runtime reports for the machine its containers
// run on, which on a VM-backed runtime is the VM.
func (r Runtime) MemoryMB(ctx context.Context) (int, error) {
	format := "--format={{.MemTotal}}"
	if r.podman() {
		format = "--format={{.Host.MemTotal}}"
	}

	out, err := r.output(ctx, "info", format)
	if err != nil {
		return 0, fmt.Errorf("runtime memory: %w", err)
	}

	total, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil || total < 1<<20 {
		return 0, errors.New("runtime memory: the runtime reported no size in bytes")
	}

	return int(total >> 20), nil
}

// Version is the version the runtime's engine reports, so it also shows that
// the engine answers.
func (r Runtime) Version(ctx context.Context) (string, error) {
	format := "--format={{.ServerVersion}}"
	if r.podman() {
		format = "--format={{.Version.Version}}"
	}

	out, err := r.output(ctx, "info", format)
	if err != nil {
		return "", fmt.Errorf("runtime version: %w", err)
	}

	return strings.TrimSpace(string(out)), nil
}

// Inspect reports a container's state.
func (r Runtime) Inspect(ctx context.Context, name string) (Info, error) {
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
		Mounts:    mountInfos(c.Mounts, c.HostConfig.Tmpfs),
	}, nil
}

type inspectMount struct{ Type, Name, Source, Destination string }

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
