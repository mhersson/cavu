# CAVU

CAVU stands for "Containerized Agents, Verified, Unattended". In aviation the same letters mean "ceiling and visibility unlimited".

CAVU is meant to be a self-hosted tool that runs a coding agent fully unattended: a task goes in, a pull request comes out, and a person merges it. The agent is to run inside a container with API keys and GitHub tokens kept outside it, and a run is to count as successful only when a verify command, run by CAVU itself, passes.

## Status

Pre-release. The verbs today:

- `cavu version` prints the version.
- `cavu settings` sets and shows the settings.
- `cavu secret set` stores a secret read from standard input.
- `cavu credential` stores, lists and removes named GitHub credentials, each read from standard input.
- `cavu user set` sets the one user and their password, read from standard input.
- `cavu token` adds, lists and removes API tokens.
- `cavu image build` builds the sandbox image on Docker or Podman.
- `cavu doctor` checks the runtime, the image, the settings, the endpoint and the access to GitHub.
- `cavu project` adds, changes, lists and shows projects, and checks a project's environment, storing its baseline.
- `cavu task` adds tasks, each with the tasks it depends on, edits a task's title, body and dependencies until its first run starts and while it is not cancelled, lists them, each with whether it needs you (`--needs-you` keeps those that do), shows them, runs one in the sandbox until its pull request opens (with the switch `publish` off, until its change is ready for review) or the task stops, prints a task's diff, and records a person's start, answer, retry, cancel, merge and mark.
- `cavu replay` re-runs the marked tasks under a configuration and compares them with the last replay.
- `cavu serve` drives every started task, watches the pull requests CAVU opened, and serves the HTTP API until interrupted.
- `cavu service` runs `cavu serve` as a service of your login, a systemd user unit on Linux or a launchd agent on macOS, and removes it.

## Try it

CAVU needs Docker or Podman and a Linux or macOS host. Linux with Docker and with Podman is tested; on macOS only the unit tests run, in CI, and nothing has run there with a container runtime. The commands below keep their data in one directory: the default is created on first use, and `CAVU_DATA_DIR` puts it elsewhere. Every `cavu` verb but `cavu service uninstall` also takes `--data-dir DIR`; `make image` takes only the variable.

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

   `ci_timeout_minutes` is how long a pull request's checks may stay unfinished before the task stops ([The pull request](#the-pull-request)): a whole number of minutes, 120 when unset and `0` for no limit.

