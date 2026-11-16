# AGENTS.md

CAVU is a self-hosted tool that runs a coding agent fully unattended: a task goes in, a pull request comes out, and a person merges it. It is one Go module and one binary, `cavu`. The agent runs in a container, CAVU keeps the credentials, and CAVU decides whether a run succeeded.

## Rules that outrank convenience

1. Code holds only what stays true however good models get: the sandbox boundary, the verify gate, budgets, the queue, the record of what happened, and the human merge.
2. Anything that makes up for a model weakness is a removable part: it sits behind a switch, and `docs/assumptions.md` records what it assumes and the test that tells when to delete it.
3. A replay tool re-runs a small reference set of finished tasks under another model, another Claude Code version, or with a step switched off, and compares outcomes. Upgrades and removals rest on that evidence.
4. Everything vendor-specific lives in one thin adapter. The Claude Code version is pinned, its behaviour is probed by tests against the real binary, and an upgrade ships only after those pass.
5. Know-how lives in files the agent reads (`AGENTS.md`, skills). CAVU's own prompts are few, plain, and kept as files.
6. Small on purpose: at most 25,000 non-test Go lines, 12,000 web lines, 25 REST routes, 6 MCP tools, 30 config keys and 30 concepts a user must learn.

## Constraints

- One Go module, one binary `cavu`, `CGO_ENABLED=0`, pure-Go SQLite. No second module, repository or shared library, and no dependency on another repository's module: code is copied in and owned here.
- Hosts are Linux and macOS. The container runtime is Docker Engine or Podman, including VM-backed setups. Never mount a host path into a container and never write into a volume from the host: files move in and out through the runtime's copy command.
- The host needs no git. Every git command runs inside a container, and host-side code never runs git in a directory an agent has written to.
- No telemetry. `cavu` makes no network request of its own outside the containers.
- Everything specific to Claude Code lives in one package. No other Go package names a `claude` flag, environment variable or stream field.
- The server half never imports the container runtime half. What the runner needs crosses the boundary as serialisable values.

## Never

- Make a model key or forge token readable inside the sandbox.
- Record a run as successful on the agent's word or on an exit code.
- Merge into a default branch.
- Leave a loop without a cap, or a stop without a reason a person can act on.
- Leave code that compensates for a model weakness without an entry in `docs/assumptions.md`.
- Add a second repository, Go module or shared library.
- Require more than one binary and a container runtime to install.

## Conventions

- Conventional commits with a scope: `type(scope): summary`, with bullet lists in bodies.
- No attribution lines, no session links, no task ids in commits or branch names.
- Documentation states only what is true of the code in the same commit.
- No em-dashes anywhere.
- Tests exercise behaviour and never assert on prose.
- One branch per plan, each task its own commit. Never push or merge.

## Layout

Every package states its one responsibility in its package comment. Run `go doc ./cmd/cavu` to read one.
