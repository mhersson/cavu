package push_test

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/cavu/internal/push"
	"github.com/mhersson/cavu/internal/store"
)

func TestSendHeaders(t *testing.T) {
	for name, enc := range map[string]*base64.Encoding{"unpadded keys": base64.RawURLEncoding, "padded keys": base64.URLEncoding} {
		t.Run(name, func(t *testing.T) {
			b := newBrowser(t)
			srv, got := pushService(t, http.StatusCreated, "")
			sender := newSender(t, srv)
			sub := b.subscription(srv.URL+"/push/JzLQ3raZ", enc)
			m := push.Message{Title: "Ready for review · cavu", Body: "Fix the sign-in page", URL: "/tasks/7", Tag: "task-7"}

			require.NoError(t, sender.Send(t.Context(), sub, m))

			p := received(t, got)
			assert.Equal(t, http.MethodPost, p.method)
			assert.Equal(t, "/push/JzLQ3raZ", p.path)
			assert.Equal(t, "aes128gcm", p.header.Get("Content-Encoding"))
			assert.Equal(t, "86400", p.header.Get("TTL"))
			assert.Equal(t, "normal", p.header.Get("Urgency"))
			assert.Equal(t, "task-7", p.header.Get("Topic"))

			authorization, err := sender.Signer.Authorization(sub.Endpoint, sub.Origin)
			require.NoError(t, err)
			assert.Equal(t, authorization, p.header.Get("Authorization"))

			assert.JSONEq(t,
				`{"title":"Ready for review · cavu","body":"Fix the sign-in page","url":"/tasks/7","tag":"task-7"}`,
				string(b.open(t, p.body)))
		})
	}
}

func TestSendGoneSubscription(t *testing.T) {
	m := push.Message{Title: "Needs an answer · cavu", Body: "Add a dark theme", URL: "/tasks/3", Tag: "task-3"}
	send := func(t *testing.T, status int) error {
		t.Helper()

		srv, _ := pushService(t, status, "quota exceeded\nsecond line")

		return newSender(t, srv).Send(t.Context(), newBrowser(t).subscription(srv.URL+"/push/JzLQ3raZ", base64.RawURLEncoding), m)
	}

	for _, status := range []int{http.StatusOK, http.StatusCreated, http.StatusAccepted} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			require.NoError(t, send(t, status))
		})
	}

	for _, status := range []int{http.StatusNotFound, http.StatusGone} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			require.ErrorIs(t, send(t, status), push.ErrGone)
		})
	}

	for _, status := range []int{http.StatusRequestEntityTooLarge, http.StatusTooManyRequests, http.StatusInternalServerError} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			err := send(t, status)
			require.Error(t, err)
			require.NotErrorIs(t, err, push.ErrGone)
			assert.Contains(t, err.Error(), strconv.Itoa(status))
			assert.Contains(t, err.Error(), "quota exceeded")
			assert.NotContains(t, err.Error(), "second line")
		})
	}

	t.Run("unreachable", func(t *testing.T) {
		srv, _ := pushService(t, http.StatusCreated, "")
		srv.Close()

		err := newSender(t, srv).Send(t.Context(), newBrowser(t).subscription(srv.URL+"/push/JzLQ3raZ", base64.RawURLEncoding), m)
		require.Error(t, err)
		require.NotErrorIs(t, err, push.ErrGone)
		assert.NotContains(t, err.Error(), "JzLQ3raZ", "the endpoint is a capability")
	})
}

func TestMessageCut(t *testing.T) {
	// Two title lengths, so that one cut falls inside a two-octet rune.
	for _, title := range []string{strings.Repeat("T", 100), strings.Repeat("T", 101)} {
		b := newBrowser(t)
		srv, got := pushService(t, http.StatusCreated, "")
		m := push.Message{Title: title, Body: strings.Repeat("é", 2000), URL: "/tasks/12345", Tag: "task-12345"}

		require.NoError(t, newSender(t, srv).Send(t.Context(), b.subscription(srv.URL, base64.RawURLEncoding), m))

		plain := b.open(t, received(t, got).body)
		assert.LessOrEqual(t, len(plain), 2048)
		assert.Greater(t, len(plain), 2040)

		var cut push.Message
		require.NoError(t, json.Unmarshal(plain, &cut))
		assert.Equal(t, m.Title, cut.Title)
		assert.Equal(t, m.URL, cut.URL)
		assert.Equal(t, m.Tag, cut.Tag)
		assert.True(t, strings.HasPrefix(m.Body, cut.Body), "cut at a rune boundary")
	}
}

// sent is a push as the push service received it.
type sent struct {
	method, path string
	header       http.Header
	body         []byte
}

// pushService answers a push with status and reply, and passes on what it received.
func pushService(t *testing.T, status int, reply string) (*httptest.Server, <-chan sent) {
	t.Helper()

	got := make(chan sent, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got <- sent{method: r.Method, path: r.URL.Path, header: r.Header.Clone(), body: body}

		w.WriteHeader(status)
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(srv.Close)

	return srv, got
}

// received is the push the service got; Send returns only after the service answered.
func received(t *testing.T, got <-chan sent) sent {
	t.Helper()

	select {
	case p := <-got:
		return p
	default:
		require.FailNow(t, "the push service received nothing")

		return sent{}
	}
}

func newSender(t *testing.T, srv *httptest.Server) *push.Sender {
	t.Helper()

	key, err := push.NewKey()
	require.NoError(t, err)

	now := time.Date(2026, 11, 20, 18, 0, 0, 0, time.UTC)

	return &push.Sender{Client: srv.Client(), Signer: push.NewSigner(key, func() time.Time { return now })}
}

func (b browser) subscription(endpoint string, enc *base64.Encoding) store.PushSubscription {
	return store.PushSubscription{
		Endpoint: endpoint,
		P256DH:   enc.EncodeToString(b.key.PublicKey().Bytes()),
		Auth:     enc.EncodeToString(b.auth),
		Origin:   "https://cavu.example.org",
	}
}
