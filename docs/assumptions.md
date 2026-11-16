# Assumptions

This file lists the removable parts of CAVU: code that makes up for a weakness of today's models and sits behind a switch, so it can be deleted when models no longer need it. Every removable part has an entry here, landed with its code, naming its switch, what the switch turns on, the assumption the part rests on, and the replay comparison that would retire it. The comparisons are to be run by a replay tool that is not built yet.

It also lists what the agent adapter assumes of the agent release it pins. Those entries have no switch: they change with the pin, and name the check against a new release that would change or retire them, to be run by a conformance suite that is not built yet.

## continuations

- Switch: `continuations`, set per project and overridden by a task's overrides, which is how a replay is to set it. On by default.
- What it turns on: when an agent session ends unfinished or crashed, the engine resumes the same session in continue mode, at most 2 times since the last human action on the task. With the switch off, such a session stops the task as `agent_error`.
- Assumption: current models sometimes end a turn with a progress report instead of finished work.
- Comparison to retire it: replay the same reference set with the switch off. Retire the part when the verify pass rate and the turns used are no worse than with it on.

## verify_fix

- Switch: `verify_fix`, set per project and overridden by a task's overrides, which is how a replay is to set it. On by default.
- What it turns on: when the verify command fails or times out, the engine resumes the same session in fix mode with the verify output, at most 2 rounds since the last human action on the task. With the switch off, the task stops as `verify_failed`.
- Assumption: a second attempt after a failed verify usually succeeds.
- Comparison to retire it: replay the same reference set with the switch off. Retire the part when the verify pass rate and the turns used are no worse than with it on.

## The pinned flag set of the Claude Code adapter

- Switch: none. The flag set is a constant in `internal/driver/claudecode`, changed together with the pinned Claude Code version.
- What it turns on: every session runs `claude -p --output-format stream-json --input-format stream-json --verbose --include-partial-messages --setting-sources user --strict-mcp-config --system-prompt-snapshot off --permission-mode bypassPermissions` with its model, session, turn cap and a settings file that turns attribution off, plus `--disallowedTools WebFetch WebSearch` unless the session allows web tools.
- Assumption: on the pinned release these flags give user-level settings only, native instruction loading and a clean headless run.
- Check to change or retire it: run the conformance suite against each new release. Change the flag set when the suite fails, and drop a flag once a release makes it unnecessary.
