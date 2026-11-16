# claudecode testdata

Stream-json fixtures recorded with Claude Code 2.1.322. `TestFixturesCarryNoToken`
scans every file here for credential shapes.

The recordings are kept as recorded, including characters this repository's own
text avoids: `synthetic/max_turns.jsonl` and `synthetic/parallel.jsonl`
contain the long dash character in recorded CLI output.

- `real/` - CLI output recorded from a live or mock-API run (see below for the one edit).
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
| `rejected.jsonl` | `real/t1.jsonl` | the `rate_limit_event` status changed to `rejected` (window `seven_day` rejected, utilization 1.0); the `result` line is kept, so the session goes on after the event and ends done |
| `max_turns.jsonl` | CLI output of a mock-API run, `--max-turns 1`, one `Bash` turn, gated by `initialize` | `result` with `subtype: "error_max_turns"`, `is_error: true`, `terminal_reason: "max_turns"`, `errors: ["Reached maximum number of turns (1)"]` and no `result` field; the process exited 1. Starts with the gate's `control_response`; usage values are the mock's |
| `two_models.jsonl` | `real/t5.jsonl` | `result.modelUsage` and `model_usage_since_last_result` gain a `claude-haiku-5-5` entry (350/12/0/0, `costUSD` 0.000041) that no assistant message served; `total_cost_usd` 0.0175914; `result.usage` unchanged |
| `unknown.jsonl` | `real/t1.jsonl` | an unknown top-level type (`x_future_event`), an unknown `system` subtype (`x_future_subtype`), `x_future_field` on every known line, and unknown keys inside the assistant `message` and `result.usage` |
| `crash.jsonl` | `real/t5.jsonl` | the `result` line removed |
| `parallel.jsonl` | CLI output of a mock-API run, gated by `initialize` | one message with five `Bash` `tool_use` blocks (`echo one` to `echo five`), each input in three `input_json_delta` fragments 0.4 s apart. The `tool_result` of each of calls 1 to 4 arrives inside the message, right after the next block's `content_block_start`; call 5's arrives after `message_stop`. Each assistant line carries one block. Starts with the gate's `control_response`; usage values are the mock's |

## conformance/

The fixture repository the conformance suite (`conformance_test.go`) plants in
the agent container's workspace and commits. Each file is stored under a name
no tool discovers, so neither the operator's Claude Code nor any other agent
working in this repository loads it, and the suite gives it its real name on
the copy. The contents are inert markers.

| Stored as | In the workspace | Plants |
|---|---|---|
| `root-claude-md.txt` | `CLAUDE.md` | a marker |
| `root-agents-md.txt` | `AGENTS.md` | a marker |
| `dot-claude-md.txt` | `.claude/CLAUDE.md` | a marker |
| `rule-md.txt` | `.claude/rules/conformance.md` | a marker |
| `pkg-claude-md.txt` | `pkg/CLAUDE.md` | a marker |
| `pkg-notes.txt` | `pkg/notes.txt` | the file a scripted `Read` opens |
| `skill-md.txt` | `.claude/skills/conformance-marker/SKILL.md` | a skill whose description is a marker |
| `hook-skill-md.txt` | `.claude/skills/conformance-hook/SKILL.md` | a skill whose frontmatter declares `PreToolUse`, `PostToolUse` and `Stop` hooks that each create a file |
| `project-settings-json.txt` | `.claude/settings.json` | a `SessionStart` and a `PostToolUse` hook that each create a file, `permissions.deny` for `Bash`, and an `env` entry whose value is a marker |
| `mcp-json.txt` | `.mcp.json` | a server whose command creates a file |
| `docs-imported-md.txt` | `docs/imported.md` | a marker, imported by an `@docs/imported.md` line in `CLAUDE.md` |
