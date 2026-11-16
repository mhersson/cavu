package proxy

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConnectAllowed(t *testing.T) {
	addr := echoListener(t)
	p := startProxy(t, Config{AllowHosts: []string{addr}})

	conn, br, resp := connectTo(t, p, addr)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	_, err := io.WriteString(conn, "ping\n")
	require.NoError(t, err)

	line, err := br.ReadString('\n')
	require.NoError(t, err)
	assert.Equal(t, "ping\n", line)
}

func TestConnectDeniedCallsOnDenied(t *testing.T) {
	var d deniedLog

	p := startProxy(t, Config{AllowHosts: []string{"allowed.example"}, OnDenied: d.add})

	_, _, resp := connectTo(t, p, "Example.COM.:8443")
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	assert.Equal(t, []string{"example.com:8443"}, d.all())
}

func TestDeniedWithoutCallbacks(t *testing.T) {
	p := startProxy(t, Config{})

	_, _, resp := connectTo(t, p, "example.com:443")
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
}

func TestAbsoluteFormAllowedAndDenied(t *testing.T) {
	var (
		redirected atomic.Int32
		gotAuth    atomic.Value
		gotConn    atomic.Value
	)

	up := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth.Store(r.Header.Get("Proxy-Authorization"))
		gotConn.Store(r.Header.Get("X-Hop"))

		switch r.URL.Path {
		case "/redirect":
			redirected.Add(1)
			http.Redirect(w, r, "/elsewhere", http.StatusFound)
		default:
			w.Header().Set("X-Seen", "yes")
			_, _ = io.WriteString(w, "hello")
		}
	}))

	var d deniedLog

	p := startProxy(t, Config{AllowHosts: []string{strings.TrimPrefix(up.URL, "http://")}, OnDenied: d.add})
	c := viaProxy(t, p)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, up.URL+"/ok", nil)
	require.NoError(t, err)
	req.Header.Set("Proxy-Authorization", "Basic c2VjcmV0")
	req.Header.Set("Connection", "X-Hop")
	req.Header.Set("X-Hop", "drop me")

	resp, err := c.Do(req)
	require.NoError(t, err)

	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "hello", string(body))
	assert.Equal(t, "yes", resp.Header.Get("X-Seen"))
	assert.Empty(t, gotAuth.Load())
	assert.Empty(t, gotConn.Load())

	// The redirect reaches the client; the proxy does not follow it.
	resp, err = c.Get(up.URL + "/redirect")
	require.NoError(t, err)

	_ = resp.Body.Close()

	assert.Equal(t, http.StatusFound, resp.StatusCode)
	assert.Equal(t, "/elsewhere", resp.Header.Get("Location"))
	assert.EqualValues(t, 1, redirected.Load())

	resp, err = c.Get("http://not-allowed.example/x")
	require.NoError(t, err)

	_ = resp.Body.Close()

	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	assert.Equal(t, []string{"not-allowed.example:80"}, d.all())
}

func TestAbsoluteFormHTTPSSchemeIsDenied(t *testing.T) {
	h, err := New(Config{AllowHosts: []string{"example.com:80"}})
	require.NoError(t, err)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "https://example.com:80/", nil))

	assert.Equal(t, http.StatusForbidden, rec.Code)
}

func TestOriginFormWithoutModelRouteIs404(t *testing.T) {
	p := startProxy(t, Config{})

	resp, err := http.Post(p.URL+"/v1/messages", "application/json", strings.NewReader("{}"))
	require.NoError(t, err)

	_ = resp.Body.Close()

	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestAllowlistPorts(t *testing.T) {
	for _, tc := range []struct {
		name, entry, host, port, def string
		want                         bool
	}{
		{"bare allows 443 for connect", "example.com", "example.com", "443", "443", true},
		{"bare refuses 8443", "example.com", "example.com", "8443", "443", false},
		{"bare refuses 22", "example.com", "example.com", "22", "443", false},
		{"port entry allows its port", "example.com:8443", "example.com", "8443", "443", true},
		{"port entry refuses 443", "example.com:8443", "example.com", "443", "443", false},
		{"port entry allows its port for absolute-form", "example.com:8443", "example.com", "8443", "80", true},
		{"bare allows 80 for absolute-form", "example.com", "example.com", "80", "80", true},
		{"bare refuses 443 for absolute-form", "example.com", "example.com", "443", "80", false},
		{"bare refuses 8080 for absolute-form", "example.com", "example.com", "8080", "80", false},
		{"wildcard with port", "*.example.com:8443", "a.example.com", "8443", "443", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l, err := parseAllow([]string{tc.entry})
			require.NoError(t, err)
			assert.Equal(t, tc.want, l.permits(tc.host, tc.port, tc.def))
		})
	}
}

