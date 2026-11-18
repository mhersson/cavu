package forge

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/cavu/internal/testkit/forgefake"
)

func run(name, status, conclusion string) forgefake.CheckRun {
	return forgefake.CheckRun{Name: name, Status: status, Conclusion: conclusion}
}

func TestObserveChecks(t *testing.T) {
	bare, head := newBareRepo(t)

	tests := []struct {
		name     string
		runs     []forgefake.CheckRun
		statuses []forgefake.Status
		ignored  []string
		pages    bool // the runs and statuses of AIM's fixture pages
		checks   string
		failed   []string
	}{
		{name: "no checks", checks: "none"},
		{name: "one queued", runs: []forgefake.CheckRun{run("build", "queued", "")}, checks: "pending"},
		{
			name: "all passed with one skipped",
			runs: []forgefake.CheckRun{
				run("build", "completed", "success"), run("docs", "completed", "skipped"), run("lint", "completed", "neutral"),
			},
			statuses: []forgefake.Status{{Context: "ci/a", State: "success"}},
			checks:   "passed",
		},
		{
			name:   "one cancelled",
			runs:   []forgefake.CheckRun{run("build", "completed", "success"), run("test", "completed", "cancelled")},
			checks: "failed", failed: []string{"test"},
		},
		{
			name:   "one failed while one runs",
			runs:   []forgefake.CheckRun{run("lint", "completed", "failure"), run("test", "in_progress", "")},
			checks: "pending", failed: []string{"lint"},
		},
		{
			name:     "a failed status",
			runs:     []forgefake.CheckRun{run("build", "completed", "success")},
			statuses: []forgefake.Status{{Context: "ci/b", State: "success"}, {Context: "ci/a", State: "error"}},
			checks:   "failed", failed: []string{"ci/a"},
		},
		{
			name:     "an ignored failing check",
			runs:     []forgefake.CheckRun{run("build", "completed", "success"), run("flaky", "completed", "failure")},
			statuses: []forgefake.Status{{Context: "coverage", State: "pending"}},
			ignored:  []string{"flaky", "coverage"},
			checks:   "passed",
		},
		{
			name:    "only ignored checks",
			runs:    []forgefake.CheckRun{run("flaky", "completed", "failure")},
			ignored: []string{"flaky"},
			checks:  "none",
		},
		{
			name: "failed ones are sorted",
			runs: []forgefake.CheckRun{
				run("zeta", "completed", "failure"), run("alpha", "completed", "timed_out"), run("beta", "completed", "success"),
			},
			statuses: []forgefake.Status{{Context: "mid", State: "failure"}},
			checks:   "failed", failed: []string{"alpha", "mid", "zeta"},
		},
		{
			name:   "two failed runs of one name give it once",
			runs:   []forgefake.CheckRun{run("test", "completed", "failure"), run("test", "completed", "failure")},
			checks: "failed", failed: []string{"test"},
		},
		{name: "the 300 runs of three pages", pages: true, checks: "pending", failed: []string{"deploy/m"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := forgefake.New(t, bare)
			f.SetChecks(head, tc.runs...)
			f.SetStatus(head, tc.statuses...)

			var (
				h      http.Handler = f
				served func() int
			)

			if tc.pages {
				mux := http.NewServeMux()
				mux.Handle("/", f)

				served = fixturePages(t, mux)
				h = mux
			}

			c := newTestClient(t, nil, fakeRepo.Host, h, nil)
			openPR(t, c)

			o, err := c.Observe(t.Context(), fakeRepo, fakeAccess, Query{Number: 1, IgnoredChecks: tc.ignored})
			require.NoError(t, err)
			assert.Equal(t, tc.checks, o.Checks)
			assert.Equal(t, tc.failed, o.Failed)

			if served != nil {
				assert.Equal(t, 3, served(), "pages read")
			}
		})
	}
}

