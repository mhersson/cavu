package auth

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mhersson/cavu/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLimiterPrunesIdleEntries(t *testing.T) {
	t.Parallel()

	clock := time.Unix(1000, 0)
	l := NewLimiter(func() time.Time { return clock })

	for i := range limiterPruneAbove + 100 {
		l.Failure(fmt.Sprintf("user%d|ip", i))
	}

	clock = clock.Add(2 * time.Hour)

	l.Failure("trigger|prune")

	l.mu.Lock()
	defer l.mu.Unlock()

	require.Len(t, l.entries, 1)
}

func TestLoginLimiterKeyHasAFixedSize(t *testing.T) {
	t.Parallel()

	s, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "cavu.db"))
	require.NoError(t, err)

	defer s.Close()

	// No user is set, so a failed login runs no argon2 verify.
	clock := time.Unix(1000, 0)
	svc := NewService(s, func() time.Time { return clock })
	long := strings.Repeat("a", 1<<20)

	for range limiterFreeFailures {
		_, _, err := svc.Login(t.Context(), long, "wrong password!", "10.0.0.1")
		require.ErrorIs(t, err, ErrBadLogin)
	}

	_, _, err = svc.Login(t.Context(), long, "wrong password!", "10.0.0.1")
	_, limited := errors.AsType[*LimitedError](err)
	require.True(t, limited, "the long name is counted and blocked")

	_, _, err = svc.Login(t.Context(), "ada", "wrong password!", "10.0.0.1")
	require.ErrorIs(t, err, ErrBadLogin, "another name from the address is not blocked")

	svc.limiter.mu.Lock()
	defer svc.limiter.mu.Unlock()

	require.Len(t, svc.limiter.entries, 2)

	var sizes []int
	for key := range svc.limiter.entries {
		sizes = append(sizes, len(key))
	}

	assert.Equal(t, sizes[0], sizes[1], "a key's size does not follow the name's")
}
