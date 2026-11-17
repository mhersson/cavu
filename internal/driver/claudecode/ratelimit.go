package claudecode

import (
	"strings"
	"time"

	"github.com/mhersson/cavu/internal/driver"
)

// parseRateLimit tolerates every field missing: a CLI talking to an endpoint
// that sends no rate-limit headers reports only the status.
func parseRateLimit(info *rateLimitInfo) driver.RateLimit {
	if info == nil {
		return driver.RateLimit{}
	}

	var resets time.Time
	if info.ResetsAt != nil {
		resets = time.Unix(int64(*info.ResetsAt), 0).UTC()
	}

	return driver.RateLimit{Window: info.RateLimitType, Status: info.Status, ResetsAt: resets}
}

// usageLimitCode is the structured api_error of a usage limit; the text is the
// fallback for a release without it.
const usageLimitCode = "usage_limit_reached"

// limitPrefix opens the pinned release's usage-limit message: "You've hit your
// <limit name>...".
const limitPrefix = "You've hit your "

func isUsageLimitText(text string) bool {
	return strings.HasPrefix(text, limitPrefix) || strings.Contains(strings.ToLower(text), "usage limit")
}
