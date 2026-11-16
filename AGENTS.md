# AGENTS.md

CAVU is meant to be a self-hosted tool that runs a coding agent fully unattended: a task goes in, a pull request comes out, and a person merges it. It is one Go module and one binary, `cavu`. The agent is to run in a container, API keys and forge tokens stay outside it, and CAVU decides whether a run succeeded.

## Rules that outrank convenience

1. Code holds only what stays true however good models get: the sandbox boundary, the verify gate, budgets, the queue, the record of what happened, and the human merge.
2. Anything that makes up for a model weakness is a removable part: it sits behind a switch, and `docs/assumptions.md` records what it assumes and the test that tells when to delete it.
3. A replay tool must exist from the start, and an upgrade or a removal of a model-weakness part must rest on its comparison of outcomes.
4. Everything vendor-specific lives in one thin adapter, the Claude Code version is pinned, and an upgrade ships only after tests against the real binary pass.
5. Know-how lives in files the agent reads (`AGENTS.md`, skills), and CAVU's own prompts are few, plain, and kept as files.
6. Small on purpose: at most 25,000 non-test Go lines, 12,000 web lines, 25 REST routes, 6 MCP tools, 30 config keys and 30 concepts a user must learn.

## Constraints

- One Go module, one binary `cavu`, `CGO_ENABLED=0`, pure-Go SQLite. No second module, repository or shared library, and no dependency on a ContextMatrix or AIM module: they are sources of code to copy, never dependencies, and code from them is copied in and owned here.
- Hosts are Linux and macOS. The container runtime is Docker Engine or Podman, including VM-backed setups. Never mount a host path into a container and never write into a volume from the host: files move in and out as streams on a runtime exec.
- The host needs no git: every git command runs inside a container, and host-side code never runs git.
- Claude Code is driven as the unmodified `claude` binary and never redistributed; the sandbox image is built locally and never published.
- No telemetry. `cavu` makes no network request of its own outside the containers.
- Everything specific to Claude Code lives in one package. No other Go package names a `claude` flag, environment variable or stream field.
- The server half never imports the container runtime half. What the runner needs crosses the boundary as serialisable values.

## Never

- Make a model key or forge token readable inside the sandbox in key mode.
- Record a run as successful on the agent's word or on an exit code.
- Merge into a default branch.
- Leave a loop without a cap, or a stop without a reason a person can act on.
- Leave code that compensates for a model weakness without an entry in `docs/assumptions.md`.
- Add a second repository, Go module or shared library.
- Require more than one binary and a container runtime to install.

## Conventions

- Conventional commits with a scope: `type(scope): summary`, with bullet lists in bodies.
- No attribution lines, and no session links, task or milestone numbers, or plan references in commits, branch names, code comments or documentation.
- No em-dashes anywhere.
- Tests exercise behaviour and never assert on prose.
- One branch per pull request, one commit per logical change. Never push or merge.

## Documentation

- Documentation must always be true: every statement matches the code at HEAD, and a change that makes a doc line false updates it in the same commit.
- Document the current state - what exists now and why, not how it got here.
- Comments explain only non-obvious decisions, constraints, safety invariants, or workarounds. Do not narrate what code does or record change history; git holds history. Keep comments to one or two tight lines unless a longer explanation is genuinely necessary. Rewrite or delete if you find comments that don't follow this rule.

## Commands

- `make check` runs build, build-darwin, fmt-check, vet, lint, test-race, tidy-check and scan.
- `make build` builds `bin/cavu`; `make test` and `make test-race` run the tests.
- `make fmt` formats with `golangci-lint fmt` (gofumpt and goimports); `make lint` runs `golangci-lint run`.
- `make scan` fails on em-dashes; `make scan-commits` fails on session links, attribution lines, task or milestone numbers, em-dashes and blobs over 500 KB in the commits since `origin/main`.
- `make size` counts non-test Go lines against the 25,000 budget.
- Tests that need a container runtime carry the build tag `integration`; tests that need the built image and the real `claude` binary carry `conformance`. Run both with `-count=1`.

## Layout

Every package states its one responsibility in its package comment. Run `go doc ./cmd/cavu` to read one.
