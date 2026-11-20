package push_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/cavu/internal/engine"
	"github.com/mhersson/cavu/internal/push"
	"github.com/mhersson/cavu/internal/store"
)

var since = time.UnixMilli(1_763_650_000_000)

func TestNotifierSendsEachNeedOnce(t *testing.T) {
	b := newBench(t)
	phone, laptop := b.subscribe(t, http.StatusCreated), b.subscribe(t, http.StatusCreated)
	// Finer than the milliseconds the table keeps.
	b.rows = []engine.Row{need(7, engine.ClearAnswer, since.Add(time.Microsecond))}
	n := b.notifier()

	n.Pass(t.Context())
	assert.Len(t, phone.messages(t), 1)
	assert.Len(t, laptop.messages(t), 1)

	for _, s := range b.subscriptions(t) {
		assert.NotNil(t, s.LastOKAt, "subscription %d delivered", s.ID)
	}

	n.Pass(t.Context())
	assert.Empty(t, phone.messages(t), "notified already")
	assert.Empty(t, laptop.messages(t), "notified already")

	b.rows = []engine.Row{need(7, engine.NeedsReview, since.Add(time.Hour))}

	n.Pass(t.Context())
	assert.Len(t, phone.messages(t), 1, "a new need of the same task")
	assert.Len(t, laptop.messages(t), 1, "a new need of the same task")

	b.notifier().Pass(t.Context())
	assert.Empty(t, phone.messages(t), "a second notifier knows what the first sent")
	assert.Empty(t, laptop.messages(t), "a second notifier knows what the first sent")
	assert.Empty(t, b.errs)
}

func TestNotifierWithoutSubscriptionsRecordsOnly(t *testing.T) {
	b := newBench(t)
	b.rows = []engine.Row{need(3, engine.ClearRetry, since)}
	n := b.notifier()

	n.Pass(t.Context())

	notified, err := b.store.PushNotified(t.Context())
	require.NoError(t, err)
	assert.Equal(t, map[int64]store.Notified{3: {Way: engine.ClearRetry, Since: since}}, notified)

	phone := b.subscribe(t, http.StatusCreated)

	n.Pass(t.Context())
	assert.Empty(t, phone.messages(t), "a need from before the first subscription")

	b.rows = append(b.rows, need(4, engine.NeedsTake, since))

	n.Pass(t.Context())

	got := phone.messages(t)
	require.Len(t, got, 1)
	assert.Equal(t, "task-4", got[0].Tag)
	assert.Empty(t, b.errs)
}

func TestNotifierSkipsTheDeviceBeingLookedAt(t *testing.T) {
	b := newBench(t)
	looking, away := b.subscribe(t, http.StatusCreated), b.subscribe(t, http.StatusCreated)
	b.focus.Set(looking.id, true)
	t.Cleanup(b.focus.Connect(looking.id))
	b.rows = []engine.Row{need(5, engine.NeedsReview, since)}

	b.notifier().Pass(t.Context())
	assert.Empty(t, looking.messages(t))
	assert.Len(t, away.messages(t), 1)
	assert.Empty(t, b.errs)
}

func TestNotifierDropsAGoneSubscription(t *testing.T) {
	b := newBench(t)
	gone, kept := b.subscribe(t, http.StatusGone), b.subscribe(t, http.StatusCreated)
	b.rows = []engine.Row{need(6, engine.ClearAnswer, since), need(8, engine.NeedsTake, since)}

	b.notifier().Pass(t.Context())
	assert.Len(t, gone.messages(t), 1, "asked once, then dropped")
	assert.Len(t, kept.messages(t), 2)

	subs := b.subscriptions(t)
	require.Len(t, subs, 1)
	assert.Equal(t, kept.id, subs[0].ID)
	assert.Empty(t, b.errs)
}

