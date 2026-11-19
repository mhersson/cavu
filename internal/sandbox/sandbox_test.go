package sandbox

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/cavu/internal/proxy"
	"github.com/mhersson/cavu/internal/sidecar"
)

const (
	instanceA  = "0123456789abcdef"
	instanceB  = "fedcba9876543210"
	baseCommit = "0123456789abcdef0123456789abcdef01234567"
	pushedLine = `{"Type":"pushed","Branch":"cavu/x"}` + "\n"
	// secret stands for every value that must not show in an error.
	secret = "hunter2hunter2"
)

var (
	readyLines = `{"Type":"ready"}` + "\n" + `{"Type":"denied","Host":"cavu-probe.invalid:80"}` + "\n"
	cleanEnv   = "PATH=/usr/bin:/bin\x00HOME=/home/agent\x00HOSTNAME=c1\x00HTTP_PROXY=\x00https_proxy=\x00"
	memoryFile = "out.--format={{.MemTotal}}"
)

// fakeContainer is what the fake runtime reports for a phase container.
type fakeContainer struct {
	network string
	stopped bool
	mounts  []MountInfo
}

func (c fakeContainer) inspect(t *testing.T) []byte {
	t.Helper()

	type mount struct{ Type, Name, Source, Destination string }

	mounts := []mount{}
	tmpfs := map[string]string{}

	for _, m := range c.mounts {
		switch m.Type {
		case "volume":
			mounts = append(mounts, mount{Type: "volume", Name: m.Source, Source: "/var/lib/docker/volumes/" + m.Source + "/_data", Destination: m.Target})
		case "tmpfs":
			tmpfs[m.Target] = ""
		default:
			mounts = append(mounts, mount{Type: m.Type, Source: m.Source, Destination: m.Target})
		}
	}

	b, err := json.Marshal([]map[string]any{{
		"State":      map[string]any{"Running": !c.stopped},
		"HostConfig": map[string]any{"NetworkMode": c.network, "Tmpfs": tmpfs},
		"Mounts":     mounts,
	}})
	require.NoError(t, err)

	return b
}

// reported is what a runtime that honoured the layout reports for phase.
func reported(s *Sandbox, phase Phase) fakeContainer {
	c := fakeContainer{
		network: "none",
		mounts: []MountInfo{
			{Type: "volume", Source: s.name("workspace"), Target: "/workspace"},
			{Type: "volume", Source: s.name("handoff"), Target: "/handoff"},
			{Type: "volume", Source: s.name("cache"), Target: "/cache"},
			{Type: "tmpfs", Target: "/run/cavu"},
		},
	}

	if phase != PhaseSetup {
		c.mounts = append(c.mounts, MountInfo{Type: "volume", Source: s.name("home"), Target: "/home/agent"})
	}

	return c
}

func goodOptions(r Runtime) Options {
	return Options{Runtime: r, Image: "cavu-sandbox:test", InstanceID: instanceA, RunID: 7, TaskID: 5, ProjectID: 3}
}

// fakeSandbox opens a sandbox on a fake runtime that answers every call of a
// sandbox whose isolation holds.
func fakeSandbox(t *testing.T, mutate ...func(*Options)) (*Sandbox, *fake) {
	t.Helper()

	r, f := fakeRuntime(t, "docker")
	o := goodOptions(r)

	for _, m := range mutate {
		m(&o)
	}

	s, err := Open(o)
	require.NoError(t, err)

	t.Cleanup(func() { _ = s.Down(context.Background()) })

	for _, kind := range []string{"workspace", "handoff", "home", "cache", "mirror"} {
		f.set(t, "out."+s.name(kind), []byte(`{"cavu.instance":"`+o.InstanceID+`"}`))
	}

	f.set(t, "early.sidecar", []byte(readyLines))
	f.set(t, "out.sidecar", []byte(`{"Type":"cloned","Commit":"`+baseCommit+`"}`+"\n"))
	f.set(t, "out."+envArgv[len(envArgv)-1], []byte(cleanEnv))
	f.set(t, memoryFile, []byte("33545605120\n"))

	f.set(t, "out."+probeArgv[len(probeArgv)-1], []byte("403"))

	for _, phase := range phases {
		setReported(t, f, s, phase, reported(s, phase))
	}

	return s, f
}

func setReported(t *testing.T, f *fake, s *Sandbox, phase Phase, c fakeContainer) {
	t.Helper()

	f.set(t, "out."+s.name(string(phase)), c.inspect(t))
}

func ensured(t *testing.T, mutate ...func(*Options)) (*Sandbox, *fake) {
	t.Helper()

	s, f := fakeSandbox(t, mutate...)
	require.NoError(t, s.EnsureVolumes(t.Context()))

	return s, f
}

// runsOf returns the indexes of the `run` calls that start the container name.
func runsOf(calls []fakeCall, name string) []int {
	var idx []int

	for i, c := range calls {
		if len(c.argv) > 0 && c.argv[0] == "run" && slices.Contains(c.argv, "--name="+name) {
			idx = append(idx, i)
		}
	}

	return idx
}

func onlyRun(t *testing.T, f *fake, name string) fakeCall {
	t.Helper()

	calls := f.calls(t)
	idx := runsOf(calls, name)
	require.Len(t, idx, 1, "runs of %s", name)

	return calls[idx[0]]
}

func mountFlags(argv []string) []string {
	var out []string

	for _, a := range argv {
		if strings.HasPrefix(a, "--mount=") {
			out = append(out, a)
		}
	}

	return out
}

func tmpfsFlags(argv []string) []string {
	var out []string

	for _, a := range argv {
		if strings.HasPrefix(a, "--tmpfs") {
			out = append(out, a)
		}
	}

	return out
}

func volumeMount(volume, target string) string {
	return "--mount=type=volume,source=" + volume + ",target=" + target
}

// removedAfter names every container an `rm` call after calls[i] removed.
func removedAfter(calls []fakeCall, i int) []string {
	var names []string

	for _, c := range calls[i+1:] {
		if len(c.argv) > 2 && c.argv[0] == "rm" {
			names = append(names, c.argv[2:]...)
		}
	}

	return names
}

// requireBothRemoved wants the sidecar and the phase container removed after
// the phase container was last started.
func requireBothRemoved(t *testing.T, f *fake, s *Sandbox, phase Phase) {
	t.Helper()

	calls := f.calls(t)
	idx := runsOf(calls, s.name(string(phase)))
	require.NotEmpty(t, idx)

	removed := removedAfter(calls, idx[len(idx)-1])
	assert.Contains(t, removed, s.name("sidecar"))
	assert.Contains(t, removed, s.name(string(phase)))
}

func TestNamesFollowTheScheme(t *testing.T) {
	r, _ := fakeRuntime(t, "docker")
	s, err := Open(goodOptions(r))
	require.NoError(t, err)

	for kind, want := range map[string]string{
		"workspace": "cavu-01234567-workspace-7",
		"handoff":   "cavu-01234567-handoff-7",
		"home":      "cavu-01234567-home-5",
		"cache":     "cavu-01234567-cache-3",
		"mirror":    "cavu-01234567-mirror-3",
		"agent":     "cavu-01234567-agent-7",
		"readonly":  "cavu-01234567-readonly-7",
		"sidecar":   "cavu-01234567-sidecar-7",
		"setup":     "cavu-01234567-setup-7",
		"clone":     "cavu-01234567-clone-7",
		"import":    "cavu-01234567-import-7",
	} {
		assert.Equal(t, want, s.name(kind), kind)
	}
}

