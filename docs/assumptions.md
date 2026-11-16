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
- What it turns on: when the verify command fails or times out, the engine resumes the same session in fix mode with the verify output, at most 2 rounds since the last human action on the task. With the switch off, the task stops as `verify_failed`.
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
  - a cleared config directory: before every session, base commit or not, the driver removes everything in the config directory but the transcripts (`projects/`), so nothing an earlier session wrote there for the user source to load reaches a later one. A session's home must be an absolute, clean path other than `/`, its config directory a clean path strictly below the home, and its working directory neither the config directory nor below it; a session that breaks one of these starts no command. The clear is a command of its own: it leaves a missing config directory alone, refuses one that is a symlink or resolves elsewhere, deleting nothing, and deletes by relative name inside the directory;
  - staged instruction files: when the session names a base commit, the driver then reads the repository's instruction files from that commit inside the agent container (git plumbing, replace objects and lazy fetching off) and writes into the config directory, as a tar stream on the stdin of a command of its own, one `CLAUDE.md` holding the root `CLAUDE.md`, `.claude/CLAUDE.md` and the root `AGENTS.md`, each under a heading naming it and cut at 64 KiB, then the paths of the nested instruction files with a line telling the agent to read a directory's file before working under it; the Markdown files of `.claude/rules` in `rules/`, each cut at 64 KiB; and `.claude/skills` in `skills/`, regular files only. Directories are written 0755 and files 0644, or 0755 for a skill file the commit marks executable. A memory or rule file that is a symlink is followed only to a regular file inside the tree, and a `.claude/skills` or `.claude/rules` that is a symlink stages nothing. Nested files are listed, not loaded, and nothing else of the repository's `.claude/` is taken;
  - a sandbox check: the clear and the write run only through a check for `/etc/cavu/sandbox`, an empty, root-owned, read-only file that only the sandbox image carries, take the config directory as an argument, never in their script, and do nothing where the file is missing. An image built without it stops every session with an error naming `cavu image build`. These commands and the git reads run only in the agent container: the unit tests record them and run none, and the conformance suite runs them there. A failure to clear or stage is an error, and no session starts.
- Assumption: on the pinned version these flags and variables give user-level settings only and a clean headless run. The pinned version reads a repository's `CLAUDE.md`, `AGENTS.md` and skills only with the project settings source, which also applies the repository's own settings file (its environment, permission rules and hooks), so the staging gives the agent those files through the user source instead.
- Check to change or retire it: run the conformance suite against each new release. Change a flag or variable when the suite fails, and drop one once a release makes it unnecessary. `TestInstructionsNeedStaging` runs a session without staging and wants none of the repository's instruction files in any request; when it fails, the release loads them without the project source, and the staging goes.
