# Assumptions

This file lists the removable parts of CAVU: code that makes up for a weakness of today's models and sits behind a switch, so it can be deleted when models no longer need it. Every removable part has an entry here, landed with its code, naming its switch, what the switch turns on, the assumption the part rests on, and the replay comparison that would retire it. The `internal/replay` package runs the comparisons; no command runs it yet.

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
