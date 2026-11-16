package claudecode

import (
	"encoding/json"
	"slices"
	"strings"
)

const redactedSecret = "[REDACTED]"

// newMask returns the function every emitted or returned string passes. Longer
// secrets go first so one that contains another is replaced whole.
func newMask(secrets []string) func(string) string {
	list := slices.DeleteFunc(slices.Clone(secrets), func(s string) bool { return s == "" })
	slices.SortFunc(list, func(a, b string) int { return len(b) - len(a) })

	return func(s string) string {
		for _, secret := range list {
			s = strings.ReplaceAll(s, secret, redactedSecret)
		}

		return s
	}
}

// maskJSON masks raw, and falls back to the masked text as a JSON string when
// masking leaves invalid JSON, so a masked field still marshals.
func maskJSON(mask func(string) string, raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return raw
	}

	masked := mask(string(raw))
	if json.Valid([]byte(masked)) {
		return json.RawMessage(masked)
	}

	quoted, _ := json.Marshal(masked) //nolint:errchkjson // a string always marshals

	return quoted
}
