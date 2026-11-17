package claudecode

import (
	"cmp"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/mhersson/cavu/internal/driver"
)

// maxLine is the longest stdout line the mapper reads; a longer one is skipped.
const maxLine = 1 << 20

// syntheticModel marks a message Claude Code made up locally (an auth or API
// error), not an API call.
const syntheticModel = "<synthetic>"

type tokenBuckets struct{ input, output, cacheRead, cacheWrite int64 }

func (b tokenBuckets) add(o tokenBuckets) tokenBuckets {
	return tokenBuckets{b.input + o.input, b.output + o.output, b.cacheRead + o.cacheRead, b.cacheWrite + o.cacheWrite}
}

// remainder is what b holds beyond streamed, clamped at 0 per bucket.
func (b tokenBuckets) remainder(streamed tokenBuckets) tokenBuckets {
	return tokenBuckets{
		max(b.input-streamed.input, 0), max(b.output-streamed.output, 0),
		max(b.cacheRead-streamed.cacheRead, 0), max(b.cacheWrite-streamed.cacheWrite, 0),
	}
}

func modelBuckets(u modelUsage) tokenBuckets {
	return tokenBuckets{u.InputTokens, u.OutputTokens, u.CacheReadInputTokens, u.CacheCreationInputTokens}
}

type toolUse struct {
	id, name  string
	input     json.RawMessage // from the assistant line
	fragments strings.Builder // input_json_delta, used when no assistant line carried the input
}

func (t *toolUse) args() json.RawMessage {
	switch {
	case len(t.input) > 0:
		return t.input
	case t.fragments.Len() > 0 && json.Valid([]byte(t.fragments.String())):
		return json.RawMessage(t.fragments.String())
	default:
		return json.RawMessage("{}")
	}
}

// apiCall is the open call: one message.id.
type apiCall struct {
	id, model   string
	text        []string
	tools       []*toolUse
	toolOfBlock map[int]int // stream content-block index to tools ordinal
	deltaUsage  apiUsage
	startUsage  apiUsage
	lineUsage   *apiUsage // first assistant line
	// results answer this call's tools but arrived before it closed.
	results []contentBlock
}

func (c *apiCall) hasTool(id string) bool {
	return slices.ContainsFunc(c.tools, func(t *toolUse) bool { return t.id == id })
}

func (c *apiCall) toolUse(id, name string) int {
	for i, t := range c.tools {
		if t.id == id {
			return i
		}
	}

	c.tools = append(c.tools, &toolUse{id: id, name: name})

	return len(c.tools) - 1
}

// usage takes each bucket from message_delta, else message_start, else the
// first assistant line.
func (c *apiCall) usage() tokenBuckets {
	var line apiUsage
	if c.lineUsage != nil {
		line = *c.lineUsage
	}

	pick := func(f func(apiUsage) *int64) int64 {
		for _, u := range []apiUsage{c.deltaUsage, c.startUsage, line} {
			if v := f(u); v != nil {
				return *v
			}
		}

		return 0
	}

	return tokenBuckets{
		input:      pick(func(u apiUsage) *int64 { return u.InputTokens }),
		output:     pick(func(u apiUsage) *int64 { return u.OutputTokens }),
		cacheRead:  pick(func(u apiUsage) *int64 { return u.CacheReadInputTokens }),
		cacheWrite: pick(func(u apiUsage) *int64 { return u.CacheCreationInputTokens }),
	}
}

// overlay copies the buckets src reports into dst.
func overlay(dst *apiUsage, src apiUsage) {
	for _, p := range []struct{ d, s **int64 }{
		{&dst.InputTokens, &src.InputTokens},
		{&dst.OutputTokens, &src.OutputTokens},
		{&dst.CacheReadInputTokens, &src.CacheReadInputTokens},
		{&dst.CacheCreationInputTokens, &src.CacheCreationInputTokens},
	} {
		if *p.s != nil {
			*p.d = *p.s
		}
	}
}

