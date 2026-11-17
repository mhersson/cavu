package api_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mhersson/cavu/internal/api"
	"github.com/mhersson/cavu/internal/auth"
	"github.com/mhersson/cavu/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	user     = "ada"
	password = "correct horse battery"
	day      = 24 * time.Hour
	csrf     = "X-CAVU-CSRF"
	cookie   = "cavu_session"
)

// clock is the time the login service reads, moved by the test.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.now
}

func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.now = c.now.Add(d)
}

// fixture is the handler over a real store with one user, served on a port the kernel picks.
type fixture struct {
	url     string
	store   *store.Store
	auth    *auth.Service
	clock   *clock
	session *http.Cookie // the cookie of the newest login
}

func newServer(t *testing.T) *fixture {
	t.Helper()

	s, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "cavu.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	c := &clock{now: time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)}
	svc := auth.NewService(s, c.Now)
	require.NoError(t, svc.SetUser(t.Context(), user, password))

	srv := httptest.NewServer(api.New(api.Deps{Store: s, Auth: svc, Version: "1.2.3"}))
	t.Cleanup(srv.Close)

	return &fixture{url: srv.URL, store: s, auth: svc, clock: c}
}

// request builds a request to route, "METHOD /path", with body.
func (f *fixture) request(t *testing.T, route, body string) *http.Request {
	t.Helper()

	method, path, ok := strings.Cut(route, " ")
	require.True(t, ok, route)

	r, err := http.NewRequestWithContext(t.Context(), method, f.url+path, strings.NewReader(body))
	require.NoError(t, err)

	return r
}

// send sends r over a connection of its own and returns the response and its body.
func send(t *testing.T, r *http.Request) (*http.Response, string) {
	t.Helper()

	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}

	resp, err := client.Do(r)
	require.NoError(t, err)

	defer resp.Body.Close()

	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	return resp, string(b)
}

func credentials(name, pass string) string {
	b, _ := json.Marshal(map[string]string{"name": name, "password": pass})

	return string(b)
}

// login logs the user in, keeps the session's cookie and returns it.
func (f *fixture) login(t *testing.T) *http.Cookie {
	t.Helper()

	resp, body := send(t, f.request(t, "POST /api/auth/login", credentials(user, password)))
	require.Equal(t, http.StatusNoContent, resp.StatusCode, body)

	f.session = sessionCookie(t, resp)

	return f.session
}

// withCookie adds the session cookie with value to r, and the CSRF header when csrfHeader is set.
func withCookie(r *http.Request, value string, csrfHeader bool) *http.Request {
	r.Header.Add("Cookie", cookie+"="+value)

	if csrfHeader {
		r.Header.Set(csrf, "1")
	}

	return r
}

func sessionCookie(t *testing.T, resp *http.Response) *http.Cookie {
	t.Helper()

	i := slices.IndexFunc(resp.Cookies(), func(c *http.Cookie) bool { return c.Name == cookie })
	require.GreaterOrEqual(t, i, 0, "the response sets the session cookie")

	return resp.Cookies()[i]
}

// alive reports whether the cookie names a live session.
func (f *fixture) alive(t *testing.T, c *http.Cookie) bool {
	t.Helper()

	ok, _, err := f.auth.Session(t.Context(), c.Value)
	require.NoError(t, err)

	return ok
}

// assertError checks that body is an error as every route sends one, one line under "error".
func assertError(t *testing.T, body string) {
	t.Helper()

	var e map[string]string
	require.NoError(t, json.Unmarshal([]byte(body), &e), body)
	require.Len(t, e, 1, body)
	assert.NotEmpty(t, e["error"], body)
	assert.NotContains(t, e["error"], "\n")
}

func TestHealthNeedsNoAuthority(t *testing.T) {
	f := newServer(t)

	resp, body := send(t, f.request(t, "GET /api/health", ""))

	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.JSONEq(t, `{"status": "ok", "version": "1.2.3"}`, body)
}