func TestAllowlistNormalisation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		entries []string
		target  string
		want    bool
	}{
		{"upper case", []string{"example.com"}, "EXAMPLE.com:443", true},
		{"trailing dot", []string{"example.com"}, "example.com.:443", true},
		{"upper case entry", []string{"Example.COM."}, "example.com:443", true},
		{"subdomain of wildcard", []string{"*.example.com"}, "sub.example.com:443", true},
		{"deeper subdomain of wildcard", []string{"*.example.com"}, "a.b.example.com:443", true},
		{"apex is not a wildcard match", []string{"*.example.com"}, "example.com:443", false},
		{"lookalike suffix", []string{"example.com", "*.example.com"}, "evilexample.com:443", false},
		{"lookalike prefix", []string{"example.com", "*.example.com"}, "example.com.evil.net:443", false},
		{"loopback denied", []string{"example.com", "*.example.com"}, "127.0.0.1:443", false},
		{"ipv6 loopback denied", []string{"example.com", "*.example.com"}, "[::1]:443", false},
		{"metadata address denied", []string{"example.com", "*.example.com"}, "169.254.169.254:443", false},
		{"listed ip allowed", []string{"169.254.169.254"}, "169.254.169.254:443", true},
		{"listed ipv6 allowed", []string{"[::1]"}, "[::1]:443", true},
		{"listed ipv6 other spelling", []string{"::1"}, "[0:0:0:0:0:0:0:1]:443", true},
		{"listed ip with port", []string{"10.0.0.5:8443"}, "10.0.0.5:8443", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l, err := parseAllow(tc.entries)
			require.NoError(t, err)

			host, port, err := splitAuthority(tc.target, "443")
			require.NoError(t, err)
			assert.Equal(t, tc.want, l.permits(host, port, "443"))
		})
	}
}

func TestNewRejectsBadConfig(t *testing.T) {
	for name, cfg := range map[string]Config{
		"empty allow entry":        {AllowHosts: []string{""}},
		"bare wildcard":            {AllowHosts: []string{"*"}},
		"bad port":                 {AllowHosts: []string{"example.com:99999"}},
		"mid wildcard":             {AllowHosts: []string{"a.*.example.com"}},
		"bundle without certs":     {CABundle: []byte("not a certificate")},
		"model no scheme":          {Model: modelRoute("gateway.example", authBearer)},
		"model userinfo":           {Model: modelRoute("https://user:"+realKey+"@gateway.example", authBearer)},
		"model bad style":          {Model: modelRoute("https://gateway.example", "basic")},
		"model no credential":      {Model: &ModelRoute{BaseURL: "https://gateway.example", AuthHeader: authBearer, StandIn: standIn}},
		"model credential newline": {Model: &ModelRoute{BaseURL: "https://gateway.example", AuthHeader: authBearer, Credential: realKey + "\n", StandIn: standIn}},
		"model stand-in control":   {Model: &ModelRoute{BaseURL: "https://gateway.example", AuthHeader: authBearer, Credential: realKey, StandIn: "a\rb"}},
		"model no stand-in":        {Model: &ModelRoute{BaseURL: "https://gateway.example", AuthHeader: authBearer, Credential: realKey}},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := New(cfg)
			require.Error(t, err)
			assert.NotContains(t, err.Error(), realKey)
		})
	}
}

func TestNewStandIn(t *testing.T) {
	a, err := NewStandIn()
	require.NoError(t, err)

	b, err := NewStandIn()
	require.NoError(t, err)

	assert.Len(t, a, 64)
	assert.NotEqual(t, a, b)
}

// stubbed builds a proxy whose name lookups come from table instead of DNS.
func stubbed(t *testing.T, cfg Config, table map[string]string) *httptest.Server {
	t.Helper()

	h, err := New(cfg)
	require.NoError(t, err)

	hh, ok := h.(*handler)
	require.True(t, ok)

	hh.lookup = func(_ context.Context, host string) ([]string, error) {
		if ip, found := table[host]; found {
			return []string{ip}, nil
		}

		return nil, errors.New("no such host")
	}

	return serve(t, hh)
}

