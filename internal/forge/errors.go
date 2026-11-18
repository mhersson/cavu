package forge

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

var (
	// ErrRateLimited is what a *RateLimitError matches.
	ErrRateLimited = errors.New("forge: rate limited")
	// ErrUnauthorized is what an answer of 401 matches: the forge refused the
	// credential.
	ErrUnauthorized = errors.New("forge: unauthorized")
)

// rateLimitFallback is the wait assumed when a rate limit names no reset time.
const rateLimitFallback = time.Minute

// RateLimitError reports a primary or secondary rate limit and when it lifts;
// it matches ErrRateLimited.
type RateLimitError struct{ Reset time.Time }

func (e *RateLimitError) Error() string {
	return "rate limited until " + e.Reset.Format(time.RFC3339)
}

func (e *RateLimitError) Is(target error) bool { return target == ErrRateLimited }

// httpError is an answer outside 2xx that is not a rate limit.
type httpError struct {
	status  int
	message string // the forge's
}

func (e *httpError) Error() string {
	if e.message == "" {
		return fmt.Sprintf("status %d", e.status)
	}

	return fmt.Sprintf("status %d: %s", e.status, e.message)
}

func (e *httpError) Is(target error) bool {
	return target == ErrUnauthorized && e.status == http.StatusUnauthorized
}

// rateLimit applies GitHub's rate-limit rules to the headers and message of a
// REST answer of 403 or 429, or of a GraphQL answer with errors: when the
// limit lifts, and whether the answer is one.
func rateLimit(h http.Header, message string, now time.Time) (time.Time, bool) {
	if v := h.Get("Retry-After"); v != "" {
		if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
			return now.Add(time.Duration(secs) * time.Second), true
		}

		return now.Add(rateLimitFallback), true
	}

	if h.Get("X-RateLimit-Remaining") == "0" {
		if unix, err := strconv.ParseInt(h.Get("X-RateLimit-Reset"), 10, 64); err == nil {
			return time.Unix(unix, 0), true
		}

		return now.Add(rateLimitFallback), true
	}

	// A secondary limit may carry neither header.
	if strings.Contains(strings.ToLower(message), "secondary rate limit") {
		return now.Add(rateLimitFallback), true
	}

	return time.Time{}, false
}
