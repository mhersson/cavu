// Package engine holds the task lifecycle: a task's state is what its
// append-only events add up to, and each next action is decided from that
// state alone.
package engine

import "time"

// Event types, each with the actor that records it. A type with a payload
// encodes it as the struct named after the type with a Payload suffix.
const (
	TaskCreated    = "task_created"    // user
	TaskStarted    = "task_started"    // user
	HumanAnswered  = "human_answered"  // user
	HumanRetried   = "human_retried"   // user
	HumanCancelled = "human_cancelled" // user
	RunStarted     = "run_started"     // engine
	PrepareStarted = "prepare_started" // engine
	Prepared       = "prepared"        // runner
	PrepareFailed  = "prepare_failed"  // runner
	SessionStarted = "session_started" // engine
	AgentText      = "agent_text"      // agent
	AgentTool      = "agent_tool"      // agent
	RateLimit      = "rate_limit"      // agent
	EgressDenied   = "egress_denied"   // runner
	SessionEnded   = "session_ended"   // runner
	VerifyStarted  = "verify_started"  // engine
	VerifyEnded    = "verify_ended"    // runner
	ExportStarted  = "export_started"  // engine
	Exported       = "exported"        // runner
	Stopped        = "stopped"         // engine
	RunEnded       = "run_ended"       // engine
)

// Session outcomes.
const (
	OutcomeDone       = "done"
	OutcomeBlocked    = "blocked"
	OutcomeFailed     = "failed"
	OutcomeUnfinished = "unfinished"
	OutcomeCrashed    = "crashed"
	OutcomeMaxTurns   = "max_turns"
	OutcomeTimeout    = "timeout"
	OutcomeUsageLimit = "usage_limit"
	OutcomeEndpoint   = "endpoint"
)

// Verify results.
const (
	ResultPassed  = "passed"
	ResultFailed  = "failed"
	ResultTimeout = "timeout"
	ResultWaived  = "waived"
)

// Work modes, of a session and of the work action that starts it.
const (
	ModeStart     = "start"
	ModeContinue  = "continue"
	ModeFixVerify = "fix_verify"
	ModeAnswer    = "answer"
)

// Event payloads. A zero time is left out of the JSON. Why is the one line
// Decide gave for the action a claim records.
type (
	HumanAnsweredPayload struct {
		Text string `json:"text"`
	}

	RunStartedPayload struct {
		Reason string `json:"reason"`
	}

	PrepareStartedPayload struct {
		Why string `json:"why"`
	}

	PreparedPayload struct {
		BaseCommit string   `json:"base_commit"`
		Tools      []string `json:"tools"`
		Baseline   string   `json:"baseline"` // result of the verify on the base commit
	}

	PrepareFailedPayload struct {
		Step   string `json:"step"`
		Output string `json:"output"`
	}

	SessionStartedPayload struct {
		Mode      string `json:"mode"`
		SessionID string `json:"session_id"`
		Resume    bool   `json:"resume"`
		Automatic bool   `json:"automatic"` // counted toward its mode's cap
		Why       string `json:"why,omitempty"`
	}

	AgentTextPayload struct {
		Text string `json:"text"`
	}

	AgentToolPayload struct {
		Name    string `json:"name"`
		Summary string `json:"summary"`
	}

	RateLimitPayload struct {
		Window   string    `json:"window"`
		Status   string    `json:"status"`
		ResetsAt time.Time `json:"resets_at,omitzero"`
	}

	EgressDeniedPayload struct {
		Host string `json:"host"`
	}

	SessionEndedPayload struct {
		Outcome         string    `json:"outcome"`
		Title           string    `json:"title"`
		Summary         string    `json:"summary"`
		Assumptions     []string  `json:"assumptions"`
		Questions       []string  `json:"questions"`
		Changed         bool      `json:"changed"`
		Turns           int       `json:"turns"`
		CostEstimateUSD float64   `json:"cost_estimate_usd"`
		ResetsAt        time.Time `json:"resets_at,omitzero"`
		Detail          string    `json:"detail"`
	}

	VerifyStartedPayload struct {
		Why string `json:"why"`
	}

	VerifyEndedPayload struct {
		Result     string `json:"result"`
		Command    string `json:"command"`
		ExitCode   int    `json:"exit_code"`
		Output     string `json:"output"`
		DurationMS int64  `json:"duration_ms"`
	}

	ExportStartedPayload struct {
		Why string `json:"why"`
	}

	ExportedPayload struct {
		HeadCommit   string `json:"head_commit"`
		DiffPath     string `json:"diff_path"`
		BundlePath   string `json:"bundle_path"`
		FilesChanged int    `json:"files_changed"`
	}

	StoppedPayload struct {
		Reason      string    `json:"reason"`
		Explanation string    `json:"explanation"`
		Action      string    `json:"action"` // the one action that clears the stop
		ResetsAt    time.Time `json:"resets_at,omitzero"`
		Why         string    `json:"why,omitempty"`    // empty for a runner fault or an export problem
		Detail      string    `json:"detail,omitempty"` // the whole text of a runner fault or an export problem
	}

	RunEndedPayload struct {
		Outcome string `json:"outcome"` // "exported", or the stop reason that ended the run
		Why     string `json:"why,omitempty"`
	}
)