// mapper turns stream-json lines into driver events and a driver.Result. It is
// fed from one goroutine.
type mapper struct {
	sink func(driver.Event)

	open   *apiCall
	closed map[string]bool
	calls  int

	totals    map[string]tokenBuckets // per model: streamed calls plus the remainder the result adds
	costs     map[string]float64      // per model, from the result
	model     string                  // from init
	sessionID string                  // from init
	lastText  string
	rateLimit *driver.RateLimit // the last rate_limit_event

	authFailed bool
	sawResult  bool
	res        driver.Result
}

func newMapper(sink func(driver.Event)) *mapper {
	return &mapper{
		sink:   sink,
		closed: map[string]bool{},
		totals: map[string]tokenBuckets{},
	}
}

// feed consumes one stdout line and reports whether the caller should stop the
// process: the run's result has been seen. Lines the mapper
// does not know, or that are not JSON, are ignored.
func (m *mapper) feed(line []byte) (stop bool) {
	if m.sawResult {
		return true
	}

	if len(line) > maxLine {
		return false
	}

	l, ok := decodeLine(line)
	if !ok {
		return false
	}

	switch l.Type {
	case "assistant":
		if l.Error == "authentication_failed" {
			m.authFailed = true
		}

		m.onAssistant(l.Message)
	case "user":
		if !l.IsReplay {
			m.onUser(l.Message)
		}
	case "stream_event":
		m.onStreamEvent(l.Event, l.APIMessageID)
	case "rate_limit_event":
		m.onRateLimit(l.RateLimitInfo)
	case "system":
		switch l.Subtype {
		case "init":
			m.onInit(l)
		case "api_retry":
			m.onRetry(l)
		}
	case "result":
		m.onResult(l)
	}

	return m.sawResult
}

// finish returns the run's result once the stream has ended. exitCode and
// stderrTail explain a stream that ended without a result line, which is never
// a success whatever the exit code says.
func (m *mapper) finish(exitCode int, stderrTail string) driver.Result {
	if m.sawResult {
		return m.res
	}

	m.closeCall()

	m.res.Turns = m.calls
	m.res.Text = m.lastText
	m.res.SessionID = m.sessionID
	m.res.Usage = m.usageTotals()
	m.res.RateLimit = m.rateLimit
	m.res.IsError = true

	switch {
	case m.authFailed:
		m.res.Reason = driver.ReasonAuth
		m.res.Detail = "not logged in"
	default:
		m.res.Reason = driver.ReasonCrashed

		detail := strings.TrimSpace(stderrTail)
		if detail == "" {
			detail = fmt.Sprintf("the stream ended without a result (exit %d)", exitCode)
		}

		m.res.Detail = detail
	}

	return m.res
}

func (m *mapper) onInit(l streamLine) {
	m.model = l.Model
	m.sessionID = l.SessionID

	m.sink(driver.Event{
		Kind: driver.KindStarted, SessionID: l.SessionID, Model: l.Model, Capabilities: l.Capabilities,
	})
}

func (m *mapper) onRetry(l streamLine) {
	r := &driver.Retry{Attempt: l.Attempt, Max: l.MaxRetries, Kind: l.Error}
	if l.ErrorStatus != nil {
		r.Status = *l.ErrorStatus
	}

	m.sink(driver.Event{Kind: driver.KindRetry, Retry: r})
}

// callFor returns the open call for id, closing a different open call and
// opening a new one as needed; nil for a call that already closed.
func (m *mapper) callFor(id, model string) *apiCall {
	if m.open != nil && m.open.id == id {
		return m.open
	}

	if m.closed[id] {
		return nil
	}

	m.closeCall()

	m.calls++
	m.open = &apiCall{id: id, model: model, toolOfBlock: map[int]int{}}

	return m.open
}

func (m *mapper) onAssistant(msg *apiMessage) {
	if msg == nil || msg.ID == "" || msg.Model == syntheticModel {
		return
	}

	c := m.callFor(msg.ID, msg.Model)
	if c == nil {
		return
	}

	if c.model == "" {
		c.model = msg.Model
	}

	if c.lineUsage == nil {
		u := msg.Usage
		c.lineUsage = &u
	}

	for _, b := range blocks(msg.Content) {
		switch b.Type {
		case "text":
			c.text = append(c.text, b.Text)
		case "tool_use":
			c.tools[c.toolUse(b.ID, b.Name)].input = b.Input
		}
	}
}

