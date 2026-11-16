# Assumptions

This file lists the removable parts of CAVU: code that makes up for a weakness of today's models and sits behind a switch, so it can be deleted when models no longer need it. Every removable part has an entry here, landed with its code, naming its switch, what the switch turns on, the assumption the part rests on, and the replay comparison that would retire it. The `internal/replay` package runs the comparisons; no command runs it yet.

It also lists what the Claude Code adapter assumes of the pinned Claude Code version. Those entries have no switch: they change when the pinned version changes, and name the check against a new release that would change or retire them, run by the conformance suite (`make test-conformance`).

## continuations

- Switch: `continuations`, set per project and overridden by a task's overrides, which is how the `internal/replay` package sets it. On by default.
- What it turns on: when an agent session ends unfinished or crashed, the engine resumes the same session in continue mode, at most 2 times since the last human action on the task. With the switch off, such a session stops the task as `agent_error`.
- Assumption: current models sometimes end a turn with a progress report instead of finished work.
- Comparison to retire it: replay the same reference set with the switch off. Retire the part when the verify pass rate and the turns used are no worse than with it on.

## verify_fix

- Switch: `verify_fix`, set per project and overridden by a task's overrides, which is how the `internal/replay` package sets it. On by default.
- What it turns on: when the verify command fails or times out, the engine resumes the same session in fix mode with the recorded verify output (at most 64 KiB, its head and tail kept), at most 2 rounds since the last human action on the task. With the switch off, the task stops as `verify_failed`.
- Assumption: a second attempt after a failed verify usually succeeds.
- Comparison to retire it: replay the same reference set with the switch off. Retire the part when the verify pass rate and the turns used are no worse than with it on.

## web_tools

- Switch: `web_tools`, set per project and overridden by a task's overrides, which is how the `internal/replay` package sets it. Off by default.
- What it turns on: the agent's web tools. With the switch off, the Claude Code adapter disallows `WebFetch` and `WebSearch` and sets `CLAUDE_CODE_DISABLE_WEB_FETCH=1`, so a session can neither fetch a page nor search the web.
- Assumption: fetched content is an injection and exfiltration channel: current models can follow instructions planted in a page they read, and a request they send can carry the repository's content out.
- Comparison to retire it: replay the same reference set with the switch on, with tasks whose fetched pages carry planted instructions. Retire the part when no session follows a planted instruction or sends the repository's content out, and the verify pass rate is no worse than with it off.

## The pinned flag set of the Claude Code adapter

- Switch: none. The flags and the environment are constants in `internal/driver/claudecode`, changed when the pinned Claude Code version changes.
- What it turns on: every session runs `claude` with
  - always: `-p --output-format stream-json --input-format stream-json --verbose --include-partial-messages --setting-sources user --strict-mcp-config --system-prompt-snapshot off --permission-mode bypassPermissions`, `--model <id>`, `--settings <file>` with a settings file that turns commit and PR attribution and every hook off, `--session-id <uuid>` or `--resume <id>`, and `--max-turns <n>`;
  - when the session sets them: `--effort <level>`, `--append-system-prompt-file <file>` holding its instructions, and `--json-schema <schema>` for a structured result;
  - unless the session allows web tools: `--disallowedTools WebFetch WebSearch`;
  - an environment built whole: `HOME`, `CLAUDE_CONFIG_DIR`, `PATH`, `DISABLE_UPDATES=1`, `DISABLE_AUTOUPDATER=1`, `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1`, `CLAUDE_CODE_DISABLE_FAST_MODE=1`, `CLAUDE_CODE_DISABLE_BACKGROUND_TASKS=1`, `CLAUDE_CODE_DISABLE_WEB_FETCH=1` unless the session allows web tools, the auth mode's variables (`CLAUDE_CODE_OAUTH_TOKEN`, or `ANTHROPIC_BASE_URL` and `ANTHROPIC_AUTH_TOKEN`), none of the other auth or provider variables the caller's environment may hold, and the loopback hosts in `NO_PROXY` and `no_proxy` when the caller's environment names a proxy;
  - a cleared config directory and staged instruction files: before every session the driver empties the config directory but for the transcripts (`projects/`), and when the session names a base commit it writes the repository's root instruction files, rules and skills from that commit into it;
  - a sandbox check: both run only through `/etc/cavu/sandbox`, a file only the sandbox image carries, and a session in an image without it stops with an error naming `cavu image build`. `internal/driver/claudecode/stage.go` holds how the clear, the staging, their limits and the check work.
- Assumption: on the pinned version these flags and variables give user-level settings only and a clean headless run. The pinned version reads a repository's `CLAUDE.md`, `AGENTS.md` and skills only with the project settings source, which also applies the repository's own settings file (its environment, permission rules and hooks), so the staging gives the agent those files through the user source instead.
- Check to change or retire it: run the conformance suite against each new release. Change a flag or variable when the suite fails, and drop one once a release makes it unnecessary. `TestInstructionsNeedStaging` runs a session without staging and wants none of the repository's instruction files in any request; when it fails, the release loads them without the project source, and the staging goes.

