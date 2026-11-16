// Package driver is the agent-neutral interface CAVU runs a coding agent
// through: a session spec goes in, normalised events and one result come out.
package driver

import (
	"context"
	"encoding/json"
	"time"

	"github.com/mhersson/cavu/internal/proc"
)

// Event kinds.
const (
	KindStarted    = "started"
	KindText       = "text"
	KindToolUse    = "tool_use"
	KindToolResult = "tool_result"
	KindUsage      = "usage"
	KindRateLimit  = "rate_limit"
	KindRetry      = "retry"
)

// Result reasons.
const (
	ReasonDone          = "done"
	ReasonMaxTurns      = "max_turns"
	ReasonMaxBudget     = "max_budget"
	ReasonUsageLimit    = "usage_limit"
	ReasonAuth          = "auth"
	ReasonEndpoint      = "endpoint"
	ReasonResumeRefused = "resume_refused"
	ReasonInterrupted   = "interrupted"
	ReasonCrashed       = "crashed"
)

// Driver runs one agent session on a Target.
type Driver interface {
	Probe(ctx context.Context, t proc.Target) (version string, err error)
	// Hosts returns the hosts the agent's own traffic reaches in the given auth
	// mode ("subscription" or "key"), for the agent phase's egress allowlist.
	Hosts(mode string) []string
	// Run starts or resumes one session and blocks until it ends. Cancelling
	// ctx interrupts it. Every event reaches sink in order.
	Run(ctx context.Context, t proc.Target, s Spec, sink func(Event)) (Result, error)
}

// Auth selects how the agent reaches its model.
type Auth struct {
	Mode    string // "subscription" or "key"
	Token   string // the subscription token, or the stand-in token
	BaseURL string // key mode: the bridge address
}

// Spec describes one session.
type Spec struct {
	Auth          Auth
	Model, Effort string
	MaxTurns      int
	SessionID     string // new session with this id
	Resume        string // or continue this one
	Prompt        string // the user message; never in argv
	Instructions  string // appended to the system prompt; never in argv
	OutputSchema  json.RawMessage
	Dir           string
	BaseCommit    string // the commit the repository's instruction files are taken from; empty takes none
	Home          string
	ConfigDir     string
	Env           []string // extra environment: proxy variables, CA path, toolchain paths
	WebTools      bool
}

// Event is one normalised step of a session; only the fields of its Kind are set.
type Event struct {
	Kind         string
	SessionID    string // started
	Model        string // started
	Capabilities []string
	Text         string // text
	ToolID       string // tool_use, tool_result
	ToolName     string
	ToolInput    json.RawMessage
	ToolError    bool
	Usage        *Usage // usage: one model call, or the tokens the result reports beyond the streamed calls
	RateLimit    *RateLimit
	Retry        *Retry
}

// Usage counts the tokens of one model.
type Usage struct {
	Model                                string
	Input, Output, CacheRead, CacheWrite int64
	// CostEstimateUSD is the model's share of Result.CostEstimateUSD in
	// Result.Usage, and always zero in a usage event.
	CostEstimateUSD float64
}

// RateLimit is the subscription's state for one window.
type RateLimit struct {
	Window, Status string
	ResetsAt       time.Time
}

// Retry reports one retried model request.
type Retry struct {
	Attempt, Max int
	Kind         string
	Status       int
}

// Result is how a session ended.
type Result struct {
	// Reason is one of the Reason constants.
	Reason          string
	IsError         bool
	Text            string
	Structured      json.RawMessage
	SessionID       string
	Turns           int
	CostEstimateUSD float64
	Usage           []Usage // totals per model, sorted by model
	RateLimit       *RateLimit
	Detail          string // masked: the stderr tail, or the error text with the upstream status when known
}
