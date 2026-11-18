package forge

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRateLimitIsReported(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)

	tests := []struct {
		name      string
		status    int
		header    map[string]string
		message   string
		wantReset time.Time // zero: not a rate limit
	}{
		{
			"an exhausted limit", http.StatusForbidden,
			map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": "1700000900"},
			"API rate limit exceeded", time.Unix(1_700_000_900, 0),
		},
		{"429 with Retry-After", http.StatusTooManyRequests, map[string]string{"Retry-After": "30"}, "", now.Add(30 * time.Second)},
		{"403 with Retry-After", http.StatusForbidden, map[string]string{"Retry-After": "5"}, "", now.Add(5 * time.Second)},
		{
			"a secondary limit with neither header", http.StatusForbidden, nil,
			"You have exceeded a Secondary Rate Limit. Please wait.", now.Add(time.Minute),
		},
		{"an exhausted limit with no reset", http.StatusForbidden, map[string]string{"X-RateLimit-Remaining": "0"}, "", now.Add(time.Minute)},
		{"404 mentioning a secondary limit", http.StatusNotFound, nil, "secondary rate limit", time.Time{}},
		{"403 without headers", http.StatusForbidden, nil, "Resource not accessible", time.Time{}},
		{"403 with requests left", http.StatusForbidden, map[string]string{"X-RateLimit-Remaining": "10"}, "", time.Time{}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestClient(t, func() time.Time { return now }, "api.github.com", answer(tc.status, tc.header, tc.message), nil)

			err := c.Check(t.Context(), testRepo, Access{Token: testToken})
			require.Error(t, err)
			require.NotErrorIs(t, err, ErrUnauthorized)

			if tc.wantReset.IsZero() {
				require.NotErrorIs(t, err, ErrRateLimited)

				he, ok := errors.AsType[*httpError](err)
				require.True(t, ok, "%v", err)
				assert.Equal(t, tc.status, he.status)

				return
			}

			var rl *RateLimitError

			require.ErrorAs(t, err, &rl)
			require.ErrorIs(t, err, ErrRateLimited)
			assert.True(t, rl.Reset.Equal(tc.wantReset), "reset %v, want %v", rl.Reset, tc.wantReset)
		})
	}

	t.Run("a 401 is unauthorized", func(t *testing.T) {
		c := newTestClient(t, nil, "api.github.com", answer(http.StatusUnauthorized, nil, "Bad credentials"), nil)

		err := c.Check(t.Context(), testRepo, Access{Token: testToken})
		require.ErrorIs(t, err, ErrUnauthorized)
		assert.NotErrorIs(t, err, ErrRateLimited)
	})
}

// answer is a handler that gives every request the same status, headers and
// GitHub's JSON message.
func answer(status int, header map[string]string, message string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		for k, v := range header {
			w.Header().Set(k, v)
		}

		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]string{"message": message})
	})
}