func TestOpenRefusesBadInstanceID(t *testing.T) {
	cases := map[string]func(*Options){
		"empty":           func(o *Options) { o.InstanceID = "" },
		"15 characters":   func(o *Options) { o.InstanceID = instanceA[:15] },
		"17 characters":   func(o *Options) { o.InstanceID = instanceA + "0" },
		"upper case":      func(o *Options) { o.InstanceID = strings.ToUpper(instanceA) },
		"not hex":         func(o *Options) { o.InstanceID = "0123456789abcdeg" },
		"leading dash":    func(o *Options) { o.InstanceID = "-123456789abcdef" },
		"trailing space":  func(o *Options) { o.InstanceID = instanceA[:15] + " " },
		"zero run":        func(o *Options) { o.RunID = 0 },
		"zero task":       func(o *Options) { o.TaskID = 0 },
		"negative task":   func(o *Options) { o.TaskID = -1 },
		"zero project":    func(o *Options) { o.ProjectID = 0 },
		"negative run id": func(o *Options) { o.RunID = -7 },
		"zero run and task, negative project": func(o *Options) {
			o.RunID, o.TaskID, o.ProjectID = 0, 0, -1
		},
		"zero run, negative task": func(o *Options) { o.RunID, o.TaskID = 0, -1 },
		"zero project, zero run":  func(o *Options) { o.RunID, o.ProjectID = 0, 0 },
	}

	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			r, f := fakeRuntime(t, "docker")
			o := goodOptions(r)
			mutate(&o)

			_, err := Open(o)
			require.Error(t, err)
			assert.Empty(t, f.calls(t), "the runtime was called")
		})
	}

	t.Run("a good one has no side effects", func(t *testing.T) {
		r, f := fakeRuntime(t, "docker")

		_, err := Open(goodOptions(r))
		require.NoError(t, err)
		assert.Empty(t, f.calls(t), "the runtime was called")
	})
}

// Run id 0 with task id 0 is the environment check's sandbox, and with
// project id 0 the doctor's.
func TestOpenAcceptsTheReservedIDs(t *testing.T) {
	r, f := fakeRuntime(t, "docker")
	o := goodOptions(r)
	o.RunID, o.TaskID = 0, 0

	s, err := Open(o)
	require.NoError(t, err)
	assert.Empty(t, f.calls(t), "the runtime was called")
	assert.Equal(t, "cavu-01234567-workspace-0", s.name("workspace"))
	assert.Equal(t, "cavu-01234567-home-0", s.name("home"))
	assert.Equal(t, "cavu-01234567-cache-3", s.name("cache"))

	o.ProjectID = 0

	s, err = Open(o)
	require.NoError(t, err, "the doctor's ids")
	assert.Equal(t, "cavu-01234567-mirror-0", s.name("mirror"))
}

func TestOpenRefusesBadOptions(t *testing.T) {
	cases := map[string]func(*Options){
		"no image":               func(o *Options) { o.Image = "" },
		"image like a flag":      func(o *Options) { o.Image = "--privileged" },
		"label naming the owner": func(o *Options) { o.Labels = map[string]string{"cavu.instance": instanceB} },
		"label naming a run":     func(o *Options) { o.Labels = map[string]string{"cavu.run": "1"} },
		"label naming a task":    func(o *Options) { o.Labels = map[string]string{"cavu.task": "1"} },
		"label naming a project": func(o *Options) { o.Labels = map[string]string{"cavu.project": "1"} },
	}

	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			r, f := fakeRuntime(t, "docker")
			o := goodOptions(r)
			mutate(&o)

			_, err := Open(o)
			require.Error(t, err)
			assert.Empty(t, f.calls(t), "the runtime was called")
		})
	}
}

func TestEnsureVolumesCreatesLabelledVolumes(t *testing.T) {
	s, f := ensured(t, func(o *Options) { o.Labels = map[string]string{"cavu.test": "t1"} })

	want := map[string]string{
		s.name("workspace"): "cavu.run=7",
		s.name("handoff"):   "cavu.run=7",
		s.name("home"):      "cavu.task=5",
		s.name("cache"):     "cavu.project=3",
		s.name("mirror"):    "cavu.project=3",
	}

	created := map[string]bool{}

	for _, c := range f.calls(t) {
		if len(c.argv) < 2 || c.argv[0] != "volume" || c.argv[1] != "create" {
			continue
		}

		name := c.argv[len(c.argv)-1]
		created[name] = true

		require.Contains(t, want, name)
		assert.Subset(t, c.argv, []string{"--label=cavu.instance=" + instanceA, "--label=" + want[name], "--label=cavu.test=t1"})
	}

	assert.Len(t, created, len(want))
}

func TestOpenCopiesLabels(t *testing.T) {
	labels := map[string]string{"cavu.test": "t1"}
	s, f := fakeSandbox(t, func(o *Options) { o.Labels = labels })

	labels["cavu.run"] = "99"
	labels["cavu.test"] = "changed"

	require.NoError(t, s.EnsureVolumes(t.Context()))

	creates := 0

	for _, c := range f.calls(t) {
		if c.argv[0] == "volume" && c.argv[1] == "create" {
			creates++

			assert.Contains(t, c.argv, "--label=cavu.test=t1")
			assert.NotContains(t, c.argv, "--label=cavu.run=99")
		}
	}

	assert.Equal(t, 5, creates)
}

func TestEnsureVolumesRefusesOtherInstance(t *testing.T) {
	for name, labels := range map[string]string{
		"another instance":    `{"cavu.instance":"` + instanceB + `"}`,
		"no labels":           "null",
		"no instance label":   `{"cavu.task":"5"}`,
		"a prefix of ours":    `{"cavu.instance":"01234567"}`,
		"an instance unknown": `{"cavu.instance":"` + secret + `"}`,
	} {
		t.Run(name, func(t *testing.T) {
			s, f := fakeSandbox(t)
			f.set(t, "out."+s.name("home"), []byte(labels))

			err := s.EnsureVolumes(t.Context())
			require.Error(t, err)
			assert.Contains(t, err.Error(), s.name("home"))
			assert.NotContains(t, err.Error(), instanceB)
			assert.NotContains(t, err.Error(), secret)

			_, err = s.Up(t.Context(), PhaseAgent, sidecar.ProxyRequest{}, nil)
			require.Error(t, err)

			_, err = s.Clone(t.Context(), sidecar.CloneRequest{})
			require.Error(t, err)

			for _, c := range f.calls(t) {
				assert.NotEqual(t, "run", c.argv[0], "a container was started")
			}
		})
	}
}