func TestNotifierMessage(t *testing.T) {
	b := newBench(t)
	phone := b.subscribe(t, http.StatusCreated)

	var want []push.Message

	for i, c := range []struct {
		way   string
		title string
	}{
		{engine.ClearAnswer, "Needs an answer · demo"},
		{engine.NeedsReview, "Ready for review · demo"},
		{engine.ClearRetry, "Stopped · demo"},
		{engine.NeedsTake, "A change to take · demo"},
	} {
		id := int64(11 + i)
		b.rows = append(b.rows, need(id, c.way, since))
		want = append(want, push.Message{Title: c.title, Body: fmt.Sprintf("Task %d", id), URL: fmt.Sprintf("/tasks/%d", id), Tag: fmt.Sprintf("task-%d", id)})
	}

	b.notifier().Pass(t.Context())
	assert.Equal(t, want, phone.messages(t))
}

func TestNotifierKeepsGoing(t *testing.T) {
	b := newBench(t)
	failing, phone := b.subscribe(t, http.StatusInternalServerError), b.subscribe(t, http.StatusCreated)
	down := errors.New("view down")
	lists := 0
	n := b.notifier()
	n.Tick = 5 * time.Millisecond
	n.List = func(context.Context) ([]engine.Row, error) {
		if lists++; lists == 1 {
			return nil, down
		}

		return []engine.Row{need(9, engine.ClearAnswer, since)}, nil
	}

	ctx, cancel := context.WithCancel(t.Context())
	ran := make(chan error, 1)

	go func() { ran <- n.Run(ctx) }()

	// failing's push comes first, so its error is reported before phone's push arrives.
	select {
	case <-phone.got:
	case <-time.After(10 * time.Second):
		require.FailNow(t, "the push after the errors never came")
	}

	cancel()
	require.NoError(t, <-ran)

	require.Len(t, b.errs, 2)
	require.ErrorIs(t, b.errs[0], down)
	require.NotErrorIs(t, b.errs[1], push.ErrGone)
	assert.NotContains(t, b.errs[1].Error(), "JzLQ3raZ", "the endpoint is a capability")
	assert.Len(t, failing.messages(t), 1)
	assert.Len(t, b.subscriptions(t), 2, "a failed push keeps its subscription")
}

// bench is a notifier's world: a real store, the rows its list gives, the
// focus, and what it reported.
type bench struct {
	store  *store.Store
	rows   []engine.Row
	focus  *push.Focus
	sender *push.Sender
	errs   []error
}

func newBench(t *testing.T) *bench {
	t.Helper()

	s, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "cavu.db"))
	require.NoError(t, err)

	t.Cleanup(func() { _ = s.Close() })

	key, err := push.NewKey()
	require.NoError(t, err)

	return &bench{store: s, focus: &push.Focus{}, sender: &push.Sender{Client: &http.Client{}, Signer: push.NewSigner(key, time.Now)}}
}

func (b *bench) notifier() *push.Notifier {
	return &push.Notifier{
		List:    func(context.Context) ([]engine.Row, error) { return b.rows, nil },
		Store:   b.store,
		Sender:  b.sender,
		Focus:   b.focus,
		OnError: func(err error) { b.errs = append(b.errs, err) },
	}
}

// device is a subscribed browser and what its push service received.
type device struct {
	browser

	id  int64
	got <-chan sent
}

// subscribe subscribes a new browser whose push service answers status.
func (b *bench) subscribe(t *testing.T, status int) device {
	t.Helper()

	br := newBrowser(t)
	srv, got := pushService(t, status, "")

	id, err := b.store.PutPushSubscription(t.Context(), br.subscription(srv.URL+"/push/JzLQ3raZ", base64.RawURLEncoding))
	require.NoError(t, err)

	return device{browser: br, id: id, got: got}
}

func (b *bench) subscriptions(t *testing.T) []store.PushSubscription {
	t.Helper()

	subs, err := b.store.PushSubscriptions(t.Context())
	require.NoError(t, err)

	return subs
}

// messages are the pushes d's service received since the last call, decrypted.
func (d device) messages(t *testing.T) []push.Message {
	t.Helper()

	var out []push.Message

	for {
		select {
		case p := <-d.got:
			var m push.Message
			require.NoError(t, json.Unmarshal(d.open(t, p.body), &m))

			out = append(out, m)
		default:
			return out
		}
	}
}

// need is a row of task id needing a person in way since at.
func need(id int64, way string, at time.Time) engine.Row {
	return engine.Row{ID: id, Project: "demo", Title: fmt.Sprintf("Task %d", id), NeedsYou: true, Needs: way, NeedsYouSince: &at}
}
