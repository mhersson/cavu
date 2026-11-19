//go:build integration

package e2e

import (
	"encoding/json"
	"fmt"
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

func TestForgeTaskEndToEnd(t *testing.T) {
	// The first task's work, then the second's, which waits until the first is merged.
	mock := mockapi.Start(t, []mockapi.Step{change, done, change, done})
	in := setup(t, mock.HostURL())
	fake, bare := in.forge()
	s := in.serve()

	defer s.stop()

	s.project("notes", nil, map[string]bool{"readiness": false, "review": false})

	var first, got apiTask

	s.call(http.MethodPost, "/api/tasks", map[string]any{"project": "notes", "title": title, "start": true}, http.StatusCreated, &first)

	read := func() string {
		got = apiTask{}

		return s.call(http.MethodGet, fmt.Sprintf("/api/tasks/%d", first.ID), nil, http.StatusOK, &got)
	}

	within(t, patience, func() (bool, string) {
		b := read()

		return len(fake.Pulls()) > 0 || got.Status == "blocked", b
	})

	pulls := fake.Pulls()
	require.Len(t, pulls, 1, "%+v", got)

	// The head's check is set at once, so the watch's first looks find it running.
	pull := pulls[0]
	head := gitrepo.Git(t, bare, "for-each-ref", "--format=%(objectname)", "refs/heads/"+pull.Head)
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
		b := read()

		return got.PullRequest != nil && got.PullRequest.Checks == "pending", b
	})
	assert.Equal(t, "in_review", got.Status)
	assert.False(t, got.PullRequest.Ready)

	// The look after the check passed marks the draft ready, and the next, a
	// minute later, shows it ready.
	check.Status, check.Conclusion = "completed", "success"
	fake.SetChecks(head, check)

	within(t, 3*time.Minute, func() (bool, string) {
		b := read()

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
		b := read()

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
