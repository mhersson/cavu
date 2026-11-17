package proxy

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
)

const (
	authBearer = "bearer"
	authAPIKey = "x-api-key"
)

type modelHandler struct {
	h          *handler
	base       *url.URL
	credential string
	standIn    []byte
	bearer     bool
	rp         *httputil.ReverseProxy
	ledger     *ledger // nil without a budget
}

// costs marks a request whose answer counts against the budget.
type costs struct{}

func newModelHandler(m *ModelRoute, tr http.RoundTripper, h *handler) (*modelHandler, error) {
	// Errors below never echo BaseURL or the credential: either may carry secrets.
	base, err := url.Parse(m.BaseURL)
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" {
		return nil, errors.New("proxy: model BaseURL must be an absolute http or https URL")
	}

	if base.User != nil {
		return nil, errors.New("proxy: model BaseURL must not carry userinfo")
	}

	if m.Credential == "" || m.StandIn == "" {
		return nil, errors.New("proxy: model route needs a credential and a stand-in")
	}

	if !headerValue(m.Credential) || !headerValue(m.StandIn) {
		return nil, errors.New("proxy: model credential and stand-in must be valid header values")
	}

	if m.AuthHeader != authBearer && m.AuthHeader != authAPIKey {
		return nil, fmt.Errorf("proxy: model AuthHeader must be %q or %q", authBearer, authAPIKey)
	}

	mh := &modelHandler{
		h: h, base: base, credential: m.Credential,
		standIn: []byte(m.StandIn), bearer: m.AuthHeader == authBearer,
	}
	mh.rp = &httputil.ReverseProxy{
		Rewrite:        mh.rewrite,
		Transport:      tr,
		FlushInterval:  -1,
		ModifyResponse: mh.modifyResponse,
		ErrorHandler:   mh.upstreamError,
		// Body copy errors would otherwise go to the default logger on stderr.
		ErrorLog: log.New(io.Discard, "", 0),
	}

	if b := h.cfg.Budget; b != nil {
		mh.ledger = &ledger{Budget: *b, onBudget: h.cfg.OnBudget}
	}

	return mh, nil
}

// headerValue rejects control bytes (a trailing newline from a key file is the
// usual one) that would fail every request as an upstream error.
func headerValue(s string) bool {
	for i := range len(s) {
		if c := s[i]; (c < ' ' && c != '\t') || c == 0x7f {
			return false
		}
	}

	return true
}

func modelPath(p string) bool { return p == "/v1/messages" || p == "/v1/messages/count_tokens" }

func (m *modelHandler) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || !modelPath(r.URL.Path) {
		http.NotFound(w, r)

		return
	}

	if !m.authentic(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)

		return
	}

	if m.ledger != nil {
		if m.ledger.refusing() {
			refuse(w)

			return
		}

		if r.URL.Path == "/v1/messages" {
			r = r.WithContext(context.WithValue(r.Context(), costs{}, true))
		}
	}

	// Without full duplex the HTTP/1 server closes the request body at the first
	// response write and the upstream stream is cut. HTTP/2 is always duplex.
	_ = http.NewResponseController(w).EnableFullDuplex()

	m.rp.ServeHTTP(w, r)
}

func (m *modelHandler) authentic(r *http.Request) bool {
	if tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok &&
		subtle.ConstantTimeCompare([]byte(tok), m.standIn) == 1 {
		return true
	}

	return subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Api-Key")), m.standIn) == 1
}

func (m *modelHandler) rewrite(pr *httputil.ProxyRequest) {
	pr.SetURL(m.base)
	pr.Out.Host = m.base.Host

	// Identity lets every response be metered; a gateway may still encode it.
	pr.Out.Header.Set("Accept-Encoding", "identity")

	pr.Out.Header.Del("Authorization")
	pr.Out.Header.Del("X-Api-Key")

	if m.bearer {
		pr.Out.Header.Set("Authorization", "Bearer "+m.credential)
	} else {
		pr.Out.Header.Set("X-Api-Key", m.credential)
	}
}

func (m *modelHandler) modifyResponse(res *http.Response) error {
	var l *ledger
	if m.ledger != nil && res.StatusCode/100 == 2 && res.Request.Context().Value(costs{}) != nil {
		l = m.ledger
	}

	res.Body = newMeterBody(res, m.h, l)

	return nil
}

func (m *modelHandler) upstreamError(w http.ResponseWriter, _ *http.Request, err error) {
	if errors.Is(err, context.Canceled) {
		return
	}

	http.Error(w, "model endpoint unavailable", http.StatusBadGateway)
}
