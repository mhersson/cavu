# Configuration

The commands below keep their data in one directory: the default is created on first use, and `CAVU_DATA_DIR` puts it elsewhere. Every `cavu` verb but `cavu service uninstall` also takes `--data-dir DIR`; `make image` takes only the variable.

## Building from source

Building from source needs Go and Node (the version in `.nvmrc`, 26); run `make web-install` once to install the web app's npm packages. Installing and running `cavu` need only the binary and a container runtime. `make image` builds the binary and the sandbox image; the build opens the data directory and uses its `runtime` and `ca_bundle_path` settings.

The binary is `bin/cavu`, which is not on your `PATH`: `make install` also builds the web app and writes `cavu` to `GOBIN`, else `$(go env GOPATH)/bin` (put that directory on your `PATH`), without the image, or write `bin/cavu` where the commands below say `cavu`. With a built `cavu`, `cavu image build` builds the image alone; off Linux it takes `../libexec/cavu-linux` beside the directory of the real executable when that file is there, and otherwise needs `--cavu-binary PATH`, a `cavu` built for Linux, which `make image` builds and passes. `make build` builds the web app and the binary, without the image, and `make build-go` builds it with the web app as last built, or with a page that says none is built in. `VERSION=x.y.z` on any build target stamps the version `cavu version` prints, which the image's tag follows, and `make dist VERSION=x.y.z` writes a release's Linux `amd64` and `arm64` archives and their `checksums.txt` into `dist/`. To have CAVU work on its own repository, add it as a project with the setup command `go mod download && make web-install` and the verify command `make check && make web-check`.

## Settings and secrets

Set what a run needs. In `subscription` mode the credential is a subscription token; in `key` mode also set the endpoint.

```
cavu settings set endpoint_mode subscription
cavu settings set model_work MODEL
cavu settings set commit_name "Your Name"
cavu settings set commit_email you@example.com
cavu settings set endpoint_url https://gateway.example.com     # key mode only
cavu settings set endpoint_auth_header x-api-key               # key mode only
```

`ci_timeout_minutes` is how long a pull request's checks may stay unfinished before the task stops ([The pull request](tasks.md#the-pull-request)): a whole number of minutes, 120 when unset and `0` for no limit.

Store the secrets from standard input, never as arguments. In `subscription` mode the credential is the long-lived token that `claude setup-token` prints when you run it on your own machine.

```
cavu secret set endpoint_credential < credential.txt
```

The web app's **Settings** page shows and sets every setting, the instance's secrets and the named GitHub credentials, and never shows a secret's value.

## Projects

Add a project.

```
cavu project add web --repo https://github.com/OWNER/REPO.git --verify "make test"
```

`--repo` takes the repository's `https://` address on github.com or on GitHub Enterprise, and `--base BRANCH` names its base branch, which tasks start from and their pull requests target, when that is not `main`. [GitHub Enterprise](github-access.md#github-enterprise) says how CAVU finds GitHub's API.

