package sandbox

import (
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/cavu/internal/proc"
)

func TestContainerStartRefuses(t *testing.T) {
	cases := map[string]proc.Command{
		"empty argv":           {},
		"argv starting with -": {Argv: []string{"-x"}},
		"a feed":               {Argv: []string{"true"}, Feed: []byte("x")},
		"relative dir":         {Argv: []string{"true"}, Dir: "workspace"},
		"relative file":        {Argv: []string{"true"}, Files: map[string][]byte{"run/cavu/a": nil}},
	}

	for name, cmd := range cases {
		t.Run(name, func(t *testing.T) {
			r, f := fakeRuntime(t, "docker")

			_, err := r.Container("cavu-agent-r1").Start(t.Context(), cmd)
			require.Error(t, err)

			_, err = r.Container("cavu-agent-r1").Run(t.Context(), cmd, time.Minute)
			require.Error(t, err)

			assert.Empty(t, f.calls(t), "the runtime was called")
		})
	}
}

func TestContainerStartArgv(t *testing.T) {
	r, f := fakeRuntime(t, "docker")

	p, err := r.Container("cavu-agent-r1").Start(t.Context(), proc.Command{
		Argv: []string{"git", "status", "--short"},
		Env:  []string{"A=1"},
		Dir:  "/workspace",
	})
	require.NoError(t, err)
	require.NoError(t, p.Stdin().Close())

	code, err := p.Wait()
	require.NoError(t, err)
	assert.Equal(t, 0, code)

	argv := f.only(t).argv
	assert.Equal(t, []string{"exec", "--interactive"}, argv[:2])
	assert.Subset(t, argv, []string{"--env-file=/dev/fd/3", "--workdir=/workspace"})
	assert.Equal(t, []string{"git", "status", "--short"}, argv[len(argv)-3:])

	name := slices.Index(argv, "cavu-agent-r1")
	require.Positive(t, name)
	assert.Equal(t, []string{"sh", "-c"}, argv[name+1:name+3])
}

func TestRunReportsExitAndOutput(t *testing.T) {
	r, f := fakeRuntime(t, "docker")
	f.set(t, "stdout", []byte("hello\n"))
	f.set(t, "exit", []byte("7"))

	res, err := r.Container("cavu-agent-r1").Run(t.Context(), proc.Command{Argv: []string{"make", "test"}}, time.Minute)
	require.NoError(t, err)
	assert.Equal(t, Result{ExitCode: 7, Output: "hello\n"}, res)
}