3. Store the secrets from standard input, never as arguments. In `subscription` mode the credential is the long-lived token that `claude setup-token` prints when you run it on your own machine.

   ```
   cavu secret set endpoint_credential < credential.txt
   ```

   CAVU reaches GitHub with a GitHub App or a personal access token, the default credential, which every project uses unless it names another. It clones a private repository with it. With a project's switch `publish` on, the default, it also pushes the task's branch, opens its pull request, reads the pull request with its checks and reviews, marks it ready for review and comments on it, on a public repository too ([The pull request](#the-pull-request)).

   A GitHub App is recommended: CAVU asks the App's installation on the repository for tokens for that repository alone, each with only what its use needs, and GitHub's installation tokens expire after an hour. Each push gets a new token with Contents write, and Workflows write only when the task's change touches a file under `.github/workflows/`; the clone and the API calls share one, which CAVU keeps until five minutes before it expires. With Checks read an App also reads the check runs, which a fine-grained personal access token cannot (below). Grant the App these repository permissions: Contents and Pull requests read and write, Checks, Commit statuses, Actions and Metadata read-only, and Workflows write for CAVU to change a workflow. CAVU's tokens ask for all of them, and GitHub's documentation says an installation token cannot be granted a permission the App was not granted, nor access to a repository its installation was not. Install the App on the repository, then set `forge_app_id` to the App's id and store its private key, in PEM, as `forge_app_key`:

   ```
   cavu settings set forge_app_id 123456
   cavu secret set forge_app_key < app-key.pem
   ```

   With `forge_app_id` empty, CAVU uses the personal access token `forge_token`, as it is, for every clone, push and call. A fine-grained personal access token limited to the repository needs these repository permissions, each of which GitHub's documentation names for one of CAVU's calls: Contents and Pull requests read and write, Actions, Commit statuses and Metadata read-only, and, for CAVU to change a workflow, Workflows, which the documentation offers as write, and write always includes read. A classic personal access token works with the `repo` scope, which GitHub's documentation describes as full access to public and private repositories, so it reaches every repository the account can, not this one alone; it needs the `workflow` scope too for CAVU to change a workflow. Then store the token:

   ```
   cavu secret set forge_token < token.txt
   ```

   A project can name another credential, for example one per organisation. Store it under a name from standard input, a personal access token or a GitHub App's private key in PEM, then name it on the project; `--forge-credential ''` returns the project to the default. The permissions above apply to each credential.

   ```
   cavu credential set acme-org < token.txt
   cavu credential set work-app --app-id 482913 < app-key.pem
   cavu project set web --forge-credential acme-org
   ```

   `cavu credential list` lists the credentials with their kind and an App's id, never a secret, and `cavu credential remove NAME` removes one that no project names.

   GitHub's documentation lists calling the Checks API among what fine-grained tokens do not support, while a classic token with `repo` reads the check runs. When GitHub refuses a personal access token the Checks API, CAVU reads the jobs of the head commit's GitHub Actions workflow runs and the commit statuses instead, and a check reported only as a check run, by another app or by a workflow step, is not seen: CAVU neither waits for it nor acts on its result. A pull request whose only checks are such check runs reads as having none, so CAVU marks it ready after 3 minutes and stops it as `no_checks` 3 minutes later, or with `--no-ci on` marks it ready at once. For the GraphQL calls with which CAVU reads the reviews and marks the pull request ready for review, GitHub's documentation of the permissions fine-grained tokens need names none, since it covers the REST routes only; its guide to GraphQL calls says that the data a call asks for decides the permissions it needs, and that the answer to a call without them names those the token needs.

   A change to a file under `.github/workflows/` needs the Workflows permission, or the `workflow` scope, which GitHub's documentation names for editing workflow files. Without it, when GitHub refuses the push, the task stops as `push_rejected` and the explanation quotes the reason; when GitHub answers the request for an App's push token with status 422, the task stops the same way and the explanation names the permissions the App must be granted.

4. Add a project.

   ```
   cavu project add web --repo https://github.com/OWNER/REPO.git --verify "make test"
   ```

   `--repo` takes the repository's `https://` address on github.com or on GitHub Enterprise, and `--base BRANCH` names its base branch, which tasks start from and their pull requests target, when that is not `main`. CAVU finds GitHub's API from the address's host, with no setting: `https://api.github.com` for `github.com`, `https://api.HOST` for a host under `.ghe.com`, and `https://HOST/api/v3` for any other host. Its calls to the API trust the CA bundle that `ca_bundle_path` names too, beside the system's certificates.

   `--setup CMD` prepares the workspace and `--no-verify` waives the verify command, so a person checks every change. The setup and verify commands, and the agent, get `CAVU_BASE_COMMIT`, the commit the task starts from (in a check, the base branch's tip), which the workspace does not otherwise name: it keeps no remote and no branch but the task's. They also get the project's variables; its secrets reach only the setup command. `cavu project set web` changes a project and `cavu project show web` prints everything it stores: the hosts, variables, secret names, tools, switches, caps, pull request policy, GitHub credential (`--forge-credential NAME`, the default when none) and baseline, and the tools the newest check found and the verify command it suggested. The pull request policy is whether the repository's pull requests may start no check (`--no-ci on` or `off`, [below](#the-pull-request)), the checks the project ignores (`--ignore-check NAME`, the name as GitHub shows it) and its review bots (`--review-bot LOGIN`, the login as GitHub shows it: a GitHub App's bot is written `name[bot]`, and a name without `[bot]` is a user's account). Each of the two lists takes the flag repeated, and one empty value empties it.

   `cavu project remove web` removes the project's cache and mirror volumes and its secrets, and the project leaves every list, including the API's and `list_projects`. Its tasks, with their events and artifacts, stay, and its finished tasks still show its name. It is refused while the project has a task that is neither done nor cancelled, naming those tasks; `cavu task cancel ID` clears one. It is refused with the container runtime's error when the runtime would not remove a volume, and the project and its secrets stay, and removing it again finishes the job. The name can be used again for a new project, which starts with none of the removed one's secrets. There is no undo.

5. Check the setup, now that the forge check has a project to read. Each line is a check; `skipped` follows from an earlier failure, or for the forge check from having no project, and `warning` passes. The forge check has a line for each project, in name order, which is skipped when the project has no credential for GitHub: it names none, and neither `forge_token` nor `forge_app_id` is set. It clones each with the credential the project names, or the default, as a run's clone does, and with the project's switch `publish` on it also reads over GitHub's API the repository and the checks of the base branch's head commit, the way CAVU reads the checks of a pull request it watches; when it reads them as GitHub Actions jobs, its line says so and names what is not seen. It pushes nothing and opens no pull request, so it does not show whether the credential may. The exit code is 1 when a check failed.

   ```
   cavu doctor
   ```

6. Check the project's environment. The check builds the workspace, runs the setup and verify commands once and stores the result as the project's baseline. A task runs only against a project with a passed baseline, or with verify waived.

   ```
   cavu project check web
   ```

7. Add a task and run it. `task run` prints each event on a line of its own as it is recorded, beginning at your newest action on the task, such as its start or an answer, says why the task waits when it does, and ends with the task's status. With the switch `publish` on it ends once the task's pull request opens, with the pull request's address and a line saying that `cavu serve` watches it from there, so start `cavu serve` ([Running the server](#running-the-server)); on a task whose pull request is watched already it prints the same and returns at once. `task show` also says when the task needs you, and prints the task's dependencies and why it waits, the turns, the duration, the budget it used since you last acted, and the files of the task's change, a binary file with its size, at most 50 of them and then the total, with the paths in it that a person should see before a merge, each kind on a line of its own: workflows under `.github/workflows/`, agent instructions (`AGENTS.md`, `CLAUDE.md` and files under a `.claude/` directory, in any directory), deleted ones too, and files over 1 MiB that the change adds. Once the task has a pull request, `task show` prints its address, its state, its checks with the failed ones named and, while CAVU watches it, what the watch waits for; the change is then the one CAVU published to the pull request last.

   With the project's switch `publish` on, the default, CAVU acts on GitHub under the account of the token it holds (for a GitHub App, the App's bot). Once the task's change is exported it pushes the task's branch, `cavu/` and a slug of the title (`cavu/fix-the-login-redirect` for the task below; when GitHub already has a branch of that name that CAVU did not push for the task, the first free name with `-2` to `-9` added), and opens a draft pull request with a text it writes, whose contents [Security](#security) lists. `cavu serve` then watches the pull request, and may mark it ready for review, push the commits of fix runs to the branch, and comment on it after a follow-up run ([The pull request](#the-pull-request)). CAVU never forces a push, never merges, never closes a pull request and never deletes a branch.

   ```
   cavu task add web --title "Fix the login redirect" --body-file task.md
   cavu task run ID
   cavu task show ID
   cavu task diff ID
   ```

   A run that is no [fix run](#the-pull-request) opens with a readiness turn: the agent reads the task and the repository and changes nothing. When a fact it needs is in neither, the task stops with its questions, and `task show` prints each with the answer the agent would assume. `cavu project set web --switch readiness=off` turns the readiness turn off.

   A change that passes verify in a run that is no fix run is reviewed: a fresh agent session reads the task and the diff, changes nothing, and judges whether the change does what the task asked and whether it is well built. A critical or important finding sends the change back for a fix, the verify command and a review of the fix, at most five times; the last two fixes are a fresh session. Findings never stop a task: after the fifth fix, after a fix that changes nothing, or when a review gives no verdict, the change is exported as it is, and `task show` prints the findings left open, the minor ones, and why the newest review gave no verdict. The reviewer runs on `model_review` and `effort_review`, the fresh fix sessions on `model_strong` and `effort_strong`, and each of the four is the work value when unset. `cavu project set web --switch review=off` turns the review off.

   A task can wait for others: `--depends-on ID[,ID]` makes it wait until every task it names is done, and stops it when one of them is cancelled. `--start` starts it as it is added.

   ```
   cavu task add web --title "Test the login redirect" --depends-on ID --start
   ```

   `cavu task edit ID` changes only the title, body or dependencies it is given, until the task's first run starts and while it is not cancelled; `--depends-on ""` removes every dependency.

   ```
   cavu task edit ID --depends-on 3,4
   ```

   `task run` drives only the task it names, so run a dependency first: on a task that waits for one, `task run` prints why and waits until that task is done or you interrupt it.

   With the switch `publish` on, `task run` ends where the pull request opens, and `cavu serve` records its merge, which makes the task `done`; `cavu task done` is refused for a task with a pull request. With `cavu project set web --switch publish=off`, a task in `in_review` has its change ready: read the diff, fetch its branch into your clone from the bundle `task show` prints (`git fetch BUNDLE 'refs/heads/cavu/*:refs/heads/cavu/*'`; `GET /api/tasks/ID/change?format=bundle` hands over the same bundle, and `format=diff` the diff), then push the branch and merge it yourself. Then record the merge, which makes the task `done`:

   ```
   cavu task done ID
   ```

When a task stops, `task run` exits 3 and prints the reason, what it means and what clears it:

```
cavu task answer ID < answer.txt    # the agent asked a question
cavu task retry ID                  # run it again after fixing the cause
cavu task cancel ID                 # give up on it
```

Then `cavu task run ID` again for an answer or a retry. A retry of one of the pull request's stops but `push_rejected` is taken up by `cavu serve` at its next look, and `task run` returns at once there ([below](#the-pull-request)). A verb that drives containers holds the data directory's lock while it runs, so a second one is refused with a message naming the lock.

### The pull request

`task run` ends where the pull request opens. From there `cavu serve` watches it ([Running the server](#running-the-server)), so keep one running; when it starts, it looks at once at every pull request CAVU watches.

The pull request opens as a draft. GitHub does not offer draft pull requests in every repository: when GitHub answers the draft with status 422, CAVU asks once more without the draft, so the pull request opens ready for review and CAVU's ready-marking below then changes nothing.

While no fix run works on it, `cavu serve` looks at the pull request every 10 seconds while its checks run, or while none has started within the 3 minutes below, and every minute otherwise, under a stop too; a fix run looks once before it pushes. A look reads the pull request, the check runs and commit statuses of its newest commit but those the project ignores, and the reviewers' requests; once GitHub has refused a personal access token the Checks API, it reads the jobs of the commit's GitHub Actions workflow runs in place of the check runs (step 3). After a look that fails, the next comes 10 seconds later, twice as long after each further failure in a row up to 5 minutes, and while GitHub limits CAVU's requests, not before the limit lifts and at most an hour later; the API's `waiting` holds the newest look's error and the time of the next look.

A merge seen at any look makes the task `done`, and a close without a merge cancels it. A commit on the branch that CAVU did not push, once a look has shown it, stops the task as `taken_over` as soon as no stop is open, unless a retry of `conflict` or `taken_over` gave the branch back to CAVU. Otherwise CAVU acts only on a look taken after its own newest push or ready-marking and after a person's newest action on the task, so a retry of a stop below takes effect at the next look, at most a minute later while GitHub answers. Then, in this order:

1. A conflict with the base branch stops the task as `conflict`.
2. Failed checks start a fix run once every check has finished, at most one for each head of the pull request and two since a person last acted; then the task stops as `ci_failed`. A check that was cancelled counts as failed, one that was skipped or neutral as passed.
3. Reviewers' requests ([who may make one](#security)) that no run was given start a follow-up run for them, at most three since a person last acted; then the task stops as `changes_requested`.
4. Checks that have not finished are waited for until `ci_timeout_minutes` (120 when unset) after CAVU's newest push, its ready-marking or a person's newest action, whichever came last; then the task stops as `ci_timeout`.
5. When no check has started on a project without `no_ci`, CAVU waits 3 minutes, marks the pull request ready for review in case the repository's CI skips a draft, waits 3 minutes more, and then stops the task as `no_checks`. CAVU marks a task's pull request ready only once and never forgets it: after that, 3 minutes with no check stop the task.
6. When the checks passed, or none started on a project with `no_ci`, CAVU marks the draft ready for review, once.
7. Then the pull request waits for a person to merge it.

A fix run starts from the pull request's newest commit and goes on with the task's agent session, with no readiness turn and no review. A fix run on failed checks gets what each failed check reported, for an Actions job the end of its log, and a follow-up run gets each request with its author and the file and line it is on. As in any run, its change must pass the verify command unless the project waives it, and its commits go on top of the pull request's, never in their place: a session whose work does not descend from the commit it started at fails, and no push is forced. Once a follow-up run's work is over, its commits pushed or the code left as it is, CAVU posts one comment on the pull request under the account of the token it holds (for a GitHub App, the App's bot), with the agent's summary and whether it changed the code. CAVU writes inside no review thread and resolves none; a reviewer's next request, a new review that requests changes or a new comment in an unresolved thread, starts the next follow-up run. For the fix runs, CAVU keeps the task's home volume, which holds the agent's session, until the task is done or cancelled, and removes it then. `cavu task cancel ID` leaves the pull request open, and CAVU no longer watches it.

Each stop of the pull request explains itself and clears with `cavu task retry ID`, which gives the task its caps again. A retry of `push_rejected` pushes again. A retry of any other is taken up at the next look, unless, for a stop other than `conflict` or `taken_over`, a look has shown a commit on the branch that CAVU did not push, which then stops the task as `taken_over` at once; a retry of `conflict` or `taken_over` gives CAVU the commit that the next look shows at the head of the branch.

- `push_rejected`: GitHub refused the push of the task's branch, or refused a GitHub App a token for it, or the branch's name and its `-2` to `-9` are all taken; the explanation quotes the reason. Fix the cause and retry: CAVU kept the run, and the retry pushes from it.
- `ci_failed`: the checks still fail after the fix runs, and the explanation names the failed ones. A retry allows two more fix runs.
- `ci_timeout`: the checks did not finish within `ci_timeout_minutes`. A retry waits that long again; `cavu settings set ci_timeout_minutes 0` removes the limit, and a larger number raises it.
- `no_checks`: no check started within 3 minutes of CAVU's newest push, its ready-marking or a person's newest action, after CAVU had marked the pull request ready. A retry waits 3 minutes again. For a repository whose pull requests may start no check, as one with no CI or with CI only for some paths, `cavu project set web --no-ci on` removes the wait: a pull request with no check is then marked ready at once, and checks that do run are still acted on.
- `conflict`: the pull request conflicts with its base branch. CAVU resolves no conflict: resolve it on the pull request's branch and retry, and CAVU goes on from your commit.
- `taken_over`: the branch holds a commit CAVU did not push, such as one a person pushed while no `conflict` stop was open. A retry gives the branch back to CAVU at the commit it then holds; the commits of a run that was open and not yet pushed are dropped. A commit you push while a stop other than `conflict` or `taken_over` is open stops the task as `taken_over` after that stop's retry, and a second retry then gives CAVU your commit.
- `changes_requested`: reviewers still ask for changes after three follow-up runs. A retry allows three more.

A check that fails or never finishes whatever the change, such as a job that never gets a runner, is left out with `cavu project set web --ignore-check NAME`, by its name as GitHub shows it.

### Budgets

A task stops when its sessions and reviews have used `task_max_turns` turns (default 1000) or run for `task_timeout_minutes` minutes (default 300) since a person last started, answered, retried or cancelled it, and each session gets what is left. The stop is `budget` or `timeout`, and `cavu task retry ID` gives the task its whole budget again. The two guard against a runaway, so their defaults are high. A project's cap applies to its own tasks over the setting:

```
cavu settings set task_max_turns 400
cavu project set web --cap task_timeout_minutes=60    # --cap task_timeout_minutes= removes it
```

In key mode money can be capped too, and a money cap needs prices. `model_prices` maps a model id to what your endpoint charges for it, in US dollars per million tokens. One price under `*` prices every model without its own and is enough for a rough cap, for instance your most expensive model's. For an exact cap, price each model by the id a response reports: the dated id, not an alias, and Claude Code's small background model too, which answers under its own id. A subagent can answer under another model than the session's, so with a cap set its model needs a price too; the default under `*` gives it one. With a cap set, a model without a price, or an answer the proxy cannot count, stops the task as `budget`, and the stop names the model or the problem.

```
cavu settings set model_prices '{"*": {"input": 15, "output": 75, "cache_read": 1.5, "cache_write": 18.75}}'
cavu settings set task_budget_usd 5           # what a task may spend since a person last acted
cavu settings set daily_budget_usd 40         # what all tasks may spend in the server's local day
cavu project set web --cap task_budget_usd=0  # no task cap for this project's tasks; the daily cap still holds them
```

There is no money cap unless one is set, and none under a subscription: unset or `0` turns a cap off, and a project's `0` lifts the instance's task cap for its own tasks. A session ends once it spends what the task or the day had left when it began. A task that spent its cap stops as `budget`, and `cavu task retry ID` gives it its whole budget again; once the day spent its cap, tasks wait until the next local midnight. With tasks running side by side, the daily cap can be passed by what the others in flight still spend, at most one task's budget each. `task show` prints what the task spent of its cap and what the day spent of the daily cap.

### Skills and instructions for every task

Every agent session of a task, readiness turns and reviews included, can have skills and instructions of your own beside the repository's. Put each skill's directory, with its `SKILL.md`, into one directory and name that directory; name one instruction file, whose text comes before the repository's own instruction files:

```
cavu settings set skills_path ~/cavu/skills
cavu settings set instructions_path ~/cavu/AGENTS.md
```

Both are read on the machine `cavu` runs on, again at each step of a run, so a change reaches the next step. Links in the directory are followed, a `.git` directory in it is left out, and its files hold at most 16 MiB together. A repository's skill of the same name is written over yours. A path that cannot be read stops the task with the setting named; fix it and retry. The turns of the subagents an agent starts do not count toward `task_max_turns`; the task's time bounds them, since a session gets what is left of it and its end ends its subagents.

### Running the server

`cavu serve` drives every started task, as `task run` drives one, watches each task's pull request until the task is done or cancelled ([The pull request](#the-pull-request)), and serves the HTTP API of [docs/api.md](docs/api.md) on the `listen_address` setting (default `127.0.0.1:7117`). Set the user first, with the password in a file, then start it:

```
cavu user set NAME < password.txt
cavu serve
```

It prints the address it listens on. Log in with `POST /api/auth/login`, or send an API token from `cavu token add NAME`. An MCP client gets the tools that create, show, list, run, answer and cancel tasks and list the projects at `http://HOST:PORT/mcp` over the streamable HTTP transport, with the header `Authorization: Bearer TOKEN` and a token from `cavu token add NAME`. While it runs, `task run`, `project check` and `replay` leave the driving to it, and `doctor` is refused. Ctrl-C stops it, and the next `cavu serve` carries on with the tasks it was running.

To keep it running without a terminal, install it as a service of your login with `cavu service install`. It writes `~/.config/systemd/user/cavu.service` on Linux, a systemd user unit, or `~/Library/LaunchAgents/com.github.mhersson.cavu.plist` on macOS, a launchd agent, and starts the service from it at once and again when it fails. The file names the `cavu` binary that installed it by its absolute path and sets your `PATH`, so the service finds the `docker` or `podman` your shell finds; `CAVU_DATA_DIR`, the absolute path of the data directory chosen as for every verb (`--data-dir`, else `CAVU_DATA_DIR`, else `$XDG_DATA_HOME/cavu`, else `~/.local/share/cavu`); `DOCKER_HOST`, only when your Docker context's endpoint is not `unix:///var/run/docker.sock`; and `HTTPS_PROXY`, `HTTP_PROXY`, `NO_PROXY`, their lower-case forms, `SSL_CERT_FILE` and `SSL_CERT_DIR`, each only when your shell sets it. The macOS agent also sets `HOME`. CAVU's secrets stay in the data directory, where `cavu secret set` stored them; the file is readable by you alone, since a proxy address in it may hold a password. As in a terminal, set the user and build the image first. On Linux, `--linger` also runs `loginctl enable-linger`, for a host nobody stays logged in to.

```
cavu service install
cavu service uninstall
```

Read the log with `journalctl --user -u cavu` on Linux, or in `~/Library/Logs/cavu.log` on macOS. Running `cavu service install` again rewrites the file when it changed and restarts the service. Each build of `cavu` has its own version and sandbox image, so after you replace the binary, build its image first (`cavu image build` with the new binary, or `make image`), then run `cavu service install` again. `cavu service uninstall` stops the service and removes its file, and keeps the data directory. To run `cavu doctor`, which a running server refuses, stop the service first with `systemctl --user stop cavu` on Linux or `launchctl bootout gui/$(id -u)/com.github.mhersson.cavu` on macOS, and start it again afterwards with `systemctl --user start cavu` or `launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.github.mhersson.cavu.plist`.

### Replaying tasks

A replay re-runs tasks that have already run, under a configuration you name, and shows each beside the same task in the previous replay. Mark the tasks to replay (each needs a base commit, which a run gives it), then replay them:

```
cavu task mark ID
cavu replay --name baseline
cavu replay --name no-continuations --no-continuations
```

`--model ID`, `--effort LEVEL`, `--no-continuations`, `--no-verify-fix`, `--no-readiness`, `--no-review` and `--web-tools` (which turns `web_tools` on, whatever the project says) change the configuration, and `--image TAG` replays on another sandbox image, for another Claude Code version; that image is not checked. Each row has the verify result, the questions the agent asked, turns, tokens, cost estimate and duration, with the previous replay's value beside it (empty on a first replay), and the path of the diff. `cavu task mark ID --unmark` clears a mark. A marked task of a removed project is not replayed.

A replayed task starts as a new task does, so its project needs a passed baseline or a waived verify, and the replay says why such a task waits when it does. When its readiness turn asks questions and suggests an answer to each, the replay answers with those and goes on. An interrupted replay cancels the task it was running and is never compared with; the next replay ends what an earlier one left unfinished. Run one replay at a time: a replay started while another runs cancels that one's unfinished shadows.

### Go projects

Download the modules in the setup command (`go mod download`): the setup phase reaches `storage.googleapis.com`, where the module proxy sends large modules, and the agent phase reads the modules from the project's cache. A task that needs a large module the setup did not fetch is refused with the denied-host line; `cavu project set NAME --egress storage.googleapis.com` allows that host for the project; `--egress` sets the whole list, so repeat it for every host the project already allows.

A task's first build starts from the build cache of the project's newest passed check. Go's test results travel with that cache, so a verify command that must rerun every test says `-count=1` (`go test -count=1 ./...`).

### Python projects

A `uv.lock` at the repository's root installs uv. Use `uv sync --frozen` as the setup command and `uv run` in the verify command (`uv run pytest -q`): every run then has its own `.venv`, which uv keeps out of git. uv runs on the Python that mise installs and never downloads its own.

Packages a setup command installs with `pip` are shared by the project's tasks and accumulate. A project's open runs share the interpreter's packages, so a bare `pip install` in the setup command changes them under every other open run of the project. A virtual environment in the workspace, in a directory `.gitignore` covers, gives per-run packages.

## Security

- The model credential depends on `endpoint_mode`. In `subscription` mode the subscription token is in the environment of the agent's `claude` process, in the agent's container. In `key` mode the agent's container holds only a stand-in for the key, which the proxy swaps for the key on the model route; neither the model key nor the GitHub token is in the agent container's processes (environment and command line), in its files or in its inspect output, and the GitHub token is handed to the container that clones or pushes on its standard input, never in its command line or inspect output.
- Who steers the agent by a comment: a follow-up run is given only the requests of a review that requests changes and has a text, and of a review thread that is not resolved, each by an account whose permission on the repository GitHub gives as `admin` or `write` (it counts the `maintain` role as `write`), or by a login among the project's review bots. No one else steers it by a comment: CAVU reads no comment outside reviews and review threads, counts a GitHub App's bot that is not among the review bots as no one, and passes on from a thread only those accounts' comments.
- The project's secrets never meet code the agent wrote for the task. The setup command, the one step that gets them, runs on the base commit in every run: a fix run is set up at the base commit, as a first run is, and only then moved to the pull request's head. A dependency the change added is therefore not installed when a fix run starts; the agent is told so, and installs again what its change added.
- When a session ends done, CAVU commits what it left uncommitted with git in the agent's container, with no hooks. A program the agent configured can still run then, in the agent's container and with its reach, and can change that commit, but it cannot make what a person reads differ from what is pushed: the diff, the files and flagged paths `task show` and the API give, the diff the reviewer reads, and the push all come from a bundle of the change's commit, which an import takes into the project's mirror only when it holds that commit, in a fresh container with no network and none of the run's volumes, where git runs under no configuration the agent wrote.
- A pull request CAVU opens holds, for anyone who can read it:
  - the branch's commits, under the `commit_name` and `commit_email` set in step 2;
  - its title, the agent's title for the change or else the task's, and the task's title and body as the person wrote them;
  - the agent's summary, where it says to look first, and the choices it made without asking;
  - the flagged paths: the workflows and agent instructions the change touches, and the files over 1 MiB it adds;
  - the verify command, or that the project waives it, a readiness turn or a review the project turns off, why a review gave no verdict, and the review's findings left open, the minor ones too;
  - the model, with its effort when one is set, the time from the task's start to the change, and the task's id.

  It names no cost. The comment that answers reviewers holds the task's id, the agent's summary and whether it changed the code, and no cost either. The cost is for the instance's owner, in `cavu task show` and the API.

## Licence

MIT. See [LICENSE](LICENSE).
