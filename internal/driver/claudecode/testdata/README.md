# claudecode testdata

Stream-json fixtures recorded with Claude Code 2.1.322. `TestFixturesCarryNoToken`
scans every file here for credential shapes.

The recordings are kept as recorded, including characters this repository's own
text avoids: `synthetic/max_budget_end_turn.jsonl`,
`synthetic/max_budget_tool_use.jsonl`, `synthetic/max_turns.jsonl` and
`synthetic/parallel.jsonl` contain the long dash character in recorded CLI output.

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

## Decisions for 2.1.325

Settled by the conformance suite, `make test-conformance`: the pinned `claude`
from the sandbox image runs in the agent container (no network, uid 1000)
against `mockapi` through the bridge and the sidecar, in key mode with a
stand-in token, the fixture repository's commit as the base commit and the
agent phase's environment (`env.AgentEnv` plus `HTTPS_PROXY` and `HTTP_PROXY`
naming the bridge). The suite passed on Docker
and on Podman. Rows that cite the binary come from `grep -a` on
`/usr/local/bin/claude` inside a container of the image.

| Decision | Value | Evidence |
|---|---|---|
| Flags accepted | the whole set, `--effort` and `--append-system-prompt-file` included | every check's run ends `done` where it should; `TestInstructionsAndEffort`: the instructions file reaches the request's `system`, `--effort high` reaches `output_config.effort` |
| Setting sources | `--setting-sources user` | `TestProjectSettingsNotLoaded`: neither hook ran, the `Bash` call ran, the `env` marker is in no request. With `user,project` (see below) the repository's deny rule and `env` reach the session. Corroborated by ContextMatrix's orchestrator docs (`docs/orchestrator.md` in contextmatrix-agent), which record Claude Code's own CLAUDE.md loading as off under `--setting-sources user --strict-mcp-config` on 2.1.322 |
| Hooks | `"disableAllHooks": true` in the `--settings` file, which also turns off the frontmatter hooks of a staged skill; CAVU uses no hooks | `TestSkillHooksOff`: the hook skill invoked through `Skill`, then a `Bash` call: none of its three hook files exists. The control, the same run with the switch left out of the settings file, creates all three (`PreToolUse`, `PostToolUse`, `Stop`) |
| Config dir between sessions | cleared before every session but `projects/`, base commit or not: a session keeps its transcripts (and the auto memory beside them) and nothing else it wrote there. The home, the config dir directly below it and a working dir outside it, none holding a control character, are checked before any command starts, by `Run` and again by the staging before it composes a command. The clear is a command of its own, `sh -c <script> sh <config dir>` behind the sandbox check: it leaves a missing config dir alone, refuses one that is a symlink or resolves elsewhere (`pwd -P` after `cd -P` must name it), deleting nothing, and deletes with `find . -mindepth 1 -maxdepth 1 ! -name projects -exec rm -rf -- {} +` inside it. A refused clear is an error from `Run`, and no session starts | the binary's list of what the user source reads from the config dir (`userConfigDir` names `commands, agents, output-styles, skills, workflows, routines, themes, rules, session-env, uploads, mcp-skill-archives, usage-data, mcp-discovery-cache`) and of the entries it guards there (`shell-snapshots, plugins, hooks, scheduled_tasks.json, launch.json, CLAUDE.md, daemon.json, policy-limits.json, backups`, beside `settings.json` and `loop.md`) is wider than any list worth keeping, so all but `projects/` goes. `TestConfigDirCleared`: a `settings.json` with an `env` entry and an `agents/` file planted between two sessions are gone and the env value reaches no request; the control, the same file written after the clear, reaches the next request through `printenv`. `TestStagingReplacesEarlierStaging`: everything an earlier session left goes but its transcript. `TestConfigDirSymlinkRefused`: a config dir that is a symlink to a directory, to `/` or to nothing stops `Run` with an error before any request, and the link and what it points to stay. `TestResumeAcrossContainers` resumes after the clear. Unit tests, which record the clear and run it nowhere: `TestClearWithoutBaseCommit`, `TestSandboxedComposition`, `TestSpecRefused`, `TestPrepareChecksPaths` |
| Sandbox check | the clear and the write run only through `proc.SandboxOnly`, which exits `proc.NotInSandbox` and runs nothing unless `/etc/cavu/sandbox`, an empty, root-owned, read-only file the image creates, exists; the config dir reaches them only as an argument. That status from either is an error naming `cavu image build`; from any other command it is an ordinary failure | every conformance check clears, and every one with a base commit writes, in the image without a refusal. Unit tests: `TestStageFailureStartsNoSession` (`proc.NotInSandbox` from the clear or the write, and from nothing else, is the refusal), `TestSandboxedComposition` (each opens with the check and holds no path in its script). The marker itself: `TestSandboxOnlyRefusesOutsideASandbox` in `internal/proc`, `TestImageCarriesSandboxMarker` in `internal/sandbox` |
| Instruction files | staged before every session from `Spec.BaseCommit` into the config dir, and nothing else of the repository's `.claude/` (no settings, hooks, MCP servers, agents or commands): the root `CLAUDE.md`, `.claude/CLAUDE.md` and the root `AGENTS.md` in one `CLAUDE.md`, each under a heading naming it, then the paths of the nested instruction files; the Markdown files of `.claude/rules` in `rules/`, subdirectories kept; `.claude/skills` in `skills/`, regular files only, a `100755` blob staged executable. A memory or rule file that is a symlink is followed when its target, relative to the link's directory, is a regular file inside the tree, and skipped otherwise; two memory names on one blob are staged once under a heading naming both; each memory and rule file is cut at 64 KiB on a rune boundary with a line saying so. No memory file and no nested file stage no `CLAUDE.md`. A `.claude/skills` or `.claude/rules` that is itself a symlink to a directory stages nothing, since the tree lists it as one symlink with no file below it; below `.claude/skills` only regular files are taken, and below `.claude/rules` regular files and symlinks to a regular file inside the tree. The staging reads the workspace's repository with `GIT_NO_LAZY_FETCH=1`, replace objects and hooks off and no global or system config, but a session after the first reads an object store and a `.git/config` the agent could have changed; that happens inside the sandbox, as the agent, and crosses no privilege, so "from the base commit" is exact only for the first session of a run. The staged files reach the config dir as a tar stream on the stdin of a command of its own behind the sandbox check, `tar -x` inside the dir after the same `cd -P` and `pwd -P` check as the clear, holding directories (0755) and regular files (0644, or 0755 for a `100755` skill blob) under clean relative names, with no owner, no link and the time at zero, never as the command's files. With nothing to stage no write runs. The staging holds at most 16 MiB, counted from the listing's sizes: each symlink target of a memory or rule file once, at about 1 KiB plus its size, before the targets are read; then each archive entry (the staged `CLAUDE.md`, every rule and skill file, every directory they sit in) at about 1 KiB plus its name and content, the memory and rule files as cut, before the staged blobs are read, so a blob staged under two names counts twice. Past 16 MiB it stops with an error naming the file at which the count passes; a tree listing past 64 MiB stops it too. The fixed text of the staged files is the embedded `staged-instructions.tmpl` | `TestInstructionsLoaded`: the root `CLAUDE.md`, `.claude/CLAUDE.md`, root `AGENTS.md` and rule markers and the path `pkg/CLAUDE.md` in the first call, `pkg/CLAUDE.md`'s marker in no request. `TestSkillsLoaded`: the skill's description in the first call. `TestStagingTakesBaseCommit`: a `CLAUDE.md` changed in the working tree after the commit reaches no request. `TestStagingReadsStoredBlobs`: a replace object, a smudge filter, a later commit and a working-tree change all stay out of the staged files. `TestStagingReplacesEarlierStaging`: the staged set, with the skill script executable and the other files not. `TestStagingRefusesCraftedTree`: a tree with a `.` or `..` component below `.claude/skills` or `.claude/rules` writes nothing. The unit tests in `stage_test.go` cover the symlinks, the cut, the list, what is never staged, the bounds (`TestStageLimits`, `TestStageBoundCountsEachName`, `TestStageBoundCountsEachEntry`, `TestStageBoundCountsDirectories`, `TestStageBoundCountsLinks`, `TestStageReadsEachLinkOnce`), the archive's entries (`TestStagedArchiveEntries`) and the missing write (`TestStageNothingToStage`) on a canned listing, through a target that records the commands and runs none |
| Staging still needed | yes, for every repository | `TestInstructionsNeedStaging`: a session without a base commit sends none of the six markers. When it fails, a release loads instruction files without the project source and the staging can go. Probe: with no `.claude/settings.json`, no `.claude/settings.local.json` and no `.mcp.json` in the repository, only a root `CLAUDE.md`, a root `AGENTS.md` and one skill, the starting flags without staging still load none of them (init lists only the built-in skills), so the gate is the flag, not the presence of a settings file |
| `AGENTS.md` | staged beside `CLAUDE.md`, never into the instructions file, which stays the caller's text | the binary loads it through its built-in `cc-plugin-agents-md` plugin, only with the project source, and by default only where the project has no `CLAUDE.md` (its `instructionFiles` option, read from `pluginConfigs` in user or `--settings` scope, has `claude-md`, `claude-md-or-agents-md` (default), `claude-md-and-agents-md`, `managed-only`). An `AGENTS.md` placed in the config dir is not read |
| Nested instruction files | listed, not loaded: the staged `CLAUDE.md` ends with their paths and a line telling the agent to read a directory's file before working under it. The list is ContextMatrix's: one file per directory, `AGENTS.md` over `CLAUDE.md`, no dot directory, at most 4 directories deep, shallow first, at most 24, and no path with a control character | `TestInstructionsLoaded`; `TestFilterNested`. Finding, not built: a user rule (`<config>/rules/pkg.md` with `paths: ["pkg/**"]`, or `paths: pkg/**`) loads once a file below `pkg/` is read and not before; with `paths: ["/workspace/pkg/**"]` it never loads |
| `@` imports in a staged file | not followed: an `@path` line resolves against the config dir, so the staged `CLAUDE.md` opens with one sentence saying what the file is and that `@path` names a repository file, relative to the directory of the file it appears in, to read when relevant | finding, not a gate: `TestInstructionsLoaded` logs that `docs/imported.md`'s marker reached no request |
| `--version` | first line `2.1.325 (Claude Code)` | `TestVersion` (`Probe` returns `Version`) |
| `--disallowedTools WebFetch WebSearch` | separate argv elements; the list ends at the next flag | `TestResultFields/done`: the `--session-id` after it is the run's session id; `TestResultFields/max_turns`: the `--max-turns 1` after it takes effect |
| Web tools | both deferred: with the switch on, `init` lists `WebFetch` and `WebSearch` and the first request names them among the tools to load through `ToolSearch`, never in its `tools`; with the switch off neither name is in `init` or in any request | `TestWebToolsAbsent` |
| Attribution | the `--settings` file's `"attribution": {"commit": "", "pr": "", "sessionUrl": false}` keeps `Co-Authored-By` and `Generated with [Claude Code]` out of every request; with `{}` the same run carries both | `TestUserSettingsStillApply` |
| `PermissionMode` | `bypassPermissions` | `TestBypassPermissions`: a writing `Bash` call runs as uid 1000, its result is no error, `permission_denials` is empty |
| `--strict-mcp-config` | kept | `TestProjectMCPNotLoaded`: `.mcp.json`'s server is not in `init`'s `mcp_servers`, its command never ran, no request offers an `mcp__` tool |
| Structured output | `--json-schema` offers a tool named `StructuredOutput`; its input becomes `result.structured_output` | `TestStructuredResult` (the mock fails a `$structured` step visibly when the request lacks that tool) |
| Result fields | `is_error` (bool) and `subtype` (string) on every result; a done run `subtype: "success"`, `terminal_reason: "completed"`; `--max-turns 1` gives `subtype: "error_max_turns"`, `is_error: true` | `TestResultFields` |
| Initialize account | key mode `{"tokenSource":"ANTHROPIC_AUTH_TOKEN","apiProvider":"firstParty"}`; subscription mode with a base URL `{"tokenSource":"CLAUDE_CODE_OAUTH_TOKEN","apiProvider":"firstParty"}`; both pass the account check | the `control_response` of the key-mode checks and of `TestTrustsCustomCAThroughTunnel` |
| Key-mode traffic | every request reaches the endpoint with the real credential in the route's header (bearer or `x-api-key`) and nothing of the stand-in; the sidecar refuses no host | `TestKeyModeTraffic`, both headers |
| Side requests | none: every request the mock received in the suite was a streaming model call, so the mock's `ok` answer to a non-streaming call is never used | asserted for every check in the shared sandbox cleanup (`mockapi.Request.ModelCall` against all requests) |
| Resume across containers | the transcript on the home volume carries a session into a new agent container | `TestResumeAcrossContainers`: a file in the old container's `/tmp` is gone, and the resumed call carries the earlier prompt and answer |
| Refused resume | `--resume` of a session with no transcript writes a `result` line (`subtype: "error_during_execution"`, `is_error: true`, `num_turns: 0`, `errors: ["No conversation found with session ID: <id>"]`), then the same text on stderr, and never answers `initialize`; the driver maps it to `resume_refused`. A new session under the same id with `--session-id` then starts cleanly | `TestResumeUnknownSession`; the fake now writes the same `result` line |
| Custom CA through a tunnel | a subscription run whose endpoint's certificate the instance's CA signed succeeds through a `CONNECT` tunnel in the image built with that CA; in the plain image it ends `endpoint`, `is_error`, with `API Error: Unable to connect to API: SSL certificate verification failed (UNABLE_TO_VERIFY_LEAF_SIGNATURE)`, and no request reaches the endpoint. The subscription run asked for no host beyond the endpoint | `TestTrustsCustomCAThroughTunnel` |
| No credential in the agent container | while `claude` waits on a held answer, the real credential is in no `/proc/*/environ` or `/proc/*/cmdline` and in no file under `/home/agent`, `/workspace`, `/handoff`, `/run/cavu` or `/tmp`; the same scan finds the stand-in in `claude`'s environment | `TestNoCredentialInContainer` |
| `system/api_retry` | fields `attempt`, `max_retries`, `retry_delay_ms`, `error_status`, `error`, `session_id`, `uuid`; `error` is `authentication_failed` (401), `rate_limit` (429), `server_error` (500), `overloaded` (529); each status is retried exactly once under `CLAUDE_CODE_MAX_RETRIES=1` | `TestErrorResults` |
| Error results | 401: `subtype: "success"`, `is_error: true`, `api_error_status: 401`, `terminal_reason: "api_error"`, result `Failed to authenticate. API Error: 401 ...`, mapped `auth`. 429: the same shape, result `API Error: Request rejected (429) ...`, mapped `endpoint` (no usage-limit text). 500 and 529: the same shape, result `API Error: <status> ...`, mapped `endpoint` | `TestErrorResults` |
| Retry bound | `CLAUDE_CODE_MAX_RETRIES` caps the retries (the binary clamps it to 15 unless the retry watchdog is on); the suite passes `1` through `Spec.Env`, so one retry waits about 0.5 s | `TestErrorResults` (exactly one retry event and two model calls per status, `max_retries: 1`, `retry_delay_ms` 534 to 605); binary `function aRr(){...let e=Ub(),n=i.CLAUDE_CODE_MAX_RETRIES;...}`, default 10 |
| Token on output | neither the stand-in nor the subscription token appears on `claude`'s stdout or stderr in any check, errors included | `requireNoSecretOutput` in `TestKeyModeTraffic`, `TestErrorResults` and `TestTrustsCustomCAThroughTunnel` |
| `rate_limit_event`, allowed | recorded live on 2.1.322, not yet seen on 2.1.325 against a real endpoint | `real/t1.jsonl`, `t3-iso`, `t5`, `t6`, `t7`, `t7b` and `p4-partial-messages` carry `rate_limit_info` with `status`, `resetsAt` (unix seconds), `rateLimitType`, `overageStatus`, `overageDisabledReason`, `isUsingOverage` and `unifiedWindows` (per window: `utilization`, `resetsAt`, `status`). On 2.1.325 against the mock, which sends no rate-limit headers, the event is only `{"status":"allowed","isUsingOverage":false}`. Its confirmation on 2.1.325 is left to `TestLiveRateLimitEventShape` |
| `rate_limit_event`, rejected | never recorded: not here, and not in contextmatrix-vendorkit, the ContextMatrix agent or AIM; the mapper's treatment of a rejected event as a stop rests on `synthetic/rejected.jsonl` alone, which is `real/t1.jsonl` with the status changed | none. Caution, from issue reports and not verified: accounts with overage enabled were reported to get `status: "rejected"` on requests that still succeeded |
| Usage-limit text | the prefix and the six window names `limitPrefix` and `limitWindows` hold are all in the 2.1.325 binary, unchanged | `grep -a` in a container of the image: the message is built as `` return`You've hit your ${e}${n}${d}` `` and the names come from `hve={five_hour:"5-hour limit",seven_day:"weekly limit",seven_day_opus:"Opus limit",seven_day_sonnet:"Sonnet limit",seven_day_overage_included:"Fable limit",overage:"usage credit limit"}`; `grep -a -c -F` finds `5-hour limit` 15 times, `weekly limit` 18, `Opus limit` 2, `Sonnet limit` 2, `Fable limit` 8, `usage credit limit` 8. No mock answer reaches the text |