## The stream the Claude Code adapter reads

- Switch: none. The mapper (`internal/driver/claudecode/mapper.go`, `ratelimit.go`, `run.go`) rests on these facts about the pinned version's `stream-json` output. A release that changes one changes what a person is told, or what is recorded as success.
- Usage limit: on the pinned release a usage limit is the `result` line with `is_error` true, `api_error_status` 429 and a result text that opens with `You've hit your ` or contains `usage limit` (`isUsageLimitText`, `outcome` in `mapper.go`), and it maps to `usage_limit`. The reset time is the `resetsAt` of the last `rate_limit_event` of the run. A session is never stopped on a `rate_limit_event`: its `rejected` status was never recorded on any release (an unverified report says accounts with overage enabled get it on requests that still succeed), so the mapper reports the event and goes on to the result. A 429 without the usage-limit text maps to `endpoint`. The text prefix and the six window names are in the 2.1.325 binary (`grep -a` in the image finds `5-hour limit`, `weekly limit`, `Opus limit`, `Sonnet limit`, `Fable limit` and `usage credit limit`); no mock answer reaches the text, so no conformance test sees a real usage limit. The newer texts `You've reached your` and `You're out of usage credits` are not matched.
  - Check: none reaches an exhausted window on purpose. `TestErrorResults` pins the 429 without the text, and the opt-in `TestLiveRateLimitEventShape` wants a window and a reset time in a real event. A run at an exhausted window settles the rest.
- Success: judged by `is_error`, never `subtype`. An authentication failure reports `subtype: "success"` with `is_error` true. Check: `TestResultFields`, `TestErrorResults`; `real/t2.jsonl` is the recorded case.
- Authentication failure: a result with `api_error_status` 401, a result text that opens with `Not logged in`, or an assistant line with `error: "authentication_failed"` maps to `auth`. Check: `TestErrorResults` for the 401; the text and the assistant line are only in `real/t2.jsonl`.
- Synthetic messages: an assistant line whose model is `<synthetic>` is a message Claude Code made up locally and is not counted as a model call. Check: `real/t2.jsonl`.
- Resumed sessions: `model_usage_since_last_result` is the share of the process that wrote it, where `modelUsage` and `total_cost_usd` are cumulative over the resumed session; the mapper takes the share and falls back to the cumulative fields when it is missing. Check: `TestResumeAcrossContainers` runs a resume; the field itself is read in `two_models.jsonl` and `real/t5.jsonl`.
- Refused resume: `--resume` of a session with no transcript ends with an error result and `No conversation found` on stderr, and maps to `resume_refused`. Check: `TestResumeUnknownSession`.

## Unknown on 2.1.325

What a mock cannot settle. Each live check (`live_test.go`, tags `conformance` and `live`) skips unless its variables are set, and never runs in CI:

```bash
CAVU_LIVE_SUBSCRIPTION_TOKEN=... go test -tags conformance,live -count=1 -run 'TestLive' ./internal/driver/claudecode/
CAVU_LIVE_ENDPOINT_URL=... CAVU_LIVE_ENDPOINT_KEY=... go test -tags conformance,live -count=1 -run 'TestLiveGateway' ./internal/driver/claudecode/
```

`CAVU_LIVE_ENDPOINT_AUTH_HEADER` (`bearer` or `x-api-key`, default `x-api-key`) and `CAVU_LIVE_MODEL` (default `claude-haiku-5-5`) adjust them.

| Question | What would settle it |
|---|---|
| The hosts a subscription run asks for beyond `api.anthropic.com` | `TestLiveSubscriptionHosts` logs every refused host; the run must still succeed |
| The `rate_limit_event` shape on 2.1.325: recorded live only on 2.1.322, where it carries `status`, `resetsAt`, `rateLimitType`, `overageStatus`, `overageDisabledReason`, `isUsingOverage` and `unifiedWindows`; against a mock on 2.1.325 it is only `{"status":"allowed","isUsingOverage":false}` | `TestLiveRateLimitEventShape` wants a window and a reset time |
| A rejected `rate_limit_event`, whether a session that gets one goes on, and the text and `result` line of a usage limit actually reached, per window, and whether the newer texts end a run too | a subscription run at an exhausted window; no live check reaches one on purpose |
| Cache reads through a real gateway | `TestLiveGatewayCacheReads`: the sidecar's meter must count cache reads on the second of two turns |
| Whether `--max-budget-usd` stops a subscription run | `TestLiveBudgetFlagUnderSubscription` logs the outcome; a finding, not a gate |
| A tail of stderr cut inside the token, or a form of it, keeps a fragment no mask can recognise | not reachable: no check saw a token on stderr in any form |
