package forgefake_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/cavu/internal/testkit/forgefake"
	"github.com/mhersson/cavu/internal/testkit/gitrepo"
)

func TestMain(m *testing.M) { gitrepo.Main(m) }

// fixture is a fake forge on a test server whose repository has the branch
// cavu/x one commit ahead of main.
type fixture struct {
	t          *testing.T
	fake       *forgefake.Fake
	url        string
	work, bare string
	head       string // cavu/x
	token      string // the bearer token a call sends, none when ""
}

func newFixture(t *testing.T) *fixture {
	t.Helper()

	work := gitrepo.New(t, map[string]string{"a.txt": "a\n"})
	gitrepo.Git(t, work, "checkout", "-q", "-b", "cavu/x")
	head := gitrepo.Commit(t, work, map[string]string{"b.txt": "b\n"}, "b")
	bare := strings.TrimPrefix(gitrepo.Bare(t, work), "file://")

	f := forgefake.New(t, bare)
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)

	return &fixture{t: t, fake: f, url: srv.URL, work: work, bare: bare, head: head}
}

// call sends a request with body as JSON unless it is nil, and returns the
// answer's status, headers and JSON.
func (fx *fixture) call(method, path string, body any) (int, http.Header, any) {
	fx.t.Helper()

	var rd io.Reader = http.NoBody

	if body != nil {
		b, err := json.Marshal(body)
		require.NoError(fx.t, err)

		rd = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(fx.t.Context(), method, fx.url+path, rd)
	require.NoError(fx.t, err)

	if fx.token != "" {
		req.Header.Set("Authorization", "Bearer "+fx.token)
	}

	resp, err := http.DefaultClient.Do(req)
	require.NoError(fx.t, err)

	defer func() { _ = resp.Body.Close() }()

	var out any

	_ = json.NewDecoder(resp.Body).Decode(&out)

	return resp.StatusCode, resp.Header, out
}

func (fx *fixture) open(draft bool) (int, any) {
	fx.t.Helper()

	status, _, out := fx.call(http.MethodPost, "/api/v3/repos/o/n/pulls",
		map[string]any{"title": "T", "body": "B", "base": "main", "head": "cavu/x", "draft": draft})

	return status, out
}

func field(t *testing.T, v any, path ...string) any {
	t.Helper()

	for _, k := range path {
		m, ok := v.(map[string]any)
		require.True(t, ok, "%v is no object at %q", v, k)

		v = m[k]
	}

	return v
}

// ids are the ids of the items in the list at key of v.
func ids(t *testing.T, v any, key string) []float64 {
	t.Helper()

	items, ok := field(t, v, key).([]any)
	require.True(t, ok, "%v holds no list at %q", v, key)

	var out []float64
	for _, it := range items {
		out = append(out, field(t, it, "id").(float64))
	}

	return out
}

// The workflow runs of a commit are those its check runs belong to and those
// added without a job; a run's jobs are its check runs. Both lists of check
// runs answer each run's newest attempt only, and every attempt for filter=all.
func TestActionsRoutes(t *testing.T) {
	fx := newFixture(t)
	fx.fake.SetChecks(fx.head,
		forgefake.CheckRun{ID: 1, Run: 10, Name: "build", Status: "completed", Conclusion: "failure", URL: "https://ghe.example/job/1"},
		forgefake.CheckRun{ID: 2, Run: 10, Attempt: 2, Name: "build", Status: "completed", Conclusion: "success"},
		forgefake.CheckRun{ID: 3, Run: 11, Name: "lint", Status: "in_progress"},
		forgefake.CheckRun{ID: 4, App: "codecov", Name: "coverage", Status: "completed", Conclusion: "success"},
	)
	fx.fake.AddRun(fx.head, 12)

	_, _, runs := fx.call(http.MethodGet, "/api/v3/repos/o/n/actions/runs?head_sha="+fx.head, nil)
	assert.ElementsMatch(t, []float64{10, 11, 12}, ids(t, runs, "workflow_runs"))

	_, _, none := fx.call(http.MethodGet, "/api/v3/repos/o/n/actions/runs?head_sha=0123abc", nil)
	assert.Empty(t, ids(t, none, "workflow_runs"))

	for _, tc := range []struct {
		path, key string
		want      []float64
	}{
		{"/actions/runs/10/jobs", "jobs", []float64{2}},
		{"/actions/runs/10/jobs?filter=all", "jobs", []float64{1, 2}},
		{"/actions/runs/11/jobs", "jobs", []float64{3}},
		{"/actions/runs/12/jobs", "jobs", nil},
		{"/commits/" + fx.head + "/check-runs", "check_runs", []float64{2, 3, 4}},
		{"/commits/" + fx.head + "/check-runs?filter=all", "check_runs", []float64{1, 2, 3, 4}},
	} {
		status, _, out := fx.call(http.MethodGet, "/api/v3/repos/o/n"+tc.path, nil)
		require.Equal(t, http.StatusOK, status, tc.path)
		assert.Equal(t, tc.want, ids(t, out, tc.key), tc.path)
	}

	_, _, out := fx.call(http.MethodGet, "/api/v3/repos/o/n/actions/runs/10/jobs?filter=all", nil)
	jobs := field(t, out, "jobs").([]any)
	assert.Equal(t, "build", field(t, jobs[0], "name"))
	assert.Equal(t, "completed", field(t, jobs[0], "status"))
	assert.Equal(t, "failure", field(t, jobs[0], "conclusion"))
	assert.Equal(t, "https://ghe.example/job/1", field(t, jobs[0], "html_url"))

	_, _, out = fx.call(http.MethodGet, "/api/v3/repos/o/n/actions/runs/11/jobs", nil)
	assert.Nil(t, field(t, field(t, out, "jobs").([]any)[0], "conclusion"), "not completed")
}

// A token denied a permission is refused, with 403 and a message, on each
// route that needs it and served on the others; a later Deny replaces the set,
// and another token is served everywhere.
func TestDeny(t *testing.T) {
	fx := newFixture(t)
	fx.fake.SetChecks(fx.head, forgefake.CheckRun{ID: 1, Run: 10, Name: "build", Status: "completed", Conclusion: "success"})
	fx.fake.SetJobLog(1, "the log")

	routes := map[string][]string{
		"checks":   {"/commits/" + fx.head + "/check-runs"},
		"statuses": {"/commits/" + fx.head + "/status"},
		"actions":  {"/actions/runs?head_sha=" + fx.head, "/actions/runs/10/jobs", "/actions/jobs/1/logs"},
	}

	check := func(token string, denied ...string) {
		t.Helper()

		fx.token = token

		for perm, paths := range routes {
			for _, p := range paths {
				status, _, out := fx.call(http.MethodGet, "/api/v3/repos/o/n"+p, nil)

				if slices.Contains(denied, perm) {
					assert.Equal(t, http.StatusForbidden, status, "%s with token %q", p, token)
					assert.NotEmpty(t, field(t, out, "message"), p)

					continue
				}

				assert.Equal(t, http.StatusOK, status, "%s with token %q", p, token)
			}
		}
	}

	fx.fake.Deny("refused", "checks", "statuses")
	check("refused", "checks", "statuses")
	check("other")

	fx.fake.Deny("refused", "actions")
	check("refused", "actions")
}

// A pull request's head is its branch's newest commit in the bare repository
// at the time of the request.
func TestPullHeadIsTheBranchsHead(t *testing.T) {
	fx := newFixture(t)

	status, pr := fx.open(true)
	require.Equal(t, http.StatusCreated, status)
	assert.InDelta(t, 1, field(t, pr, "number"), 0)
	assert.Equal(t, true, field(t, pr, "draft"))
	assert.Equal(t, []forgefake.Pull{{
		Number: 1, NodeID: field(t, pr, "node_id").(string), Title: "T", Body: "B", Base: "main", Head: "cavu/x",
		Draft: true, State: "open",
	}}, fx.fake.Pulls())

	_, _, got := fx.call(http.MethodGet, "/api/v3/repos/o/n/pulls/1", nil)
	assert.Equal(t, fx.head, field(t, got, "head", "sha"))
	assert.Nil(t, field(t, got, "mergeable"), "not yet computed")

	next := gitrepo.Commit(t, fx.work, map[string]string{"c.txt": "c\n"}, "c")
	gitrepo.Git(t, fx.work, "push", "-q", fx.bare, "cavu/x")

	_, _, got = fx.call(http.MethodGet, "/api/v3/repos/o/n/pulls/1", nil)
	assert.Equal(t, next, field(t, got, "head", "sha"))

	_, _, list := fx.call(http.MethodGet, "/api/v3/repos/o/n/pulls?head=o:cavu/x&state=open", nil)
	assert.Len(t, list, 1)

	fx.fake.Close(1)

	_, _, list = fx.call(http.MethodGet, "/api/v3/repos/o/n/pulls?head=o:cavu/x&state=open", nil)
	assert.Empty(t, list)

	status, _, _ = fx.call(http.MethodPost, "/api/v3/repos/o/n/pulls",
		map[string]any{"title": "T", "base": "main", "head": "cavu/missing"})
	assert.Equal(t, http.StatusUnprocessableEntity, status, "a branch the repository lacks")
}

func TestNoDrafts(t *testing.T) {
	fx := newFixture(t)
	fx.fake.NoDrafts()

	status, _ := fx.open(true)
	assert.Equal(t, http.StatusUnprocessableEntity, status)
	assert.Empty(t, fx.fake.Pulls())

	status, pr := fx.open(false)
	require.Equal(t, http.StatusCreated, status)
	assert.Equal(t, false, field(t, pr, "draft"))
}

func TestFailAndLimit(t *testing.T) {
	fx := newFixture(t)
	graphQL := map[string]any{"query": "query Draft($id: ID!) { node(id: $id) { ... on PullRequest { isDraft } } }"}

	fx.fake.Fail(2)

	for range 2 {
		status, _, _ := fx.call(http.MethodGet, "/api/v3/repos/o/n/pulls/1", nil)
		assert.Equal(t, http.StatusBadGateway, status)
	}

	status, _, _ := fx.call(http.MethodGet, "/api/v3/repos/o/n/pulls/1", nil)
	assert.Equal(t, http.StatusNotFound, status, "answered again")

	until := time.Unix(1_900_000_000, 0)
	fx.fake.Limit(until)

	status, h, _ := fx.call(http.MethodGet, "/api/v3/repos/o/n/pulls/1", nil)
	assert.Equal(t, http.StatusForbidden, status)
	assert.Equal(t, "0", h.Get("X-RateLimit-Remaining"))
	assert.Equal(t, strconv.FormatInt(until.Unix(), 10), h.Get("X-RateLimit-Reset"))

	status, h, out := fx.call(http.MethodPost, "/api/graphql", graphQL)
	assert.Equal(t, http.StatusOK, status)
	assert.Equal(t, "0", h.Get("X-RateLimit-Remaining"))
	assert.Equal(t, strconv.FormatInt(until.Unix(), 10), h.Get("X-RateLimit-Reset"))

	errs, ok := field(t, out, "errors").([]any)
	require.True(t, ok)
	require.Len(t, errs, 1)
	assert.Equal(t, "RATE_LIMITED", field(t, errs[0], "type"))

	fx.fake.Limit(time.Time{})

	status, _, _ = fx.call(http.MethodGet, "/api/v3/repos/o/n/pulls/1", nil)
	assert.Equal(t, http.StatusNotFound, status, "lifted")

	reqs := fx.fake.Requests()
	require.Len(t, reqs, 6)
	assert.Equal(t, "GET", reqs[0].Method)
	assert.Equal(t, "/api/v3/repos/o/n/pulls/1", reqs[0].Path)
	assert.Equal(t, "Draft", reqs[4].Operation)
}

// A repository's read answers with its name, its default branch and whether
// it is private, as GitHub does.
func TestRepository(t *testing.T) {
	fx := newFixture(t)

	status, _, repo := fx.call(http.MethodGet, "/api/v3/repos/o/n", nil)
	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, "o/n", field(t, repo, "full_name"))
	assert.Equal(t, "main", field(t, repo, "default_branch"))
	assert.IsType(t, true, field(t, repo, "private"))

	reqs := fx.fake.Requests()
	require.Len(t, reqs, 1)
	assert.Equal(t, "/api/v3/repos/o/n", reqs[0].Path)
}