### How instruction loading was settled

Probes against the same fixture repository, each a key-mode run of a `Read`
below `pkg/` and a `Bash` step that prints the `env` marker's variable and
writes a file:

- `--setting-sources user` alone: no `CLAUDE.md`, `AGENTS.md`, nested
  `CLAUDE.md` or skill reaches any request; no hook ran, `Bash` ran, the `env`
  marker stayed out, no MCP server. The binary's `CLAUDE.md` loader is
  `let k=wN("projectSettings")&&!s;if(k){...CLAUDE.md...}`, its `AGENTS.md`
  walk logs `project memory is off` without that source, and its skill
  discovery logs `skipped: projectSettings disabled`.
- `--setting-sources user,project` with `"disableAllHooks": true` in the
  `--settings` file: root and nested `CLAUDE.md` and the skill load and no
  hook runs, but the repository's `permissions.deny` removes `Bash` from the
  session (`No such tool available: Bash. Bash is disabled for this session`),
  and with a repository settings file holding only `env`, its value reached the
  `claude` process. `AGENTS.md` loaded beside `CLAUDE.md` only with
  `"pluginConfigs": {"cc-plugin-agents-md@builtin": {"options":
  {"instructionFiles": "claude-md-and-agents-md"}}}` in `--settings`.
- `--setting-sources user` with `--add-dir /workspace` and
  `CLAUDE_CODE_ADDITIONAL_DIRECTORIES_CLAUDE_MD=1`: only the root `CLAUDE.md`
  loads.
