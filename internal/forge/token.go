package forge

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// tokenRefresh is how long before its expiry a cached App token is replaced.
const tokenRefresh = 5 * time.Minute

var (
	pushPermissions = map[Use]map[string]string{
		ForPush:         {"contents": "write"},
		ForWorkflowPush: {"contents": "write", "workflows": "write"},
	}
	// Metadata is named because a repository's read and a collaborator's
	// permission need it, and a narrowed token is not documented to keep it.
	apiPermissions = map[string]string{
		"contents": "read", "pull_requests": "write", "checks": "read",
		"statuses": "read", "actions": "read", "metadata": "read",
	}
)

// Use is what a token is for.
type Use int

const (
	ForAPI          Use = iota // the API calls this package makes, and a clone
	ForPush                    // a push
	ForWorkflowPush            // a push that changes a workflow
)

// Access is the credential for a repository's forge.
type Access struct {
	Token  string // a personal access token, used when AppID is 0
	AppID  int64
	AppKey string // the App's private key, PEM
}

type tokenKey struct {
	repo  Repo
	appID int64
}

// appToken is a cached App token. Its lock makes callers for one repository
// wait for one mint.
type appToken struct {
	mu      sync.Mutex
	token   string
	expires time.Time
}

// Token is a token for git and for the API on r. For a personal access token it is that token.
// For an App: for a push, a new token each call that may write r's contents and, for a push that
// changes a workflow, its workflows, and nothing else; a 422 on that mint is a *TokenRefusedError.
// For the API, a token kept until five minutes before it expires.
func (c *Client) Token(ctx context.Context, r Repo, a Access, use Use) (string, error) {
	switch {
	case a.AppID == 0 && a.Token == "":
		return "", errors.New("forge: no credential: a personal access token or a GitHub App is needed")
	case a.AppID == 0:
		return a.Token, nil
	case a.AppKey == "":
		return "", errors.New("forge: the GitHub App has no private key")
	case use != ForAPI:
		token, _, err := c.mint(ctx, r, a, pushPermissions[use])
		if he, ok := errors.AsType[*httpError](err); ok && he.status == http.StatusUnprocessableEntity {
			return "", &TokenRefusedError{Use: use, Message: he.message}
		}

		return token, err
	}

	t := c.cached(tokenKey{repo: r, appID: a.AppID})

	t.mu.Lock()
	defer t.mu.Unlock()

	if c.now().Before(t.expires.Add(-tokenRefresh)) {
		return t.token, nil
	}

	token, expires, err := c.mint(ctx, r, a, apiPermissions)
	if err != nil {
		return "", err
	}

	t.token, t.expires = token, expires

	return token, nil
}

func (c *Client) cached(k tokenKey) *appToken {
	c.mu.Lock()
	defer c.mu.Unlock()

	t := c.tokens[k]
	if t == nil {
		t = &appToken{}
		c.tokens[k] = t
	}

	return t
}

// mint asks the App's installation on r for a token for r alone with perms.
func (c *Client) mint(ctx context.Context, r Repo, a Access, perms map[string]string) (string, time.Time, error) {
	signed, err := c.appJWT(a)
	if err != nil {
		return "", time.Time{}, err
	}

	resp, err := c.rest(ctx, r, signed, http.MethodGet, r.path()+"/installation", nil)
	if err != nil {
		return "", time.Time{}, err
	}

	var inst struct {
		ID int64 `json:"id"`
	}

	if err := resp.decode(&inst); err != nil {
		return "", time.Time{}, err
	}

	resp, err = c.rest(ctx, r, signed, http.MethodPost, fmt.Sprintf("/app/installations/%d/access_tokens", inst.ID),
		map[string]any{"repositories": []string{r.Name}, "permissions": perms})
	if err != nil {
		return "", time.Time{}, err
	}

	var tok struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}

	if err := resp.decode(&tok); err != nil {
		return "", time.Time{}, err
	}

	return tok.Token, tok.ExpiresAt, nil
}

// appJWT is the App's signed token. It is issued a minute in the past for
// clock skew, as GitHub advises.
func (c *Client) appJWT(a Access) (string, error) {
	key, err := jwt.ParseRSAPrivateKeyFromPEM([]byte(a.AppKey))
	if err != nil {
		return "", fmt.Errorf("forge: the GitHub App's private key: %w", err)
	}

	now := c.now()

	signed, err := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.RegisteredClaims{
		Issuer:    strconv.FormatInt(a.AppID, 10),
		IssuedAt:  jwt.NewNumericDate(now.Add(-time.Minute)),
		ExpiresAt: jwt.NewNumericDate(now.Add(10 * time.Minute)),
	}).SignedString(key)
	if err != nil {
		return "", fmt.Errorf("forge: sign the GitHub App's token: %w", err)
	}

	return signed, nil
}
