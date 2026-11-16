package git

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mhersson/cavu/internal/testkit/gitrepo"
)

func TestRunIgnoresInheritedRepoVariables(t *testing.T) {
	root := gitrepo.New(t, map[string]string{"f": "x\n"})

	t.Setenv("GIT_DIR", "/nonexistent")
	t.Setenv("GIT_INDEX_FILE", "/nonexistent")
	t.Setenv("GIT_WORK_TREE", "/nonexistent")

	ctx, envLog := wrapGit(t, `env > "$LOG.env"`)

	out, err := run(ctx, call{dir: root, args: []string{"rev-parse", "--show-toplevel"}, read: true})
	if err != nil {
		t.Fatal(err)
	}

	if got := strings.TrimSpace(string(out)); got != root {
		t.Errorf("toplevel = %q, want %q", got, root)
	}

	env, err := os.ReadFile(envLog + ".env")
	if err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"GIT_DIR=", "GIT_INDEX_FILE=", "GIT_WORK_TREE="} {
		if strings.Contains(string(env), name) {
			t.Errorf("environment still has %s", name)
		}
	}

	if !strings.Contains(string(env), "GIT_OPTIONAL_LOCKS=0") {
		t.Error("read call lacks GIT_OPTIONAL_LOCKS=0")
	}
}

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

// The wrapper forks without pause, so the cancel lands mid-fork: a child that
// misses the group kill outlives the call.
func TestRunCancelKillsChildrenForkedDuringTheKill(t *testing.T) {
	root := gitrepo.New(t, map[string]string{"f": "x\n"})
	ctx, logPath := wrapGit(t, `echo $$ > "$LOG.pgid"
i=0
while [ $i -lt 300 ]; do
	sh -c 'echo $$ >> "$1"; exec sleep 30' sh "$LOG.pids" &
	i=$((i+1))
done
wait`)

	t.Cleanup(func() {
		if raw, err := os.ReadFile(logPath + ".pgid"); err == nil {
			if pgid, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil {
				_ = syscall.Kill(-pgid, syscall.SIGKILL)
			}
		}
	})

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	go func() {
		for ctx.Err() == nil {
			if raw, err := os.ReadFile(logPath + ".pids"); err == nil && strings.Count(string(raw), "\n") >= 5 {
				cancel()

				return
			}

			time.Sleep(time.Millisecond)
		}
	}()

	_, err := run(ctx, call{dir: root, args: []string{"status"}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want canceled", err)
	}

	raw, err := os.ReadFile(logPath + ".pids")
	if err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(time.Second)

	for field := range strings.FieldsSeq(string(raw)) {
		pid, err := strconv.Atoi(field)
		if err != nil {
			t.Fatal(err)
		}

		for alive(pid) && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}

		if alive(pid) {
			t.Errorf("child %d survived cancel", pid)
		}
	}
}

func TestNetCallHasNoTerminal(t *testing.T) {
	for _, name := range []string{"GIT_TERMINAL_PROMPT", "GCM_INTERACTIVE"} {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}

	netCmd := call{args: []string{"fetch"}, net: true}.command(context.Background())
	if !netCmd.SysProcAttr.Setsid || netCmd.SysProcAttr.Setpgid {
		t.Errorf("net call attr = %+v, want Setsid without Setpgid", netCmd.SysProcAttr)
	}

	for _, kv := range []string{"GIT_TERMINAL_PROMPT=0", "GCM_INTERACTIVE=never"} {
		if !slices.Contains(netCmd.Env, kv) {
			t.Errorf("net call lacks %s", kv)
		}
	}

	plain := call{args: []string{"status"}}.command(context.Background())
	if !plain.SysProcAttr.Setpgid || plain.SysProcAttr.Setsid {
		t.Errorf("plain call attr = %+v, want Setpgid without Setsid", plain.SysProcAttr)
	}

	for _, kv := range []string{"GIT_TERMINAL_PROMPT=0", "GCM_INTERACTIVE=never"} {
		if slices.Contains(plain.Env, kv) {
			t.Errorf("plain call has %s", kv)
		}
	}
}

func TestHideURLs(t *testing.T) {
	t.Parallel()

	tests := []struct{ in, want string }{
		{"", ""},
		{"fatal: no url", "fatal: no url"},
		{"fatal: unable to access 'https://user:secret@example.invalid/o/r.git/'", "fatal: unable to access '<url>'"},
		{": ssh://host:22/o/r and git://h/x done", ": <url> and <url> done"},
	}
	for _, tt := range tests {
		if got := hideURLs(tt.in); got != tt.want {
			t.Errorf("hideURLs(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
