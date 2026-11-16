package verify

// Results of a verify command. The engine records these exact strings.
const (
	Passed  = "passed"
	Failed  = "failed"
	Timeout = "timeout"
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