func TestInstancesNeverShareNames(t *testing.T) {
	a, fa := ensured(t)
	b, fb := ensured(t, func(o *Options) { o.InstanceID = instanceB })

	kinds := []string{"workspace", "handoff", "home", "cache", "mirror", "agent", "sidecar", "setup", "clone"}
	namesOf := func(s *Sandbox) []string {
		var n []string
		for _, k := range kinds {
			n = append(n, s.name(k))
		}

		return n
	}

	namesA, namesB := namesOf(a), namesOf(b)

	for _, n := range namesA {
		assert.NotContains(t, namesB, n)
	}

	for f, others := range map[*fake][]string{fa: namesB, fb: namesA} {
		for _, c := range f.calls(t) {
			joined := strings.Join(c.argv, " ")
			for _, n := range others {
				assert.NotContains(t, joined, n, "a call touched the other instance")
			}
		}
	}

	t.Run("orphan removal reads only its own instance", func(t *testing.T) {
		r, f := fakeRuntime(t, "docker")
		filter := "--filter=label=cavu.instance=" + instanceA

		// A runtime that ignored the filter would list the other instance's volume.
		f.outFor(t, b.name("workspace"), "volume", "ls", filter, "--format={{.Name}}")
		f.set(t, "out."+b.name("workspace"), []byte(`{"cavu.instance":"`+instanceB+`","cavu.run":"7"}`))

		require.NoError(t, RemoveOrphans(t.Context(), r, instanceA, nil))

		for _, c := range f.calls(t) {
			if c.argv[0] == "ps" || c.argv[1] == "ls" {
				assert.Contains(t, c.argv, filter)
			}

			assert.NotEqual(t, []string{"volume", "rm"}, c.argv[:2], "a volume was removed")
		}
	})
}

func TestAgentPhaseMountsCacheReadOnly(t *testing.T) {
	s, f := ensured(t, func(o *Options) { o.Labels = map[string]string{"cavu.test": "t1"} })

	c, err := s.Up(t.Context(), PhaseAgent, sidecar.ProxyRequest{AllowHosts: []string{"example.com"}}, nil)
	require.NoError(t, err)
	assert.Equal(t, s.name("agent"), c.name)

	agent := onlyRun(t, f, s.name("agent"))
	assert.Subset(t, agent.argv, []string{
		"--detach",
		"--network=none",
		"--cap-drop=ALL",
		"--security-opt=no-new-privileges",
		"--init",
		"--pids-limit=2048",
		"--user=1000:1000",
		"--memory=8192m",
		"--label=cavu.instance=" + instanceA,
		"--label=cavu.run=7",
		"--label=cavu.test=t1",
	})
	assert.ElementsMatch(t, []string{
		volumeMount(s.name("workspace"), "/workspace"),
		volumeMount(s.name("handoff"), "/handoff"),
		volumeMount(s.name("cache"), "/cache") + ",readonly",
		volumeMount(s.name("home"), "/home/agent"),
	}, mountFlags(agent.argv))
	assert.Equal(t, []string{"--tmpfs=/run/cavu"}, tmpfsFlags(agent.argv), "only the pid directory is a tmpfs")
	assert.Equal(t, []string{"cavu-sandbox:test", "cavu", "bridge", "--listen", "127.0.0.1:3128", "--socket", "/handoff/proxy.sock"}, agent.argv[len(agent.argv)-7:])

	side := onlyRun(t, f, s.name("sidecar"))
	assert.Subset(t, side.argv, []string{
		"--network=bridge",
		"--add-host=host.docker.internal:host-gateway",
		"--interactive",
		"--label=cavu.instance=" + instanceA,
		"--label=cavu.run=7",
		"--label=cavu.test=t1",
	})
	assert.NotContains(t, side.argv, "--detach")
	assert.Equal(t, []string{volumeMount(s.name("handoff"), "/handoff")}, mountFlags(side.argv))
	assert.Equal(t, []string{"cavu-sandbox:test", "cavu", "sidecar"}, side.argv[len(side.argv)-3:])

	t.Run("a runtime with less memory", func(t *testing.T) {
		s, f := ensured(t)
		f.set(t, memoryFile, []byte("4294967296\n"))

		_, err := s.Up(t.Context(), PhaseAgent, sidecar.ProxyRequest{}, nil)
		require.NoError(t, err)
		assert.Contains(t, onlyRun(t, f, s.name("agent")).argv, "--memory=4096m")
	})
}

func TestScratch(t *testing.T) {
	s, f := fakeSandbox(t)

	c, err := s.Scratch(t.Context())
	require.NoError(t, err)
	assert.Equal(t, s.name("scratch"), c.name)

	run := onlyRun(t, f, s.name("scratch"))
	assert.Subset(t, run.argv, []string{
		"--detach", "--network=none", "--cap-drop=ALL", "--user=1000:1000", "--memory=1024m",
		"--label=cavu.instance=" + instanceA, "--label=cavu.run=7",
	})
	assert.Empty(t, mountFlags(run.argv))
	assert.Equal(t, []string{"--tmpfs=/run/cavu"}, tmpfsFlags(run.argv))
	assert.Equal(t, []string{"cavu-sandbox:test", "sleep", "infinity"}, run.argv[len(run.argv)-3:])

	n := len(f.calls(t))
	require.NoError(t, s.Down(t.Context()))
	assert.Contains(t, removedAfter(f.calls(t), n-1), s.name("scratch"))
}

// The import container is the scratch container with the project's mirror,
// writable, and no other volume. It is removed alone, or with the rest by Down.
func TestImporter(t *testing.T) {
	t.Run("before EnsureVolumes", func(t *testing.T) {
		s, f := fakeSandbox(t)

		_, err := s.Importer(t.Context())
		require.ErrorIs(t, err, errNoVolumes)
		assert.Empty(t, runsOf(f.calls(t), s.name("import")))
	})

	s, f := ensured(t)

	c, err := s.Importer(t.Context())
	require.NoError(t, err)
	assert.Equal(t, s.name("import"), c.name)

	run := onlyRun(t, f, s.name("import"))
	assert.Subset(t, run.argv, []string{
		"--detach", "--network=none", "--cap-drop=ALL", "--user=1000:1000", "--memory=2048m",
		"--label=cavu.instance=" + instanceA, "--label=cavu.run=7",
	})
	assert.Equal(t, []string{volumeMount(s.name("mirror"), "/mirror")}, mountFlags(run.argv))
	assert.Equal(t, []string{"--tmpfs=/run/cavu"}, tmpfsFlags(run.argv))
	assert.Equal(t, []string{"cavu-sandbox:test", "sleep", "infinity"}, run.argv[len(run.argv)-3:])

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	n := len(f.calls(t))
	require.NoError(t, s.RemoveImporter(ctx), "a cancelled context still removes it")
	assert.Equal(t, []string{s.name("import")}, removedAfter(f.calls(t), n-1))

	_, err = s.Importer(t.Context())
	require.NoError(t, err)

	n = len(f.calls(t))
	require.NoError(t, s.Down(t.Context()))
	assert.Contains(t, removedAfter(f.calls(t), n-1), s.name("import"))
}

// The fill container is the scratch container with the task's home, read-only,
// and the project's cache, writable, and no other volume. Down removes it.
func TestFiller(t *testing.T) {
	t.Run("before EnsureVolumes", func(t *testing.T) {
		s, f := fakeSandbox(t)

		_, err := s.Filler(t.Context())
		require.ErrorIs(t, err, errNoVolumes)
		assert.Empty(t, runsOf(f.calls(t), s.name("fill")))
	})

	s, f := ensured(t)

	c, err := s.Filler(t.Context())
	require.NoError(t, err)
	assert.Equal(t, s.name("fill"), c.name)

	run := onlyRun(t, f, s.name("fill"))
	assert.Subset(t, run.argv, []string{
		"--detach", "--network=none", "--cap-drop=ALL", "--user=1000:1000", "--memory=1024m",
		"--label=cavu.instance=" + instanceA, "--label=cavu.run=7",
	})
	assert.ElementsMatch(t, []string{
		volumeMount(s.name("home"), "/home/agent") + ",readonly",
		volumeMount(s.name("cache"), "/cache"),
	}, mountFlags(run.argv))
	assert.Equal(t, []string{"--tmpfs=/run/cavu"}, tmpfsFlags(run.argv))
	assert.Equal(t, []string{"cavu-sandbox:test", "sleep", "infinity"}, run.argv[len(run.argv)-3:])

	n := len(f.calls(t))
	require.NoError(t, s.Down(t.Context()))
	assert.Contains(t, removedAfter(f.calls(t), n-1), s.name("fill"))
}

