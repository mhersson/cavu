# AGENTS.md

CAVU is meant to be a self-hosted tool that runs a coding agent fully unattended: a task goes in, a pull request comes out, and a person merges it. It is one Go module and one binary, `cavu`. The agent is to run in a container with API keys and forge tokens kept outside it, and CAVU is to decide whether a run succeeded.

## Rules that outrank convenience

1. Code holds only what stays true however good models get: the sandbox boundary, the verify gate, budgets, the queue, the record of what happened, and the human merge.
2. Anything that makes up for a model weakness is a removable part: it sits behind a switch, and `docs/assumptions.md` records what it assumes and the test that tells when to delete it.
3. Upgrades, and removals of model-weakness parts, rest on a replay tool's comparison of outcomes.
4. Everything vendor-specific lives in one thin adapter, the Claude Code version is pinned, and an upgrade ships only after tests against the real binary pass.
5. Know-how lives in files the agent reads (`AGENTS.md`, skills), and CAVU's own prompts are few, plain, and kept as files.
6. Small on purpose, with at most 25 REST routes, 7 MCP tools, 30 config keys and 30 concepts a user must learn, and the Go and web line counts reported, not capped. The limits are not hard: when one would make CAVU a worse tool, the case is made to the project's owner, and the number changes with their word.

## Constraints

- One Go module, one binary `cavu`, `CGO_ENABLED=0`, pure-Go SQLite. No second module, repository or shared library, and no dependency on a ContextMatrix or AIM module: they are sources of code to copy, never dependencies, and code from them is copied in and owned here. The web app is one npm package in `web/`, build input embedded in the binary and never a second deliverable.
- Hosts are Linux and macOS. The container runtime is Docker Engine or Podman, including VM-backed setups. Never mount a host path into a container and never write into a volume from the host: files move in and out as streams on a runtime exec.
- `cavu` needs no git on the host: it runs every git command inside a container, and its host-side code never runs git.
- Claude Code is driven as the unmodified `claude` binary and never redistributed; the sandbox image is built locally and never published.
- No telemetry. `cavu` makes one kind of network request of its own outside the containers, to the API of the forge a project's repository is on, for that project's tasks, its environment checks and the doctor's forge check. It follows that API's redirects, since GitHub answers a request for a failed job's log with an address to download it from, and sends the token to no other host. When its environment names a proxy, these requests go through it. It dials nothing else. The web app it serves asks the browser for nothing but its own origin: its fonts, scripts and styles come from the binary, and the page's policy allows no other.
- Everything specific to Claude Code lives in one package. No other Go package names a `claude` flag, environment variable or stream field.
- Everything specific to GitHub's API lives in `internal/forge`. No other package of the binary names a GitHub route, header or field; `internal/testkit/forgefake`, the tests' fake GitHub, is not part of it.
- The server half never imports the container runtime half or `internal/forge`. What the runner needs crosses the boundary as serialisable values.

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

- `make check` runs build-go, build-darwin, fmt-check, vet, lint, test-race, tidy-check, scan and size. It is Go only and needs no Node.
- `make build` builds the web app, then `bin/cavu` with it embedded; `make build-go` builds `bin/cavu` alone, with the app as built last or with the placeholder page when none is built; `make install` builds the web app, then installs `cavu` with it embedded into `GOBIN`, else `$(go env GOPATH)/bin`; `make build-darwin` builds `bin/cavu-darwin` for macOS on arm64; `make test` and `make test-race` run the Go tests.
- Building the web app needs Node, the version in the root `.nvmrc`, and `make web-install` once (`npm ci` in `web/`); `make web-build` builds it into `web/dist/app`, and `make web-check` runs its lint, tests and build. Run `make web-check` when `web/` changes. Installing and running CAVU need only the binary and a container runtime.
- `make fmt` formats with `golangci-lint fmt` (gofumpt and goimports); `make lint` runs `golangci-lint run`.
- `make scan` fails on em-dashes in the Go, Markdown, YAML, TOML, shell and Dockerfile files and the web app's TypeScript, JavaScript, CSS, HTML and JSON files; `make scan-commits` fails on session links, attribution lines, task or milestone numbers, em-dashes and blobs over 500 KB in the commits since `origin/main`.
- `make size` prints two lines: the non-test Go lines, and the non-test TypeScript, JavaScript, CSS and HTML lines under `web/`.
- `make image` builds the web app and `bin/cavu` (as `make build` does) and a Linux `bin/cavu-linux` for the host's architecture, then the sandbox image `cavu-sandbox:<version>` from it with `cavu image build`, which opens the data directory (`CAVU_DATA_DIR`, else the default, created on first use) and builds on the runtime its `runtime` setting names (else Docker, then Podman) with the CA bundle its `ca_bundle_path` setting names.
- `make test-integration` runs the `integration` tests with `-count=1`, a 30 minute timeout and all packages; `make test-ecosystems` runs the `ecosystems` tests.
- Before a commit, run `make check`. The whole `integration` suite runs on Docker and on Podman in `.github/workflows/integration.yaml`, every night on `main` and by hand, never on a pull request; a change of the pinned Claude Code version merges only after that workflow has passed on its branch.
- Test tags, each run with `-count=1`: `integration` needs a container runtime and builds the sandbox image, and includes the conformance suite against the real `claude` binary; `ecosystems` prepares a Go, a Node, a Python and a Rust repository in real containers and needs the internet on every run; `live` is added to `integration` (`-tags integration,live`) and needs credentials.
- Prerequisites for the tagged tests: Docker, or Podman with `CAVU_TEST_RUNTIME=podman`; the image `docker.io/library/busybox:1.37`, which the runtime tests run with `--pull=never`, so pull it first (`docker pull docker.io/library/busybox:1.37`); the internet for the first sandbox image build, since later builds come from the layer cache.
- `live` checks skip unless their variables are set and never run in CI: `CAVU_LIVE_SUBSCRIPTION_TOKEN` for a subscription, `CAVU_LIVE_ENDPOINT_URL` and `CAVU_LIVE_ENDPOINT_KEY` for a gateway, and optionally `CAVU_LIVE_ENDPOINT_AUTH_HEADER` and `CAVU_LIVE_MODEL`. `docs/assumptions.md` has the commands.

## Layout

Every package states its one responsibility in its package comment. Run `go doc ./cmd/cavu` to read one. `web/` is the web app's npm package (`web/src` its sources, `web/dist/placeholder.html` the page a binary built without the app serves) and the Go package `web`, which embeds the built app in `web/dist/app` and serves it in front of the API.
