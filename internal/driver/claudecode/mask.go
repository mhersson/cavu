package claudecode

import "encoding/json"

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