func TestSetupPhaseHasNoHomeMount(t *testing.T) {
	s, f := ensured(t)

	c, err := s.Up(t.Context(), PhaseSetup, sidecar.ProxyRequest{}, nil)
	require.NoError(t, err)
	assert.Equal(t, s.name("setup"), c.name)

	setup := onlyRun(t, f, s.name("setup"))
	assert.Subset(t, setup.argv, []string{"--detach", "--network=none", "--label=cavu.run=7"})
	assert.Equal(t, []string{"--tmpfs=/run/cavu"}, tmpfsFlags(setup.argv), "only the pid directory is a tmpfs")
	assert.ElementsMatch(t, []string{
		volumeMount(s.name("workspace"), "/workspace"),
		volumeMount(s.name("handoff"), "/handoff"),
		volumeMount(s.name("cache"), "/cache"),
	}, mountFlags(setup.argv))
}

// TestPhaseSpecReadOnly: the read-only phase is the agent phase with the
// workspace mounted read-only.
func TestPhaseSpecReadOnly(t *testing.T) {
	r, _ := fakeRuntime(t, "docker")
	s, err := Open(goodOptions(r))
	require.NoError(t, err)

	agent, readOnly := s.phaseSpec(PhaseAgent), s.phaseSpec(PhaseReadOnly)
	assert.Equal(t, s.name("readonly"), readOnly.Name)

	i := slices.IndexFunc(agent.Mounts, func(m Mount) bool { return m.Path == "/workspace" })
	require.GreaterOrEqual(t, i, 0)
	require.False(t, agent.Mounts[i].ReadOnly)

	agent.Name = readOnly.Name
	agent.Mounts[i].ReadOnly = true
	assert.Equal(t, agent, readOnly)
}

func TestEveryContainerIsHardened(t *testing.T) {
	for _, tc := range []struct {
		name    string
		totalMB int
		want    map[string]string // container kind: its memory flag
	}{
		{"a large runtime", 32 << 10, map[string]string{"clone": "--memory=2048m", "push": "--memory=2048m", "import": "--memory=2048m", "sidecar": "--memory=1024m", "setup": "--memory=8192m", "agent": "--memory=8192m", "readonly": "--memory=8192m"}},
		{"a runtime below the phase limit", 1536, map[string]string{"clone": "--memory=1536m", "push": "--memory=1536m", "import": "--memory=1536m", "sidecar": "--memory=1024m", "setup": "--memory=1536m", "agent": "--memory=1536m", "readonly": "--memory=1536m"}},
		{"a runtime below every limit", 768, map[string]string{"clone": "--memory=768m", "push": "--memory=768m", "import": "--memory=768m", "sidecar": "--memory=768m", "setup": "--memory=768m", "agent": "--memory=768m", "readonly": "--memory=768m"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, f := ensured(t)
			f.set(t, memoryFile, []byte(strconv.Itoa(tc.totalMB<<20)+"\n"))

			_, err := s.Clone(t.Context(), sidecar.CloneRequest{URL: "https://git.example/repo.git"})
			require.NoError(t, err)

			f.set(t, "out.sidecar", []byte(pushedLine))

			_, _, err = s.Push(t.Context(), sidecar.PushRequest{URL: "https://git.example/repo.git", Branch: "cavu/x"})
			require.NoError(t, err)

			_, err = s.Importer(t.Context())
			require.NoError(t, err)

			for _, phase := range phases {
				_, err := s.Up(t.Context(), phase, sidecar.ProxyRequest{}, nil)
				require.NoError(t, err)
			}

			for kind, memory := range tc.want {
				assert.Subset(t, onlyRun(t, f, s.name(kind)).argv, []string{
					"--cap-drop=ALL",
					"--security-opt=no-new-privileges",
					"--init",
					"--pids-limit=2048",
					"--user=1000:1000",
					memory,
				}, kind)
			}
		})
	}
}

func TestMirrorOnlyInCloneImportAndPushContainers(t *testing.T) {
	s, f := ensured(t)

	base, err := s.Clone(t.Context(), sidecar.CloneRequest{
		URL: "https://git.example/repo.git", BaseBranch: "main", Branch: "cavu/x",
		MirrorDir: "/elsewhere", WorkspaceDir: "/elsewhere", Name: "n", Email: "e@example.com",
	})
	require.NoError(t, err)
	assert.Equal(t, baseCommit, base)

	f.set(t, "out.sidecar", []byte(pushedLine))

	branch, rejected, err := s.Push(t.Context(), sidecar.PushRequest{
		URL: "https://git.example/repo.git", Ref: "refs/cavu/tasks/1", Commit: baseCommit, Branch: "cavu/x", MirrorDir: "/elsewhere",
	})
	require.NoError(t, err)
	assert.Equal(t, "cavu/x", branch)
	assert.Empty(t, rejected)

	_, err = s.Importer(t.Context())
	require.NoError(t, err)

	_, err = s.Up(t.Context(), PhaseSetup, sidecar.ProxyRequest{}, nil)
	require.NoError(t, err)

	_, err = s.Up(t.Context(), PhaseAgent, sidecar.ProxyRequest{}, nil)
	require.NoError(t, err)

	calls := f.calls(t)
	mirror := volumeMount(s.name("mirror"), "/mirror")

	for _, c := range calls {
		if c.argv[0] == "run" {
			holds := slices.ContainsFunc([]string{"clone", "import", "push"}, func(k string) bool { return slices.Contains(c.argv, "--name="+s.name(k)) })
			assert.Equal(t, holds, slices.ContainsFunc(c.argv, func(a string) bool { return strings.HasPrefix(a, mirror) }), strings.Join(c.argv, " "))
		}
	}

	idx := runsOf(calls, s.name("clone"))
	require.Len(t, idx, 1)

	clone := calls[idx[0]]
	assert.Subset(t, clone.argv, []string{"--network=bridge", "--add-host=host.docker.internal:host-gateway", "--interactive", "--label=cavu.run=7"})
	assert.NotContains(t, clone.argv, "--detach")
	assert.ElementsMatch(t, []string{mirror, volumeMount(s.name("workspace"), "/workspace")}, mountFlags(clone.argv))
	assert.Equal(t, []string{"cavu-sandbox:test", "cavu", "sidecar"}, clone.argv[len(clone.argv)-3:])
	assert.Contains(t, removedAfter(calls, idx[0]), s.name("clone"))

	var req sidecar.Request
	require.NoError(t, json.Unmarshal(clone.stdin, &req))
	require.NotNil(t, req.Clone)
	assert.Equal(t, "/mirror", req.Clone.MirrorDir)
	assert.Equal(t, "/workspace", req.Clone.WorkspaceDir)
	assert.Equal(t, "https://git.example/repo.git", req.Clone.URL)

	idx = runsOf(calls, s.name("push"))
	require.Len(t, idx, 1)

	push := calls[idx[0]]
	assert.Subset(t, push.argv, []string{"--network=bridge", "--add-host=host.docker.internal:host-gateway", "--interactive", "--label=cavu.run=7"})
	assert.NotContains(t, push.argv, "--detach")
	assert.Equal(t, []string{mirror + ",readonly"}, mountFlags(push.argv))
	assert.Equal(t, []string{"cavu-sandbox:test", "cavu", "sidecar"}, push.argv[len(push.argv)-3:])
	assert.Contains(t, removedAfter(calls, idx[0]), s.name("push"))

	req = sidecar.Request{}
	require.NoError(t, json.Unmarshal(push.stdin, &req))
	require.NotNil(t, req.Push)
	assert.Equal(t, "/mirror", req.Push.MirrorDir)
	assert.Equal(t, "refs/cavu/tasks/1", req.Push.Ref)

	assert.Equal(t, []string{mirror}, mountFlags(onlyRun(t, f, s.name("import")).argv), "the import writes the mirror")
}

