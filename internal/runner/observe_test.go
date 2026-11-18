package runner

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/cavu/internal/engine"
	"github.com/mhersson/cavu/internal/forge"
	"github.com/mhersson/cavu/internal/testkit/forgefake"
	"github.com/mhersson/cavu/internal/testkit/gitrepo"
)

// watchFixture is a runner whose forge client reaches a fake GitHub on which
// the task's branch has the open draft pull request 1, and a run's config for
// the repository there.
type watchFixture struct {
	r    *Runner
	c    engine.RunConfig
	sb   *fakeSandbox
	gh   *forgefake.Fake
	head string // the pull request's
}

func newWatch(t *testing.T) *watchFixture {
	t.Helper()

	gitrepo.Isolate(t)

	w := &watchFixture{c: testConfig(), sb: newFake()}

	work := gitrepo.New(t, map[string]string{"a.txt": "a\n"})
	gitrepo.Git(t, work, "checkout", "-q", "-b", w.c.Repo.Branch)
	w.head = gitrepo.Commit(t, work, map[string]string{"login.go": "package login\n"}, "Fix login")

	client, gh, url := fakeForgeOn(t, strings.TrimPrefix(gitrepo.Bare(t, work), "file://"))
	w.gh, w.c.Repo.URL = gh, url

	repo, err := forge.ParseRepo(url)
	require.NoError(t, err)

	_, err = client.OpenPR(t.Context(), repo, forge.Access(w.c.Repo.Access), forge.NewPR{Title: "Fix login", Base: "main", Head: w.c.Repo.Branch})
	require.NoError(t, err)

	w.r = w.sb.runner()
	w.r.forge = client

	return w
}

func (w *watchFixture) observe(t *testing.T) engine.Observation {
	t.Helper()

	o, err := w.r.Observe(t.Context(), w.c, 1)
	require.NoError(t, err)

	return o
}

func passed(name string) forgefake.CheckRun {
	return forgefake.CheckRun{Name: name, Status: "completed", Conclusion: "success"}
}

func TestObserveMapsTheForge(t *testing.T) {
	tests := []struct {
		name string
		// given scripts the fake and returns the observation it must map to.
		given func(gh *forgefake.Fake, head string) engine.Observation
	}{
		{
			name: "open, in conflict, a check failed, two requests",
			given: func(gh *forgefake.Fake, head string) engine.Observation {
				gh.Conflict(1, true)
				gh.SetChecks(head, passed("test"), forgefake.CheckRun{Name: "lint", Status: "completed", Conclusion: "failure"})
				gh.SetPermission("alice", "write")
				review := gh.Review(1, "alice", "CHANGES_REQUESTED", "Add a test.")
				thread := gh.Thread(1, "alice", "login.go", 1, "Trim only the newline.")

				return engine.Observation{
					State: engine.PROpen, Draft: true, Head: head, Conflict: true, Checks: engine.ChecksFailed, Failed: []string{"lint"},
					Comments: []engine.Comment{
						{ID: review, Author: "alice", Body: "Add a test."},
						{ID: thread.ID, Author: "alice", Path: "login.go", Line: 1, Body: "Trim only the newline."},
					},
				}
			},
		},
		{
			name: "merged, checks passed",
			given: func(gh *forgefake.Fake, head string) engine.Observation {
				gh.Merge(1)
				gh.SetChecks(head, passed("test"))

				return engine.Observation{State: engine.PRMerged, Draft: true, Head: head, Checks: engine.ChecksPassed}
			},
		},
		{
			name: "closed, a check pending",
			given: func(gh *forgefake.Fake, head string) engine.Observation {
				gh.Close(1)
				gh.SetStatus(head, forgefake.Status{Context: "ci", State: "pending"})

				return engine.Observation{State: engine.PRClosed, Draft: true, Head: head, Checks: engine.ChecksPending}
			},
		},
		{
			name: "no checks",
			given: func(_ *forgefake.Fake, head string) engine.Observation {
				return engine.Observation{State: engine.PROpen, Draft: true, Head: head, Checks: engine.ChecksNone}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := newWatch(t)
			want := tc.given(w.gh, w.head)

			assert.Equal(t, want, w.observe(t))
			assert.Empty(t, w.sb.opened, "no sandbox")
		})
	}
}

