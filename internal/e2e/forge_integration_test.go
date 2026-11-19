//go:build integration

package e2e

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/cavu/internal/engine"
	"github.com/mhersson/cavu/internal/testkit/forgefake"
	"github.com/mhersson/cavu/internal/testkit/gitrepo"
	"github.com/mhersson/cavu/internal/testkit/mockapi"
	"github.com/mhersson/cavu/internal/testkit/sandboxtest"
)

// forge moves the instance's repository onto a fake GitHub Enterprise host:
// one TLS server with forgefake under /api/ and the repository's git under
// /o/, which takes pushes with the forge token or a token the fake minted.
// Containers reach it as host.docker.internal; the child on the host, where
// that name need not resolve, through a proxy the test runs, which tunnels to
// it alone. It returns the fake and the bare repository's path.
func (in *instance) forge() (*forgefake.Fake, string) {
	t := in.t
	t.Helper()

	bare := strings.TrimPrefix(gitrepo.Bare(t, gitrepo.New(t, map[string]string{"notes.txt": "first line\n"})), "file://")
	gitrepo.AllowPush(t, bare)

	fake, repo := forgefake.New(t, bare), gitrepo.Handler(t, bare)
	mux := http.NewServeMux()
	mux.Handle("/api/", fake)
	// The user name is the one CAVU's git credential helper sends.
	mux.Handle("/o/", http.StripPrefix("/o", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, _ := r.BasicAuth()
		minted := slices.ContainsFunc(fake.Tokens(), func(tok forgefake.Token) bool { return tok.Value == p })

		if u != "x-access-token" || p != forgeToken && !minted {
			w.Header().Set("WWW-Authenticate", `Basic realm="fixture"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)

			return
		}

		repo.ServeHTTP(w, r)
	})))

	addr := sandboxtest.HostAddr(t, in.rt)
	port := sandboxtest.StartTLSServer(t, addr, mux)
	fixture := net.JoinHostPort("host.docker.internal", port)
	in.repo = "https://" + fixture + "/o/" + filepath.Base(bare)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	var seen struct {
		mu      sync.Mutex
		targets []string
	}

	proxy := &http.Server{ReadHeaderTimeout: 10 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.mu.Lock()
		seen.targets = append(seen.targets, r.Method+" "+r.Host)
		seen.mu.Unlock()

		if r.Method != http.MethodConnect || r.Host != fixture {
			http.Error(w, "not the fixture", http.StatusForbidden)

			return
		}

		up, err := (&net.Dialer{}).DialContext(r.Context(), "tcp", net.JoinHostPort(addr, port))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)

			return
		}

		w.WriteHeader(http.StatusOK)

		down, _, err := http.NewResponseController(w).Hijack()
		if err != nil {
			_ = up.Close()

			return
		}

		go func() { _, _ = io.Copy(up, down); _ = up.Close() }()
		go func() { _, _ = io.Copy(down, up); _ = down.Close() }()
	})}

	go func() { _ = proxy.Serve(ln) }()

	t.Cleanup(func() {
		_ = proxy.Close()

		seen.mu.Lock()
		defer seen.mu.Unlock()

		assert.Empty(t, slices.DeleteFunc(slices.Clone(seen.targets), func(s string) bool { return s == "CONNECT "+fixture }),
			"the child dialled through the proxy something other than the forge")
	})

	// Not before setup: Podman's image build hands its caller's proxy variables
	// to the build's steps, and setup's first child builds the image.
	in.env = []string{"HTTPS_PROXY=http://" + ln.Addr().String(), "NO_PROXY=", "no_proxy="}

	return fake, bare
}

// tip is the commit branch is at in the bare repository at bare, "" while it has none.
func tip(t *testing.T, bare, branch string) string {
	t.Helper()

	return gitrepo.Git(t, bare, "for-each-ref", "--format=%(objectname)", "refs/heads/"+branch)
}

func TestForgeTaskEndToEnd(t *testing.T) {
	t.Parallel()

	// The first task's work, then the second's, which waits until the first is merged.
	mock := mockapi.Start(t, []mockapi.Step{change, done, change, done})
	in := setup(t, mock.HostURL())
	fake, bare := in.forge()
	s := in.serve()

	defer s.stop()

	s.project("notes", nil, map[string]bool{"readiness": false, "review": false})

	var first, got apiTask

	s.call(http.MethodPost, "/api/tasks", map[string]any{"project": "notes", "title": title, "start": true}, http.StatusCreated, &first)

	within(t, patience, func() (bool, string) {
		b := s.read(first.ID, &got)

		return len(fake.Pulls()) > 0 || got.Status == "blocked", b
	})

	pulls := fake.Pulls()
	require.Len(t, pulls, 1, "%+v", got)

	// The head's check is set at once, so the watch's first looks find it running.
	pull := pulls[0]
	head := tip(t, bare, pull.Head)
	check := forgefake.CheckRun{ID: 1, App: "github-actions", Name: "test", Status: "in_progress"}
	fake.SetChecks(head, check)

	exported := payloads[engine.ExportedPayload](t, s.events(first.ID), "exported")
	require.Len(t, exported, 1)
	assert.Equal(t, exported[0].HeadCommit, head, "the branch is at the exported head")
	assert.True(t, pull.Draft)

	_, text, err := engine.PullRequestText(engine.PullRequestData{TaskID: first.ID})
	require.NoError(t, err)

	want, _, _ := strings.Cut(text, "\n")
	line, _, _ := strings.Cut(pull.Body, "\n")
	assert.Equal(t, want, line, "the text starts with CAVU's line for the task")

	within(t, 2*time.Minute, func() (bool, string) {
		b := s.read(first.ID, &got)

		return got.PullRequest != nil && got.PullRequest.Checks == "pending", b
	})
	assert.Equal(t, "in_review", got.Status)
	assert.False(t, got.PullRequest.Ready)

	// The look after the check passed marks the draft ready, and the next, a
	// minute later, shows it ready.
	check.Status, check.Conclusion = "completed", "success"
	fake.SetChecks(head, check)

	within(t, 3*time.Minute, func() (bool, string) {
		b := s.read(first.ID, &got)

		return got.PullRequest.Ready, b
	})
	assert.False(t, fake.Pulls()[0].Draft)

	// The task's home, which holds its session, is kept for fix runs until the merge.
	home := "cavu-" + in.id[:8] + "-home-" + strconv.FormatInt(first.ID, 10)
	volumes := func() []string {
		_, vs, err := in.rt.List(t.Context(), "cavu.instance="+in.id)
		require.NoError(t, err)

		return vs
	}
	assert.Contains(t, volumes(), home)

	var second apiTask

	s.call(http.MethodPost, "/api/tasks", map[string]any{
		"project": "notes", "title": title, "depends_on": []int64{first.ID}, "start": true,
	}, http.StatusCreated, &second)

	fake.Merge(pull.Number)

	within(t, 2*time.Minute, func() (bool, string) {
		b := s.read(first.ID, &got)

		return got.Status == "done", b
	})
	assert.Equal(t, "merged", got.PullRequest.State)

	var events []apiEvent

	within(t, time.Minute, func() (bool, string) {
		events = s.events(first.ID)

		return slices.ContainsFunc(events, func(e apiEvent) bool { return e.Type == "home_removed" }), strings.Join(eventLines(events), "\n")
	})

	assert.NotContains(t, volumes(), home)

	// The watch records the merge as the look that saw it.
	merged := slices.IndexFunc(events, func(e apiEvent) bool {
		var o struct{ State string }

		return e.Type == "pr_observed" && json.Unmarshal(e.Payload, &o) == nil && o.State == "merged"
	})
	require.GreaterOrEqual(t, merged, 0, eventLines(events))

	var started []apiEvent

	within(t, time.Minute, func() (bool, string) {
		started = s.events(second.ID)

		return slices.ContainsFunc(started, func(e apiEvent) bool { return e.Type == "run_started" }), strings.Join(eventLines(started), "\n")
	})
	assert.Greater(t, idOf(t, started, "run_started"), events[merged].ID)
}

func TestForgeCIFixRound(t *testing.T) {
	t.Parallel()

	// The first run's work, then the fix session's: another line on top of it.
	mock := mockapi.Start(t, []mockapi.Step{change, done, change, done})
	in := setup(t, mock.HostURL())
	fake, bare := in.forge()

	// The setup passes only at the base commit, so a fix run set up at its head would stop.
	in.ok("project", "add", "notes", "--repo", in.repo, "--verify", verify,
		"--setup", `h=$(git rev-parse HEAD) && test "$h" = "$CAVU_BASE_COMMIT"`)
	in.ok("project", "set", "notes", "--switch", "readiness=off", "--switch", "review=off")
	in.ok("project", "check", "notes")

	s := in.serve()

	defer s.stop()

	var task, got apiTask

	s.call(http.MethodPost, "/api/tasks", map[string]any{"project": "notes", "title": title, "start": true}, http.StatusCreated, &task)

	within(t, patience, func() (bool, string) {
		b := s.read(task.ID, &got)

		return len(fake.Pulls()) > 0 || got.Status == "blocked", b
	})

	pulls := fake.Pulls()
	require.Len(t, pulls, 1, "%+v", got)

	// The head's job fails at once. Its log is set first, so a look that finds
	// the job failed also finds its log.
	const planted = "E2E-JOB-LOG-0123456789"

	fake.SetJobLog(1, "Run make test\nnotes_test: want "+planted+"\nError: Process completed with exit code 1.\n")

	branch := pulls[0].Head
	head := tip(t, bare, branch)
	fake.SetChecks(head, forgefake.CheckRun{ID: 1, App: "github-actions", Run: 1, Name: "test", Status: "completed", Conclusion: "failure"})

	var fixHead string

	within(t, patience, func() (bool, string) {
		b := s.read(task.ID, &got)
		fixHead = tip(t, bare, branch)

		return fixHead != head || got.Status == "blocked", b
	})
	require.NotEqual(t, head, fixHead, "%+v", got)

	// The fix's job passes at once, so the look after its push finds it passed.
	fake.SetChecks(fixHead, forgefake.CheckRun{ID: 2, App: "github-actions", Run: 2, Name: "test", Status: "completed", Conclusion: "success"})
	assert.Equal(t, head, gitrepo.Git(t, bare, "rev-parse", fixHead+"^"), "the fix is a commit on top of the head CAVU published")

	// That look marks the draft ready, and the next, a minute later, shows it ready.
	within(t, 3*time.Minute, func() (bool, string) {
		b := s.read(task.ID, &got)

		return got.PullRequest.Ready, b
	})
	assert.False(t, fake.Pulls()[0].Draft)

	events := s.events(task.ID)
	assert.Equal(t, []engine.RunStartedPayload{{Reason: "initial"}, {Reason: "ci_fix", Head: head}},
		payloads[engine.RunStartedPayload](t, events, "run_started"))

	sessions := payloads[engine.SessionStartedPayload](t, events, "session_started")
	require.Len(t, sessions, 2)
	assert.Equal(t, "fix_ci", sessions[1].Mode)
	assert.True(t, sessions[1].Resume)
	assert.Equal(t, sessions[0].SessionID, sessions[1].SessionID, "the fix resumes the first run's session")

	var steps []string

	for _, e := range events {
		if e.Type == "run_started" || e.Type == "prepared" || e.Type == "stopped" {
			steps = append(steps, e.Type)
		}
	}

	assert.Equal(t, []string{"run_started", "prepared", "run_started", "prepared"}, steps, "each run's setup passed, and nothing stopped the task")

	// The fix session's first call is the first to hold the failed job's log.
	calls := modelCalls(mock)
	require.Len(t, calls, 4)

	for i, r := range calls[:3] {
		assert.Equal(t, i == 2, bytes.Contains(r.Body, []byte(planted)), "model call %d holds the job's log", i+1)
	}
}