func TestPushResults(t *testing.T) {
	s, f := ensured(t)
	f.set(t, "out.sidecar", []byte(`{"Type":"rejected","Branch":"cavu/x-2","Error":"[rejected] (fetch first)"}`+"\n"))

	branch, rejected, err := s.Push(t.Context(), sidecar.PushRequest{URL: "https://git.example/repo.git", Branch: "cavu/x"})
	require.NoError(t, err)
	assert.Equal(t, "cavu/x-2", branch)
	assert.Equal(t, "[rejected] (fetch first)", rejected)

	calls := f.calls(t)
	idx := runsOf(calls, s.name("push"))
	require.Len(t, idx, 1)
	assert.Contains(t, removedAfter(calls, idx[0]), s.name("push"))
}

func TestPushFailures(t *testing.T) {
	for name, out := range map[string]string{
		"error line": `{"Type":"error","Error":"ask the forge for the branch: exit status 128: fatal: <url> not found"}`,
		"no result":  "",
	} {
		t.Run(name, func(t *testing.T) {
			s, f := ensured(t)
			f.set(t, "out.sidecar", []byte(out+"\n"))

			if name == "no result" {
				f.set(t, "out."+s.name("push"), []byte(`[{"State":{"OOMKilled":false,"ExitCode":1}}]`))
			}

			_, _, err := s.Push(t.Context(), sidecar.PushRequest{URL: "https://git.example/repo.git"})
			require.Error(t, err)
			require.NotErrorIs(t, err, ErrOutOfMemory)

			calls := f.calls(t)
			idx := runsOf(calls, s.name("push"))
			require.Len(t, idx, 1)
			assert.Contains(t, removedAfter(calls, idx[0]), s.name("push"))
		})
	}

	t.Run("killed for memory", func(t *testing.T) {
		s, f := ensured(t)
		f.set(t, "out.sidecar", []byte(`{"Type":"error","Error":"push: signal: killed"}`+"\n"))
		f.set(t, "out."+s.name("push"), []byte(`[{"State":{"OOMKilled":true,"ExitCode":137}}]`))

		_, _, err := s.Push(t.Context(), sidecar.PushRequest{URL: "https://git.example/repo.git"})
		require.ErrorIs(t, err, ErrOutOfMemory)
	})

	t.Run("a hung push is bounded", func(t *testing.T) {
		s, f := ensured(t)
		f.set(t, "sleep", []byte("sidecar"))

		s.timeouts.clone = 300 * time.Millisecond

		begun := time.Now()
		_, _, err := s.Push(t.Context(), sidecar.PushRequest{URL: "https://git.example/repo.git"})
		require.ErrorIs(t, err, context.DeadlineExceeded)
		assert.Less(t, time.Since(begun), 10*time.Second)
	})

	t.Run("before the volumes exist", func(t *testing.T) {
		s, _ := fakeSandbox(t)

		_, _, err := s.Push(t.Context(), sidecar.PushRequest{URL: "https://git.example/repo.git"})
		require.ErrorIs(t, err, errNoVolumes)
	})
}

func TestCloneFailures(t *testing.T) {
	for name, out := range map[string]string{
		"error line":       `{"Type":"error","Error":"update mirror: exit status 128: fatal: <url> not found"}`,
		"no result":        "",
		"malformed commit": `{"Type":"cloned","Commit":"--orphan"}`,
		"short commit":     `{"Type":"cloned","Commit":"0123456"}`,
	} {
		t.Run(name, func(t *testing.T) {
			s, f := ensured(t)
			f.set(t, "out.sidecar", []byte(out+"\n"))

			if name == "no result" {
				f.set(t, "out."+s.name("clone"), []byte(`[{"State":{"OOMKilled":false,"ExitCode":1}}]`))
			}

			_, err := s.Clone(t.Context(), sidecar.CloneRequest{URL: "https://git.example/repo.git"})
			require.Error(t, err)
			require.NotErrorIs(t, err, ErrOutOfMemory)

			calls := f.calls(t)
			idx := runsOf(calls, s.name("clone"))
			require.Len(t, idx, 1)
			assert.Contains(t, removedAfter(calls, idx[0]), s.name("clone"))
		})
	}

	for name, out := range map[string]string{
		"killed for memory, git":      `{"Type":"error","Error":"update mirror: signal: killed"}`,
		"killed for memory, the main": "",
	} {
		t.Run(name, func(t *testing.T) {
			s, f := ensured(t)
			f.set(t, "out.sidecar", []byte(out+"\n"))
			f.set(t, "out."+s.name("clone"), []byte(`[{"State":{"OOMKilled":true,"ExitCode":137}}]`))

			_, err := s.Clone(t.Context(), sidecar.CloneRequest{URL: "https://git.example/repo.git"})
			require.ErrorIs(t, err, ErrOutOfMemory)

			calls := f.calls(t)
			idx := runsOf(calls, s.name("clone"))
			require.Len(t, idx, 1)
			assert.Contains(t, removedAfter(calls, idx[0]), s.name("clone"))
		})
	}

	t.Run("a hung clone is bounded", func(t *testing.T) {
		s, f := ensured(t)
		f.set(t, "sleep", []byte("sidecar"))

		s.timeouts.clone = 300 * time.Millisecond

		begun := time.Now()
		_, err := s.Clone(t.Context(), sidecar.CloneRequest{URL: "https://git.example/repo.git"})
		require.ErrorIs(t, err, context.DeadlineExceeded)
		assert.Less(t, time.Since(begun), 10*time.Second)
	})
}

func TestCredentialOnlyOnSidecarStdin(t *testing.T) {
	const (
		modelKey = "sk-real-credential-0123456789"
		forgeKey = "ghp_forgetoken0123456789abcdef"
	)

	s, f := ensured(t)

	_, err := s.Clone(t.Context(), sidecar.CloneRequest{URL: "https://git.example/repo.git", Token: forgeKey, BaseBranch: "main", Branch: "cavu/x"})
	require.NoError(t, err)

	f.set(t, "out.sidecar", []byte(pushedLine))

	_, _, err = s.Push(t.Context(), sidecar.PushRequest{URL: "https://git.example/repo.git", Token: forgeKey, Branch: "cavu/x"})
	require.NoError(t, err)

	model := &proxy.ModelRoute{BaseURL: "https://gateway.example", Credential: modelKey, AuthHeader: "bearer", StandIn: "stand-in"}
	_, err = s.Up(t.Context(), PhaseAgent, sidecar.ProxyRequest{Model: model}, nil)
	require.NoError(t, err)
	require.NoError(t, s.Down(t.Context()))

	for _, c := range f.calls(t) {
		for _, a := range c.argv {
			assert.NotContains(t, a, modelKey)
			assert.NotContains(t, a, forgeKey)
		}

		assert.NotContains(t, string(c.fd3), modelKey)
		assert.NotContains(t, string(c.fd3), forgeKey)
	}

	var req sidecar.Request
	require.NoError(t, json.Unmarshal(onlyRun(t, f, s.name("sidecar")).stdin, &req))
	require.NotNil(t, req.Proxy)
	require.NotNil(t, req.Proxy.Model)
	assert.Equal(t, modelKey, req.Proxy.Model.Credential)
	assert.Equal(t, "/handoff/proxy.sock", req.Proxy.Socket)

	req = sidecar.Request{}
	require.NoError(t, json.Unmarshal(onlyRun(t, f, s.name("clone")).stdin, &req))
	require.NotNil(t, req.Clone)
	assert.Equal(t, forgeKey, req.Clone.Token)

	req = sidecar.Request{}
	require.NoError(t, json.Unmarshal(onlyRun(t, f, s.name("push")).stdin, &req))
	require.NotNil(t, req.Push)
	assert.Equal(t, forgeKey, req.Push.Token)
}

