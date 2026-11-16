package git

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/mhersson/cavu/internal/testkit/gitrepo"
)

func TestRunEnvAndLocks(t *testing.T) {
	t.Parallel()

	root := gitrepo.New(t, map[string]string{"f": "x\n"})
	ctx, envLog := wrapGit(t, `env > "$LOG.env"`)
	idx := filepath.Join(t.TempDir(), "index")

	if _, err := run(ctx, call{dir: root, args: []string{"status", "-z"}, env: []string{"CAVU_PROBE=" + idx}}); err != nil {
		t.Fatal(err)
	}

	argv, err := os.ReadFile(envLog)
	if err != nil {
		t.Fatal(err)
	}

	if want := "-c core.fsmonitor=false -c core.quotePath=false -c gc.autoDetach=false -c maintenance.autoDetach=false status -z"; !strings.Contains(string(argv), want) {
		t.Errorf("log %q lacks %q", argv, want)
	}

	env, err := os.ReadFile(envLog + ".env")
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(env), "CAVU_PROBE="+idx) {
		t.Error("call env not passed on")
	}

	if strings.Contains(string(env), "GIT_OPTIONAL_LOCKS") {
		t.Error("write call must not set GIT_OPTIONAL_LOCKS")
	}
}

func TestRunErrorCarriesStderrFirstLine(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	_, err := run(context.Background(), call{dir: root, args: []string{"rev-parse", "--verify", "nope"}, read: true})
	if err == nil {
		t.Fatal("want an error")
	}

	// The stderr line follows the exit status directly.
	if !regexp.MustCompile(`^git rev-parse: exit status \d+: \S`).MatchString(err.Error()) {
		t.Errorf("err = %q, want git rev-parse: exit status N: <stderr line>", err)
	}
}

// A read takes no optional locks and a network call never prompts; a plain
// call gets neither setting.
func TestCallEnv(t *testing.T) {
	for _, name := range []string{"GIT_OPTIONAL_LOCKS", "GIT_TERMINAL_PROMPT", "GCM_INTERACTIVE"} {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}

	read := []string{"GIT_OPTIONAL_LOCKS=0"}
	net := []string{"GIT_TERMINAL_PROMPT=0", "GCM_INTERACTIVE=never"}

	for _, tt := range []struct {
		name string
		c    call
		want []string
	}{
		{"read", call{args: []string{"status"}, read: true}, read},
		{"net", call{args: []string{"fetch"}, net: true}, net},
		{"plain", call{args: []string{"status"}}, nil},
	} {
		env := tt.c.command(context.Background()).Env

		for _, kv := range append(slices.Clone(read), net...) {
			if got, want := slices.Contains(env, kv), slices.Contains(tt.want, kv); got != want {
				t.Errorf("%s call has %s = %v, want %v", tt.name, kv, got, want)
			}
		}
	}
}