// fixturePages serves AIM's three pages of check runs, linked by Link headers,
// and its statuses on mux, and returns how many pages were served.
func fixturePages(t *testing.T, mux *http.ServeMux) func() int {
	t.Helper()

	var (
		mu    sync.Mutex
		count int
	)

	pages := [][]byte{readFixture(t, "check_runs_1.json"), readFixture(t, "check_runs_2.json"), readFixture(t, "check_runs_3.json")}
	status := readFixture(t, "status.json")

	mux.HandleFunc("GET /api/v3/repos/o/n/commits/{sha}/check-runs", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		count++
		mu.Unlock()

		link := func(page int, rel string) string {
			return fmt.Sprintf(`<https://%s%s?per_page=100&page=%d>; rel=%q`, fakeRepo.Host, r.URL.Path, page, rel)
		}

		page := 1
		_, _ = fmt.Sscan(r.URL.Query().Get("page"), &page)

		switch page {
		case 1:
			w.Header().Set("Link", link(2, "next"))
		case 2:
			w.Header().Set("Link", link(1, "prev")+", "+link(3, "next"))
		}

		_, _ = w.Write(pages[page-1])
	})
	mux.HandleFunc("GET /api/v3/repos/o/n/commits/{sha}/status", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(status)
	})

	return func() int {
		mu.Lock()
		defer mu.Unlock()

		return count
	}
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()

	b, err := os.ReadFile(filepath.Join("testdata", name))
	require.NoError(t, err)

	return b
}

func TestChecksPaginates(t *testing.T) {
	mux := http.NewServeMux()
	fixturePages(t, mux)

	c := newTestClient(t, nil, fakeRepo.Host, mux, nil)

	got, err := c.checks(t.Context(), fakeRepo, testToken, "abc123", nil)
	require.NoError(t, err)
	require.Len(t, got, 303, "300 runs and 3 statuses")
	assert.Equal(t, "p1-000", got[0].name)
	assert.Equal(t, "p3-099", got[299].name)

	var statuses []string
	for _, ch := range got[300:] {
		statuses = append(statuses, ch.name)
	}

	assert.ElementsMatch(t, []string{"build/a", "deploy/m", "lint/z"}, statuses)
}

func TestChecksStopsAtFivePages(t *testing.T) {
	var (
		mu    sync.Mutex
		pages int
	)

	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/status") {
			_, _ = w.Write([]byte(`{"statuses":[]}`))

			return
		}

		mu.Lock()
		pages++
		n := pages
		mu.Unlock()

		w.Header().Set("Link", fmt.Sprintf(`<https://%s%s?per_page=100&page=%d>; rel="next"`, fakeRepo.Host, r.URL.Path, n+1))
		_, _ = fmt.Fprintf(w, `{"check_runs":[{"name":"r%d","status":"completed","conclusion":"success"}]}`, n)
	})
	c := newTestClient(t, nil, fakeRepo.Host, h, nil)

	got, err := c.checks(t.Context(), fakeRepo, testToken, "abc123", nil)
	require.NoError(t, err)
	assert.Len(t, got, 5)

	mu.Lock()
	defer mu.Unlock()

	assert.Equal(t, 5, pages)
}

func TestChecksRefuseAForeignNextLink(t *testing.T) {
	for name, next := range map[string]string{
		"another host":   "https://evil.example.com/steal?token=1",
		"another scheme": "http://ghe.example/api/v3/repos/o/n/commits/abc123/check-runs?page=2",
		"not an address": "://",
	} {
		t.Run(name, func(t *testing.T) {
			h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Link", fmt.Sprintf(`<%s>; rel="next"`, next))
				_, _ = w.Write([]byte(`{"check_runs":[{"name":"r","status":"completed","conclusion":"success"}]}`))
			})
			// The test transport fails the test on a request to any other host.
			c := newTestClient(t, nil, fakeRepo.Host, h, nil)

			_, err := c.checks(t.Context(), fakeRepo, testToken, "abc123", nil)
			require.Error(t, err)
			assert.NotContains(t, err.Error(), "token=1")
		})
	}
}

func TestCheckClassification(t *testing.T) {
	runs := []struct{ status, conclusion, want string }{
		{"completed", "success", "passed"},
		{"completed", "neutral", "passed"},
		{"completed", "skipped", "passed"},
		{"completed", "failure", "failed"},
		{"completed", "timed_out", "failed"},
		{"completed", "cancelled", "failed"},
		{"completed", "action_required", "failed"},
		{"completed", "startup_failure", "failed"},
		{"completed", "stale", "failed"},
		{"completed", "something_new", "failed"},
		{"queued", "", "pending"},
		{"in_progress", "", "pending"},
		{"waiting", "", "pending"},
		{"pending", "", "pending"},
	}

	for _, tc := range runs {
		assert.Equal(t, tc.want, runState(tc.status, tc.conclusion), "%s/%s", tc.status, tc.conclusion)
	}

	statuses := []struct{ state, want string }{
		{"success", "passed"}, {"pending", "pending"}, {"failure", "failed"}, {"error", "failed"}, {"odd", "failed"},
	}

	for _, tc := range statuses {
		assert.Equal(t, tc.want, statusState(tc.state), tc.state)
	}
}