func TestUpChecksNetworkNone(t *testing.T) {
	for _, phase := range phases {
		t.Run(string(phase), func(t *testing.T) {
			s, f := ensured(t)

			c := reported(s, phase)
			c.network = "bridge"
			setReported(t, f, s, phase, c)

			_, err := s.Up(t.Context(), phase, sidecar.ProxyRequest{}, nil)
			require.ErrorIs(t, err, ErrNotIsolated)
			requireBothRemoved(t, f, s, phase)
		})
	}
}

func TestUpRefusesAStoppedPhaseContainer(t *testing.T) {
	for _, phase := range phases {
		t.Run(string(phase), func(t *testing.T) {
			s, f := ensured(t)

			c := reported(s, phase)
			c.stopped = true
			setReported(t, f, s, phase, c)

			_, err := s.Up(t.Context(), phase, sidecar.ProxyRequest{}, nil)
			require.ErrorIs(t, err, ErrNotIsolated)
			assert.Contains(t, err.Error(), s.name(string(phase)))
			requireBothRemoved(t, f, s, phase)
		})
	}
}

func TestUpRefusesUnexpectedEnvOrMount(t *testing.T) {
	cases := map[string]struct {
		container func(*fakeContainer)
		execEnv   string
		names     string
	}{
		"a variable an exec sees": {
			execEnv: cleanEnv + "LEAKED_TOKEN=" + secret + "\x00",
			names:   "LEAKED_TOKEN",
		},
		"a proxy variable with a value": {
			execEnv: cleanEnv + "HTTPS_PROXY=http://" + secret + ".invalid:3128\x00",
			names:   "HTTPS_PROXY",
		},
		"a variable with an invalid name": {
			execEnv: cleanEnv + secret + "\x00",
		},
		"a bind mount": {
			container: func(c *fakeContainer) {
				c.mounts = append(c.mounts, MountInfo{Type: "bind", Source: "/srv/" + secret, Target: "/host"})
			},
			names: "/host",
		},
		"another volume at the workspace": {
			container: func(c *fakeContainer) { c.mounts[0].Source = "cavu-" + secret },
			names:     "/workspace",
		},
		"an extra tmpfs": {
			container: func(c *fakeContainer) { c.mounts = append(c.mounts, MountInfo{Type: "tmpfs", Target: "/scratch"}) },
			names:     "/scratch",
		},
		"a missing mount": {
			container: func(c *fakeContainer) { c.mounts = c.mounts[1:] },
			names:     "/workspace",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			s, f := ensured(t)

			c := reported(s, PhaseAgent)
			if tc.container != nil {
				tc.container(&c)
			}

			setReported(t, f, s, PhaseAgent, c)

			if tc.execEnv != "" {
				f.set(t, "out."+envArgv[len(envArgv)-1], []byte(tc.execEnv))
			}

			_, err := s.Up(t.Context(), PhaseAgent, sidecar.ProxyRequest{}, nil)
			require.ErrorIs(t, err, ErrNotIsolated)
			assert.Contains(t, err.Error(), tc.names)
			assert.NotContains(t, err.Error(), secret)
			requireBothRemoved(t, f, s, PhaseAgent)
		})
	}
}

func TestUpTimesOutWithoutReady(t *testing.T) {
	t.Run("no ready line", func(t *testing.T) {
		s, f := ensured(t)
		require.NoError(t, os.Remove(filepath.Join(f.dir, "early.sidecar")))

		s.timeouts.ready = 300 * time.Millisecond

		begun := time.Now()
		_, err := s.Up(t.Context(), PhaseAgent, sidecar.ProxyRequest{}, nil)
		require.ErrorIs(t, err, ErrSidecarDown)
		assert.Less(t, time.Since(begun), 10*time.Second)
		requireBothRemoved(t, f, s, PhaseAgent)
	})

	t.Run("an error line", func(t *testing.T) {
		s, f := ensured(t)
		f.set(t, "early.sidecar", []byte(`{"Type":"error","Error":"proxy: listen on the socket: address in use"}`+"\n"))

		begun := time.Now()
		_, err := s.Up(t.Context(), PhaseAgent, sidecar.ProxyRequest{}, nil)
		require.ErrorIs(t, err, ErrSidecarDown)
		assert.Less(t, time.Since(begun), readyTimeout)
		requireBothRemoved(t, f, s, PhaseAgent)
	})

	t.Run("killed for memory", func(t *testing.T) {
		s, f := ensured(t)
		require.NoError(t, os.Remove(filepath.Join(f.dir, "early.sidecar")))
		f.set(t, "ends.sidecar", []byte("\n"))
		f.set(t, "end", nil)
		f.set(t, "out."+s.name("sidecar"), []byte(`[{"State":{"OOMKilled":true,"ExitCode":137}}]`))

		_, err := s.Up(t.Context(), PhaseAgent, sidecar.ProxyRequest{}, nil)
		require.ErrorIs(t, err, ErrSidecarDown)
		require.ErrorIs(t, err, ErrOutOfMemory)
		requireBothRemoved(t, f, s, PhaseAgent)
	})

	t.Run("a probe the proxy does not refuse", func(t *testing.T) {
		s, f := ensured(t)
		f.set(t, "out."+probeArgv[len(probeArgv)-1], []byte("000"))
		f.exitFor(t, 7, append([]string{"exec", s.name("agent")}, probeArgv...)...)
		s.timeouts.probe = 300 * time.Millisecond

		_, err := s.Up(t.Context(), PhaseAgent, sidecar.ProxyRequest{}, nil)
		require.ErrorIs(t, err, ErrSidecarDown)
		requireBothRemoved(t, f, s, PhaseAgent)
	})
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

func TestUpForwardsSidecarLines(t *testing.T) {
	s, f := ensured(t)
	f.set(t, "early.sidecar", []byte(readyLines+
		`{"Type":"denied","Host":"other.example:443"}`+"\n"+
		`{"Type":"usage","Usage":{"Model":"m","Input":3,"Output":4}}`+"\n"+
		`{"Type":"budget","Error":"run cap spent"}`+"\n"))

	var got lineLog

	_, err := s.Up(t.Context(), PhaseAgent, sidecar.ProxyRequest{}, got.add)
	require.NoError(t, err)

	want := []sidecar.Line{
		{Type: "denied", Host: "other.example:443"},
		{Type: "usage", Usage: &proxy.Usage{Model: "m", Input: 3, Output: 4}},
		{Type: "budget", Error: "run cap spent"},
	}

	require.Eventually(t, func() bool { return len(got.all()) >= len(want) }, 5*time.Second, 10*time.Millisecond)
	assert.Equal(t, want, got.all())
}

func errorLines(lines []sidecar.Line) []sidecar.Line {
	return slices.DeleteFunc(lines, func(l sidecar.Line) bool { return l.Type != "error" })
}

func TestUpReportsASidecarThatEnds(t *testing.T) {
	const cause = "cause-4711"

	for _, tc := range []struct {
		name    string
		out     string // the sidecar's last output
		stderr  string
		inspect string // what the runtime reports for the sidecar container
		want    string // what the error line carries
	}{
		{name: "its output ends", stderr: "panic: " + cause, inspect: `[{"State":{"OOMKilled":false,"ExitCode":1}}]`, want: cause},
		{name: "its own error line", out: `{"Type":"error","Error":"proxy: ` + cause + `"}`, want: cause},
		{name: "killed for memory", inspect: `[{"State":{"OOMKilled":true,"ExitCode":137}}]`, want: ErrOutOfMemory.Error()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, f := ensured(t)
			f.set(t, "ends.sidecar", []byte(tc.out+"\n"))
			f.set(t, "stderr", []byte(tc.stderr+"\n"))
			f.set(t, "out."+s.name("sidecar"), []byte(tc.inspect))

			var got lineLog

			_, err := s.Up(t.Context(), PhaseAgent, sidecar.ProxyRequest{}, got.add)
			require.NoError(t, err)
			assert.Empty(t, got.all())

			f.set(t, "end", nil)
			require.Eventually(t, func() bool { return len(got.all()) > 0 }, 10*time.Second, 10*time.Millisecond)

			// Down waits for the reader, so a second line would show by now.
			require.NoError(t, s.Down(t.Context()))

			lines := got.all()
			require.Len(t, lines, 1)
			assert.Equal(t, "error", lines[0].Type)
			assert.Contains(t, lines[0].Error, tc.want)
		})
	}
}

