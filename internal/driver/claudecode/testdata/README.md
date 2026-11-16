# claudecode testdata

Stream-json fixtures recorded with Claude Code 2.1.322. `TestFixturesCarryNoToken`
scans every file here for credential shapes.

- `real/` - CLI output recorded on 2026-11-11 (see below for the one edit).
- `synthetic/` - shapes the recorded runs do not cover, derived from a real
  fixture or captured against a mock Messages API.

## Tool names

The prototype's MCP server was named `cmlocal`, so recorded tool calls read
`mcp__cmlocal__finish` (`t5`, `t6`, `p3`, `p3b`). The names are kept as
recorded.

## real/

| File | Recorded | Shows |
|---|---|---|
| `t1.jsonl` | live, `claude-haiku-5-5`, default tools | one text call: `init`, `assistant`, `rate_limit_event` (allowed, `seven_day`), `result` |
| `t2.jsonl` | live, authentication failed | `subtype: "success"` with `is_error: true`, result `Not logged in · Please run /login`, `terminal_reason: "api_error"`; a `system/ui_invalidate` line before `init` |
| `t3-iso.jsonl` | live, isolation flags | one `Bash` (`echo hi`) tool turn |
| `t5.jsonl` | live, `claude-sonnet-5-5`, `Read` plus `cmlocal` finish | two calls: text and the finish `tool_use` under one `message.id`, then `Done.`; `result.usage` 4/125/3844/3977 (input/output/cache read/cache creation) |
| `t6.jsonl` | live, http `PostToolUse` stop hook | ends with `terminal_reason: "hook_stopped"` right after finish |
| `t7.jsonl` | live, stream-json input | the message written at process start was dropped; only the second was answered |
| `t7b.jsonl` | live, stream-json input, second message after a delay | two turns, each with its own `init` and `result`; replayed `user` lines (`isReplay`) |
| `p4-partial-messages.jsonl` | live, `claude-haiku-5-5`, `--include-partial-messages` | `stream_event` lines (`message_start`, `content_block_*`, `message_delta`, `message_stop`) and a `system/status` line |
| `p3.jsonl` | mock Messages API | finish refused by MCP schema validation (`isError`), then accepted; the refusal fires `PostToolUseFailure` only |
| `p3b.jsonl` | mock Messages API | as `p3`, refused by a handler error |
| `dl-httphook.jsonl` | mock Messages API, distroless image | an http `PostToolUse` hook ends the run with `hook_stopped` |

The only edit to these captures: in `p3.jsonl` and `p3b.jsonl` the
prototype's local scratch directory is rewritten to `/scratch/spike`, and its
dash-encoded `projects/` key to `-scratch-spike` (`cwd`, `original_cwd`,
`memory_paths`). Every other byte is as recorded.

## synthetic/

| File | Derived from | Change |
|---|---|---|
| `partial.jsonl` | `real/t5.jsonl`, in the shapes of `real/p4-partial-messages.jsonl` | `stream_event` lines added per call: `message_start` (all four buckets; the early `output_tokens` of the call's assistant line), text deltas in fragments, a `tool_use` block start with three `input_json_delta` fragments that concatenate to the finish input, `message_delta` with cumulative complete usage and `stop_reason` (call 1: input 2, output 120, cache read 0, cache creation 3844, `tool_use`; call 2: 2, 5, 3844, 133, `end_turn`), `message_stop`. The `t5` lines are unchanged; the `message_delta` usages sum to `result.usage`. Call 1's early `output_tokens` (2) differs from its final 120 |
| `rejected.jsonl` | `real/t1.jsonl` | `rate_limit_event` status `rejected` (window `seven_day` rejected, utilization 1.0); no `result` line. A real rejection is unrecorded |
| `max_turns.jsonl` | CLI output of a mock-API run, `--max-turns 1`, one `Bash` turn, gated by `initialize` | `result` with `subtype: "error_max_turns"`, `is_error: true`, `terminal_reason: "max_turns"`, `errors: ["Reached maximum number of turns (1)"]` and no `result` field; the process exited 1. Starts with the gate's `control_response`; usage values are the mock's |
| `max_budget_end_turn.jsonl` | CLI output of a mock-API run, `--max-budget-usd 0.000001`, one text-only call (scenario `text`), gated by `initialize` | `result` with `subtype: "error_max_budget_usd"`, `is_error: true`, `terminal_reason: "budget_exhausted"`, `stop_reason: "end_turn"`, `errors: ["Reached maximum budget ($0.000001)"]` and `result: null`; one call, whose answer is `ok`. Starts with the gate's `control_response`; usage values are the mock's |
| `max_budget_tool_use.jsonl` | the same run with the `bash` scenario (`{"command":"echo one"}`) | the same `result` shape with `stop_reason: "tool_use"`; the call's tool never runs |
| `two_models.jsonl` | `real/t5.jsonl` | `result.modelUsage` and `model_usage_since_last_result` gain a `claude-haiku-5-5` entry (350/12/0/0, `costUSD` 0.000041) that no assistant message served; `total_cost_usd` 0.0175914; `result.usage` unchanged |
| `unknown.jsonl` | `real/t1.jsonl` | an unknown top-level type (`x_future_event`), an unknown `system` subtype (`x_future_subtype`), `x_future_field` on every known line, and unknown keys inside the assistant `message` and `result.usage` |
| `crash.jsonl` | `real/t5.jsonl` | the `result` line removed |
| `parallel.jsonl` | CLI output of a mock-API run, gated by `initialize` | one message with five `Bash` `tool_use` blocks (`echo one` to `echo five`), each input in three `input_json_delta` fragments 0.4 s apart. The `tool_result` of each of calls 1 to 4 arrives inside the message, right after the next block's `content_block_start`; call 5's arrives after `message_stop`. Each assistant line carries one block. Starts with the gate's `control_response`; usage values are the mock's |
