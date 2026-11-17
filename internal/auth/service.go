package auth

import (
	"context"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/mhersson/cavu/internal/store"
)

const (
	minPasswordLen = 12
	sessionLife    = 30 * 24 * time.Hour
	// A session with more than this left is not written again, so a busy one
	// costs at most one write a day.
	sessionRefreshBelow = 29 * 24 * time.Hour
	apiTokenPrefix      = "cavu_"
)

// ErrBadLogin is returned for a wrong name and for a wrong password alike.
var ErrBadLogin = errors.New("auth: wrong name or password")

// LimitedError is returned while too many logins failed for a name from a remote address.
type LimitedError struct{ RetryAfter time.Duration }

func (e *LimitedError) Error() string {
	return fmt.Sprintf("auth: too many failed logins, retry in %s", e.RetryAfter.Round(time.Second))
}

// Service logs the one user in and knows their sessions and API tokens.
type Service struct {
	store   *store.Store
	now     func() time.Time
	limiter *Limiter
}

// NewService returns a Service over s that reads time from now.
func NewService(s *store.Store, now func() time.Time) *Service {
	return &Service{store: s, now: now, limiter: NewLimiter(now)}
}

// SetUser creates the user or replaces the name and password, and ends every session.
func (a *Service) SetUser(ctx context.Context, name, password string) error {
	if name == "" {
		return errors.New("auth: the user needs a name")
	}

	if utf8.RuneCountInString(password) < minPasswordLen {
		return fmt.Errorf("auth: the password needs at least %d characters", minPasswordLen)
	}

	hash, err := HashPassword(password)
	if err != nil {
		return err
	}

	if err := a.store.PutUser(ctx, store.User{Name: name, PasswordHash: hash, CreatedAt: a.now()}); err != nil {
		return err
	}

	return a.store.DeleteSessions(ctx, time.Time{})
}

// Login returns a new session's cookie value and when it expires.
func (a *Service) Login(ctx context.Context, name, password, remote string) (cookie string, expires time.Time, err error) {
	key := name + "|" + remote
	if wait, ok := a.limiter.Allow(key); !ok {
		return "", time.Time{}, &LimitedError{RetryAfter: wait}
	}

	user, found, err := a.store.User(ctx)
	if err != nil {
		return "", time.Time{}, err
	}

	if !found {
		a.limiter.Failure(key)

		return "", time.Time{}, ErrBadLogin
	}

	// The hash is verified whatever the name, so a wrong name costs what a wrong password does.
	match, err := VerifyPassword(password, user.PasswordHash)
	if err != nil {
		return "", time.Time{}, err
	}

	if !match || name != user.Name {
		a.limiter.Failure(key)

		return "", time.Time{}, ErrBadLogin
	}

	a.limiter.Reset(key)

	now := a.now()
	if err := a.store.DeleteSessions(ctx, now); err != nil {
		return "", time.Time{}, err
	}

	raw, hash, err := NewToken()
	if err != nil {
		return "", time.Time{}, err
	}

	expires = now.Add(sessionLife)

	return raw, expires, a.store.CreateSession(ctx, hash, now, expires)
}

// Session reports whether the cookie value names a live session, and keeps it alive.
func (a *Service) Session(ctx context.Context, cookie string) (bool, error) {
	hash := HashToken(cookie)

	expiry, found, err := a.store.SessionExpiry(ctx, hash)
	if err != nil || !found {
		return false, err
	}

	now := a.now()
	if !expiry.After(now) {
		return false, nil
	}

	if expiry.Sub(now) < sessionRefreshBelow {
		return true, a.store.ExtendSession(ctx, hash, now.Add(sessionLife))
	}

	return true, nil
}

// Logout ends the session; an unknown cookie value is a no-op.
func (a *Service) Logout(ctx context.Context, cookie string) error {
	return a.store.DeleteSession(ctx, HashToken(cookie))
}

// NewAPIToken stores a token under name and returns it, the only time it is readable.
func (a *Service) NewAPIToken(ctx context.Context, name string) (string, error) {
	raw, _, err := NewToken()
	if err != nil {
		return "", err
	}

	token := apiTokenPrefix + raw

	if err := a.store.CreateAPIToken(ctx, name, HashToken(token), a.now()); err != nil {
		return "", err
	}

	return token, nil
}

// Bearer reports whether the token is a stored API token, and marks it used.
func (a *Service) Bearer(ctx context.Context, token string) (bool, error) {
	return a.store.UseAPIToken(ctx, HashToken(token), a.now())
}