// refusedToken is a personal access token the fake refuses the Checks API.
const refusedToken = "ghp_refused0123456789"

func job(id, run int64, attempt int, name, conclusion string) forgefake.CheckRun {
	return forgefake.CheckRun{
		ID: id, App: "github-actions", Run: run, Attempt: attempt, Name: name, Status: "completed", Conclusion: conclusion,
	}
}

// actionsChecks returns a fake whose pull request 1 has a head with the jobs
// of four workflow runs, another app's check run and two statuses, a client,
// and the head. The fake refuses refusedToken the Checks API.
func actionsChecks(t *testing.T) (*forgefake.Fake, *Client, string) {
	t.Helper()

	bare, head := newBareRepo(t)
	f, c := newFake(t, bare, nil)
	f.Deny(refusedToken, "checks")
	f.SetChecks(head,
		// A matrix whose failed job was re-run alone: the newest attempt lists
		// every job of the run anew.
		job(101, 10, 1, "build (ubuntu-latest)", "success"),
		job(102, 10, 1, "build (macos-latest)", "failure"),
		job(111, 10, 2, "build (ubuntu-latest)", "success"),
		job(112, 10, 2, "build (macos-latest)", "success"),
		// Two runs of one workflow on the commit.
		job(201, 20, 1, "label", "skipped"),
		job(211, 21, 1, "label", "failure"),
		forgefake.CheckRun{ID: 300, App: "codecov", Name: "coverage", Status: "completed", Conclusion: "success"},
	)
	f.AddRun(head, 30) // awaiting approval
	f.SetStatus(head, forgefake.Status{Context: "ci/docs", State: "success"}, forgefake.Status{Context: "ci/deploy", State: "error"})
	openPR(t, c)

	return f, c, head
}

func TestChecksFallBackWhenTheChecksAPIIsRefused(t *testing.T) {
	f, c, _ := actionsChecks(t)

	want, err := c.Observe(t.Context(), fakeRepo, fakeAccess, Query{Number: 1})
	require.NoError(t, err)
	assert.Equal(t, "failed", want.Checks)
	assert.Equal(t, []string{"ci/deploy", "label"}, want.Failed)
	require.Zero(t, sent(f, http.MethodGet, "/api/v3/repos/o/n/actions/runs"))

	got, err := c.Observe(t.Context(), fakeRepo, Access{Token: refusedToken}, Query{Number: 1})
	require.NoError(t, err)
	assert.Equal(t, want, got)
	assert.Equal(t, 1, sent(f, http.MethodGet, "/api/v3/repos/o/n/actions/runs"), "the second way")
}

func TestIgnoredChecksMatchUnderBothCredentials(t *testing.T) {
	_, c, _ := actionsChecks(t)

	for _, a := range []Access{fakeAccess, {Token: refusedToken}} {
		o, err := c.Observe(t.Context(), fakeRepo, a, Query{Number: 1, IgnoredChecks: []string{"label", "ci/deploy"}})
		require.NoError(t, err)
		assert.Equal(t, "passed", o.Checks)
		assert.Empty(t, o.Failed)
	}
}

// Once GitHub refused the check runs, a refusal of the workflow runs or the
// statuses fails the look with the permissions each kind of credential needs
// and GitHub's status.
func TestChecksRefusedBothWaysNamesThePermissions(t *testing.T) {
	bare, _ := newBareRepo(t)

	tests := []struct {
		name     string
		deny     []string
		notFound string // a route that answers 404
		status   int
	}{
		{name: "the workflow runs refused", deny: []string{"checks", "actions"}, status: http.StatusForbidden},
		{name: "the statuses refused", deny: []string{"checks", "statuses"}, status: http.StatusForbidden},
		{
			name: "the workflow runs not found", deny: []string{"checks"}, notFound: "/api/v3/repos/o/n/actions/runs",
			status: http.StatusNotFound,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := forgefake.New(t, bare)
			f.Deny(refusedToken, tc.deny...)

			mux := http.NewServeMux()
			mux.Handle("/", f)

			if tc.notFound != "" {
				mux.Handle("GET "+tc.notFound, answer(http.StatusNotFound, nil, "Not Found"))
			}

			c := newTestClient(t, nil, fakeRepo.Host, mux, nil)
			openPR(t, c)

			_, err := c.Observe(t.Context(), fakeRepo, Access{Token: refusedToken}, Query{Number: 1})
			require.Error(t, err)

			for _, datum := range []string{"Checks", "Actions", "Commit statuses", strconv.Itoa(tc.status)} {
				assert.Contains(t, err.Error(), datum)
			}

			he, ok := errors.AsType[*httpError](err)
			require.True(t, ok, "%v", err)
			assert.Equal(t, tc.status, he.status)
			assert.NotContains(t, err.Error(), refusedToken)
		})
	}
}

