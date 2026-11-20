package push

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/mhersson/cavu/internal/store"
)

// maxMessage caps a message's JSON well under the 4096 octets of body every
// push service must accept (RFC 8030 section 7.2).
const maxMessage = 2048

// ErrGone is returned when the push service no longer knows the subscription.
var ErrGone = errors.New("push subscription gone")

// Message is what a notification shows. Tag names what it is about, so a
// newer push replaces one not yet delivered.
type Message struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	URL   string `json:"url"`
	Tag   string `json:"tag"`
}

// Sender posts messages to the push services subscriptions name.
type Sender struct {
	Client *http.Client
	Signer *Signer
}

// Send encrypts m for sub and posts it. It returns ErrGone when the push
// service no longer knows sub.
func (s *Sender) Send(ctx context.Context, sub store.PushSubscription, m Message) error {
	payload, err := m.encode()
	if err != nil {
		return err
	}

	p256dh, err := b64.DecodeString(strings.TrimRight(sub.P256DH, "="))
	if err != nil {
		return fmt.Errorf("subscription key: %w", err)
	}

	auth, err := b64.DecodeString(strings.TrimRight(sub.Auth, "="))
	if err != nil {
		return fmt.Errorf("subscription secret: %w", err)
	}

	body, err := Seal(p256dh, auth, payload)
	if err != nil {
		return err
	}

	authorization, err := s.Signer.Authorization(sub.Endpoint, sub.Origin)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sub.Endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("push request: %w", err)
	}

	req.Header.Set("Authorization", authorization)
	req.Header.Set("Content-Encoding", "aes128gcm")
	req.Header.Set("TTL", "86400")
	req.Header.Set("Urgency", "normal")
	req.Header.Set("Topic", m.Tag)

	resp, err := s.Client.Do(req)
	if err != nil {
		// A *url.Error quotes the endpoint, which is a capability (RFC 8030 section 8.3).
		if ue, ok := errors.AsType[*url.Error](err); ok {
			err = ue.Err
		}

		return fmt.Errorf("push to %s: %w", req.URL.Host, err)
	}

	defer func() { _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		return ErrGone
	case resp.StatusCode >= 200 && resp.StatusCode <= 299:
		return nil
	}

	answer, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	line, _, _ := strings.Cut(strings.TrimSpace(string(answer)), "\n")

	return fmt.Errorf("push to %s: %s: %s", req.URL.Host, resp.Status, strings.TrimSpace(line))
}

// encode is m's JSON within maxMessage octets, its body cut at a rune boundary
// when it would not fit.
func (m Message) encode() ([]byte, error) {
	for {
		b, err := json.Marshal(m)
		if err != nil {
			return nil, fmt.Errorf("push message: %w", err)
		}

		over := len(b) - maxMessage
		if over <= 0 || m.Body == "" {
			return b, nil
		}

		// An octet of the body is at least one octet of its JSON.
		cut := max(len(m.Body)-over, 0)
		for cut > 0 && !utf8.RuneStart(m.Body[cut]) {
			cut--
		}

		m.Body = m.Body[:cut]
	}
}