// The App's token routes mint a token for each request and record what it may
// do, so a test can tell a push token from an API token.
func TestTokens(t *testing.T) {
	fx := newFixture(t)

	_, _, inst := fx.call(http.MethodGet, "/api/v3/repos/o/n/installation", nil)
	id, ok := field(t, inst, "id").(float64)
	require.True(t, ok)

	path := "/api/v3/app/installations/" + strconv.Itoa(int(id)) + "/access_tokens"
	perms := []map[string]string{{"contents": "write"}, {"contents": "read", "pull_requests": "write"}}

	var minted []string

	for _, p := range perms {
		status, _, tok := fx.call(http.MethodPost, path, map[string]any{"repositories": []string{"n"}, "permissions": p})
		require.Equal(t, http.StatusCreated, status)

		minted = append(minted, field(t, tok, "token").(string))
	}

	assert.NotEqual(t, minted[0], minted[1])
	assert.Equal(t, []forgefake.Token{
		{Value: minted[0], Permissions: perms[0]},
		{Value: minted[1], Permissions: perms[1]},
	}, fx.fake.Tokens())
}

// An App that lacks a permission is refused a token that asks for it, with
// 422 and a message, and the fake mints nothing; a token without it is minted.
func TestAppLacksAPermission(t *testing.T) {
	fx := newFixture(t)
	fx.fake.Lack("workflows")

	_, _, inst := fx.call(http.MethodGet, "/api/v3/repos/o/n/installation", nil)
	id, ok := field(t, inst, "id").(float64)
	require.True(t, ok)

	path := "/api/v3/app/installations/" + strconv.Itoa(int(id)) + "/access_tokens"

	status, _, out := fx.call(http.MethodPost, path, map[string]any{"permissions": map[string]string{"contents": "write", "workflows": "write"}})
	assert.Equal(t, http.StatusUnprocessableEntity, status)
	assert.NotEmpty(t, field(t, out, "message"))
	assert.Empty(t, fx.fake.Tokens())

	status, _, _ = fx.call(http.MethodPost, path, map[string]any{"permissions": map[string]string{"contents": "write"}})
	assert.Equal(t, http.StatusCreated, status)
	assert.Len(t, fx.fake.Tokens(), 1)
}

