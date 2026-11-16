package claudecode

import (
	"encoding/json"
	"errors"
)

// streamLine holds the fields the mapper reads from any stream-json line;
// encoding/json skips the rest.
type streamLine struct {
	Type     string `json:"type"`
	Subtype  string `json:"subtype"`
	IsReplay bool   `json:"isReplay"`

	Error string `json:"error"` // assistant: authentication_failed, ...; api_retry: the error kind

	// system/init
	SessionID    string   `json:"session_id"`
	Model        string   `json:"model"`
	Capabilities []string `json:"capabilities"`

	// system/api_retry
	Attempt     int  `json:"attempt"`
	MaxRetries  int  `json:"max_retries"`
	ErrorStatus *int `json:"error_status"`

	Message *apiMessage `json:"message"` // assistant, user

	Event        *streamEvent `json:"event"` // stream_event
	APIMessageID string       `json:"api_message_id"`

	RateLimitInfo *rateLimitInfo `json:"rate_limit_info"` // rate_limit_event

	// result
	IsError                   bool                  `json:"is_error"`
	APIErrorStatus            *int                  `json:"api_error_status"`
	Result                    string                `json:"result"`
	StructuredOutput          json.RawMessage       `json:"structured_output"`
	Errors                    []string              `json:"errors"`
	NumTurns                  int                   `json:"num_turns"`
	TerminalReason            string                `json:"terminal_reason"`
	TotalCostUSD              float64               `json:"total_cost_usd"`
	Usage                     apiUsage              `json:"usage"`
	ModelUsage                map[string]modelUsage `json:"modelUsage"`
	ModelUsageSinceLastResult map[string]modelUsage `json:"model_usage_since_last_result"`
}

type apiMessage struct {
	ID         string          `json:"id"`
	Model      string          `json:"model"`
	Content    json.RawMessage `json:"content"` // a string for plain user text, else blocks
	StopReason string          `json:"stop_reason"`
	Usage      apiUsage        `json:"usage"`
}

type contentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	IsError   bool            `json:"is_error"`
}

type streamEvent struct {
	Type         string        `json:"type"`
	Index        int           `json:"index"`
	Message      *apiMessage   `json:"message"`       // message_start
	ContentBlock *contentBlock `json:"content_block"` // content_block_start
	Delta        *eventDelta   `json:"delta"`
	Usage        *apiUsage     `json:"usage"` // message_delta
}

type eventDelta struct {
	Type        string `json:"type"`
	PartialJSON string `json:"partial_json"`
	StopReason  string `json:"stop_reason"`
}

// apiUsage keeps absent buckets nil so a later source can fill them.
type apiUsage struct {
	InputTokens              *int64 `json:"input_tokens"`
	OutputTokens             *int64 `json:"output_tokens"`
	CacheReadInputTokens     *int64 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens *int64 `json:"cache_creation_input_tokens"`
}

type modelUsage struct {
	InputTokens              int64   `json:"inputTokens"`
	OutputTokens             int64   `json:"outputTokens"`
	CacheReadInputTokens     int64   `json:"cacheReadInputTokens"`
	CacheCreationInputTokens int64   `json:"cacheCreationInputTokens"`
	CostUSD                  float64 `json:"costUSD"`
}

type rateLimitInfo struct {
	Status        string   `json:"status"`
	ResetsAt      *float64 `json:"resetsAt"`
	RateLimitType string   `json:"rateLimitType"`
}

// decodeLine reports false for a line that is not a JSON object. A known field
// whose type changed is left zero instead of dropping the line, so a newer
// release degrades rather than failing the run.
func decodeLine(b []byte) (streamLine, bool) {
	var l streamLine

	err := json.Unmarshal(b, &l)

	var typeErr *json.UnmarshalTypeError
	if err != nil && !errors.As(err, &typeErr) {
		return streamLine{}, false
	}

	return l, true
}

// blocks decodes a content array; plain-string content yields none.
func blocks(raw json.RawMessage) []contentBlock {
	var out []contentBlock
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}

	return out
}