func TestSidecarTheSandboxStopsIsNotReported(t *testing.T) {
	for name, stop := range map[string]func(t *testing.T, s *Sandbox){
		"down": func(t *testing.T, s *Sandbox) {
			t.Helper()
			require.NoError(t, s.Down(t.Context()))
		},
		"destroy": func(t *testing.T, s *Sandbox) {
			t.Helper()
			require.NoError(t, s.Destroy(t.Context(), true))
		},
		"restart, then down": func(t *testing.T, s *Sandbox) {
			t.Helper()

			_, err := s.Restart(t.Context())
			require.NoError(t, err)
			require.NoError(t, s.Down(t.Context()))
		},
		"an up that replaces it": func(t *testing.T, s *Sandbox) {
			t.Helper()

			_, err := s.Up(t.Context(), PhaseAgent, sidecar.ProxyRequest{AllowHosts: []string{"other.example"}}, nil)
			require.NoError(t, err)
		},
	} {
		t.Run(name, func(t *testing.T) {
			s, _ := ensured(t)

			var got lineLog

			_, err := s.Up(t.Context(), PhaseAgent, sidecar.ProxyRequest{}, got.add)
			require.NoError(t, err)

			stop(t, s)
			assert.Empty(t, errorLines(got.all()))
		})
	}

	t.Run("before ready", func(t *testing.T) {
		s, f := ensured(t)
		f.set(t, "early.sidecar", []byte(`{"Type":"error","Error":"proxy: listen on the socket: address in use"}`+"\n"))

		var got lineLog

		_, err := s.Up(t.Context(), PhaseAgent, sidecar.ProxyRequest{}, got.add)
		require.ErrorIs(t, err, ErrSidecarDown)
		assert.Empty(t, got.all(), "Up's error is the report")
	})
}

func TestRestartKeepsSidecarAndVolumes(t *testing.T) {
	s, f := ensured(t)

	_, err := s.Up(t.Context(), PhaseAgent, sidecar.ProxyRequest{}, nil)
	require.NoError(t, err)

	before := f.calls(t)
	first := onlyRun(t, f, s.name("agent"))

	c, err := s.Restart(t.Context())
	require.NoError(t, err)
	assert.Equal(t, s.name("agent"), c.name)

	calls := f.calls(t)
	after := calls[len(before):]
	agent := s.name("agent")

	var removed, started, inspected, probed bool

	for _, c := range after {
		joined := strings.Join(c.argv, " ")
		assert.NotContains(t, joined, s.name("sidecar"), "the sidecar was touched")
		assert.NotEqual(t, "volume", c.argv[0], "a volume was touched")

		switch {
		case c.argv[0] == "rm" && slices.Contains(c.argv, agent):
			removed = true
		case c.argv[0] == "run":
			started = true

			assert.Equal(t, mountFlags(first.argv), mountFlags(c.argv))
			assert.True(t, removed, "started before the old container was removed")
		case slices.Equal(c.argv, []string{"container", "inspect", agent}):
			inspected = true
		case slices.Equal(c.argv, append([]string{"exec", agent}, probeArgv...)):
			probed = true
		}
	}

	assert.True(t, removed && started && inspected && probed, "removed %v, started %v, inspected %v, probed %v", removed, started, inspected, probed)
	assert.False(t, onlyRun(t, f, s.name("sidecar")).stdinRead, "the sidecar's stdin was closed")

	t.Run("without an Up", func(t *testing.T) {
		s, f := ensured(t)
		n := len(f.calls(t))

		_, err := s.Restart(t.Context())
		require.Error(t, err)
		assert.Len(t, f.calls(t), n, "the runtime was called")
	})
}

func TestUpReusesWhatRuns(t *testing.T) {
	s, f := ensured(t)
	req := sidecar.ProxyRequest{AllowHosts: []string{"example.com"}}

	for range 2 {
		_, err := s.Up(t.Context(), PhaseSetup, req, nil)
		require.NoError(t, err)
	}

	calls := f.calls(t)
	assert.Len(t, runsOf(calls, s.name("setup")), 1)
	assert.Len(t, runsOf(calls, s.name("sidecar")), 1)

	_, err := s.Up(t.Context(), PhaseAgent, req, nil)
	require.NoError(t, err)

	calls = f.calls(t)
	assert.Len(t, runsOf(calls, s.name("sidecar")), 1, "the same request restarted the sidecar")
	assert.Contains(t, removedAfter(calls, runsOf(calls, s.name("setup"))[0]), s.name("setup"))
	assert.Len(t, runsOf(calls, s.name("agent")), 1)

	for _, phase := range []Phase{PhaseReadOnly, PhaseAgent} {
		_, err = s.Up(t.Context(), phase, req, nil)
		require.NoError(t, err)
	}

	calls = f.calls(t)
	assert.Len(t, runsOf(calls, s.name("sidecar")), 1, "a change of phase restarted the sidecar")
	assert.Len(t, runsOf(calls, s.name("readonly")), 1)
	assert.Contains(t, removedAfter(calls, runsOf(calls, s.name("readonly"))[0]), s.name("readonly"))
	assert.Len(t, runsOf(calls, s.name("agent")), 2)

	_, err = s.Up(t.Context(), PhaseAgent, sidecar.ProxyRequest{AllowHosts: []string{"other.example"}}, nil)
	require.NoError(t, err)

	calls = f.calls(t)
	sides := runsOf(calls, s.name("sidecar"))
	require.Len(t, sides, 2, "another request kept the old sidecar")
	assert.True(t, calls[sides[0]].stdinRead, "the old sidecar's stdin is still open")
	assert.Len(t, runsOf(calls, s.name("agent")), 2)
}

