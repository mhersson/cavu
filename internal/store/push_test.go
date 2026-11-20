package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestPushSubscriptions(t *testing.T) {
	t.Parallel()

	s := openTest(t)
	ctx := context.Background()
	created, delivered := time.UnixMilli(1_763_650_000_000), time.UnixMilli(1_763_650_060_000)

	phone := PushSubscription{Endpoint: "https://push.example.org/a", P256DH: "k1", Auth: "s1", Origin: "https://cavu.example.org", CreatedAt: created}
	laptop := PushSubscription{Endpoint: "https://push.example.org/b", P256DH: "k2", Auth: "s2", Origin: "https://cavu.example.org", CreatedAt: created}

	var err error

	phone.ID, err = s.PutPushSubscription(ctx, phone)
	require.NoError(t, err)

	laptop.ID, err = s.PutPushSubscription(ctx, laptop)
	require.NoError(t, err)
	require.NotEqual(t, phone.ID, laptop.ID)

	require.NoError(t, s.PushDelivered(ctx, phone.ID, delivered))

	again := phone
	again.P256DH, again.Auth, again.Origin, again.CreatedAt = "k3", "s3", "https://cavu.example.net", delivered

	id, err := s.PutPushSubscription(ctx, again)
	require.NoError(t, err)
	require.Equal(t, phone.ID, id, "the same endpoint is the same subscription")

	phone.P256DH, phone.Auth, phone.Origin, phone.LastOKAt = "k3", "s3", "https://cavu.example.net", &delivered

	got, err := s.PushSubscriptions(ctx)
	require.NoError(t, err)
	require.Equal(t, []PushSubscription{phone, laptop}, got, "new keys and origin, the first creation and the last delivery kept")

	require.NoError(t, s.RemovePushSubscription(ctx, phone.ID))
	require.ErrorIs(t, s.RemovePushSubscription(ctx, phone.ID), ErrNotFound)
	require.NoError(t, s.PushDelivered(ctx, phone.ID, delivered), "a delivery to a subscription removed meanwhile")

	got, err = s.PushSubscriptions(ctx)
	require.NoError(t, err)
	require.Equal(t, []PushSubscription{laptop}, got)
}

func TestPushNotified(t *testing.T) {
	t.Parallel()

	s := openTest(t)
	ctx := context.Background()
	first, later := time.UnixMilli(1_763_650_000_000), time.UnixMilli(1_763_650_060_000)

	got, err := s.PushNotified(ctx)
	require.NoError(t, err)
	require.Empty(t, got)

	require.NoError(t, s.MarkPushNotified(ctx, 7, Notified{Way: "answer", Since: first}))
	require.NoError(t, s.MarkPushNotified(ctx, 9, Notified{Way: "take", Since: first}))

	got, err = s.PushNotified(ctx)
	require.NoError(t, err)
	require.Equal(t, map[int64]Notified{7: {"answer", first}, 9: {"take", first}}, got)

	require.NoError(t, s.MarkPushNotified(ctx, 7, Notified{Way: "review", Since: later}))

	got, err = s.PushNotified(ctx)
	require.NoError(t, err)
	require.Equal(t, map[int64]Notified{7: {"review", later}, 9: {"take", first}}, got)
}
