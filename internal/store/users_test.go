package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

var epoch = time.UnixMilli(1_700_000_000_000)

func TestUsers(t *testing.T) {
	t.Parallel()

	s := openTest(t)
	ctx := context.Background()

	_, ok, err := s.User(ctx)
	require.NoError(t, err)
	require.False(t, ok)

	require.NoError(t, s.PutUser(ctx, User{Name: "ada", PasswordHash: "h1", CreatedAt: epoch}))
	require.NoError(t, s.PutUser(ctx, User{Name: "grace", PasswordHash: "h2", CreatedAt: epoch.Add(time.Hour)}))

	u, ok, err := s.User(ctx)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "grace", u.Name)
	require.Equal(t, "h2", u.PasswordHash)
	require.True(t, epoch.Equal(u.CreatedAt), "the first creation time is kept")

	var n int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n))
	require.Equal(t, 1, n)
}

func TestSessions(t *testing.T) {
	t.Parallel()

	s := openTest(t)
	ctx := context.Background()

	_, ok, err := s.SessionExpiry(ctx, "a")
	require.NoError(t, err)
	require.False(t, ok)

	require.NoError(t, s.CreateSession(ctx, "a", epoch, epoch.Add(time.Hour)))
	require.NoError(t, s.CreateSession(ctx, "b", epoch, epoch.Add(3*time.Hour)))

	got, ok, err := s.SessionExpiry(ctx, "a")
	require.NoError(t, err)
	require.True(t, ok)
	require.True(t, epoch.Add(time.Hour).Equal(got))

	require.NoError(t, s.ExtendSession(ctx, "a", epoch.Add(2*time.Hour)))

	got, _, err = s.SessionExpiry(ctx, "a")
	require.NoError(t, err)
	require.True(t, epoch.Add(2*time.Hour).Equal(got))

	require.NoError(t, s.DeleteSessions(ctx, epoch.Add(150*time.Minute)))

	_, ok, err = s.SessionExpiry(ctx, "a")
	require.NoError(t, err)
	require.False(t, ok)

	_, ok, err = s.SessionExpiry(ctx, "b")
	require.NoError(t, err)
	require.True(t, ok)

	require.NoError(t, s.DeleteSession(ctx, "b"))

	_, ok, err = s.SessionExpiry(ctx, "b")
	require.NoError(t, err)
	require.False(t, ok)

	require.NoError(t, s.CreateSession(ctx, "c", epoch, epoch.Add(time.Hour)))
	require.NoError(t, s.DeleteSessions(ctx, time.Time{}))

	_, ok, err = s.SessionExpiry(ctx, "c")
	require.NoError(t, err)
	require.False(t, ok)
}

func TestAPITokenRows(t *testing.T) {
	t.Parallel()

	s := openTest(t)
	ctx := context.Background()

	require.NoError(t, s.CreateAPIToken(ctx, "ci", "hash-ci", epoch))
	require.ErrorIs(t, s.CreateAPIToken(ctx, "ci", "other", epoch), ErrConflict)
	require.NoError(t, s.CreateAPIToken(ctx, "bot", "hash-bot", epoch.Add(time.Minute)))

	ok, err := s.UseAPIToken(ctx, "unknown", epoch)
	require.NoError(t, err)
	require.False(t, ok)

	used := epoch.Add(time.Hour)
	ok, err = s.UseAPIToken(ctx, "hash-ci", used)
	require.NoError(t, err)
	require.True(t, ok)

	tokens, err := s.APITokens(ctx)
	require.NoError(t, err)
	require.Len(t, tokens, 2)
	require.Equal(t, "bot", tokens[0].Name)
	require.Nil(t, tokens[0].LastUsedAt)
	require.Equal(t, "ci", tokens[1].Name)
	require.True(t, epoch.Equal(tokens[1].CreatedAt))
	require.NotNil(t, tokens[1].LastUsedAt)
	require.True(t, used.Equal(*tokens[1].LastUsedAt))

	require.NoError(t, s.DeleteAPIToken(ctx, "ci"))
	require.ErrorIs(t, s.DeleteAPIToken(ctx, "ci"), ErrNotFound)

	ok, err = s.UseAPIToken(ctx, "hash-ci", used)
	require.NoError(t, err)
	require.False(t, ok)
}