func TestUpKeepsASidecarOlderThanTheReadyWindow(t *testing.T) {
	s, f := ensured(t)
	req := sidecar.ProxyRequest{AllowHosts: []string{"example.com"}}

	_, err := s.Up(t.Context(), PhaseAgent, req, nil)
	require.NoError(t, err)

	s.timeouts.ready = 50 * time.Millisecond

	time.Sleep(100 * time.Millisecond)

	for i := range 20 {
		_, err := s.Up(t.Context(), PhaseAgent, req, nil)
		require.NoError(t, err, "Up number %d", i+2)
	}

	assert.False(t, onlyRun(t, f, s.name("sidecar")).stdinRead, "the sidecar's stdin was closed")
}

func volumesRemoved(calls []fakeCall) []string {
	var names []string

	for _, c := range calls {
		if len(c.argv) > 3 && c.argv[0] == "volume" && c.argv[1] == "rm" {
			names = append(names, c.argv[3:]...)
		}
	}

	return names
}

func TestDownAndDestroyOutliveACancelledContext(t *testing.T) {
	cancelled := func(t *testing.T) context.Context {
		t.Helper()

		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		return ctx
	}

	t.Run("down", func(t *testing.T) {
		s, f := ensured(t)

		_, err := s.Up(t.Context(), PhaseAgent, sidecar.ProxyRequest{}, nil)
		require.NoError(t, err)

		n := len(f.calls(t))
		require.NoError(t, s.Down(cancelled(t)))

		removed := removedAfter(f.calls(t), n-1)
		for _, k := range []string{"agent", "setup", "readonly", "sidecar", "clone", "push", "import", "fill"} {
			assert.Contains(t, removed, s.name(k))
		}
	})

	t.Run("destroy", func(t *testing.T) {
		s, f := ensured(t)

		_, err := s.Up(t.Context(), PhaseAgent, sidecar.ProxyRequest{}, nil)
		require.NoError(t, err)

		n := len(f.calls(t))
		require.NoError(t, s.Destroy(cancelled(t), false))

		calls := f.calls(t)
		assert.Contains(t, removedAfter(calls, n-1), s.name("agent"))
		assert.ElementsMatch(t, []string{s.name("workspace"), s.name("handoff"), s.name("home")}, volumesRemoved(calls))
	})
}

func TestDownAndDestroy(t *testing.T) {
	t.Run("down keeps the volumes", func(t *testing.T) {
		s, f := ensured(t)

		_, err := s.Up(t.Context(), PhaseAgent, sidecar.ProxyRequest{}, nil)
		require.NoError(t, err)

		n := len(f.calls(t))
		require.NoError(t, s.Down(t.Context()))

		calls := f.calls(t)
		removed := removedAfter(calls, n-1)

		for _, k := range []string{"agent", "setup", "readonly", "sidecar", "clone", "push", "import", "fill"} {
			assert.Contains(t, removed, s.name(k))
		}

		assert.Empty(t, volumesRemoved(calls))
		assert.True(t, onlyRun(t, f, s.name("sidecar")).stdinRead, "the sidecar's stdin is still open")
	})

	for _, keepHome := range []bool{false, true} {
		t.Run("destroy, keep home "+strconv.FormatBool(keepHome), func(t *testing.T) {
			s, f := ensured(t)

			_, err := s.Up(t.Context(), PhaseAgent, sidecar.ProxyRequest{}, nil)
			require.NoError(t, err)
			require.NoError(t, s.Destroy(t.Context(), keepHome))

			calls := f.calls(t)
			assert.Contains(t, removedAfter(calls, runsOf(calls, s.name("agent"))[0]), s.name("agent"))

			want := []string{s.name("workspace"), s.name("handoff")}
			if !keepHome {
				want = append(want, s.name("home"))
			}

			assert.ElementsMatch(t, want, volumesRemoved(calls))

			_, err = s.Up(t.Context(), PhaseAgent, sidecar.ProxyRequest{}, nil)
			require.Error(t, err, "an Up after Destroy found volumes")
		})
	}
}

func TestRemoveOrphansKeepsRunsAndSharedVolumes(t *testing.T) {
	r, f := fakeRuntime(t, "docker")
	filter := "--filter=label=cavu.instance=" + instanceA

	f.outFor(t, "cavu-01234567-agent-1", "ps", "--all", filter, "--format={{.Names}}")
	f.outFor(t, "cavu-01234567-sidecar-2", "ps", "--all", filter, "--format={{.Names}}")

	vols := map[string]string{
		"cavu-01234567-workspace-1": `{"cavu.instance":"` + instanceA + `","cavu.run":"1"}`,
		"cavu-01234567-handoff-1":   `{"cavu.instance":"` + instanceA + `","cavu.run":"1"}`,
		"cavu-01234567-workspace-2": `{"cavu.instance":"` + instanceA + `","cavu.run":"2"}`,
		"cavu-01234567-home-5":      `{"cavu.instance":"` + instanceA + `","cavu.task":"5"}`,
		"cavu-01234567-cache-3":     `{"cavu.instance":"` + instanceA + `","cavu.project":"3"}`,
		"cavu-01234567-mirror-3":    `{"cavu.instance":"` + instanceA + `","cavu.project":"3"}`,
		"cavu-01234567-odd-9":       `{"cavu.instance":"` + instanceA + `","cavu.run":"nine"}`,
	}

	for name, labels := range vols {
		f.outFor(t, name, "volume", "ls", filter, "--format={{.Name}}")
		f.set(t, "out."+name, []byte(labels))
	}

	require.NoError(t, RemoveOrphans(t.Context(), r, instanceA, []int64{2}))

	var containers, volumes []string

	for _, c := range f.calls(t) {
		switch {
		case c.argv[0] == "rm":
			containers = append(containers, c.argv[2:]...)
		case c.argv[0] == "volume" && c.argv[1] == "rm":
			volumes = append(volumes, c.argv[3:]...)
		}
	}

	assert.ElementsMatch(t, []string{"cavu-01234567-agent-1", "cavu-01234567-sidecar-2"}, containers)
	assert.ElementsMatch(t, []string{"cavu-01234567-workspace-1", "cavu-01234567-handoff-1"}, volumes)

	t.Run("only this instance's containers", func(t *testing.T) {
		r, f := fakeRuntime(t, "docker")

		// A runtime that ignored the filter would list everyone's containers.
		for _, n := range []string{"cavu-01234567-agent-1", "cavu-fedcba98-agent-1", "cavu-0123456789-agent-1", "postgres"} {
			f.outFor(t, n, "ps", "--all", filter, "--format={{.Names}}")
		}

		require.NoError(t, RemoveOrphans(t.Context(), r, instanceA, nil))

		var removed []string

		for _, c := range f.calls(t) {
			if c.argv[0] == "rm" {
				removed = append(removed, c.argv[2:]...)
			}
		}

		assert.Equal(t, []string{"cavu-01234567-agent-1"}, removed)
	})

	t.Run("a bad instance id", func(t *testing.T) {
		r, f := fakeRuntime(t, "docker")

		require.Error(t, RemoveOrphans(t.Context(), r, "", nil))
		require.Error(t, RemoveOrphans(t.Context(), r, strings.ToUpper(instanceA), nil))
		assert.Empty(t, f.calls(t), "the runtime was called")
	})
}
