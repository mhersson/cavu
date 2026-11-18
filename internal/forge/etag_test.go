package forge

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestETagMakesARepeatFree(t *testing.T) {
	var inm []string

	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inm = append(inm, r.Header.Get("If-None-Match"))

		if r.Header.Get("If-None-Match") == `"v1"` {
			w.WriteHeader(http.StatusNotModified)

			return
		}

		w.Header().Set("ETag", `"v1"`)
		_, _ = w.Write([]byte(`{"full_name":"o/n"}`))
	})
	c := newTestClient(t, nil, "api.github.com", h, nil)

	first, err := c.rest(t.Context(), testRepo, testToken, http.MethodGet, testRepo.path(), nil)
	require.NoError(t, err)

	second, err := c.rest(t.Context(), testRepo, testToken, http.MethodGet, testRepo.path(), nil)
	require.NoError(t, err)

	assert.Equal(t, []string{"", `"v1"`}, inm)
	assert.JSONEq(t, `{"full_name":"o/n"}`, string(second.body))
	assert.Equal(t, first.body, second.body)
}

type fakeOrigin struct {
	etag    string
	body    string
	status  int
	lastINM string
}

func (o *fakeOrigin) RoundTrip(req *http.Request) (*http.Response, error) {
	o.lastINM = req.Header.Get("If-None-Match")

	h := http.Header{}
	status := o.status

	if status == 0 {
		status = http.StatusOK
	}

	if o.etag != "" {
		h.Set("ETag", o.etag)
	}

	if o.etag != "" && o.lastINM == o.etag && req.Method == http.MethodGet && status == http.StatusOK {
		return &http.Response{StatusCode: http.StatusNotModified, Header: h, Body: http.NoBody, Request: req}, nil
	}

	h.Set("Content-Type", "application/json")

	return &http.Response{
		StatusCode: status, Status: fmt.Sprint(status), Header: h, Request: req,
		Body: io.NopCloser(strings.NewReader(o.body)),
	}, nil
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func roundTrip(t *testing.T, rt http.RoundTripper, method, url string) (*http.Response, string) {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), method, url, nil)
	require.NoError(t, err)

	resp, err := rt.RoundTrip(req)
	require.NoError(t, err)

	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	_ = resp.Body.Close()

	return resp, string(b)
}

func TestETagCache(t *testing.T) {
	const u = "https://api.github.com/repos/o/r"

	t.Run("a 304 serves the cached body as a 200", func(t *testing.T) {
		o := &fakeOrigin{etag: `"a"`, body: `{"x":1}`}
		rt := newETagTransport(o)

		roundTrip(t, rt, http.MethodGet, u)
		assert.Empty(t, o.lastINM)

		resp, body := roundTrip(t, rt, http.MethodGet, u)
		assert.Equal(t, `"a"`, o.lastINM)
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		assert.JSONEq(t, `{"x":1}`, body)
		assert.Equal(t, `"a"`, resp.Header.Get("ETag"))
		assert.NotNil(t, resp.Request)
	})

	t.Run("a new etag replaces the entry", func(t *testing.T) {
		o := &fakeOrigin{etag: `"a"`, body: `1`}
		rt := newETagTransport(o)

		roundTrip(t, rt, http.MethodGet, u)

		o.etag, o.body = `"b"`, `2`

		_, body := roundTrip(t, rt, http.MethodGet, u)
		require.Equal(t, "2", body)

		roundTrip(t, rt, http.MethodGet, u)
		assert.Equal(t, `"b"`, o.lastINM)
	})

	t.Run("a POST carries no If-None-Match", func(t *testing.T) {
		o := &fakeOrigin{etag: `"a"`, body: `1`}
		rt := newETagTransport(o)

		roundTrip(t, rt, http.MethodGet, u)
		roundTrip(t, rt, http.MethodPost, u)
		assert.Empty(t, o.lastINM)
	})

	t.Run("a Range request bypasses the cache", func(t *testing.T) {
		o := &fakeOrigin{etag: `"a"`, body: `1`}
		rt := newETagTransport(o)

		roundTrip(t, rt, http.MethodGet, u)

		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, u, nil)
		require.NoError(t, err)
		req.Header.Set("Range", "bytes=-10")

		resp, err := rt.RoundTrip(req)
		require.NoError(t, err)

		_ = resp.Body.Close()

		assert.Empty(t, o.lastINM)
	})

	t.Run("a 304 without an entry is an error", func(t *testing.T) {
		rt := newETagTransport(roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusNotModified, Header: http.Header{}, Body: http.NoBody, Request: r}, nil
		}))

		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, u, nil)
		require.NoError(t, err)

		resp, err := rt.RoundTrip(req)
		if err == nil {
			_ = resp.Body.Close()
		}

		require.Error(t, err)
	})

	t.Run("a 2 MiB body is served but not stored", func(t *testing.T) {
		big := strings.Repeat("x", 2<<20)
		o := &fakeOrigin{etag: `"a"`, body: big}
		rt := newETagTransport(o)

		_, body := roundTrip(t, rt, http.MethodGet, u)
		require.Equal(t, big, body)

		roundTrip(t, rt, http.MethodGet, u)
		assert.Empty(t, o.lastINM, "the oversized body was stored")
	})

	t.Run("the least recently used of 257 entries is evicted", func(t *testing.T) {
		o := &fakeOrigin{etag: `"a"`, body: `1`}
		rt := newETagTransport(o)

		for i := range 256 {
			roundTrip(t, rt, http.MethodGet, fmt.Sprintf("%s/%d", u, i))
		}

		roundTrip(t, rt, http.MethodGet, u+"/0") // a hit makes it the most recent
		roundTrip(t, rt, http.MethodGet, u+"/256")

		roundTrip(t, rt, http.MethodGet, u+"/0")
		assert.Equal(t, `"a"`, o.lastINM, "the recently used entry was evicted")

		roundTrip(t, rt, http.MethodGet, u+"/1")
		assert.Empty(t, o.lastINM, "the least recently used entry survived")
	})

	t.Run("an error response keeps the cached entry", func(t *testing.T) {
		o := &fakeOrigin{etag: `"a"`, body: `1`}
		rt := newETagTransport(o)

		roundTrip(t, rt, http.MethodGet, u)

		o.status = http.StatusBadGateway

		roundTrip(t, rt, http.MethodGet, u)

		o.status = 0

		roundTrip(t, rt, http.MethodGet, u)
		assert.Equal(t, `"a"`, o.lastINM, "the error evicted the entry")
	})

	t.Run("a cache hit hands out independent headers", func(t *testing.T) {
		o := &fakeOrigin{etag: `"a"`, body: `1`}
		rt := newETagTransport(o)

		roundTrip(t, rt, http.MethodGet, u)

		r1, _ := roundTrip(t, rt, http.MethodGet, u)
		r1.Header.Set("ETag", "mutated")

		r2, _ := roundTrip(t, rt, http.MethodGet, u)
		assert.Equal(t, `"a"`, r2.Header.Get("ETag"), "a hit shares headers with an earlier response")
	})
}