func TestARefusalIsRemembered(t *testing.T) {
	bare, head := newBareRepo(t)
	f, c := newFake(t, bare, nil)
	f.Deny(refusedToken, "checks")
	openPR(t, c)

	look := func(token string) error {
		_, err := c.Observe(t.Context(), fakeRepo, Access{Token: token}, Query{Number: 1})

		return err
	}
	asked := func() int { return sent(f, http.MethodGet, "/api/v3/repos/o/n/commits/"+head+"/check-runs") }

	require.NoError(t, look(refusedToken))
	require.NoError(t, look(refusedToken))
	assert.Equal(t, 1, asked(), "a second look asks the refused route no more")

	require.NoError(t, look(testToken))
	assert.Equal(t, 2, asked(), "another token asks it")

	const lacking = "ghp_lacking0123456789"

	f.Deny(lacking, "checks", "actions")
	require.Error(t, look(lacking))

	f.Deny(lacking, "checks")
	require.NoError(t, look(lacking), "a refusal of the second way is not remembered")
}

// A refusal of the check runs worded otherwise, and a 404, fail the look, take
// no second way and are not remembered.
func TestOtherCheckRunsRefusalsFailTheLook(t *testing.T) {
	bare, head := newBareRepo(t)
	checkRuns := "/api/v3/repos/o/n/commits/" + head + "/check-runs"

	for _, tc := range []struct {
		name    string
		status  int
		message string
	}{
		{"a 403 worded otherwise", http.StatusForbidden, "Resource not accessible by integration"},
		{"a 404", http.StatusNotFound, "Not Found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := forgefake.New(t, bare)

			var refuse atomic.Bool

			refuse.Store(true)

			mux := http.NewServeMux()
			mux.Handle("/", f)
			mux.HandleFunc("GET "+checkRuns, func(w http.ResponseWriter, r *http.Request) {
				if refuse.Load() {
					answer(tc.status, nil, tc.message).ServeHTTP(w, r)

					return
				}

				f.ServeHTTP(w, r)
			})

			c := newTestClient(t, nil, fakeRepo.Host, mux, nil)
			openPR(t, c)

			_, err := c.Observe(t.Context(), fakeRepo, fakeAccess, Query{Number: 1})
			he, ok := errors.AsType[*httpError](err)
			require.True(t, ok, "%v", err)
			assert.Equal(t, tc.status, he.status)

			refuse.Store(false)

			_, err = c.Observe(t.Context(), fakeRepo, fakeAccess, Query{Number: 1})
			require.NoError(t, err)
			assert.Equal(t, 1, sent(f, http.MethodGet, checkRuns), "asked again")
			assert.Zero(t, sent(f, http.MethodGet, "/api/v3/repos/o/n/actions/runs"), "no second way")
		})
	}
}

func TestReadChecksSaysWhichWay(t *testing.T) {
	f, c, head := actionsChecks(t)

	jobs, err := c.ReadChecks(t.Context(), fakeRepo, fakeAccess, head)
	require.NoError(t, err)
	assert.False(t, jobs, "by the Checks API")
	assert.Equal(t, 1, sent(f, http.MethodGet, "/api/v3/repos/o/n/commits/"+head+"/check-runs"))

	jobs, err = c.ReadChecks(t.Context(), fakeRepo, Access{Token: refusedToken}, head)
	require.NoError(t, err)
	assert.True(t, jobs, "as Actions jobs")

	const lacking = "ghp_lacking0123456789"

	f.Deny(lacking, "checks", "actions")

	_, err = c.ReadChecks(t.Context(), fakeRepo, Access{Token: lacking}, head)
	require.Error(t, err)
}
