//go:build integration

package e2e

import (
	"bytes"
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

// tip is the commit branch is at in the bare repository at bare, "" while it has none.
func tip(t *testing.T, bare, branch string) string {
	t.Helper()

	return gitrepo.Git(t, bare, "for-each-ref", "--format=%(objectname)", "refs/heads/"+branch)
}

// pushOutside commits a file on branch in a clone of the bare repository at
// bare, as a person does outside CAVU, pushes it there and returns the commit.
func pushOutside(t *testing.T, bare, branch string) string {
	t.Helper()

	dir := t.TempDir()
	gitrepo.Git(t, dir, "clone", "-q", "--branch", branch, bare, ".")
	commit := gitrepo.Commit(t, dir, map[string]string{"person.txt": "a person's line\n"}, "docs(person): add a line")
	gitrepo.Git(t, dir, "push", "-q", "origin", branch)

	return commit
}

// pulled waits until the fake holds a pull request, or task id is blocked,
// requires that it holds one, and returns it.
func (s *server) pulled(fake *forgefake.Fake, id int64) forgefake.Pull {
	t := s.in.t
	t.Helper()

	var got apiTask

	within(t, patience, func() (bool, string) {
		b := s.read(id, &got)

		return len(fake.Pulls()) > 0 || got.Status == "blocked", b
	})

	pulls := fake.Pulls()
	require.Len(t, pulls, 1, "%+v", got)

	return pulls[0]
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

	// The head's check is set at once, so the watch's first looks find it running.
	pull := s.pulled(fake, first.ID)
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

	branch := s.pulled(fake, task.ID).Head

	// The head's job fails at once. Its log is set first, so a look that finds
	// the job failed also finds its log.
	const planted = "E2E-JOB-LOG-0123456789"

	fake.SetJobLog(1, "Run make test\nnotes_test: want "+planted+"\nError: Process completed with exit code 1.\n")

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

	// Each setup passed at the base it was given, and both runs were given main's commit.
	base := tip(t, bare, "main")
	for _, p := range payloads[engine.PreparedPayload](t, events, "prepared") {
		assert.Equal(t, base, p.BaseCommit)
	}

	calls := modelCalls(mock)
	require.Len(t, calls, 4)
	assert.True(t, bytes.Contains(calls[2].Body, []byte(planted)), "the fix session's first call holds the failed job's log")
}

func TestForgeReviewCommentRun(t *testing.T) {
	t.Parallel()

	// The first run's work, then two follow-up sessions, each ending with a
	// summary that holds a token of its own; the second changes nothing.
	const answer1, answer2 = "E2E-ANSWER-ONE-0123456789", "E2E-ANSWER-TWO-0123456789"

	answer := func(token string) mockapi.Step {
		return mockapi.Step{Tool: mockapi.Structured, Input: map[string]any{"status": "done", "summary": "Did what the reviewer asked. " + token}}
	}

	mock := mockapi.Start(t, []mockapi.Step{change, done, change, answer(answer1), answer(answer2)})
	in := setup(t, mock.HostURL())
	fake, bare := in.forge()
	s := in.serve()

	defer s.stop()

	s.project("notes", nil, map[string]bool{"readiness": false, "review": false})

	var task, got apiTask

	s.call(http.MethodPost, "/api/tasks", map[string]any{"project": "notes", "title": title, "start": true}, http.StatusCreated, &task)

	pull := s.pulled(fake, task.ID)
	head := tip(t, bare, pull.Head)

	// An account that may only read asks for changes, then the checks pass: a look reads
	// the checks before the reviews, so the look that finds them passed finds the review too.
	fake.SetPermission("visitor", "read")
	fake.Review(pull.Number, "visitor", "CHANGES_REQUESTED", "Rename notes.txt.")
	fake.SetChecks(head, forgefake.CheckRun{ID: 1, App: "github-actions", Name: "test", Status: "completed", Conclusion: "success"})

	// The look that finds the checks passed marks the draft ready, which CAVU does
	// only when no request waits for a run: the visitor's starts none.
	within(t, 2*time.Minute, func() (bool, string) {
		return !fake.Pulls()[0].Draft, s.read(task.ID, &got)
	})

	const asked, threaded = "E2E-REVIEW-0123456789", "E2E-THREAD-0123456789"

	fake.SetPermission("maintainer", "write")
	review := fake.Review(pull.Number, "maintainer", "CHANGES_REQUESTED", "Add one more line. "+asked)

	var moved string

	within(t, patience, func() (bool, string) {
		b := s.read(task.ID, &got)
		moved = tip(t, bare, pull.Head)

		return moved != head || got.Status == "blocked", b
	})
	require.NotEqual(t, head, moved, "%+v", got)

	fake.SetChecks(moved, forgefake.CheckRun{ID: 2, App: "github-actions", Name: "test", Status: "completed", Conclusion: "success"})
	assert.Equal(t, head, gitrepo.Git(t, bare, "rev-parse", moved+"^"), "the follow-up's commit is on top of the head CAVU published")

	// replies waits until the pull request holds n comments, or the task is
	// blocked, and requires that it holds n.
	replies := func(n int) []string {
		var comments []string

		within(t, patience, func() (bool, string) {
			b := s.read(task.ID, &got)
			comments = fake.Comments(pull.Number)

			return len(comments) >= n || got.Status == "blocked", b
		})
		require.Len(t, comments, n, "%+v", got)

		return comments
	}

	assert.Contains(t, replies(1)[0], answer1, "the reply carries the follow-up session's summary")

	// The reviewer asks once more, in a thread on the line the follow-up added.
	thread := fake.Thread(pull.Number, "maintainer", "notes.txt", 3, "Is this line needed? "+threaded)

	assert.Contains(t, replies(2)[1], answer2, "the second reply carries the second session's summary")

	within(t, time.Minute, func() (bool, string) {
		b := s.read(task.ID, &got)

		return got.Status == "in_review" && got.Waiting != "", b
	})
	assert.True(t, got.PullRequest.Ready, "no reviewer's request waits for a follow-up run")
	assert.Equal(t, moved, tip(t, bare, pull.Head), "the second follow-up pushed nothing")

	// Each follow-up run is given the write account's newest request alone.
	assert.Equal(t, []engine.RunStartedPayload{
		{Reason: "initial"},
		{Reason: "comments", Head: head, Requests: []string{review}},
		{Reason: "comments", Head: moved, Requests: []string{thread.ID}},
	}, payloads[engine.RunStartedPayload](t, s.events(task.ID), "run_started"))

	calls := modelCalls(mock)
	require.Len(t, calls, 5)
	assert.True(t, bytes.Contains(calls[2].Body, []byte(asked)), "the first follow-up session gets the review")
	assert.True(t, bytes.Contains(calls[4].Body, []byte(threaded)), "the second gets the thread")
}

func TestForgeTakenOver(t *testing.T) {
	t.Parallel()

	mock := mockapi.Start(t, []mockapi.Step{change, done})
	in := setup(t, mock.HostURL())
	fake, bare := in.forge()
	s := in.serve()

	defer s.stop()

	s.project("notes", nil, map[string]bool{"readiness": false, "review": false})

	var task, got apiTask

	s.call(http.MethodPost, "/api/tasks", map[string]any{"project": "notes", "title": title, "start": true}, http.StatusCreated, &task)

	pull := s.pulled(fake, task.ID)
	theirs := pushOutside(t, bare, pull.Head)

	within(t, 2*time.Minute, func() (bool, string) {
		b := s.read(task.ID, &got)

		return got.Status == "blocked", b
	})
	require.Equal(t, "taken_over", got.Stop.Reason, "%+v", got)

	// The look after the stop is CAVU's next action: it finds the merge.
	fake.Merge(pull.Number)

	within(t, 2*time.Minute, func() (bool, string) {
		b := s.read(task.ID, &got)

		return got.Status == "done", b
	})
	assert.Zero(t, got.Stop, "the merge closed the stop")
	assert.Equal(t, theirs, tip(t, bare, pull.Head), "CAVU pushed nothing over the person's commit")
}

func TestForgeResolvedConflictTakesOneRetry(t *testing.T) {
	t.Parallel()

	mock := mockapi.Start(t, []mockapi.Step{change, done})
	in := setup(t, mock.HostURL())
	fake, bare := in.forge()
	s := in.serve()

	defer s.stop()

	s.project("notes", nil, map[string]bool{"readiness": false, "review": false})

	var task, got apiTask

	s.call(http.MethodPost, "/api/tasks", map[string]any{"project": "notes", "title": title, "start": true}, http.StatusCreated, &task)

	pull := s.pulled(fake, task.ID)
	fake.Conflict(pull.Number, true)

	within(t, 2*time.Minute, func() (bool, string) {
		b := s.read(task.ID, &got)

		return got.Status == "blocked", b
	})
	require.Equal(t, "conflict", got.Stop.Reason, "%+v", got)

	// A person resolves the conflict with a commit whose checks pass, and retries
	// at once, without waiting for CAVU to look.
	theirs := pushOutside(t, bare, pull.Head)
	fake.SetChecks(theirs, forgefake.CheckRun{ID: 1, App: "github-actions", Name: "test", Status: "completed", Conclusion: "success"})
	fake.Conflict(pull.Number, false)
	s.call(http.MethodPost, fmt.Sprintf("/api/tasks/%d/actions", task.ID), map[string]string{"action": "retry"}, http.StatusOK, nil)

	// The look that gives CAVU the person's commit finds no stop and marks the draft ready.
	within(t, 2*time.Minute, func() (bool, string) {
		ready := !fake.Pulls()[0].Draft

		return ready, s.read(task.ID, &got)
	})
	assert.Equal(t, theirs, got.PullRequest.Head, "CAVU watches from the person's commit")

	fake.Merge(pull.Number)

	within(t, 2*time.Minute, func() (bool, string) {
		b := s.read(task.ID, &got)

		return got.Status == "done", b
	})

	stops := payloads[engine.StoppedPayload](t, s.events(task.ID), "stopped")
	require.Len(t, stops, 1)
	assert.Equal(t, "conflict", stops[0].Reason)
}

func TestForgeChecksReadAsJobs(t *testing.T) {
	t.Parallel()

	// Each doctor's endpoint check: a session's tool call and its result, then
	// the resumed session's result. Then the task's work.
	health := []mockapi.Step{
		{Tool: "Bash", Input: map[string]any{"command": "echo cavu"}},
		{Tool: mockapi.Structured, Input: map[string]any{"status": "done"}},
		{Tool: mockapi.Structured, Input: map[string]any{"status": "done"}},
	}
	mock := mockapi.Start(t, slices.Concat(health, health, []mockapi.Step{change, done}))
	in := setup(t, mock.HostURL())
	fake, bare := in.forge()

	// The doctor's forge check needs a project, and the doctor runs only while no server does.
	in.ok("project", "add", "notes", "--repo", in.repo, "--verify", verify)
	in.ok("project", "set", "notes", "--switch", "readiness=off", "--switch", "review=off")

	// doctor runs cavu doctor, requires its exit code, and returns each check's
	// status by name and the requests the fake was sent meanwhile.
	doctor := func(code int) (map[string]string, []string) {
		n := len(fake.Requests())
		r := in.cavu("", "doctor")
		require.Equal(t, code, r.code, r)

		status := map[string]string{}

		for line := range strings.Lines(r.stdout) {
			// A fix line has no name and no status of its own.
			if f := strings.Fields(line); len(f) > 1 && !strings.HasPrefix(line, " ") {
				status[f[0]] = f[1]
			}
		}

		var sent []string
		for _, q := range fake.Requests()[n:] {
			sent = append(sent, q.Method+" "+q.Path)
		}

		return status, sent
	}

	// Refused the check runs, the doctor reads the base head's checks as the jobs of its workflow runs.
	fake.Deny(forgeToken, "checks")

	status, sent := doctor(0)
	assert.Equal(t, "ok", status["forge"], status)

	base := tip(t, bare, "main")
	runs := slices.IndexFunc(sent, func(s string) bool { return strings.Contains(s, "/commits/"+base+"/check-runs") })
	jobs := slices.IndexFunc(sent, func(s string) bool { return strings.Contains(s, "/actions/runs?head_sha="+base) })
	require.GreaterOrEqual(t, runs, 0, sent)
	assert.Greater(t, jobs, runs, sent)

	// Refused the jobs too, the forge check alone fails.
	fake.Deny(forgeToken, "checks", "actions")

	status, _ = doctor(1)

	var failed []string

	for name, v := range status {
		if v == "FAIL" {
			failed = append(failed, name)
		}
	}

	assert.Equal(t, []string{"forge"}, failed, status)

	// The watch, refused the check runs, finds the head's job passed and marks the draft ready.
	fake.Deny(forgeToken, "checks")
	in.ok("project", "check", "notes")

	s := in.serve()

	defer s.stop()

	var task, got apiTask

	s.call(http.MethodPost, "/api/tasks", map[string]any{"project": "notes", "title": title, "start": true}, http.StatusCreated, &task)

	pull := s.pulled(fake, task.ID)
	fake.SetChecks(tip(t, bare, pull.Head), forgefake.CheckRun{ID: 1, App: "github-actions", Run: 1, Name: "test", Status: "completed", Conclusion: "success"})

	// The look that finds the job passed marks the draft ready, and the next, a
	// minute later, shows it ready.
	within(t, 3*time.Minute, func() (bool, string) {
		b := s.read(task.ID, &got)

		return got.PullRequest != nil && got.PullRequest.Ready, b
	})
	assert.False(t, fake.Pulls()[0].Draft)
	assert.Equal(t, "passed", got.PullRequest.Checks)
	assert.True(t, slices.ContainsFunc(fake.Requests(), func(q forgefake.Request) bool {
		return strings.Contains(q.Path, "/actions/runs/1/jobs")
	}), "the watch read the jobs of the head's workflow run")
}

func TestForgeCredentialsNeverInSandbox(t *testing.T) {
	t.Parallel()

	mock := mockapi.Start(t, []mockapi.Step{change, done})
	in := setup(t, mock.HostURL())
	fake, _ := in.forge()

	// The instance reaches the forge as a GitHub App, and its forge token is one the
	// fixture's git refuses, so a clone and a push that pass took the App's tokens.
	const refused = "ghp_REFUSEDFORGETOKEN0123456789"

	in.ok("settings", "set", "forge_app_id", "1")

	for _, s := range [][2]string{{"forge_app_key", forgefake.AppKey()}, {"forge_token", refused}} {
		r := in.cavu(s[1], "secret", "set", s[0])
		require.Zero(t, r.code, r)
	}

	// The key is looked for by a line from its middle: the PEM's first and last are every key's.
	lines := strings.Split(forgefake.AppKey(), "\n")
	keyLine := lines[len(lines)/2]

	// secrets is the key's line, the forge token and every token the fake minted so far.
	secrets := func() []string {
		vs := []string{keyLine, refused}
		for _, tok := range fake.Tokens() {
			vs = append(vs, tok.Value)
		}

		return vs
	}

	s := in.serve()

	defer s.stop()

	// It runs after the deferred stop, so the server's own output is in out too.
	t.Cleanup(func() {
		out := in.out.String() + s.answers.String()
		for _, v := range secrets() {
			assert.NotContains(t, out, v)
		}
	})

	s.project("notes", nil, map[string]bool{"readiness": false, "review": false})

	release := mock.Hold()

	var task apiTask

	s.call(http.MethodPost, "/api/tasks", map[string]any{"project": "notes", "title": title, "start": true}, http.StatusCreated, &task)
	require.True(t, modelCalled(mock, s.ended), "cavu serve made no model call")

	minted := len(fake.Tokens())
	require.Positive(t, minted, "the clone took a token the App minted")

	var marks []string
	for _, v := range secrets() {
		marks = append(marks, mark(v))
	}

	agent, held := in.container("agent"), strings.Join(marks, "|")
	assert.Empty(t, in.sh(agent, procScan, held))
	assert.Empty(t, in.files(agent, held))
	assert.NotEmpty(t, in.files(agent, mark(title)), "the scanned directories hold the task's files")

	release()

	s.pulled(fake, task.ID)
	assert.Greater(t, len(fake.Tokens()), minted, "the push took a token of its own")

	// The run ends after its publish, so by then its events are written.
	var events []apiEvent

	within(t, time.Minute, func() (bool, string) {
		events = s.events(task.ID)

		return slices.ContainsFunc(events, func(e apiEvent) bool { return e.Type == "run_ended" }), strings.Join(eventLines(events), "\n")
	})

	recorded := strings.Join(eventLines(events), "\n")
	for _, v := range secrets() {
		assert.NotContains(t, recorded, v)
	}
}

func TestForgeRestartKeepsWatching(t *testing.T) {
	t.Parallel()

	mock := mockapi.Start(t, []mockapi.Step{change, done})
	in := setup(t, mock.HostURL())
	fake, bare := in.forge()
	s := in.serve()

	// It stops the server that runs at the end: the one the restart started.
	defer s.stop()

	s.project("notes", nil, map[string]bool{"readiness": false, "review": false})

	var task, got apiTask

	s.call(http.MethodPost, "/api/tasks", map[string]any{"project": "notes", "title": title, "start": true}, http.StatusCreated, &task)

	// The head's check runs, so the pull request waits and the watch looks every 10 seconds.
	pull := s.pulled(fake, task.ID)
	fake.SetChecks(tip(t, bare, pull.Head), forgefake.CheckRun{ID: 1, App: "github-actions", Name: "test", Status: "in_progress"})

	within(t, 2*time.Minute, func() (bool, string) {
		b := s.read(task.ID, &got)

		return got.PullRequest != nil && got.PullRequest.Checks == "pending", b
	})

	s.stop()
	s.start()

	fake.Merge(pull.Number)

	within(t, 2*time.Minute, func() (bool, string) {
		b := s.read(task.ID, &got)

		return got.Status == "done", b
	})
	assert.Len(t, payloads[engine.RunStartedPayload](t, s.events(task.ID), "run_started"), 1, "the restart started no run")
}