func (m *mapper) onStreamEvent(ev *streamEvent, apiMessageID string) {
	if ev == nil {
		return
	}

	if ev.Type == "message_start" {
		if ev.Message == nil || ev.Message.ID == "" {
			return
		}

		if c := m.callFor(ev.Message.ID, ev.Message.Model); c != nil {
			c.startUsage = ev.Message.Usage
		}

		return
	}

	c := m.streamCall(apiMessageID)
	if c == nil {
		return
	}

	switch ev.Type {
	case "content_block_start":
		if b := ev.ContentBlock; b != nil && b.Type == "tool_use" {
			c.toolOfBlock[ev.Index] = c.toolUse(b.ID, b.Name)
		}
	case "content_block_delta":
		if d := ev.Delta; d != nil && d.Type == "input_json_delta" {
			if i, ok := c.toolOfBlock[ev.Index]; ok {
				c.tools[i].fragments.WriteString(d.PartialJSON)
			}
		}
	case "message_delta":
		if ev.Usage != nil {
			overlay(&c.deltaUsage, *ev.Usage)
		}
	case "message_stop":
		m.closeCall()
	}
}

// streamCall returns the open call a stream event belongs to. An event of
// another, unseen call closes the open one.
func (m *mapper) streamCall(apiMessageID string) *apiCall {
	if m.open == nil {
		return nil
	}

	if apiMessageID == "" || apiMessageID == m.open.id {
		return m.open
	}

	if !m.closed[apiMessageID] {
		m.closeCall()
	}

	return nil
}

func (m *mapper) onUser(msg *apiMessage) {
	if msg == nil {
		return
	}

	// Claude Code runs a call's tools while its later tool_use blocks still
	// stream, so a result can land inside the call; it waits for the close.
	for _, b := range blocks(msg.Content) {
		if b.Type != "tool_result" {
			continue
		}

		if m.open != nil && m.open.hasTool(b.ToolUseID) {
			m.open.results = append(m.open.results, b)

			continue
		}

		// Any other result leaves the open call open: closing it would cut the
		// message again.
		m.onToolResult(b)
	}
}

func (m *mapper) onToolResult(r contentBlock) {
	m.sink(driver.Event{Kind: driver.KindToolResult, ToolID: r.ToolUseID, ToolError: r.IsError})
}

func (m *mapper) onRateLimit(info *rateLimitInfo) {
	rl := parseRateLimit(info)
	m.rateLimit = &rl

	m.sink(driver.Event{Kind: driver.KindRateLimit, RateLimit: &rl})
}

// closeCall emits the open call's events: its text, its usage, its tool uses,
// then the results that arrived early.
func (m *mapper) closeCall() {
	c := m.open
	if c == nil {
		return
	}

	m.open = nil
	m.closed[c.id] = true

	m.lastText = strings.Join(c.text, "\n\n")
	if m.lastText != "" {
		m.sink(driver.Event{Kind: driver.KindText, Text: m.lastText})
	}

	m.emitUsage(c.model, c.usage())

	for _, t := range c.tools {
		m.sink(driver.Event{Kind: driver.KindToolUse, ToolID: t.id, ToolName: t.name, ToolInput: t.args()})
	}

	for _, r := range c.results {
		m.onToolResult(r)
	}
}

func (m *mapper) emitUsage(model string, u tokenBuckets) {
	m.totals[model] = m.totals[model].add(u)

	m.sink(driver.Event{Kind: driver.KindUsage, Usage: &driver.Usage{
		Model: model, Input: u.input, Output: u.output, CacheRead: u.cacheRead, CacheWrite: u.cacheWrite,
	}})
}

