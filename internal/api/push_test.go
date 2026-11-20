package api_test

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	endpoint = "https://push.example.org/send/JzLQ3raZ"
	origin   = "https://cavu.example.org"
)

var b64 = base64.RawURLEncoding

// pushBody is a browser's subscription as PUT /api/push takes it.
func pushBody(endpoint, p256dh, auth string, focused bool) string {
	b, _ := json.Marshal(map[string]any{
		"endpoint": endpoint, "keys": map[string]string{"p256dh": p256dh, "auth": auth}, "origin": origin, "focused": focused,
	})

	return string(b)
}

// browserKeys are a new subscription's keys as a browser gives them.
func browserKeys(t *testing.T) (p256dh, auth string) {
	t.Helper()

	key, err := ecdh.P256().GenerateKey(rand.Reader)
	require.NoError(t, err)

	secret := make([]byte, 16)
	_, _ = rand.Read(secret)

	return b64.EncodeToString(key.PublicKey().Bytes()), b64.EncodeToString(secret)
}

// putPush sends body to PUT /api/push under the API token and returns the id it answers.
func (f *fixture) putPush(t *testing.T, body string) int64 {
	t.Helper()

	status, got := f.call(t, "PUT /api/push", body)
	require.Equal(t, http.StatusOK, status, got)

	var v struct{ ID int64 }
	require.NoError(t, json.Unmarshal([]byte(got), &v))
	assert.JSONEq(t, fmt.Sprintf(`{"id": %d}`, v.ID), got)

	return v.ID
}

func TestPushRoutes(t *testing.T) {
	f := newServer(t)
	p256dh, auth := browserKeys(t)

	id := f.putPush(t, pushBody(endpoint, p256dh, auth, true))

	done := f.focus.Connect(id) // as the device's open stream does
	defer done()

	assert.True(t, f.focus.Looking(id))

	newP256DH, newAuth := browserKeys(t)
	assert.Equal(t, id, f.putPush(t, pushBody(endpoint, newP256DH, newAuth, false)), "the same endpoint is the same device")
	assert.False(t, f.focus.Looking(id), "the later word on focus holds")

	subs, err := f.store.PushSubscriptions(t.Context())
	require.NoError(t, err)
	require.Len(t, subs, 1)
	assert.Equal(t, []string{endpoint, newP256DH, newAuth, origin}, []string{subs[0].Endpoint, subs[0].P256DH, subs[0].Auth, subs[0].Origin})
	assert.Equal(t, f.clock.Now(), subs[0].CreatedAt.UTC())

	// The page sends its session's cookie with the CSRF header.
	resp, got := send(t, withCookie(f.request(t, "PUT /api/push", pushBody(endpoint+"-tab", p256dh, auth, false)), f.login(t).Value, true))
	require.Equal(t, http.StatusOK, resp.StatusCode, got)

	for name, body := range map[string]string{
		"an http endpoint":                pushBody("http://push.example.org/send/JzLQ3raZ-1", p256dh, auth, true),
		"an endpoint with no host":        pushBody("https:///send/JzLQ3raZ-2", p256dh, auth, true),
		"a relative endpoint":             pushBody("/send/JzLQ3raZ-3", p256dh, auth, true),
		"a key that is not base64url":     pushBody(endpoint+"-4", "not+base64/url", auth, true),
		"a key that is no P-256 point":    pushBody(endpoint+"-5", b64.EncodeToString(append([]byte{4}, make([]byte, 64)...)), auth, true),
		"a secret that is not base64url":  pushBody(endpoint+"-6", p256dh, "not+base64/url", true),
		"a secret that is not 16 octets":  pushBody(endpoint+"-7", p256dh, b64.EncodeToString(make([]byte, 8)), true),
		"a field the route does not take": strings.TrimSuffix(pushBody(endpoint+"-8", p256dh, auth, true), "}") + `, "admin": true}`,
	} {
		t.Run(name, func(t *testing.T) {
			status, got := f.call(t, "PUT /api/push", body)
			require.Equal(t, http.StatusBadRequest, status, got)
			assertError(t, got)
			assert.NotContains(t, got, "JzLQ3raZ", "the endpoint is a capability")
		})
	}

	subs, err = f.store.PushSubscriptions(t.Context())
	require.NoError(t, err)
	assert.Len(t, subs, 2, "a refused subscription is not stored")

	route := fmt.Sprintf("DELETE /api/push/%d", id)

	status, got := f.call(t, route, "")
	require.Equal(t, http.StatusNoContent, status, got)
	assert.Empty(t, got)

	status, got = f.call(t, route, "")
	require.Equal(t, http.StatusNotFound, status, got)
	assertError(t, got)

	subs, err = f.store.PushSubscriptions(t.Context())
	require.NoError(t, err)
	require.Len(t, subs, 1)
	assert.Equal(t, endpoint+"-tab", subs[0].Endpoint)
}

func TestFocusEndsWithTheStream(t *testing.T) {
	f := newServer(t)
	p256dh, auth := browserKeys(t)
	id := f.putPush(t, pushBody(endpoint, p256dh, auth, true))

	assert.False(t, f.focus.Looking(id), "focused, with no stream open")

	f.stream(t, "", "")
	assert.False(t, f.focus.Looking(id), "a stream that names no device holds none")

	s := f.stream(t, fmt.Sprintf("?device=%d", id), "")
	assert.True(t, f.focus.Looking(id), "focused, with its stream open")

	require.NoError(t, s.resp.Body.Close())
	require.Eventually(t, func() bool { return !f.focus.Looking(id) }, within, 10*time.Millisecond, "the closed stream holds it no longer")
}
