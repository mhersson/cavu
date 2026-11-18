// Package forge talks to GitHub and GitHub Enterprise over their HTTP APIs.
package forge

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	requestTimeout = 30 * time.Second
	maxBody        = 8 << 20
	maxRedirects   = 10
)

// Options configures a Client.
type Options struct {
	CABundle  []byte            // PEM, added to the system's certificates; nil for none
	Transport http.RoundTripper // tests; nil for the standard one
	Now       func() time.Time  // nil for time.Now
	Version   string            // cavu's version, for the User-Agent
}

// Client calls the API of the forge a repository is on. It is safe for
// concurrent use.
type Client struct {
	http      *http.Client
	now       func() time.Time
	userAgent string

	mu     sync.Mutex
	tokens map[tokenKey]*appToken
}

// New returns a client; it is an error for a CA bundle that holds no
// certificate.
func New(o Options) (*Client, error) {
	base := o.Transport
	if base == nil {
		t := http.DefaultTransport.(*http.Transport).Clone()

		if len(o.CABundle) > 0 {
			pool, err := x509.SystemCertPool()
			if err != nil {
				return nil, fmt.Errorf("forge: the system's certificates: %w", err)
			}

			if !pool.AppendCertsFromPEM(o.CABundle) {
				return nil, errors.New("forge: the CA bundle holds no certificate")
			}

			t.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
		}

		base = t
	}

	now := o.Now
	if now == nil {
		now = time.Now
	}

	return &Client{
		http: &http.Client{
			Transport:     newETagTransport(base),
			Timeout:       requestTimeout,
			CheckRedirect: tokenOnItsHost,
		},
		now:       now,
		userAgent: "cavu/" + o.Version,
		tokens:    map[tokenKey]*appToken{},
	}, nil
}

// tokenOnItsHost drops the token from a redirect to another host: net/http
// keeps it for a subdomain, which is where a forge may store a job's log.
func tokenOnItsHost(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		return fmt.Errorf("stopped after %d redirects", maxRedirects)
	}

	if req.URL.Host != via[0].URL.Host {
		req.Header.Del("Authorization")
	}

	return nil
}

// Check reads r over the API: it proves the address, the certificates and the
// credential.
func (c *Client) Check(ctx context.Context, r Repo, a Access) error {
	token, err := c.Token(ctx, r, a, ForAPI)
	if err != nil {
		return err
	}

	_, err = c.rest(ctx, r, token, http.MethodGet, r.path(), nil)

	return err
}

// response is a 2xx answer, its body read to 8 MiB.
type response struct {
	op     string // the method and API address, for errors
	header http.Header
	body   []byte
}

// decode reads the answer's JSON into v.
func (resp *response) decode(v any) error {
	if err := json.Unmarshal(resp.body, v); err != nil {
		return fmt.Errorf("%s: read the answer: %w", resp.op, err)
	}

	return nil
}

// rest calls r's REST API at path, which may carry a query, with body as JSON
// unless it is nil.
func (c *Client) rest(ctx context.Context, r Repo, token, method, path string, body any) (*response, error) {
	req, err := c.newRequest(ctx, method, r.restURL()+path, token, body)
	if err != nil {
		return nil, err
	}

	return c.do(req)
}

// newRequest builds a request to a forge's API with its headers and token, and
// body as JSON unless it is nil. Headers set on the request, not in a
// transport, follow a redirect.
func (c *Client) newRequest(ctx context.Context, method, addr, token string, body any) (*http.Request, error) {
	var rd io.Reader

	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("forge: %s: %w", method, err)
		}

		rd = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, addr, rd)
	if err != nil {
		return nil, fmt.Errorf("forge: %w", err)
	}

	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Authorization", "Bearer "+token)

	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	return req, nil
}

// do sends req and reads its answer. One outside 2xx is an error: a
// *RateLimitError, or an *httpError with the status and the forge's message.
func (c *Client) do(req *http.Request) (*response, error) {
	// The scheme and host name the API address tried, which no setting shows.
	// Never the query, and never a redirect's address.
	op := "forge: " + req.Method + " " + req.URL.Scheme + "://" + req.URL.Host + req.URL.Path

	resp, err := c.http.Do(req)
	if err != nil {
		// A *url.Error quotes the address, and a redirect's may carry a signed
		// credential in its query.
		if ue, ok := errors.AsType[*url.Error](err); ok {
			err = ue.Err
		}

		return nil, fmt.Errorf("%s: %w", op, err)
	}

	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, fmt.Errorf("%s: read the answer: %w", op, err)
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var m struct {
			Message string `json:"message"`
		}

		_ = json.Unmarshal(body, &m)

		if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests {
			if reset, ok := rateLimit(resp.Header, m.Message, c.now()); ok {
				return nil, fmt.Errorf("%s: %w", op, &RateLimitError{Reset: reset})
			}
		}

		return nil, fmt.Errorf("%s: %w", op, &httpError{status: resp.StatusCode, message: m.Message})
	}

	return &response{op: op, header: resp.Header, body: body}, nil
}

// graphQL sends doc with vars to r's GraphQL API and reads the answer's data
// into v unless v is nil. An answer with errors is an error: a
// *RateLimitError when the rate-limit rules or an error's type say so.
func (c *Client) graphQL(ctx context.Context, r Repo, token, doc string, vars map[string]any, v any) error {
	req, err := c.newRequest(ctx, http.MethodPost, r.graphQLURL(), token, map[string]any{"query": doc, "variables": vars})
	if err != nil {
		return err
	}

	resp, err := c.do(req)
	if err != nil {
		return err
	}

	var out struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"errors"`
	}

	if err := resp.decode(&out); err != nil {
		return err
	}

	if len(out.Errors) > 0 {
		msgs := make([]string, len(out.Errors))
		limited := false

		for i, e := range out.Errors {
			msgs[i] = e.Message
			limited = limited || e.Type == "RATE_LIMITED"
		}

		msg := strings.Join(msgs, "; ")

		reset, ok := rateLimit(resp.header, msg, c.now())
		if !ok && limited {
			reset, ok = c.now().Add(rateLimitFallback), true
		}

		if ok {
			return fmt.Errorf("%s: %w", resp.op, &RateLimitError{Reset: reset})
		}

		return errors.New(resp.op + ": " + msg)
	}

	if v == nil {
		return nil
	}

	if err := json.Unmarshal(out.Data, v); err != nil {
		return fmt.Errorf("%s: read the answer: %w", resp.op, err)
	}

	return nil
}
