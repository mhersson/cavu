# CAVU

CAVU stands for "Containerized Agents, Verified, Unattended". In aviation the same letters mean "ceiling and visibility unlimited".

CAVU is meant to be a self-hosted tool that runs a coding agent fully unattended: a task goes in, a pull request comes out, and a person merges it. The agent is to run inside a container with API keys and forge tokens kept outside it, and a run is to count as successful only when a verify command, run by CAVU itself, passes.

## Status

Pre-release. The verbs today:

- `cavu version` prints the version.
- `cavu settings` sets and shows the settings.
- `cavu secret set` stores a secret read from standard input.
- `cavu user set` sets the one user and their password, read from standard input.
- `cavu token` adds, lists and removes API tokens.
- `cavu image build` builds the sandbox image on Docker or Podman.
- `cavu doctor` checks the runtime, the image, the settings, the endpoint and the forge.
- `cavu project` adds, changes, lists and shows projects, and checks a project's environment, storing its baseline.
- `cavu task` adds tasks, each with the tasks it depends on, edits a task's title, body and dependencies until its first run starts, lists and shows them, runs one in the sandbox until its change is ready for review or the task stops, prints a task's diff, and records a person's start, answer, retry, cancel, merge and mark.
- `cavu replay` re-runs the marked tasks under a configuration and compares them with the last replay.
- `cavu serve` drives every started task and serves the HTTP API until interrupted.

## Try it

CAVU needs Docker or Podman and a Linux or macOS host. Linux with Docker and with Podman is tested; on macOS only the unit tests run, in CI, and nothing has run there with a container runtime. The commands below keep their data in one directory: the default is created on first use, and `CAVU_DATA_DIR` puts it elsewhere. Every `cavu` verb also takes `--data-dir DIR`; `make image` takes only the variable.

1. Build the binary and the sandbox image. The build opens the data directory and uses its `runtime` and `ca_bundle_path` settings.

   ```
   make image
   ```

   The binary is `bin/cavu`, which is not on your `PATH`: copy it to a directory that is (`install bin/cavu ~/.local/bin/`), or write `bin/cavu` where the commands below say `cavu`. With a built `cavu`, `cavu image build` builds the image alone.

2. Set what a run needs. In `subscription` mode the credential is a subscription token; in `key` mode also set the endpoint.

   ```
   cavu settings set endpoint_mode subscription
   cavu settings set model_work MODEL
   cavu settings set commit_name "Your Name"
   cavu settings set commit_email you@example.com
   cavu settings set endpoint_url https://gateway.example.com     # key mode only
   cavu settings set endpoint_auth_header x-api-key               # key mode only
   ```

3. Store the secrets from standard input, never as arguments. `forge_token` is for a private repository. In `subscription` mode the credential is the long-lived token that `claude setup-token` prints when you run it on your own machine.

   ```
   cavu secret set endpoint_credential < credential.txt
   cavu secret set forge_token < token.txt
   ```

4. Check the setup. Each line is a check; `skipped` follows from an earlier failure, or for the forge check from having no forge token or no project yet, and `warning` passes. The exit code is 1 when a check failed.

   ```
   cavu doctor
   ```

