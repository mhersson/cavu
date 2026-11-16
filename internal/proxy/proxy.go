// Package proxy is the single way out of the sandbox: an egress allowlist for
// CONNECT and plain HTTP, and the model route that swaps a stand-in token for
// the real credential. It never intercepts TLS.
package proxy

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"time"
)

// Config configures the handler returned by New.
type Config struct {
	// AllowHosts lists "host", "host:port" or "*.example.com" (one label and
	// deeper). Without a port, 443 is allowed for CONNECT and 80 for
	// absolute-form requests; with a port, exactly that port.
	AllowHosts []string
	// Model is nil in subscription mode; origin-form requests then answer 404.
	Model *ModelRoute
	// CABundle is PEM added to the system roots for the proxy's own TLS.
	CABundle []byte
	// The callbacks may be nil and may be called from concurrent requests; the
	// caller serialises its own output. They run inline and must not block: a
	// slow OnUsage holds back the end of the response.
	//
	// OnDenied receives the normalised host:port that was refused.
	OnDenied func(hostPort string)
	// OnUsage is called once per metered model response, after it ends.
	OnUsage func(Usage)
	// OnModelError is called with the status the client saw (the upstream
	// status for an error response) and a kind: auth, rate_limit, upstream,
	// transport or unmetered (the response arrived content-encoded, so its usage
	// was not read).
	OnModelError func(status int, kind string)
}

// ModelRoute is the real model endpoint and the credential only the proxy holds.
type ModelRoute struct {
	// BaseURL is the real endpoint, e.g. https://gateway.example/anthropic.
	BaseURL string
	// Credential is the real key.
	Credential string
	// AuthHeader is "bearer" or "x-api-key".
	AuthHeader string
	// StandIn is the token the agent container holds.
	StandIn string
}

// Usage is the token usage of one model response.
type Usage struct {
	Model      string
	Input      int64
	Output     int64
	CacheRead  int64
	CacheWrite int64
}

const (
	kindAuth      = "auth"
	kindRateLimit = "rate_limit"
	kindUpstream  = "upstream"
	kindTransport = "transport"
	kindUnmetered = "unmetered"
)

type handler struct {
	// lookup replaces DNS for the egress dialer; tests only.
	lookup func(context.Context, string) ([]string, error)
	cfg    Config
	allow  allowlist
	egress *http.Transport
	model  *modelHandler
}

// New builds the handler. It performs no network I/O.
func New(c Config) (http.Handler, error) {
	allow, err := parseAllow(c.AllowHosts)
	if err != nil {
		return nil, err
	}

	roots, err := rootsWith(c.CABundle)
	if err != nil {
		return nil, err
	}

	h := &handler{cfg: c, allow: allow}
	h.egress = newTransport(roots, h.dialContext)

	if c.Model != nil {
		plain := (&net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second}).DialContext
		if h.model, err = newModelHandler(c.Model, newTransport(roots, plain), h); err != nil {
			return nil, err
		}
	}

	return h, nil
}

// NewStandIn returns a fresh stand-in token: 32 random bytes, hex.
func NewStandIn() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("proxy: stand-in token: %w", err)
	}

	return hex.EncodeToString(b), nil
}

// newTransport never uses a proxy from the environment: every upstream
// connection is direct. Compression is off so bodies and headers pass untouched.
func newTransport(roots *x509.CertPool, dial func(context.Context, string, string) (net.Conn, error)) *http.Transport {
	tr := &http.Transport{
		DialContext:           dial,
		DisableCompression:    true,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		// The connection is shared by every request, so a dead one must be
		// detected before the agent's stall timeout.
		HTTP2: &http.HTTP2Config{SendPingTimeout: 30 * time.Second},
	}
	if roots != nil {
		tr.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
	}

	return tr
}

// ServeHTTP dispatches on the request shape: CONNECT, absolute-form, or
// origin-form (the model route).
func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodConnect:
		h.connect(w, r)
	case r.URL.IsAbs():
		h.forward(w, r)
	default:
		if h.model == nil {
			http.NotFound(w, r)

			return
		}

		h.model.serve(w, r)
	}
}

func (h *handler) denied(hostPort string) {
	if h.cfg.OnDenied != nil {
		h.cfg.OnDenied(hostPort)
	}
}

func (h *handler) usage(u Usage) {
	if h.cfg.OnUsage != nil {
		h.cfg.OnUsage(u)
	}
}

func (h *handler) modelError(status int, kind string) {
	if h.cfg.OnModelError != nil {
		h.cfg.OnModelError(status, kind)
	}
}
