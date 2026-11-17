package auth_test

import (
	"testing"
	"time"

	"github.com/mhersson/cavu/internal/auth"
	"github.com/stretchr/testify/require"
)

func newLimiter() (*auth.Limiter, *time.Time) {
	clock := time.Unix(1000, 0)

	return auth.NewLimiter(func() time.Time { return clock }), &clock
}

func TestLimiterAllowsFirstFailures(t *testing.T) {
	t.Parallel()

	l, _ := newLimiter()

	for range 2 {
		_, ok := l.Allow("ada|1.2.3.4")
		require.True(t, ok)
		l.Failure("ada|1.2.3.4")
	}

	_, ok := l.Allow("ada|1.2.3.4")
	require.True(t, ok)
}

func TestLimiterBacksOffExponentially(t *testing.T) {
	t.Parallel()

	l, clock := newLimiter()
	key := "ada|1.2.3.4"

	for range 3 {
		l.Failure(key)
	}

	retry, ok := l.Allow(key)
	require.False(t, ok)
	require.Equal(t, time.Second, retry)

	*clock = clock.Add(time.Second)

	_, ok = l.Allow(key)
	require.True(t, ok)

	l.Failure(key)

	retry, ok = l.Allow(key)
	require.False(t, ok)
	require.Equal(t, 2*time.Second, retry)
}

func TestLimiterCapsAtFiveMinutes(t *testing.T) {
	t.Parallel()

	l, _ := newLimiter()

	for i := 1; i <= 100; i++ {
		l.Failure("k")

		if i < 3 {
			continue
		}

		retry, ok := l.Allow("k")
		require.False(t, ok)
		require.Positive(t, retry)
		require.LessOrEqual(t, retry, 5*time.Minute, "failures=%d", i)
	}

	retry, _ := l.Allow("k")
	require.Equal(t, 5*time.Minute, retry)
}

func TestLimiterResetClears(t *testing.T) {
	t.Parallel()

	l, _ := newLimiter()

	for range 5 {
		l.Failure("k")
	}

	l.Reset("k")

	_, ok := l.Allow("k")
	require.True(t, ok)
}

func TestLimiterKeysAreIndependent(t *testing.T) {
	t.Parallel()

	l, _ := newLimiter()

	for range 10 {
		l.Failure("ada|1.2.3.4")
	}

	_, ok := l.Allow("ada|5.6.7.8")
	require.True(t, ok)

	_, ok = l.Allow("bob|1.2.3.4")
	require.True(t, ok)
}