- `--setting-sources user` with files placed in the config dir: its
  `CLAUDE.md` and `skills/<name>/SKILL.md` load; its `AGENTS.md` does not, so
  the driver puts the root `AGENTS.md` into the staged `CLAUDE.md`.
- `--setting-sources user` against a repository with no settings file at all:
  nothing loads.

## Unknown on 2.1.325

What a mock cannot settle. Each live check (`live_test.go`, tags `conformance`
and `live`) skips unless its variables are set, and never runs in CI:

```bash
CAVU_LIVE_SUBSCRIPTION_TOKEN=... go test -tags conformance,live -count=1 -run 'TestLive' ./internal/driver/claudecode/
CAVU_LIVE_ENDPOINT_URL=... CAVU_LIVE_ENDPOINT_KEY=... go test -tags conformance,live -count=1 -run 'TestLiveGateway' ./internal/driver/claudecode/
```

`CAVU_LIVE_ENDPOINT_AUTH_HEADER` (`bearer` or `x-api-key`, default `x-api-key`)
and `CAVU_LIVE_MODEL` (default `claude-haiku-5-5`) adjust them.

| Question | What would settle it |
|---|---|
| The hosts a subscription run asks for beyond `api.anthropic.com` | `TestLiveSubscriptionHosts` logs every refused host; the run must still succeed |
| The `rate_limit_event` shape on 2.1.325: recorded live only on 2.1.322 (see the allowed row above) | `TestLiveRateLimitEventShape` wants a window and a reset time |
| A rejected `rate_limit_event`, and the text and the `result` line of a usage limit actually reached, per window, and whether the newer texts (`You've reached your`, `You're out of usage credits`) end a run too | a subscription run at an exhausted window; no live check reaches one on purpose |
| Cache reads through a real gateway | `TestLiveGatewayCacheReads`: the sidecar's meter must count cache reads on the second of two turns |
| Whether `--max-budget-usd` stops a subscription run | `TestLiveBudgetFlagUnderSubscription` logs the outcome; a finding, not a gate |
| A tail of stderr cut inside a base64 form of the token keeps a fragment `stderrDetail` does not drop | not reachable: no check saw a token on stderr in any form |
