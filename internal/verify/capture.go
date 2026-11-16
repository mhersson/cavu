package verify

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

const (
	excerptCap     = 4000
	excerptTail    = 1500
	failBlockLines = 12
)

// failureMarkers flag a line, after leading whitespace, as part of a failure.
// "--- FAIL" and Rust's panic line are handled separately.
var failureMarkers = []string{
	"FAIL", "panic:", "FAILED", "ERROR", "Error:", "error[", "Traceback",
	"AssertionError", "not ok", "npm ERR!", "failures:", "✗", "✕", "●",
}

// jvmFailureRe matches a JVM test summary with failures or errors. The green
// form appears once per class in a passing build and must not match.
var jvmFailureRe = regexp.MustCompile(`^Tests run: \d+, Failures: (?:[1-9]\d*, Errors: \d+|\d+, Errors: [1-9]\d*)`)

// Argv wraps a declared command for execution: bash -c "set -o pipefail; <cmd>".
func Argv(command string) []string {
	return []string{"bash", "-c", "set -o pipefail; " + command}
}

// Excerpt is what a fix prompt gets: the failure lines and "--- FAIL" blocks of
// the output, capped at 4000 bytes keeping the first, then the last 1500 bytes.
// With no failure signal it is the last 4000 bytes.
func Excerpt(output string) string {
	lines := strings.Split(output, "\n")

	var b strings.Builder

	for i := 0; i < len(lines); i++ {
		trimmed := strings.TrimLeft(lines[i], " \t")

		switch {
		case strings.HasPrefix(trimmed, "--- FAIL"):
			i += writeFailBlock(&b, lines, i) - 1
		case isFailureLine(trimmed):
			b.WriteString(lines[i])
			b.WriteByte('\n')
		}

		if b.Len() >= excerptCap {
			break
		}
	}

	if b.Len() == 0 {
		return tail(output, excerptCap)
	}

	head := strings.TrimRight(b.String(), "\n")
	if len(head) > excerptCap {
		cut := excerptCap
		for cut > 0 && !utf8.RuneStart(head[cut]) {
			cut--
		}

		head = head[:cut]
	}

	return head + "\n\nOutput tail:\n" + tail(output, excerptTail)
}

func isFailureLine(trimmed string) bool {
	if strings.HasPrefix(trimmed, "[ERROR]") || strings.HasPrefix(trimmed, "[FATAL]") ||
		jvmFailureRe.MatchString(trimmed) {
		return true
	}

	for _, m := range failureMarkers {
		if strings.HasPrefix(trimmed, m) {
			return true
		}
	}

	return strings.HasPrefix(trimmed, "thread '") && strings.Contains(trimmed, "panicked")
}

// isStopLine closes a "--- FAIL" window: a new test result or the suite summary.
func isStopLine(trimmed string) bool {
	for _, p := range []string{"--- ", "=== ", "FAIL", "ok", "PASS"} {
		if strings.HasPrefix(trimmed, p) {
			return true
		}
	}

	return false
}

// writeFailBlock writes lines[i] and the lines after it up to the window or the
// next stop line, and returns how many lines it consumed.
func writeFailBlock(b *strings.Builder, lines []string, i int) int {
	b.WriteString(lines[i])
	b.WriteByte('\n')

	consumed := 1

	for j := i + 1; j < len(lines) && j <= i+failBlockLines; j++ {
		if isStopLine(strings.TrimLeft(lines[j], " \t")) {
			break
		}

		b.WriteString(lines[j])
		b.WriteByte('\n')

		consumed++
	}

	return consumed
}

// tail returns at most the last n bytes, starting on a rune boundary.
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}

	start := len(s) - n
	for start < len(s) && !utf8.RuneStart(s[start]) {
		start++
	}

	return s[start:]
}