func TestLoginSetsTheCookie(t *testing.T) {
	f := newServer(t)

	for _, tc := range []struct {
		name, proto string
		secure      bool
	}{
		{"over plain HTTP", "", false},
		{"behind a proxy that ends TLS", "https", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := f.request(t, "POST /api/auth/login", credentials(user, password))
			if tc.proto != "" {
				r.Header.Set("X-Forwarded-Proto", tc.proto)
			}

			resp, body := send(t, r)
			require.Equal(t, http.StatusNoContent, resp.StatusCode, body)

			c := sessionCookie(t, resp)
			assert.True(t, c.HttpOnly)
			assert.Equal(t, http.SameSiteStrictMode, c.SameSite)
			assert.Equal(t, "/", c.Path)
			assert.Equal(t, tc.secure, c.Secure)
			assert.Equal(t, f.clock.Now().Add(30*day), c.Expires)
			assert.True(t, f.alive(t, c), "the cookie names a session")
		})
	}

	t.Run("a wrong password, then the limiter over several connections", func(t *testing.T) {
		for range 3 {
			resp, body := send(t, f.request(t, "POST /api/auth/login", credentials(user, "wrong password!")))
			require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
			assert.Empty(t, resp.Cookies())
			assertError(t, body)
		}

		f.clock.add(300 * time.Millisecond) // the third failure blocks for a second

		resp, body := send(t, f.request(t, "POST /api/auth/login", credentials(user, password)))
		require.Equal(t, http.StatusTooManyRequests, resp.StatusCode)
		assert.Empty(t, resp.Cookies())
		assert.Equal(t, "1", resp.Header.Get("Retry-After"), "whole seconds, rounded up")
		assertError(t, body)
	})
}

func TestSessionCookieIsRenewed(t *testing.T) {
	for _, tc := range []struct {
		name    string
		after   time.Duration
		renewed bool
	}{
		{"less than 29 days left", 2 * day, true},
		{"more than 29 days left", time.Hour, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newServer(t)
			c := f.login(t)
			f.clock.add(tc.after)

			resp, body := send(t, withCookie(f.request(t, "POST /api/auth/logout", ""), c.Value, true))
			require.Equal(t, http.StatusNoContent, resp.StatusCode, body)

			// Logout then clears the cookie with an expiry in the past, so a renewal is one that expires later.
			var renewals []*http.Cookie

			for _, got := range resp.Cookies() {
				if got.Name == cookie && got.Expires.After(f.clock.Now()) {
					renewals = append(renewals, got)
				}
			}

			if !tc.renewed {
				assert.Empty(t, renewals)

				return
			}

			require.Len(t, renewals, 1)
			assert.Equal(t, c.Value, renewals[0].Value)
			assert.Equal(t, f.clock.Now().Add(30*day), renewals[0].Expires)
			assert.True(t, renewals[0].HttpOnly)
			assert.Equal(t, http.SameSiteStrictMode, renewals[0].SameSite)
			assert.Equal(t, "/", renewals[0].Path)
		})
	}
}

// open are the routes served without authority.
var open = []string{"GET /api/health", "POST /api/auth/login"}

