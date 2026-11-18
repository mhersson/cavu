package forge

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
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