5. Add a project, then check its environment. The check builds the workspace, runs the setup and verify commands once and stores the result as the project's baseline. A task runs only against a project with a passed baseline, or with verify waived.

   ```
   cavu project add web --repo https://github.com/OWNER/REPO.git --verify "make test"
   cavu project check web
   ```

   `--setup CMD` prepares the workspace and `--no-verify` waives the verify command, so a person checks every change. The setup and verify commands, and the agent, get `CAVU_BASE_COMMIT`, the commit the task starts from (in a check, the base branch's tip), which the workspace does not otherwise name: it keeps no remote and no branch but the task's. They also get the project's variables; its secrets reach only the setup command. `cavu project set web` changes a project and `cavu project show web` prints everything it stores: the hosts, variables, secret names, tools, switches and baseline, and the tools the newest check found and the verify command it suggested.

6. Add a task and run it. `task run` prints each event on a line of its own as it is recorded, beginning at your newest action on the task, such as its start or an answer, says why the task waits when it does, and ends with the task's status. `task show` also prints the task's dependencies and why it waits, the turns, the duration and the files the newest export changed, a binary file with its size, at most 50 of them and then the total.

   ```
   cavu task add web --title "Fix the login redirect" --body-file task.md
   cavu task run ID
   cavu task show ID
   cavu task diff ID
   ```

   A run opens with a readiness turn: the agent reads the task and the repository and changes nothing. When a fact it needs is in neither, the task stops with its questions, and `task show` prints each with the answer the agent would assume. `cavu project set web --switch readiness=off` turns the readiness turn off.

   A change that passes verify is reviewed: a fresh agent session reads the task and the diff, changes nothing, and judges whether the change does what the task asked and whether it is well built. A critical or important finding sends the change back for a fix, the verify command and a review of the fix, at most five times; the last two fixes are a fresh session. Findings never stop a task: after the fifth fix, after a fix that changes nothing, or when a review gives no verdict, the change is exported as it is, and `task show` prints the findings left open, the minor ones, and why the newest review gave no verdict. The reviewer runs on `model_review` and `effort_review`, the fresh fix sessions on `model_strong` and `effort_strong`, and each of the four is the work value when unset. `cavu project set web --switch review=off` turns the review off.

   A task can wait for others: `--depends-on ID[,ID]` makes it wait until every task it names is done, and stops it when one of them is cancelled. `--start` starts it as it is added.

   ```
   cavu task add web --title "Test the login redirect" --depends-on ID --start
   ```

   `cavu task edit ID` changes only the title, body or dependencies it is given, until the task's first run starts; `--depends-on ""` removes every dependency.

   ```
   cavu task edit ID --depends-on 3,4
   ```

   `task run` drives only the task it names, so run a dependency first: on a task that waits for one, `task run` prints why and waits until that task is done or you interrupt it.

   A task in `in_review` has its change ready: read the diff, fetch its branch into your clone from the bundle `task show` prints (`git fetch BUNDLE 'refs/heads/cavu/*:refs/heads/cavu/*'`), then push the branch and merge it yourself. Then record the merge, which makes the task `done`:

   ```
   cavu task done ID
   ```

When a task stops, `task run` exits 3 and prints the reason, what it means and what clears it:

```
cavu task answer ID < answer.txt    # the agent asked a question
cavu task retry ID                  # run it again after fixing the cause
cavu task cancel ID                 # give up on it
```

Then `cavu task run ID` again for an answer or a retry. A verb that drives containers holds the data directory's lock while it runs, so a second one is refused with a message naming the lock.

### Skills and instructions for every task

Every agent session of a task, readiness turns and reviews included, can have skills and instructions of your own beside the repository's. Put each skill's directory, with its `SKILL.md`, into one directory and name that directory; name one instruction file, whose text comes before the repository's own instruction files:

```
cavu settings set skills_path ~/cavu/skills
cavu settings set instructions_path ~/cavu/AGENTS.md
```

Both are read on the machine `cavu` runs on, again at each step of a run, so a change reaches the next step. Links in the directory are followed, and its files hold at most 16 MiB together. A repository's skill of the same name is written over yours. A path that cannot be read stops the task with the setting named; fix it and retry. The turns of subagents an agent starts do not count toward `work_max_turns`, a session's turn limit; `work_timeout_minutes`, a session's time limit, bounds them, since it ends the session with its subagents.

### Running the server

`cavu serve` drives every started task, as `task run` drives one, and serves the HTTP API of [docs/api.md](docs/api.md) on the `listen_address` setting (default `127.0.0.1:7117`). Set the user first, with the password in a file, then start it:

```
cavu user set NAME < password.txt
cavu serve
```

It prints the address it listens on. Log in with `POST /api/auth/login`, or send an API token from `cavu token add NAME`. An MCP client gets the tools that create, show, list, run and cancel tasks and list the projects at `http://HOST:PORT/mcp` over the streamable HTTP transport, with the header `Authorization: Bearer TOKEN` and a token from `cavu token add NAME`. While it runs, `task run`, `project check` and `replay` leave the driving to it, and `doctor` is refused. Ctrl-C stops it, and the next `cavu serve` carries on with the tasks it was running.

### Replaying tasks

A replay re-runs tasks that have already run, under a configuration you name, and shows each beside the same task in the previous replay. Mark the tasks to replay (each needs a base commit, which a run gives it), then replay them:

```
cavu task mark ID
cavu replay --name baseline
cavu replay --name no-continuations --no-continuations
```

`--model ID`, `--effort LEVEL`, `--no-continuations`, `--no-verify-fix`, `--no-readiness`, `--no-review` and `--web-tools` (which turns `web_tools` on, whatever the project says) change the configuration, and `--image TAG` replays on another sandbox image, for another Claude Code version; that image is not checked. Each row has the verify result, the questions the agent asked, turns, tokens, cost estimate and duration, with the previous replay's value beside it (empty on a first replay), and the path of the diff. `cavu task mark ID --unmark` clears a mark.

A replayed task starts as a new task does, so its project needs a passed baseline or a waived verify, and the replay says why such a task waits when it does. When its readiness turn asks questions and suggests an answer to each, the replay answers with those and goes on. An interrupted replay cancels the task it was running and is never compared with; the next replay ends what an earlier one left unfinished. Run one replay at a time: a replay started while another runs cancels that one's unfinished shadows.

### Go projects

Download the modules in the setup command (`go mod download`): the setup phase reaches `storage.googleapis.com`, where the module proxy sends large modules, and the agent phase reads the modules from the project's cache. A task that needs a large module the setup did not fetch is refused with the denied-host line; `cavu project set NAME --egress storage.googleapis.com` allows that host for the project; `--egress` sets the whole list, so repeat it for every host the project already allows.

### Python projects

A `uv.lock` at the repository's root installs uv. Use `uv sync --frozen` as the setup command and `uv run` in the verify command (`uv run pytest -q`): every run then has its own `.venv`, which uv keeps out of git. uv runs on the Python that mise installs and never downloads its own.

Packages a setup command installs with `pip` are shared by the project's tasks and accumulate. A project's open runs share the interpreter's packages, so a bare `pip install` in the setup command changes them under every other open run of the project. A virtual environment in the workspace, in a directory `.gitignore` covers, gives per-run packages.

## Licence

MIT. See [LICENSE](LICENSE).