func TestRequestsWithoutAuthority(t *testing.T) {
	const (
		logout = "POST /api/auth/logout"
		login  = "POST /api/auth/login"
	)

	t.Run("nothing sent, to every route but the open ones", func(t *testing.T) {
		f := newServer(t)

		for _, route := range api.Routes() {
			if slices.Contains(open, route) {
				continue
			}

			resp, body := send(t, f.request(t, route, ""))
			assert.Equal(t, http.StatusUnauthorized, resp.StatusCode, route)
			assertError(t, body)
		}
	})

	token := func(t *testing.T, f *fixture) string {
		t.Helper()

		tok, err := f.auth.NewAPIToken(t.Context(), "ci")
		require.NoError(t, err)

		return tok
	}

	cases := []struct {
		name  string
		route string
		body  string
		add   func(t *testing.T, f *fixture, r *http.Request) // what the request carries
		want  int
		kept  bool // the session of the newest login is still alive afterwards
	}{
		{
			name: "the cookie and the CSRF header on a POST", route: logout, want: http.StatusNoContent,
			add: func(t *testing.T, f *fixture, r *http.Request) { withCookie(r, f.login(t).Value, true) },
		},
		{
			name: "the cookie and no CSRF header on a POST", route: logout, want: http.StatusForbidden, kept: true,
			add: func(t *testing.T, f *fixture, r *http.Request) { withCookie(r, f.login(t).Value, false) },
		},
		{
			name: "a bearer token on a POST with no CSRF header", route: logout, want: http.StatusNoContent,
			add: func(t *testing.T, f *fixture, r *http.Request) { r.Header.Set("Authorization", "Bearer "+token(t, f)) },
		},
		{
			name: "a bearer token sent as the cookie", route: logout, want: http.StatusUnauthorized,
			add: func(t *testing.T, f *fixture, r *http.Request) { withCookie(r, token(t, f), true) },
		},
		{
			name: "an unknown bearer token", route: logout, want: http.StatusUnauthorized,
			add: func(t *testing.T, f *fixture, r *http.Request) {
				token(t, f)
				r.Header.Set("Authorization", "Bearer cavu_"+strings.Repeat("A", 43))
			},
		},
		{
			name: "a cookie after logout", route: logout, want: http.StatusUnauthorized,
			add: func(t *testing.T, f *fixture, r *http.Request) {
				c := f.login(t)
				resp, body := send(t, withCookie(f.request(t, logout, ""), c.Value, true))
				require.Equal(t, http.StatusNoContent, resp.StatusCode, body)
				withCookie(r, c.Value, true)
			},
		},
		{
			name: "a body of 5 MB", route: login, want: http.StatusRequestEntityTooLarge,
			body: credentials(strings.Repeat("a", 5<<20), password),
		},
		{
			name: "JSON with an unknown field", route: login, want: http.StatusBadRequest,
			body: `{"name": "ada", "password": "correct horse battery", "admin": true}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newServer(t)
			r := f.request(t, tc.route, tc.body)

			if tc.add != nil {
				tc.add(t, f, r)
			}

			resp, body := send(t, r)
			require.Equal(t, tc.want, resp.StatusCode, body)

			if tc.want >= http.StatusBadRequest {
				assertError(t, body)
				assert.Empty(t, resp.Cookies(), "a refused request sets no cookie")
			}

			if tc.kept {
				assert.True(t, f.alive(t, f.session), "nothing is written")
			}
		})
	}
}

func TestRouteLimit(t *testing.T) {
	routes := api.Routes()
	f := newServer(t)

	assert.LessOrEqual(t, len(routes), 25)

	for i, route := range routes {
		assert.NotContains(t, routes[:i], route, "a route is listed once")

		// Sent without authority, a route that needs it answers 401 before its
		// handler runs, so a 404 or 405 is the mux finding no route.
		resp, _ := send(t, f.request(t, route, ""))
		assert.NotContains(t, []int{http.StatusNotFound, http.StatusMethodNotAllowed}, resp.StatusCode, route)
	}

	for _, route := range []string{"GET /api/nothing", "DELETE /api/auth/login", "GET /"} {
		resp, body := send(t, f.request(t, route, ""))
		assert.Equal(t, http.StatusNotFound, resp.StatusCode, "%s: the mux's own answer is seen", route)
		assertError(t, body)
	}
}

func TestBrokenBodiesAreRefused(t *testing.T) {
	f := newServer(t)

	for _, body := range []string{"", "not json", `{"name": 1}`, `["ada"]`} {
		resp, got := send(t, f.request(t, "POST /api/auth/login", body))
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode, strconv.Quote(body))
		assert.Equal(t, "application/json", resp.Header.Get("Content-Type"))
		assertError(t, got)
	}
}

func TestInternalErrorsHoldNoDetail(t *testing.T) {
	f := newServer(t)
	require.NoError(t, f.store.Close())

	_, _, cause := f.store.User(t.Context())
	require.Error(t, cause)

	resp, body := send(t, f.request(t, "POST /api/auth/login", credentials(user, password)))

	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)
	assertError(t, body)
	assert.NotContains(t, body, cause.Error())
}