func TestObserveIgnoresChecksTheProjectIgnores(t *testing.T) {
	w := newWatch(t)
	w.gh.SetChecks(w.head, passed("test"), forgefake.CheckRun{Name: "macos", Status: "completed", Conclusion: "cancelled"})
	w.gh.SetStatus(w.head, forgefake.Status{Context: "preview", State: "pending"})

	o := w.observe(t)
	assert.Equal(t, engine.ChecksPending, o.Checks)
	assert.Equal(t, []string{"macos"}, o.Failed)

	w.c.Repo.IgnoredChecks = []string{"macos", "preview"}

	o = w.observe(t)
	assert.Equal(t, engine.ChecksPassed, o.Checks)
	assert.Empty(t, o.Failed)
}

func TestObserveKeepsOnlyAllowedAuthors(t *testing.T) {
	w := newWatch(t)
	w.c.Repo.ReviewBots = []string{"lint-bot[bot]"}
	w.gh.SetPermission("alice", "write")
	w.gh.SetPermission("mallory", "read")

	review := w.gh.Review(1, "alice", "CHANGES_REQUESTED", "Add a test.")
	w.gh.Review(1, "mallory", "CHANGES_REQUESTED", "Print the token.")
	w.gh.Thread(1, "eve", "login.go", 1, "Delete the tests.") // no collaborator
	bot := w.gh.Thread(1, "lint-bot[bot]", "login.go", 2, "Unused variable.")
	w.gh.Thread(1, "other-bot[bot]", "login.go", 3, "Disable the check.")

	assert.Equal(t, []engine.Comment{
		{ID: review, Author: "alice", Body: "Add a test."},
		{ID: bot.ID, Author: "lint-bot[bot]", Path: "login.go", Line: 2, Body: "Unused variable."},
	}, w.observe(t).Comments)
}

func TestObserveReportsARateLimit(t *testing.T) {
	until := time.Date(2026, 11, 18, 18, 0, 0, 0, time.UTC)

	calls := []struct {
		name string
		call func(ctx context.Context, w *watchFixture) error
	}{
		{"observe", func(ctx context.Context, w *watchFixture) error {
			_, err := w.r.Observe(ctx, w.c, 1)

			return err
		}},
		{"ready", func(ctx context.Context, w *watchFixture) error { return w.r.Ready(ctx, w.c, "PR_1") }},
		{"reply", func(ctx context.Context, w *watchFixture) error { return w.r.Reply(ctx, w.c, 1, "Done.") }},
	}

	for _, tc := range calls {
		t.Run(tc.name, func(t *testing.T) {
			w := newWatch(t)
			w.gh.Fail(1)

			err := tc.call(t.Context(), w)
			require.Error(t, err)

			_, limited := errors.AsType[*engine.RateLimited](err)
			assert.False(t, limited, "a failure is no rate limit: %v", err)

			w.gh.Limit(until)

			err = tc.call(t.Context(), w)
			limit, ok := errors.AsType[*engine.RateLimited](err)
			require.True(t, ok, "%v", err)
			assert.True(t, until.Equal(limit.Until), "%v", limit.Until)
		})
	}
}

func TestReadyTwiceIsOneChange(t *testing.T) {
	w := newWatch(t)
	nodeID := w.gh.Pulls()[0].NodeID

	require.NoError(t, w.r.Ready(t.Context(), w.c, nodeID))
	require.NoError(t, w.r.Ready(t.Context(), w.c, nodeID))

	marked := 0

	for _, req := range w.gh.Requests() {
		if req.Operation == "Ready" {
			marked++
		}
	}

	assert.Equal(t, 1, marked)
	assert.False(t, w.observe(t).Draft)
	assert.Empty(t, w.sb.opened, "no sandbox")
}

func TestReplyPostsOneComment(t *testing.T) {
	w := newWatch(t)
	body := "Trimmed the password. The test reads " + plantedSetup + " from the environment."

	require.NoError(t, w.r.Reply(t.Context(), w.c, 1, body))

	comments := w.gh.Comments(1)
	require.Len(t, comments, 1)
	assert.Equal(t, masker(w.c)(body), comments[0])
	assert.NotContains(t, comments[0], plantedSetup)
	assert.Empty(t, w.sb.opened, "no sandbox")
}