// usageTotals lists the per-model totals sorted by model.
func (m *mapper) usageTotals() []driver.Usage {
	var out []driver.Usage

	for _, model := range slices.Sorted(maps.Keys(m.totals)) {
		if b := m.totals[model]; b != (tokenBuckets{}) {
			out = append(out, driver.Usage{
				Model: model, Input: b.input, Output: b.output, CacheRead: b.cacheRead, CacheWrite: b.cacheWrite,
				CostEstimateUSD: m.costs[model],
			})
		}
	}

	return out
}

func (m *mapper) onResult(l streamLine) {
	m.closeCall()

	m.sawResult = true

	// modelUsage and total_cost_usd are cumulative over a resumed session; this
	// process's share is model_usage_since_last_result.
	perModel := l.ModelUsageSinceLastResult

	var cost float64
	for _, u := range perModel {
		cost += u.CostUSD
	}

	if perModel == nil {
		perModel = l.ModelUsage
		cost = l.TotalCostUSD
	}

	m.costs = map[string]float64{}
	for model, u := range perModel {
		m.costs[model] = u.CostUSD
	}

	// A run whose result carries no per-model usage still reports its totals
	// and its cost once, under the model init named.
	if len(perModel) == 0 && m.model != "" {
		perModel = map[string]modelUsage{m.model: {
			InputTokens: deref(l.Usage.InputTokens), OutputTokens: deref(l.Usage.OutputTokens),
			CacheReadInputTokens: deref(l.Usage.CacheReadInputTokens), CacheCreationInputTokens: deref(l.Usage.CacheCreationInputTokens),
		}}
		m.costs[m.model] = cost
	}

	// Calls Claude Code made without streaming them (compaction, for one) show
	// up only here; one event per model carries the difference.
	for _, model := range slices.Sorted(maps.Keys(perModel)) {
		if rest := modelBuckets(perModel[model]).remainder(m.totals[model]); rest != (tokenBuckets{}) {
			m.emitUsage(model, rest)
		}
	}

	m.res = driver.Result{
		Turns:           l.NumTurns,
		CostEstimateUSD: cost,
		Usage:           m.usageTotals(),
		SessionID:       cmp.Or(l.SessionID, m.sessionID),
		Text:            l.Result,
		RateLimit:       m.rateLimit,
	}

	if len(l.StructuredOutput) > 0 && string(l.StructuredOutput) != "null" {
		m.res.Structured = l.StructuredOutput
	}

	m.outcome(l)
}

// outcome judges success by is_error, never subtype: an auth failure reports
// subtype success. A result that is an error never maps to done.
func (m *mapper) outcome(l streamLine) {
	status := -1
	if l.APIErrorStatus != nil {
		status = *l.APIErrorStatus
	}

	reason := driver.ReasonCrashed

	switch {
	case m.authFailed || (l.IsError && strings.HasPrefix(l.Result, "Not logged in")) || (l.IsError && status == 401):
		reason = driver.ReasonAuth
	case l.IsError && (l.APIError == usageLimitCode || status == 429 && isUsageLimitText(l.Result)):
		reason = driver.ReasonUsageLimit
	case l.Subtype == "error_max_turns":
		reason = driver.ReasonMaxTurns
	case !l.IsError:
		// terminal_reason is not consulted, so a release that drops or renames
		// it degrades instead of failing every run.
		reason = driver.ReasonDone
	case status >= 0 || l.TerminalReason == "api_error":
		reason = driver.ReasonEndpoint
	}

	m.res.Reason = reason
	m.res.IsError = reason != driver.ReasonDone

	if !m.res.IsError {
		return
	}

	// The status is the one fact a person can act on that the CLI's text may
	// leave out.
	detail := errorText(l)
	if status >= 0 {
		detail = fmt.Sprintf("upstream status %d: %s", status, detail)
	}

	m.res.Detail = detail
}

// errorText is the text an error result carries, from the most to the least
// specific field.
func errorText(l streamLine) string {
	switch {
	case l.Result != "":
		return l.Result
	case len(l.Errors) > 0:
		return strings.Join(l.Errors, "; ")
	default:
		return fmt.Sprintf("%s (terminal reason %q)", l.Subtype, l.TerminalReason)
	}
}

func deref(v *int64) int64 {
	if v == nil {
		return 0
	}

	return *v
}