`--setup CMD` prepares the workspace and `--no-verify` waives the verify command, so a person checks every change. The setup and verify commands, and the agent, get `CAVU_BASE_COMMIT`, the commit the task starts from (in a check, the base branch's tip), which the workspace does not otherwise name: it keeps no remote and no branch but the task's. They also get the project's variables; its secrets reach only the setup command. `cavu project set web` changes a project and `cavu project show web` prints everything it stores: the hosts, variables, secret names, tools, switches, caps, pull request policy, GitHub credential (`--forge-credential NAME`, the default when none) and baseline, and the tools the newest check found and the verify command it suggested. The pull request policy is whether the repository's pull requests may start no check (`--no-ci on` or `off`, [The pull request](tasks.md#the-pull-request)), the checks the project ignores (`--ignore-check NAME`, the name as GitHub shows it) and its review bots (`--review-bot LOGIN`, the login as GitHub shows it: a GitHub App's bot is written `name[bot]`, and a name without `[bot]` is a user's account). Each of the two lists takes the flag repeated, and one empty value empties it.

`cavu project remove web` removes the project's cache and mirror volumes and its secrets, and the project leaves every list, including the API's and `list_projects`. Its tasks, with their events and artifacts, stay, and its finished tasks still show its name. It is refused while the project has a task that is neither done nor cancelled, naming those tasks; `cavu task cancel ID` clears one. It is refused with the container runtime's error when the runtime would not remove a volume, and the project and its secrets stay, and removing it again finishes the job. The name can be used again for a new project, which starts with none of the removed one's secrets. There is no undo.

Check the project's environment. The check builds the workspace, runs the setup and verify commands once and stores the result as the project's baseline. A task runs only against a project with a passed baseline, or with verify waived.

```
cavu project check web
```

## The doctor

Check the setup once a project exists, so that the forge check has a project to read. Each line is a check; `skipped` follows from an earlier failure, or for the forge check from having no project, and `warning` passes. The forge check has a line for each project, in name order, which is skipped when the project has no credential for GitHub: it names none, and neither `forge_token` nor `forge_app_id` is set. It clones each with the credential the project names, or the default, as a run's clone does, and with the project's switch `publish` on it also reads over GitHub's API the repository and the checks of the base branch's head commit, the way CAVU reads the checks of a pull request it watches; when it reads them as GitHub Actions jobs, its line says so and names what is not seen. It pushes nothing and opens no pull request, so it does not show whether the credential may. The exit code is 1 when a check failed.

```
cavu doctor
```

The doctor runs only while `cavu serve` is stopped: a running server refuses it, and [Running the server](running.md) says how to stop the service.

## Skills and instructions for every task

Every agent session of a task, readiness turns and reviews included, can have skills and instructions of your own beside the repository's. Put each skill's directory, with its `SKILL.md`, into one directory and name that directory; name one instruction file, whose text comes before the repository's own instruction files:

```
cavu settings set skills_path ~/cavu/skills
cavu settings set instructions_path ~/cavu/AGENTS.md
```

Both are read on the machine `cavu` runs on, again at each step of a run, so a change reaches the next step. Links in the directory are followed, a `.git` directory in it is left out, and its files hold at most 16 MiB together. A repository's skill of the same name is written over yours. A path that cannot be read stops the task with the setting named; fix it and retry. The turns of the subagents an agent starts do not count toward `task_max_turns`; the task's time bounds them, since a session gets what is left of it and its end ends its subagents.

## Go projects

Download the modules in the setup command (`go mod download`): the setup phase reaches `storage.googleapis.com`, where the module proxy sends large modules, and the agent phase reads the modules from the project's cache. A task that needs a large module the setup did not fetch is refused with the denied-host line; `cavu project set NAME --egress storage.googleapis.com` allows that host for the project; `--egress` sets the whole list, so repeat it for every host the project already allows.

A task's first build starts from the build cache of the project's newest passed check. Go's test results travel with that cache, so a verify command that must rerun every test says `-count=1` (`go test -count=1 ./...`).

A `.golangci.yml`, `.golangci.yaml`, `.golangci.toml` or `.golangci.json` at the repository's root installs golangci-lint, at the newest release of the major version its `version` field names, or of 1 when it has none. A newer release can report what an older one does not, so a repository whose CI pins golangci-lint pins the same version in the project: `cavu project set NAME --tool golangci-lint=VERSION`, or a line under **Versions set here** on the project's page. `--tool` sets the whole list, so repeat it for every version the project already sets.

## Python projects

A `uv.lock` at the repository's root installs uv. Use `uv sync --frozen` as the setup command and `uv run` in the verify command (`uv run pytest -q`): every run then has its own `.venv`, which uv keeps out of git. uv runs on the Python that mise installs and never downloads its own.

Packages a setup command installs with `pip` are shared by the project's tasks and accumulate. A project's open runs share the interpreter's packages, so a bare `pip install` in the setup command changes them under every other open run of the project. A virtual environment in the workspace, in a directory `.gitignore` covers, gives per-run packages.

## Replaying tasks

A replay re-runs tasks that have already run, under a configuration you name, and shows each beside the same task in the previous replay. Mark the tasks to replay (each needs a base commit, which a run gives it), then replay them:

```
cavu task mark ID
cavu replay --name baseline
cavu replay --name no-continuations --no-continuations
```

`--model ID`, `--effort LEVEL`, `--no-continuations`, `--no-verify-fix`, `--no-readiness`, `--no-review` and `--web-tools` (which turns `web_tools` on, whatever the project says) change the configuration, and `--image TAG` replays on another sandbox image, for another Claude Code version; that image is not checked. Each row has the verify result, the questions the agent asked, turns, tokens, cost estimate and duration, with the previous replay's value beside it (empty on a first replay), and the path of the diff. `cavu task mark ID --unmark` clears a mark. A marked task of a removed project is not replayed.

A replayed task starts as a new task does, so its project needs a passed baseline or a waived verify, and the replay says why such a task waits when it does. When its readiness turn asks questions and suggests an answer to each, the replay answers with those and goes on. An interrupted replay cancels the task it was running and is never compared with; the next replay ends what an earlier one left unfinished. Run one replay at a time: a replay started while another runs cancels that one's unfinished shadows.
