package forge

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testAppID = 123

var (
	testClock = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	testRepo  = Repo{Host: "github.com", Owner: "o", Name: "n"}
	testKey   = sync.OnceValue(func() *rsa.PrivateKey {
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			panic(err)
		}

		return key
	})
)

func appAccess() Access {
	pemKey := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(testKey())})

	return Access{AppID: testAppID, AppKey: string(pemKey)}
}

// appServer answers the installation lookup and the token request as GitHub
// does, once the App's signed token holds against the public key. Token n
// expires n hours after testClock.
type appServer struct {
	t   *testing.T
	mux *http.ServeMux

	mu     sync.Mutex
	calls  []string
	bodies []map[string]any
	claims jwt.RegisteredClaims
}

func newAppServer(t *testing.T) *appServer {
	t.Helper()

	s := &appServer{t: t, mux: http.NewServeMux()}

	s.mux.HandleFunc("GET /repos/o/{name}/installation", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":42}`))
	})
	s.mux.HandleFunc("POST /app/installations/42/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&body))

		s.mu.Lock()
		s.bodies = append(s.bodies, body)
		n := len(s.bodies)
		s.mu.Unlock()

		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":      fmt.Sprintf("ghs_%d", n),
			"expires_at": testClock.Add(time.Duration(n) * time.Hour),
		})
	})

	return s
}

func (s *appServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var claims jwt.RegisteredClaims

	signed := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	_, err := jwt.ParseWithClaims(signed, &claims,
		func(*jwt.Token) (any, error) { return &testKey().PublicKey, nil },
		jwt.WithValidMethods([]string{"RS256"}), jwt.WithoutClaimsValidation())

	if !assert.NoError(s.t, err) || !assert.Equal(s.t, strconv.Itoa(testAppID), claims.Issuer) {
		w.WriteHeader(http.StatusUnauthorized)

		return
	}

	s.mu.Lock()
	s.calls = append(s.calls, r.Method+" "+r.URL.Path)
	s.claims = claims
	s.mu.Unlock()

	s.mux.ServeHTTP(w, r)
}

func (s *appServer) mints() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return len(s.bodies)
}

func TestAppTokenIsScopedToTheRepository(t *testing.T) {
	apiPerms := map[string]any{
		"contents": "read", "pull_requests": "write", "checks": "read",
		"statuses": "read", "actions": "read", "metadata": "read",
	}

	tests := []struct {
		name  string
		push  bool
		perms map[string]any
	}{
		{"push", true, map[string]any{"contents": "write"}},
		{"api", false, apiPerms},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newAppServer(t)
			c := newTestClient(t, func() time.Time { return testClock }, "api.github.com", s, nil)

			token, err := c.Token(t.Context(), testRepo, appAccess(), tc.push)
			require.NoError(t, err)
			assert.Equal(t, "ghs_1", token)

			s.mu.Lock()
			defer s.mu.Unlock()

			assert.Equal(t, []string{"GET /repos/o/n/installation", "POST /app/installations/42/access_tokens"}, s.calls)
			assert.Equal(t, []map[string]any{{"repositories": []any{"n"}, "permissions": tc.perms}}, s.bodies)
			require.NotNil(t, s.claims.IssuedAt)
			require.NotNil(t, s.claims.ExpiresAt)
			assert.True(t, s.claims.IssuedAt.Equal(testClock.Add(-time.Minute)), "issued at %v", s.claims.IssuedAt)
			assert.True(t, s.claims.ExpiresAt.Equal(testClock.Add(10*time.Minute)), "expires at %v", s.claims.ExpiresAt)
		})
	}
}

func TestAppTokenIsCachedUntilItNearsExpiry(t *testing.T) {
	clock := testClock
	s := newAppServer(t)
	c := newTestClient(t, func() time.Time { return clock }, "api.github.com", s, nil)

	token := func(r Repo) string {
		t.Helper()

		tok, err := c.Token(t.Context(), r, appAccess(), false)
		require.NoError(t, err)

		return tok
	}

	// Callers on a cold cache, as when the watch polls many tasks at once, share one mint.
	var wg sync.WaitGroup

	got := make([]string, 8)
	errs := make([]error, len(got))

	for i := range got {
		wg.Go(func() { got[i], errs[i] = c.Token(t.Context(), testRepo, appAccess(), false) })
	}

	wg.Wait()

	for i := range got {
		require.NoError(t, errs[i])
		assert.Equal(t, "ghs_1", got[i])
	}

	clock = testClock.Add(55*time.Minute - time.Second)

	assert.Equal(t, "ghs_1", token(testRepo))

	clock = testClock.Add(55 * time.Minute)

	assert.Equal(t, "ghs_2", token(testRepo))
	assert.Equal(t, "ghs_2", token(testRepo))
	assert.Equal(t, 2, s.mints())

	assert.Equal(t, "ghs_3", token(Repo{Host: "github.com", Owner: "o", Name: "m"}))
}

func TestPushTokenIsNeverCached(t *testing.T) {
	s := newAppServer(t)
	c := newTestClient(t, func() time.Time { return testClock }, "api.github.com", s, nil)

	var got []string

	for _, push := range []bool{false, true, true, false} {
		tok, err := c.Token(t.Context(), testRepo, appAccess(), push)
		require.NoError(t, err)

		got = append(got, tok)
	}

	assert.Equal(t, []string{"ghs_1", "ghs_2", "ghs_3", "ghs_1"}, got)
	assert.Equal(t, 3, s.mints())
}

func TestAccessWithoutAKeyIsRefused(t *testing.T) {
	refuse := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		t.Errorf("a request reached the forge: %s %s", r.Method, r.URL.Path)
	})
	c := newTestClient(t, nil, "api.github.com", refuse, nil)

	for name, a := range map[string]Access{
		"an App id without a key": {AppID: testAppID},
		"no credential":           {},
	} {
		t.Run(name, func(t *testing.T) {
			for _, push := range []bool{false, true} {
				_, err := c.Token(t.Context(), testRepo, a, push)
				require.Error(t, err)
			}

			require.Error(t, c.Check(t.Context(), testRepo, a))
		})
	}
}

func TestPersonalTokenIsItsOwnToken(t *testing.T) {
	var got []http.Header

	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/o/n", func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Header.Clone())
		_, _ = w.Write([]byte(`{}`))
	})

	c := newTestClient(t, nil, "api.github.com", mux, nil)
	a := Access{Token: testToken}

	for _, push := range []bool{false, true} {
		tok, err := c.Token(t.Context(), testRepo, a, push)
		require.NoError(t, err)
		assert.Equal(t, testToken, tok)
	}

	require.NoError(t, c.Check(t.Context(), testRepo, a))
	require.Len(t, got, 1)
	assert.Equal(t, "Bearer "+testToken, got[0].Get("Authorization"))
	assert.Equal(t, "application/vnd.github+json", got[0].Get("Accept"))
	assert.Equal(t, "2022-11-28", got[0].Get("X-GitHub-Api-Version"))
	assert.Equal(t, "cavu/"+testVersion, got[0].Get("User-Agent"))
}
