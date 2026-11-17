package auth_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mhersson/cavu/internal/auth"
	"github.com/mhersson/cavu/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	day      = 24 * time.Hour
	password = "correct horse battery"
)

type fixture struct {
	svc   *auth.Service
	store *store.Store
	dir   string
	clock *time.Time
}

func newService(t *testing.T) *fixture {
	t.Helper()

	dir := t.TempDir()

	s, err := store.Open(t.Context(), filepath.Join(dir, "cavu.db"))
	require.NoError(t, err)

	clock := time.Unix(1_000_000, 0)
	svc := auth.NewService(s, func() time.Time { return clock })

	f := &fixture{svc: svc, store: s, dir: dir, clock: &clock}

	t.Cleanup(func() { _ = s.Close() })

	require.NoError(t, svc.SetUser(t.Context(), "ada", password))

	return f
}

func TestLogin(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	f := newService(t)

	cookie, expires, err := f.svc.Login(ctx, "ada", password, "10.0.0.1")
	require.NoError(t, err)
	assert.Equal(t, f.clock.Add(30*day), expires)

	ok, renewed, err := f.svc.Session(ctx, cookie)
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Zero(t, renewed, "a new session is not written again")

	ok, _, err = f.svc.Session(ctx, "not-a-cookie")
	require.NoError(t, err)
	assert.False(t, ok)

	_, _, err = f.svc.Login(ctx, "ada", "wrong password!", "10.0.0.1")
	require.ErrorIs(t, err, auth.ErrBadLogin)

	_, _, err = f.svc.Login(ctx, "grace", password, "10.0.0.1")
	require.ErrorIs(t, err, auth.ErrBadLogin)

	other, _, err := f.svc.Login(ctx, "ada", password, "10.0.0.2")
	require.NoError(t, err)

	require.NoError(t, f.svc.Logout(ctx, cookie))

	ok, _, err = f.svc.Session(ctx, cookie)
	require.NoError(t, err)
	assert.False(t, ok)

	ok, _, err = f.svc.Session(ctx, other)
	require.NoError(t, err)
	assert.True(t, ok, "logging one session out leaves the other")

	require.NoError(t, f.svc.SetUser(ctx, "ada", "another long password"))

	ok, _, err = f.svc.Session(ctx, other)
	require.NoError(t, err)
	assert.False(t, ok, "setting the user ends every session")
}

func TestLoginWithoutAUserIsBad(t *testing.T) {
	t.Parallel()

	s, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "cavu.db"))
	require.NoError(t, err)

	defer s.Close()

	svc := auth.NewService(s, time.Now)

	_, _, err = svc.Login(t.Context(), "ada", password, "10.0.0.1")
	require.ErrorIs(t, err, auth.ErrBadLogin)
}

func TestLoginLimiter(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	f := newService(t)

	for range 3 {
		_, _, err := f.svc.Login(ctx, "ada", "wrong password!", "10.0.0.1")
		require.ErrorIs(t, err, auth.ErrBadLogin)
	}

	_, _, err := f.svc.Login(ctx, "ada", password, "10.0.0.1")
	limited, ok := errors.AsType[*auth.LimitedError](err)
	require.True(t, ok, "the right password is refused while blocked")
	assert.Positive(t, limited.RetryAfter)

	_, _, err = f.svc.Login(ctx, "ada", password, "10.0.0.2")
	require.NoError(t, err, "another address is not limited")

	*f.clock = f.clock.Add(limited.RetryAfter)

	_, _, err = f.svc.Login(ctx, "ada", password, "10.0.0.1")
	require.NoError(t, err)

	for range 2 {
		_, _, err := f.svc.Login(ctx, "ada", "wrong password!", "10.0.0.1")
		require.ErrorIs(t, err, auth.ErrBadLogin)
	}

	_, _, err = f.svc.Login(ctx, "ada", password, "10.0.0.1")
	require.NoError(t, err, "a success cleared the count")

	for range 2 {
		_, _, err := f.svc.Login(ctx, "ada", "wrong password!", "10.0.0.1")
		require.ErrorIs(t, err, auth.ErrBadLogin, "two more failures are not yet a block")
	}
}

func TestLoginLimiterCountsParallelAttempts(t *testing.T) {
	t.Parallel()

	f := newService(t)
	errs := make(chan error, 10)

	for range cap(errs) {
		go func() {
			_, _, err := f.svc.Login(t.Context(), "ada", "wrong password!", "10.0.0.1")
			errs <- err
		}()
	}

	var bad, limited int

	for range cap(errs) {
		err := <-errs
		if _, ok := errors.AsType[*auth.LimitedError](err); ok {
			limited++
		} else if errors.Is(err, auth.ErrBadLogin) {
			bad++
		}
	}

	assert.Equal(t, 3, bad)
	assert.Equal(t, 7, limited)
}

