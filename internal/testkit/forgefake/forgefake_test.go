package forgefake_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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
