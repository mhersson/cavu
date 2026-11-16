package verify

import "time"

// Results of a verify command. The engine records these exact strings.
const (
	Passed  = "passed"
	Failed  = "failed"
	Timeout = "timeout"
)

const (
	defaultTimeout = 10 * time.Minute
	minTimeout     = 30 * time.Second
	maxTimeout     = 2 * time.Hour
)

// Classify maps a finished command to Passed, Failed or Timeout. A timeout wins
// over any exit code, and any non-zero code, including 127, is a failure.
func Classify(exitCode int, timedOut bool) string {
	switch {
	case timedOut:
		return Timeout
	case exitCode == 0:
		return Passed
	default:
		return Failed
	}
}

// ClampTimeout bounds a verify timeout to 30 seconds to 2 hours; zero or less
// gives 10 minutes. Sub-second values clamp to the floor, not to the default.
func ClampTimeout(d time.Duration) time.Duration {
	if d <= 0 {
		d = defaultTimeout
	}

	return min(max(d, minTimeout), maxTimeout)
}