func TestSessionLifetime(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	f := newService(t)

	cookie, _, err := f.svc.Login(ctx, "ada", password, "10.0.0.1")
	require.NoError(t, err)

	for range 4 {
		*f.clock = f.clock.Add(20 * day)

		ok, renewed, err := f.svc.Session(ctx, cookie)
		require.NoError(t, err)
		require.True(t, ok, "a session used every 20 days lives on")
		assert.Equal(t, f.clock.Add(30*day), renewed, "the moved expiry is returned")
	}

	hash := auth.HashToken(cookie)

	expiry, ok, err := f.store.SessionExpiry(ctx, hash)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, f.clock.Add(30*day), expiry)

	*f.clock = f.clock.Add(time.Hour)

	_, renewed, err := f.svc.Session(ctx, cookie)
	require.NoError(t, err)
	assert.Zero(t, renewed, "an expiry that does not move is not returned")

	same, _, err := f.store.SessionExpiry(ctx, hash)
	require.NoError(t, err)
	assert.Equal(t, expiry, same, "the row is written at most once a day")

	*f.clock = f.clock.Add(31 * day)

	ok, _, err = f.svc.Session(ctx, cookie)
	require.NoError(t, err)
	assert.False(t, ok)

	_, found, err := f.store.SessionExpiry(ctx, hash)
	require.NoError(t, err)
	assert.True(t, found, "refusing does not remove the row")

	_, _, err = f.svc.Login(ctx, "ada", password, "10.0.0.1")
	require.NoError(t, err)

	_, found, err = f.store.SessionExpiry(ctx, hash)
	require.NoError(t, err)
	assert.False(t, found, "the next login removes it")
}

func TestAPITokens(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	f := newService(t)

	token, err := f.svc.NewAPIToken(ctx, "ci-bot")
	require.NoError(t, err)
	assert.Len(t, token, len("cavu_")+43)
	assert.Equal(t, "cavu_", token[:5])

	_, err = f.svc.NewAPIToken(ctx, "ci-bot")
	require.ErrorIs(t, err, store.ErrConflict)

	ok, err := f.svc.Bearer(ctx, token)
	require.NoError(t, err)
	assert.True(t, ok)

	ok, err = f.svc.Bearer(ctx, token[:len(token)-1])
	require.NoError(t, err)
	assert.False(t, ok)

	cookie, _, err := f.svc.Login(ctx, "ada", password, "10.0.0.1")
	require.NoError(t, err)

	ok, err = f.svc.Bearer(ctx, cookie)
	require.NoError(t, err)
	assert.False(t, ok, "a cookie value is no bearer token")

	tokens, err := f.store.APITokens(ctx)
	require.NoError(t, err)
	require.Len(t, tokens, 1)
	require.NotNil(t, tokens[0].LastUsedAt)

	require.NoError(t, f.store.Close())

	entries, err := os.ReadDir(f.dir)
	require.NoError(t, err)

	var all []byte

	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(f.dir, e.Name()))
		require.NoError(t, err)

		all = append(all, b...)
	}

	assert.Contains(t, string(all), "ci-bot", "positive control: a stored value is found")
	assert.NotContains(t, string(all), token)
	assert.NotContains(t, string(all), cookie)
	assert.NotContains(t, string(all), password)
}

func TestAPITokenRemoved(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	f := newService(t)

	token, err := f.svc.NewAPIToken(ctx, "ci-bot")
	require.NoError(t, err)

	require.NoError(t, f.store.DeleteAPIToken(ctx, "ci-bot"))

	ok, err := f.svc.Bearer(ctx, token)
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestSetUserRefusesAShortPassword(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	f := newService(t)

	require.Error(t, f.svc.SetUser(ctx, "ada", "elevenchars"))

	// Twelve characters of three bytes each: the minimum counts characters.
	require.NoError(t, f.svc.SetUser(ctx, "ada", "日本語日本語日本語日本語"))
	_, _, err := f.svc.Login(ctx, "ada", "日本語日本語日本語日本語", "10.0.0.1")
	require.NoError(t, err)

	require.Error(t, f.svc.SetUser(ctx, "ada", "日本語日本語日本語日"))
}
