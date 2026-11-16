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

// limitPrefix opens the pinned release's usage-limit message: "You've hit your
// <limit name>...", the name taken from limitWindows.
const limitPrefix = "You've hit your "

// limitWindows maps the pinned release's limit names back to their
// rateLimitType.
var limitWindows = []struct{ name, window string }{
	{"5-hour limit", "five_hour"},
	{"weekly limit", "seven_day"},
	{"Opus limit", "seven_day_opus"},
	{"Sonnet limit", "seven_day_sonnet"},
	{"Fable limit", "seven_day_overage_included"},
	{"usage credit limit", "overage"},
}

func isUsageLimitText(text string) bool {
	return strings.HasPrefix(text, limitPrefix) || strings.Contains(strings.ToLower(text), "usage limit")
}

// resultLimit names the usage limit a 429 result carried by its text; the
// result has no reset time.
func resultLimit(text string) *driver.RateLimit {
	rl := &driver.RateLimit{Status: "rejected"}

	if rest, ok := strings.CutPrefix(text, limitPrefix); ok {
		for _, w := range limitWindows {
			if strings.HasPrefix(rest, w.name) {
				rl.Window = w.window

				break
			}
		}
	}

	return rl
}
