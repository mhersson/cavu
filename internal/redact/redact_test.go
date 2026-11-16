package redact_test

import (
	"encoding/base64"
	"net/url"
	"testing"

	"github.com/mhersson/cavu/internal/redact"
	"github.com/stretchr/testify/assert"
)

func TestRedact(t *testing.T) {
	const (
		secret = "ghp_abc123XYZ"
		spaced = "pass word+1/=x"
	)

	tests := []struct {
		name    string
		secrets []string
		in      string
		want    string
	}{
		{"plain value", []string{secret}, secret, "[redacted]"},
		{"inside a longer line", []string{secret}, "token=" + secret + " end", "token=[redacted] end"},
		{"repeated", []string{secret}, secret + " " + secret, "[redacted] [redacted]"},
		{"base64 form", []string{secret}, "auth " + base64.StdEncoding.EncodeToString([]byte(secret)), "auth [redacted]"},
		{"url-encoded form", []string{spaced}, "q=" + url.QueryEscape(spaced), "q=[redacted]"},
		{"prefix secret longest wins", []string{"abcdefgh", "abcdefghij"}, "x abcdefghij y", "x [redacted] y"},
		{"prefix secret order independent", []string{"abcdefghij", "abcdefgh"}, "x abcdefghij y", "x [redacted] y"},
		{"short secret left alone", []string{"abcdefg"}, "abcdefg", "abcdefg"},
		{"length counts runes", []string{"åååååååå"}, "åååååååå", "[redacted]"},
		{"seven runes left alone", []string{"ååååååå"}, "ååååååå", "ååååååå"},
		{"empty input", []string{secret}, "", ""},
		{"no secrets", nil, "plain text", "plain text"},
		{"empty secret ignored", []string{""}, "plain text", "plain text"},
		{"no match", []string{secret}, "nothing here", "nothing here"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, redact.New(tt.secrets...)(tt.in))
		})
	}
}
