package forge

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mhersson/cavu/internal/testkit/forgefake"
	"github.com/mhersson/cavu/internal/testkit/gitrepo"
)

const (
	testToken   = "ghp_testtoken0123456789"
	testVersion = "1.2.3"
)

var errUnexpectedHost = errors.New("request to an unexpected host")

// routeTransport sends requests for one API host to a local server and fails
// the test for any other host, so no request leaves the process.
type routeTransport struct {
	t      *testing.T
	host   string
	target *url.URL
	next   *http.Transport
	others map[string]*url.URL // host to server, for a redirect's target
}

func (rt *routeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	target := rt.target

	if req.URL.Host != rt.host {
		other, ok := rt.others[req.URL.Host]
		if !ok {
			rt.t.Errorf("request to unexpected host %q", req.URL.Host)

			return nil, errUnexpectedHost
		}

		target = other
	}

	out := req.Clone(req.Context())
	out.URL.Scheme = target.Scheme
	out.URL.Host = target.Host

	resp, err := rt.next.RoundTrip(out)
	if err != nil {
		return nil, err
	}

	resp.Request = req

	return resp, nil
}

// newTestClient returns a client whose requests for the API host reach h, and
// for each host of others its handler. now is the client's clock, nil for the
// real one.
func newTestClient(t *testing.T, now func() time.Time, host string, h http.Handler, others map[string]http.Handler) *Client {
	t.Helper()

	rt := &routeTransport{t: t, host: host, target: serve(t, h), next: &http.Transport{}, others: map[string]*url.URL{}}
	for oh, oHandler := range others {
		rt.others[oh] = serve(t, oHandler)
	}

	t.Cleanup(rt.next.CloseIdleConnections)

	c, err := New(Options{Transport: rt, Now: now, Version: testVersion})
	require.NoError(t, err)

	return c
}

func serve(t *testing.T, h http.Handler) *url.URL {
	t.Helper()

	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	u, err := url.Parse(srv.URL)
	require.NoError(t, err)

	return u
}

// fakeRepo is the repository a forgefake serves, on a GitHub Enterprise host.
var (
	fakeRepo   = Repo{Host: "ghe.example", Owner: "o", Name: "n"}
	fakeAccess = Access{Token: testToken}
)

// newBareRepo returns a bare repository holding main and the branches
// cavu/x and cavu/y, and cavu/x's head.
func newBareRepo(t *testing.T) (dir, head string) {
	t.Helper()

	work := gitrepo.New(t, map[string]string{"a.txt": "a\n"})
	gitrepo.Git(t, work, "checkout", "-q", "-b", "cavu/y")
	gitrepo.Commit(t, work, map[string]string{"y.txt": "y\n"}, "y")
	gitrepo.Git(t, work, "checkout", "-q", "-b", "cavu/x", "main")
	head = gitrepo.Commit(t, work, map[string]string{"x.txt": "x\n"}, "x")

	return strings.TrimPrefix(gitrepo.Bare(t, work), "file://"), head
}

// newFake returns a fake forge over the bare repository at bare and a client
// that reaches it; others are further hosts, as for newTestClient.
func newFake(t *testing.T, bare string, others map[string]http.Handler) (*forgefake.Fake, *Client) {
	t.Helper()

	f := forgefake.New(t, bare)

	return f, newTestClient(t, nil, fakeRepo.Host, f, others)
}

// openPR opens the draft pull request 1 for cavu/x.
func openPR(t *testing.T, c *Client) PR {
	t.Helper()

	pr, err := c.OpenPR(t.Context(), fakeRepo, fakeAccess, NewPR{Title: "T", Body: "B", Base: "main", Head: "cavu/x"})
	require.NoError(t, err)

	return pr
}

// sent counts the requests f was sent whose method and path, query left out,
// are method and path.
func sent(f *forgefake.Fake, method, path string) int {
	n := 0

	for _, r := range f.Requests() {
		if p, _, _ := strings.Cut(r.Path, "?"); r.Method == method && p == path {
			n++
		}
	}

	return n
}

// operations are the GraphQL operations f was sent, in order.
func operations(f *forgefake.Fake) []string {
	var ops []string

	for _, r := range f.Requests() {
		if r.Operation != "" {
			ops = append(ops, r.Operation)
		}
	}

	return ops
}