func TestEgressRefusesLinkLocalAfterResolution(t *testing.T) {
	addr := echoListener(t)
	_, port, err := net.SplitHostPort(addr)
	require.NoError(t, err)

	table := map[string]string{
		"meta.example": "169.254.169.254", "v6.example": "fe80::1", "any.example": "0.0.0.0",
		"priv.example": "127.0.0.1", "aws.example": "fd00:ec2::254", "ali.example": "100.100.100.200",
	}

	for _, tc := range []struct {
		name, host string
		status     int
	}{
		{"metadata address", "meta.example", http.StatusForbidden},
		{"aws v6 metadata address", "aws.example", http.StatusForbidden},
		{"alibaba metadata address", "ali.example", http.StatusForbidden},
		{"link-local v6", "v6.example", http.StatusForbidden},
		{"unspecified", "any.example", http.StatusForbidden},
		{"private address", "priv.example", http.StatusOK},
	} {
		t.Run("connect "+tc.name, func(t *testing.T) {
			var d deniedLog

			p := stubbed(t, Config{AllowHosts: []string{tc.host + ":" + port}, OnDenied: d.add}, table)

			_, _, resp := connectTo(t, p, tc.host+":"+port)
			assert.Equal(t, tc.status, resp.StatusCode)

			if tc.status == http.StatusForbidden {
				assert.Equal(t, []string{tc.host + ":" + port}, d.all())
			} else {
				assert.Empty(t, d.all())
			}
		})
	}

	t.Run("absolute-form", func(t *testing.T) {
		var d deniedLog

		p := stubbed(t, Config{AllowHosts: []string{"meta.example"}, OnDenied: d.add}, table)

		resp, err := viaProxy(t, p).Get("http://meta.example/latest")
		require.NoError(t, err)

		_ = resp.Body.Close()

		assert.Equal(t, http.StatusForbidden, resp.StatusCode)
		assert.Equal(t, []string{"meta.example:80"}, d.all())
	})

	t.Run("listed unspecified address is allowed", func(t *testing.T) {
		p := startProxy(t, Config{AllowHosts: []string{"0.0.0.0:" + port}})

		_, _, resp := connectTo(t, p, "0.0.0.0:"+port)
		assert.Equal(t, http.StatusOK, resp.StatusCode)
	})
}

func TestControlAllowsListedLinkLocal(t *testing.T) {
	h, err := New(Config{AllowHosts: []string{"169.254.169.254", "[fe80::1]"}})
	require.NoError(t, err)

	hh, ok := h.(*handler)
	require.True(t, ok)

	control := hh.controlFor("")

	assert.NoError(t, control("tcp4", "169.254.169.254:443", nil))
	assert.NoError(t, control("tcp6", "[fe80::1]:443", nil))
	require.Error(t, control("tcp4", "169.254.1.1:443", nil))
	require.Error(t, control("tcp6", "[fe80::2]:443", nil))
	assert.NoError(t, control("tcp4", "10.1.2.3:443", nil))
	assert.NoError(t, control("tcp4", "127.0.0.1:443", nil))
}

func TestControlRefusesCloudMetadataUnlessListed(t *testing.T) {
	for _, tc := range []struct {
		name    string
		allow   []string
		address string
		ok      bool
	}{
		{"aws v6 not listed", []string{"meta.example"}, "[fd00:ec2::254]:80", false},
		{"aws v6 listed", []string{"[fd00:ec2::254]"}, "[fd00:ec2::254]:80", true},
		{"aws v6 listed in another spelling", []string{"fd00:ec2:0::254"}, "[fd00:ec2::254]:80", true},
		{"alibaba not listed", []string{"meta.example"}, "100.100.100.200:80", false},
		{"alibaba listed", []string{"100.100.100.200"}, "100.100.100.200:80", true},
		{"alibaba v4-mapped not listed", []string{"meta.example"}, "[::ffff:100.100.100.200]:80", false},
		{"a neighbour of aws v6", []string{"meta.example"}, "[fd00:ec2::253]:80", true},
		{"a neighbour of alibaba", []string{"meta.example"}, "100.100.100.201:80", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, err := New(Config{AllowHosts: tc.allow})
			require.NoError(t, err)

			hh, ok := h.(*handler)
			require.True(t, ok)

			err = hh.controlFor("meta.example")("tcp", tc.address, nil)
			if tc.ok {
				assert.NoError(t, err)
			} else {
				assert.ErrorIs(t, err, errRefusedAddress)
			}
		})
	}
}