// Each author's latest opinion leaves out reviews that only comment, as
// GitHub's list of reviewers does.
func TestLatestOpinionPerAuthor(t *testing.T) {
	fx := newFixture(t)
	status, _ := fx.open(false)
	require.Equal(t, http.StatusCreated, status)

	asks := fx.fake.Review(1, "alice", "CHANGES_REQUESTED", "Fix it.")
	fx.fake.Review(1, "alice", "COMMENTED", "One more thing.")
	fx.fake.Review(1, "bob", "CHANGES_REQUESTED", "Fix it.")
	approves := fx.fake.Review(1, "bob", "APPROVED", "Fine now.")
	fx.fake.Review(1, "carol", "COMMENTED", "Nice.")

	_, _, out := fx.call(http.MethodPost, "/api/graphql", map[string]any{
		"query": "query Observe($number: Int!) { x }", "variables": map[string]any{"number": 1},
	})

	nodes, ok := field(t, out, "data", "repository", "pullRequest", "latestOpinionatedReviews", "nodes").([]any)
	require.True(t, ok)

	var got []string
	for _, n := range nodes {
		got = append(got, field(t, n, "id").(string)+" "+field(t, n, "state").(string))
	}

	assert.Equal(t, []string{asks + " CHANGES_REQUESTED", approves + " APPROVED"}, got)
}
