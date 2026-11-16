# Assumptions

This file lists the removable parts of CAVU: code that makes up for a weakness of today's models and sits behind a switch, so it can be deleted when models no longer need it. Every removable part has an entry here, landed with its code, naming its switch, what the switch turns on, the assumption the part rests on, and the replay comparison that would retire it. The `internal/replay` package runs the comparisons; no command runs it yet.

It also lists what the Claude Code adapter assumes of the pinned Claude Code version. Those entries have no switch: they change when the pinned version changes, and name the check against a new release that would change or retire them, to be run by a conformance suite that is not built yet.

## continuations

- Switch: `continuations`, set per project and overridden by a task's overrides, which is how the `internal/replay` package sets it. On by default.
- What it turns on: when an agent session ends unfinished or crashed, the engine resumes the same session in continue mode, at most 2 times since the last human action on the task. With the switch off, such a session stops the task as `agent_error`.
- Assumption: current models sometimes end a turn with a progress report instead of finished work.
- Comparison to retire it: replay the same reference set with the switch off. Retire the part when the verify pass rate and the turns used are no worse than with it on.

## verify_fix

- Switch: `verify_fix`, set per project and overridden by a task's overrides, which is how the `internal/replay` package sets it. On by default.
- What it turns on: when the verify command fails or times out, the engine resumes the same session in fix mode with the verify output, at most 2 rounds since the last human action on the task. With the switch off, the task stops as `verify_failed`.
- Assumption: a second attempt after a failed verify usually succeeds.
- Comparison to retire it: replay the same reference set with the switch off. Retire the part when the verify pass rate and the turns used are no worse than with it on.

## The pinned flag set of the Claude Code adapter

- Switch: none. The flags and the environment are constants in `internal/driver/claudecode`, changed when the pinned Claude Code version changes.
- What it turns on: every session runs `claude` with
  - always: `-p --output-format stream-json --input-format stream-json --verbose --include-partial-messages --setting-sources user --strict-mcp-config --system-prompt-snapshot off --permission-mode bypassPermissions`, `--model <id>`, `--settings <file>` with a settings file that turns commit and PR attribution off, `--session-id <uuid>` or `--resume <id>`, and `--max-turns <n>`;
  - when the session sets them: `--effort <level>`, `--append-system-prompt-file <file>` holding its instructions, and `--json-schema <schema>` for a structured result;
  - unless the session allows web tools: `--disallowedTools WebFetch WebSearch`;
  - an environment built whole: `HOME`, `CLAUDE_CONFIG_DIR`, `PATH`, `DISABLE_UPDATES=1`, `DISABLE_AUTOUPDATER=1`, `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1`, `CLAUDE_CODE_DISABLE_FAST_MODE=1`, `CLAUDE_CODE_DISABLE_BACKGROUND_TASKS=1`, `CLAUDE_CODE_DISABLE_WEB_FETCH=1` unless the session allows web tools, the auth mode's variables (`CLAUDE_CODE_OAUTH_TOKEN`, or `ANTHROPIC_BASE_URL` and `ANTHROPIC_AUTH_TOKEN`), none of the other auth or provider variables the caller's environment may hold, and the loopback hosts in `NO_PROXY` and `no_proxy` when the caller's environment names a proxy.
- Assumption: on the pinned version these flags and variables give user-level settings only, native instruction loading and a clean headless run.
- Check to change or retire it: run the conformance suite against each new release. Change a flag or variable when the suite fails, and drop one once a release makes it unnecessary.