func TestControlLetsTheListedHostGatewayThrough(t *testing.T) {
	// Rootless Podman maps host.docker.internal to a link-local address.
	const gatewayIP = "169.254.1.2:8080"

	for _, tc := range []struct {
		name    string
		allow   []string
		host    string
		address string
		ok      bool
	}{
		{"docker name allowed", []string{"host.docker.internal:8080"}, "host.docker.internal", gatewayIP, true},
		// The sandbox writes no hosts entry for it, so DNS could answer.
		{"podman name refused", []string{"host.containers.internal"}, "host.containers.internal", gatewayIP, false},
		{"name in another case", []string{"host.docker.internal:8080"}, "Host.Docker.Internal.", gatewayIP, true},
		{"v6 link-local", []string{"host.docker.internal"}, "host.docker.internal", "[fe80::1]:443", true},
		{"another allowed name on the same address", []string{"host.docker.internal:8080", "other.example:8080"}, "other.example", gatewayIP, false},
		{"only a wildcard covers the name", []string{"*.docker.internal"}, "host.docker.internal", gatewayIP, false},
		{"name not allowed", []string{"other.example"}, "host.docker.internal", gatewayIP, false},
		{"unspecified address", []string{"host.docker.internal"}, "host.docker.internal", "0.0.0.0:443", false},
		{"ip literal of the gateway", []string{"host.docker.internal"}, "169.254.1.2", gatewayIP, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, err := New(Config{AllowHosts: tc.allow})
			require.NoError(t, err)

			hh, ok := h.(*handler)
			require.True(t, ok)

			err = hh.controlFor(tc.host)("tcp", tc.address, nil)
			if tc.ok {
				assert.NoError(t, err)
			} else {
				assert.ErrorIs(t, err, errRefusedAddress)
			}
		})
	}
}

func TestEgressRefusesOtherNamesOnTheGatewayAddress(t *testing.T) {
	var d deniedLog

	table := map[string]string{"host.docker.internal": "169.254.1.2", "other.example": "169.254.1.2"}
	p := stubbed(t, Config{AllowHosts: []string{"host.docker.internal:8080", "other.example:8080"}, OnDenied: d.add}, table)

	_, _, resp := connectTo(t, p, "other.example:8080")
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)

	r, err := viaProxy(t, p).Get("http://other.example:8080/")
	require.NoError(t, err)

	_ = r.Body.Close()
	assert.Equal(t, http.StatusForbidden, r.StatusCode)
	assert.Equal(t, []string{"other.example:8080", "other.example:8080"}, d.all())
}

func TestDeniedViaHandler(t *testing.T) {
	var d deniedLog

	p := startProxy(t, Config{AllowHosts: []string{"example.com", "127.0.0.1"}, OnDenied: d.add})

	_, _, resp := connectTo(t, p, "example.com:80")
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)

	r, err := viaProxy(t, p).Get("http://example.com:443/")
	require.NoError(t, err)

	_ = r.Body.Close()
	assert.Equal(t, http.StatusForbidden, r.StatusCode)

	assert.Equal(t, []string{"example.com:80", "example.com:443"}, d.all())
}

func TestAbsoluteFormHTTPSReportsItsOwnPort(t *testing.T) {
	var d deniedLog

	h, err := New(Config{OnDenied: d.add})
	require.NoError(t, err)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "https://example.com/", nil))

	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Equal(t, []string{"example.com:443"}, d.all())
}

func TestDeniedTargetIsNeverDialled(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })

	var accepts atomic.Int32

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}

			accepts.Add(1)

			_ = c.Close()
		}
	}()

	target := ln.Addr().String()
	p := startProxy(t, Config{AllowHosts: []string{"other.example"}})

	_, _, resp := connectTo(t, p, target)
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)

	r, err := viaProxy(t, p).Get("http://" + target + "/")
	require.NoError(t, err)

	_ = r.Body.Close()
	assert.Equal(t, http.StatusForbidden, r.StatusCode)

	time.Sleep(50 * time.Millisecond)
	assert.Zero(t, accepts.Load())
}

func TestAllowlistEdgeCases(t *testing.T) {
	for _, tc := range []struct {
		name    string
		entries []string
		target  string
		want    bool
	}{
		{"ipv4-mapped literal is not the listed v4", []string{"127.0.0.1"}, "[::ffff:127.0.0.1]:443", false},
		{"numeric wildcard never matches an ip", []string{"*.0.0.1"}, "127.0.0.1:443", false},
		{"userinfo is part of the host", []string{"example.com"}, "evil@example.com:443", false},
		{"userinfo with listed ip", []string{"127.0.0.1"}, "user@127.0.0.1:443", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l, err := parseAllow(tc.entries)
			require.NoError(t, err)

			host, port, err := splitAuthority(tc.target, "443")
			require.NoError(t, err)
			assert.Equal(t, tc.want, l.permits(host, port, "443"))
		})
	}
}
