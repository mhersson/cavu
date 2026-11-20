# HTTP API

`cavu serve` serves this API on the `listen_address` setting (default `127.0.0.1:7117`), over plain HTTP: the REST routes under `/api/`, and the [MCP tools](#mcp) at `/mcp`. Under `/api/`, request and response bodies are JSON, but for the event stream's and the change route's file. A request body is read up to 1 MiB, and a field the route does not take is refused. Times are RFC 3339 in UTC. No response holds a secret's value.

On the same address `cavu serve` serves the web app, whose files are not routes of this API. A `GET` or `HEAD` of a path outside `/api/` and `/mcp` answers the app's file that the path names, else the app's page; a path under `/assets/` that names no file is 404. These answers need no authentication, and their `Content-Security-Policy` allows content and connections from the app's own origin only. A request with any other method goes to this API, whatever its path. The app signs in with `POST /api/auth/login` and works through the routes below, as any client may. A binary built without the web app answers with a page that says so in place of the app's, and serves this API as usual.

## Authentication

Every route under `/api/` but `GET /api/health` and `POST /api/auth/login` needs one of these:

- **An API token**, sent as `Authorization: Bearer <token>`. `cavu token add NAME` creates one and prints it once: `cavu_` followed by 43 URL-safe characters.
- **The session cookie** `cavu_session`, which `POST /api/auth/login` sets. It is `HttpOnly`, `SameSite=Strict` and `Path=/`, and `Secure` when the login came over TLS or carries `X-Forwarded-Proto: https`. A session lasts 30 days after its last use: when a request moves its expiry, which happens at most once a day, the response sets the cookie again with the new expiry.

A request with the cookie whose method is not `GET` or `HEAD` must also carry the header `X-CAVU-CSRF` with any non-empty value. A page served from another port of the same host can make a browser send the cookie, but cannot add this header. A request with a bearer token needs no such header.

Under `/api/`, a request with a bearer token the server does not know is judged by its cookie. `/mcp` takes only an API token: see [MCP](#mcp).

## Errors

An error under `/api/`, and a 401 at `/mcp`, is a JSON object with one field, a line saying what went wrong:

```json
{"error": "wrong name or password"}
```

| Status | Meaning |
|--------|---------|
| 400 | The body is not the JSON the route takes (not JSON, the wrong shape, a field the route does not take, or more than one value), or what it holds is refused, such as a project field that holds what a project may not, which the line names, or an action the task's state makes meaningless, or a project removal refused because the project has open tasks, or a credential removal refused because a project names it; or the query names a project or a status that does not exist; or a query value or header that must be a whole number, a boolean or one of the words its route names is not one, or is absent when the route requires it. |
| 401 | The request has neither a known API token nor a live session cookie, or, at `/mcp`, no known API token; or a login named the wrong name or password. |
| 403 | The request has the session cookie, is not `GET` or `HEAD`, and lacks `X-CAVU-CSRF`. |
| 404 | No route matches the method and the path, the id in the path names nothing, or the task the query names does not exist; or the task shows no change, or the change's file is gone. |
| 409 | The write conflicts with what is stored: another project has the name, or events kept being recorded on the task between an action's read and its write every time the server tried. Or the container runtime would not remove a project's volumes, with its error in the line. Nothing was written; send it again. |
| 413 | The body is larger than 1 MiB. |
| 429 | Too many failed logins; `Retry-After` gives the seconds to wait. |
| 500 | The server failed. It logs the cause; the answer holds no detail. |

## Routes

### `GET /api/health`

Needs no authentication. Answers 200:

```json
{"status": "ok", "version": "dev"}
```

### `POST /api/auth/login`

Needs no authentication. The body names the user that `cavu user set` created:

```json
{"name": "ada", "password": "correct horse battery"}
```

Answers 204 with the session cookie, or 401 for a wrong name or password. From the third failed login for a name from one address, logins for that name from that address are refused with 429 for a while: 1 second, doubling with each further failure, up to 5 minutes. A successful login clears the count, and so does a restart of the server. The address is the host of the connection; no forwarded header changes it.

### `POST /api/auth/logout`

Ends the session of the cookie the request carries and answers 204 with the cookie expired. A request with a bearer token and no cookie ends nothing.

### `GET /api/projects`

Answers 200 with every project that is not removed, ordered by name:

```json
{"projects": [{"id": 1, "name": "web", "...": "..."}]}
```

### `POST /api/projects`

Creates a project. The body holds `name`, `repo_url` and any of the other fields a request sets (see [The project](#the-project)); `base_branch` is `main` when left out. Answers 201 with the project, 400 when a field holds what a project may not, or 409 when another project has the name.

### `GET /api/projects/{id}`

Answers 200 with the project.

### `PATCH /api/projects/{id}`

Changes the fields the body names and keeps the others: a field left out or sent as null keeps its value, unlike a setting, which null unsets. `switches`, `caps` and `secrets` merge per key; every other field replaces its whole value, so `""`, `[]` or `{}` empties one that may be empty. The name cannot change, since the project's secrets are stored under it. An edit of anything a baseline rests on clears the baseline: the repository, the base branch, the setup or verify command, the waiver, the tool versions, the environment or the secret variables. Answers 200 with the project.

### `DELETE /api/projects/{id}`

Removes the project, as `cavu project remove` does: its cache and mirror volumes and its secrets. Its tasks, with their events and artifacts, stay, and its finished tasks still show its name. Answers 204 with no body. A removed project answers 404 on every project route, and its name can be used for a new project. Answers 400 while the project has a task that is neither done nor cancelled, with the line naming them, and 409 when the container runtime would not remove the volumes, with the runtime's error in the line; the project and its secrets stay, and removing it again finishes the job. There is no undo.

### `POST /api/projects/{id}/check`

Asks for an environment check, the one `cavu project check` runs, which the server starts as soon as it can. Answers 202 with the project. `check.requested_at` stays set until a check that started after the request ends; a request made while a check runs is kept, and the check runs again. `check.started_at` is set while the server's check of the project runs.

### The project

```json
{"id": 1, "name": "web", "repo_url": "https://github.com/acme/web.git", "base_branch": "main",
 "setup_command": "npm ci", "verify_command": "npm test", "verify_waived": false,
 "tool_overrides": {"node": "22"}, "egress_hosts": ["registry.npmjs.org"], "env": {"CI": "1"},
 "secret_vars": ["NPM_TOKEN"],
 "switches": {"continuations": true, "publish": true, "readiness": true, "review": true,
              "verify_fix": true, "web_tools": false},
 "caps": {"task_timeout_minutes": "60"},
 "no_ci": false, "ignored_checks": ["lint"], "review_bots": ["copilot[bot]"],
 "forge_credential": "",
 "runnable": true,
 "baseline": {"commit": "4b825dc", "passed": true, "at": "2030-01-02T03:04:05Z", "output": ""},
 "check": {"requested_at": null, "started_at": null, "tools": ["node"], "ignored_tools": [], "failed_step": "",
           "verify_suggestion": "npm test", "fault": "", "commit": "4b825dc", "output": ""}}
```

A list is `[]` and a map `{}` when empty, never null. These fields are set by a request, `name` only when the project is created:

| Field | Meaning |
|-------|---------|
| `name` | 1 to 40 of `a-z`, `0-9` and `-`. |
| `repo_url` | The repository's `https://` URL. |
| `base_branch` | The branch tasks start from. |
| `setup_command` | The command that prepares the workspace, such as installing dependencies; `""` for none. |
| `verify_command` | The command whose exit code decides whether a change passes. |
| `verify_waived` | `true` when a person checks every change instead. A project may not have both a verify command and a waiver, so a change from one to the other names both fields. |
| `tool_overrides` | Tool name to version, over the versions the repository names. |
| `egress_hosts` | The hosts the setup command, the agent and verify may reach. |
| `env` | Variables for the setup command, the agent's sessions and verify. |
| `switches` | Switch name to `true` or `false`. |
| `caps` | `task_timeout_minutes`, `task_max_turns` or `task_budget_usd` to a value that setting takes, for the project's own tasks over the instance's setting. An empty value removes the cap. |
| `no_ci` | `true` when the repository has no CI. |
| `ignored_checks` | The checks the project ignores, each by its name as GitHub shows it. |
| `review_bots` | The project's review bots, each by its login as GitHub shows it: a GitHub App's bot is written `name[bot]`, and a name without `[bot]` is a user's account. |
| `forge_credential` | The named GitHub credential the project's runs and calls use; `""` for the default (`forge_token`, or `forge_app_id` with `forge_app_key`). A name no credential has is refused. |
| `secrets` | Variable to value, for the setup command. Each value is sealed as the secret `project.<name>.<VAR>` and the variable added to `secret_vars`; an empty value removes both. It is never given back. |

The others are the server's:

| Field | Meaning |
|-------|---------|
| `id` | The project's id. |
| `secret_vars` | The variables whose values are sealed. |
| `switches` | Every switch and its state, the defaults included. |
| `runnable` | Whether the project's tasks may start: the baseline passed, or verify is waived. |
| `baseline` | The verify command's result on the base commit at the newest check that ran it, or null. |
| `check` | The newest check that ended: the tools it found and ignored, the step that failed, the verify command the repository suggests, the error it met (`fault`), the base commit and the output. `requested_at` is the time of a check asked for and not yet answered, or null. `started_at` is the time the server started the check of the project that runs now, or null: a check asked for and not yet started waits for a free slot and for any other project's check to end, since one check runs at a time. |

### `GET /api/settings`

Answers 200 with every key `cavu settings set` takes whose value is not empty, whether each instance secret is set, every named GitHub credential, the key a browser subscribes to notifications with, every settable key's default and every switch's default:

```json
{"settings": {"endpoint_mode": "subscription", "model_work": "sonnet"},
 "secrets": {"endpoint_credential": true, "forge_token": false, "forge_app_key": false},
 "forge_credentials": [{"name": "acme-org", "kind": "token"}, {"name": "work-app", "kind": "app", "app_id": 482913}],
 "push_key": "BCVxsr7N_eNgVRqvHtD0zTZsEc6-VV-JvLexhqUzORcxaOzi6-AYWXvTBHm4bjyPjs7Vd8pZGH6SRpkNtoIAiw4",
 "defaults": {"concurrency": "3", "verify_timeout_minutes": "10", "model_review": "", "...": "..."},
 "switches": {"continuations": true, "verify_fix": true, "web_tools": false, "readiness": true, "review": true, "publish": true}}
```

A value stored as a JSON object is given as that object, and any other as a string. A secret's value is never given. `forge_credentials` lists the named credentials by name, each with its `kind` (`token` or `app`) and, for an App, its `app_id` as a number; it is `[]` when there are none, and a credential's secret is never given or shown as set. `push_key` is the public half of the instance's VAPID key (RFC 8292), the uncompressed P-256 point in base64url, for a page to subscribe with as its `applicationServerKey`. CAVU makes the key once, when it opens a data directory that has none, and keeps it, sealed, as the secret `push.vapid_key`. `defaults` has every key `cavu settings set` takes, its default as text; a key with none is `""`, as is one that falls back on another key (`model_review` on `model_work`) or that is off when unset. `switches` has every switch a project has, at its default.

### `PATCH /api/settings`

Sets settings, instance secrets and named GitHub credentials; any part may be left out:

```json
{"settings": {"model_work": "opus", "effort_work": null}, "secrets": {"forge_token": "..."},
 "forge_credentials": {"acme-org": {"token": "..."}, "work-app": {"app_id": 482913, "app_key": "..."}, "old": null}}
```

A setting's value is a string, null or a JSON object, which is stored as its text; null or `""` unsets the key. A secret is `endpoint_credential`, `forge_token` or `forge_app_key`, sealed at rest; `""` removes it. A credential's name is 1 to 40 of `a-z`, `0-9` and `-`. An object adds the credential or replaces it whole, and its kind follows from its fields: `token` alone is a token, and `app_id` of 1 or more with `app_key` is an App. `null` removes it, which is refused when no credential has the name or when a project names it, the line naming the projects. Every value is checked before anything is written: a key that cannot be set, a value of another kind or one the server would not read, another secret, or a credential change that is refused (a name outside the rule, an object that is neither a token nor an App id with its key, a removal refused) refuses the whole request with 400. Answers 200 with the settings as `GET` gives them.

### `GET /api/tasks`

Answers 200 with every task but a replay's shadow tasks, oldest first:

```json
{"tasks": [{"id": 7, "project": "web", "title": "Add a footer", "status": "blocked", "needs_you": true, "needs": "answer",
            "needs_you_since": "2030-01-02T04:00:00Z", "stop_reason": "question",
            "waiting": "", "waiting_until": null, "pull_request_ready": false, "created_at": "2030-01-02T03:04:05Z"}]}
```

The query `project=NAME` keeps the tasks of one project, `status=STATUS` the tasks with one status, or with any of several separated by commas (`status=draft,queued`), and `needs_you=true` the tasks that need a person. `order=newest` lists the newest first; `order=oldest` is the default. `limit=N` keeps the first N of the tasks the other queries keep, so `limit=20&needs_you=true` is 20 tasks that need a person; 0 or absent keeps every one. There is no cursor: a longer list is a larger limit. A project that does not exist, a status that is not one of [the task's](#the-task), a `needs_you` that is not a boolean, an `order` that is neither `oldest` nor `newest`, or a `limit` that is not a whole number is 400. `stop_reason` is the open stop's reason, else `""`; `pull_request_ready` is the `ready` of the task's `pull_request`, false for a task with none; `needs`, `needs_you_since`, `waiting` and `waiting_until` are the task's.

### `POST /api/tasks`

Creates a draft task, with the requester `api`:

```json
{"project": "web", "title": "Add a footer", "body": "Below the fold, on every page.", "depends_on": [3], "start": true}
```

`title` is required: at most 200 characters, with no line break or other control character. `depends_on` names the tasks that must be done before this one runs. With `start` set to `true` the task is started as well, which its project must allow (the project's `runnable`); when it does not, nothing is created. Answers 201 with the task, or 400 when the project does not exist, a field holds what a task may not, a dependency does not exist, or the project is not runnable for a start.

### `GET /api/tasks/{id}`

Answers 200 with the task.

### `PATCH /api/tasks/{id}`

Changes the `title`, `body` or `depends_on` the body names and keeps the others: a field left out or sent as null keeps its value, unlike a setting, which null unsets. `""` empties the body; `depends_on` replaces the whole list, and `[]` removes every dependency. A task can be edited until its first run starts, unless it is cancelled. An edit after that or of a cancelled task is 400, and so is a title or dependency that `POST` refuses, or a dependency that is the task itself or leads back to it. Answers 200 with the task.

### `POST /api/tasks/{id}/actions`

Records a person's action on the task:

```json
{"action": "answer", "text": "Link the privacy page and the contact page."}
```

| Action | What it does | Refused when |
|--------|--------------|--------------|
| `start` | Starts a draft task; a task already started is left as it is. | The task is cancelled, or its project is not runnable. |
| `answer` | Answers the question that stopped the task with `text`. | The task has no open question, or `text` is blank. |
| `retry` | Clears the open stop, so the task carries on. | The task is cancelled, or has no open stop. |
| `cancel` | Cancels the task. | The task is cancelled or done. |
| `done` | Records that the task's change was merged. | The task is not `in_review`, or has a pull request. |

Only `answer` reads `text`. Answers 200 with the task, 400 for an unknown action or one that is refused, or 409 when events kept being recorded on the task, such as the server's own progress, between the action's read and its write every time the server tried; nothing is recorded then, and the request may be sent again.

### The task

```json
{"id": 7, "project": "web", "title": "Add a footer", "body": "Below the fold, on every page.", "requester": "api",
 "created_at": "2030-01-02T03:04:05Z", "status": "blocked", "needs_you": true, "needs": "answer", "needs_you_since": "2030-01-02T04:00:00Z",
 "waiting": "", "waiting_until": null,
 "dependencies": [{"id": 3, "status": "done"}],
 "stop": {"reason": "question", "explanation": "Which pages should the footer link to?", "action": "answer", "resets_at": null},
 "questions": [{"question": "Which pages should the footer link to?", "suggested_answer": ""},
               {"question": "Should it show the year?", "suggested_answer": ""}],
 "review": {"rounds": 1, "failed": "",
            "open": [{"severity": "important", "where": "footer.html:10", "what": "The footer covers the cookie banner.",
                      "why": "A visitor cannot dismiss the banner."}],
            "minor": []},
 "pull_request": null, "change": null, "turns": 33, "duration_ms": 226000, "cost_estimate_usd": 0.325, "cost_missing_sessions": 0,
 "usage": [{"source": "proxy", "model": "claude-haiku-5-5", "input": 21000, "output": 18000, "cache_read": 160000,
            "cache_write": 9000, "cost_estimate_usd": 0.325}],
 "budget": {"turns": 33, "max_turns": 1000, "minutes": 4, "max_minutes": 300, "usd": 0.29, "max_usd": 5, "day_usd": 1.75, "max_day_usd": 20}}
```

A list is `[]` when empty, never null.

| Field | Meaning |
|-------|---------|
| `id` | The task's id. |
| `project` | The name of the task's project. |
| `title`, `body` | What the task asks for. |
| `requester` | What created the task: `cli` for a `cavu task add`, `api` for `POST /api/tasks`, `mcp` for the MCP tool `create_task`. |
| `created_at` | When the task was created. |
| `status` | Where the task stands: `cancelled` once cancelled, started or not; else `draft` until started; `done` once its change is merged; `blocked` while a stop is open; `in_review` once its change is exported, or while it has a pull request and no run is open; `queued` while no run is open; else `running`. |
| `needs_you` | Whether the task waits for a person: it is `blocked` on a stop that only a person clears, which is any stop but a usage limit, which resets by itself, and a `sandbox` stop or a failed model endpoint while CAVU still retries it; its `pull_request` is `ready`; or it is `in_review` with an exported change and no pull request, as with the switch `publish` off, once no run is open (while the exported run is still open, before its publish or finish, it does not), which waits for a person to take the change and record `done`. |
| `needs` | In what way the task needs a person: `answer` or `retry`, the `action` of its stop; `review`, when its `pull_request` is `ready`; `take`, when its change was exported with no pull request; else `""`. It is `""` exactly when `needs_you` is false. |
| `needs_you_since` | When the task came to need a person, as a UTC time, null when `needs` is `""`: the time of the stop for `answer` and `retry`; the end of the newest run for `take`; for `review` the later of the newest look at the pull request and the end of the newest run. |
| `waiting` | Why a task that would act does not, such as a dependency that is not done, a usage limit that has not reset, another task's stop on the model endpoint, or every slot taken; for a task whose pull request CAVU watches, what the watch waits for, or the error of the last look at the pull request and when the next look comes; else `""`. |
| `waiting_until` | When the wait in `waiting` ends on a clock: the reset of the usage limit of the task, or of another task that holds it, the next try after a fault, local midnight under a spent daily cap, or the end of the time the pull request's checks are given to start or to finish; else null. |
| `dependencies` | The tasks that must be done before this one runs, each with its `id` and its `status`. `POST` and `PATCH` take their ids as `depends_on`. |
| `stop` | The open stop, or null: its `reason` (`question`, `environment`, `verify_failed`, `budget`, `timeout`, `no_change`, `agent_error`, `usage_limit`, `endpoint`, `sandbox`, `cancelled`, `dependency`, `push_rejected`, `ci_failed`, `ci_timeout`, `no_checks`, `conflict`, `taken_over` or `changes_requested`), a one-line `explanation`, the one `action` that clears it (`answer`, `retry`, or `none` for a stop that a clock ends or that stays), and `resets_at`, the time a usage limit resets, else null. |
| `questions` | While the open stop is `question`, every question the agent asked, of which the explanation holds the first, each with the `suggested_answer` the agent would assume, `""` where it gave none; else `[]`. |
| `review` | The newest run's review, or null until a review gives a verdict or fails to give one: `rounds`, the fix rounds after its findings since a person last acted; `failed`, why the newest review gave no verdict, else `""`; `open`, the critical and important findings left open; and `minor`, the minor ones, which never start a fix round. A finding has a `severity` (`critical`, `important` or `minor`), `where`, `what` and `why`. A review that a usage limit, a timeout or the like cut short shows as the stop instead. |
| `pull_request` | The task's pull request on GitHub, or null before the first publish: its `number`, `url`, `branch` and `head`, the commit CAVU has on the branch; `state`, `open`, `merged` or `closed` as CAVU's newest look at the pull request shows it, `open` before any look; `checks`, `none`, `pending`, `passed` or `failed`, the checks of `head`, where `""` means they were not read yet, as between a publish and the first look at it or when the newest look shows another commit; `failed_checks`, the names of the failed ones, `[]` for none; and `ready`, true only when the newest look shows `head`, the pull request is open and no draft, no request of a reviewer is waiting for a follow-up run, its checks are `passed` (or `none`, on a project with `no_ci`), and the task waits for a person: no run or stop is open, and it is neither done nor cancelled. |
| `change` | The change the task shows, or null: the export CAVU published last whenever the task has a pull request, in the middle of a fix run too, else the newest exported change. After a retry that gave the branch back to CAVU (a takeover, or a conflict a person resolved) the pull request's `head` is the person's commit and `change` is still CAVU's last export, so the two differ until CAVU publishes again. Its fields: its `head_commit`, the number of `files_changed`, `diff_stat`, git's summary of the diff: a line per file, a long list cut short, then the totals, and `flagged`, the paths a person should know of before a merge, at most 20 in each list and `[]` for none: `workflows`, a path under `.github/workflows/`; `instructions`, a file named `AGENTS.md` or `CLAUDE.md`, or a path under a `.claude/` directory, in any directory; and `large`, a file the change adds that is over 1 MiB. The first two name a path the change deletes too, and a rename counts as deleting the old path and adding the new one. |
| `turns` | The turns of all the task's agent sessions. |
| `duration_ms` | The milliseconds from the task's start to its newest event other than the agent's progress, 0 before it starts. |
| `cost_estimate_usd` | The sum of the costs recorded for the task, an estimate from list prices, not a bill. |
| `cost_missing_sessions` | How many of the task's sessions and reviews ended without a cost, which `cost_estimate_usd` leaves out. |
| `usage` | The tokens of the whole task, a row per `source` and `model`: `input`, `output`, `cache_read` and `cache_write`, and the row's `cost_estimate_usd`. The source is `proxy` for the proxy's count of every model call in key mode, else `agent` for the agent's own totals. |
| `budget` | What the task used since a person last started, answered, retried or cancelled it, of what it may: `turns` of `max_turns`, and `minutes` of `max_minutes`, the time its sessions and reviews ran, in whole minutes. While a money cap on the task is in force, which is only in key mode, `usd` of `max_usd`, its spend at `model_prices`; else both are left out. Likewise while the daily cap is in force, `day_usd` of `max_day_usd`, what every task spent in the server's local day. A task the daily cap holds says so in `waiting`. |

### `GET /api/tasks/{id}/events`

Answers 200 with a page of the task's events, oldest first, or newest first with the query `order=newest` (`order=oldest` is the default):

```json
{"events": [{"id": 42, "task_id": 7, "run_id": 3, "...": "..."}]}
```

The query `after=ID` starts the page after the event with that id, or under `order=newest` below it, and `limit=N` sets how many events the page holds at most: 200 when it is left out or 0, and never more than 1000. `agent=false` leaves out the events whose `actor` is `agent`, the agent's progress (`agent_text`, `agent_tool` and `rate_limit`); `agent=true` is the default. A client pages in either order by sending the id of the last event it got as `after`, until a page comes back empty. An `after` or `limit` that is not a whole number, an `order` that is neither `oldest` nor `newest`, or an `agent` that is not a boolean is 400.

### `GET /api/tasks/{id}/change`

Answers 200 with the task's exported change as a file, the one `change` shows (for a task with a pull request, the export CAVU published last). The query `format` is required: `diff` answers the diff (`git diff --binary`) as `text/x-diff` with `Content-Disposition: attachment; filename="task-ID.diff"`, and `bundle` answers the git bundle as `application/octet-stream` with `filename="task-ID.bundle"`. The file is streamed, never read whole, and the route honours `Range` and `HEAD`. A GET needs no CSRF header, so a link carrying the session's cookie downloads it. A `format` that is absent or neither is 400; a task that does not exist, or shows no change, or whose file is gone, is 404.

### `GET /api/events`

Answers 200 with a stream of [server-sent events](https://html.spec.whatwg.org/multipage/server-sent-events.html) (`Content-Type: text/event-stream`) that carries the events of every task as they are recorded, or those of one task with the query `task=ID`. Each event is one message: `id:` the event's id, `data:` the event as one line of JSON, then a blank line.

```
id: 42
data: {"id":42,"task_id":7,"run_id":3,"at":"2030-01-02T03:04:05Z","type":"agent_text","actor":"agent","payload":{"text":"Adding the footer."},"text":"Adding the footer."}

```

The stream starts after the event named by the header `Last-Event-ID`, else by the query `after=ID`, else after the newest event when the request arrives. It opens with a message that holds only that id (`id: 41` and a blank line) and carries no event. A client that reconnects with the id it got last, as a browser's `EventSource` does, misses no event and gets none twice, even when it drops before its first event. The server looks for new events once a second and sends them in id order, and sends a comment line, `:`, every 15 seconds. The stream's answer carries `Cache-Control: no-store` and `X-Accel-Buffering: no`, which keeps a proxy such as nginx from holding the events back. The stream ends when the client leaves or the server stops. With the query `device=ID`, the id [`PUT /api/push`](#put-apipush) answered, the device counts as looked at while the stream is open and its page last said it has focus. A `task` that does not exist is 404; a `task`, a `device`, or the `Last-Event-ID` or `after` the stream starts after, that is not a whole number is 400.

### The event

An event is one fact in a task's history, recorded once and never changed:

```json
{"id": 42, "task_id": 7, "run_id": 3, "at": "2030-01-02T03:04:05Z", "type": "agent_text", "actor": "agent",
 "payload": {"text": "Adding the footer."}, "text": "Adding the footer."}
```

| Field | Meaning |
|-------|---------|
| `id` | The event's id. Ids rise with every event recorded, over all tasks. |
| `task_id` | The id of the event's task. |
| `run_id` | The id of the run the event belongs to, or null. |
| `at` | When the event was recorded, to the millisecond. |
| `type` | What happened, such as `task_started`, `agent_text` or `stopped`. |
| `actor` | Who recorded it: `user`, `engine`, `runner` or `agent`. |
| `payload` | The event's details, a JSON value whose shape the type sets. |
| `text` | One sentence that tells the event, for a person; the sentence `cavu task run` prints after the clock and the type. |

Of the payloads, a client may rely on these fields and no others:

| Type | Payload fields |
|------|----------------|
| `verify_ended` | `result` (`passed`, `failed`, `timeout` or `waived`), `command`, `exit_code`, `output`, what the command printed, its middle cut out when it is long, and `duration_ms`. |
| `prepare_failed` | `step`, the step that failed (`clone`, `toolchain`, `setup` or `baseline`), and `output`. |
| `session_ended` | `summary`, the agent's summary of the session, `""` when it gave none; `assumptions`, the choices it made without asking, a list that may be null; and `where_to_look`, the parts of the change a reviewer should read first, left out when it named none. |

A task's pull request leaves these events: `publish_started` and `published` for a push of the branch and the pull request opened or updated by it, `pr_observed` for what GitHub showed of the pull request: CAVU's first look since its push, its ready-marking or a person's action, each later look that differs from the last record, and the look before a push that finds the pull request merged or closed or its branch moved by a person, and `ready_started` and `pr_ready` for CAVU marking it ready for review. After a follow-up run for reviewers' requests, `reply_started` and `replied` record CAVU's one comment on the pull request that answers them. `home_removed` records that the task's home volume was removed once the task could run no more: it is done or cancelled, or its change was exported with no pull request to follow, as with the switch `publish` off.

### `PUT /api/push`

Registers a browser's push subscription, or updates the one with the same endpoint, and records whether the browser's CAVU page has focus. The body holds the subscription as the browser gives it, the origin of the page, and the focus:

```json
{"endpoint": "https://push.example.org/send/...", "keys": {"p256dh": "...", "auth": "..."},
 "origin": "https://cavu.example.org", "focused": true}
```

Answers 200 with the id of the device, the same for every call with that endpoint while the device stays registered; an id is never given to another device:

```json
{"id": 3}
```

A later call with the same endpoint replaces the keys and the origin. An `endpoint` that is not an `https` URL with a host, a `p256dh` that is not a P-256 public key, or an `auth` that is not 16 octets, each in base64url, is 400, and the line does not quote the endpoint.

While `cavu serve` runs, it looks every 2 seconds for the tasks that need a person and sends each new need, a task's `needs` with its `needs_you_since`, once, to every registered device but those being looked at: a device whose page last said it has focus while an event stream opened with its id is open ([`GET /api/events`](#get-apievents)). A push is encrypted for the subscription (RFC 8291) and signed with the instance's VAPID key, whose token names the `origin` as its subject; it holds `{"title": "Needs an answer · web", "body": "Add a footer", "url": "/tasks/7", "tag": "task-7"}`, at most 2 KiB with the body cut to fit, the title being `Needs an answer`, `Ready for review`, `Stopped` or `A change to take` for `answer`, `review`, `retry` and `take`, then the project, and the body the task's title. A device whose push service answers 404 or 410 is removed, and a push that fails otherwise is logged and not sent again. A need found while no device is registered is recorded and never sent. Only `cavu serve` sends notifications: a task that `cavu task run` drives while no server runs sends none.

### `DELETE /api/push/{id}`

Removes the device and answers 204 with no body; an id that names no device is 404.

## MCP

`POST /mcp` serves eight tools to an MCP client over the streamable HTTP transport. It takes only an API token, as `Authorization: Bearer <token>`: a request without a token the server knows is 401 with the JSON error above, whatever cookie it carries, and no request there needs `X-CAVU-CSRF`. The server is stateless: it keeps no session between requests and sets no `Mcp-Session-Id`. It advertises the tools and nothing else: no `listChanged`, no logging, no resources or prompts, so a client has no reason to open a stream and the server holds none. It answers each call with one JSON message, and any method but `POST` with 405.

A tool that does what a REST route does calls that route's code, so it refuses what the route refuses, and every tool gives a task as [the task](#the-task) shows it:

| Tool | Input | What it does |
|------|-------|--------------|
| `create_task` | `project`, `title`, and optionally `body`, `depends_on` and `start` | Creates a task as `POST /api/tasks` does, with the requester `mcp`, and answers with the task. |
| `get_task` | `id` | Answers with the task. |
| `list_tasks` | optionally `project`, `status` and `needs_you` | Answers with the list `GET /api/tasks` gives for that query. |
| `edit_task` | `id`, and any of `title`, `body` and `depends_on` | Changes the fields it is given as `PATCH /api/tasks/{id}` does, with the same refusals, and answers with the task. |
| `run_task` | `id` | Records the action `start` for a task with no open stop, else the action `retry`, as `POST /api/tasks/{id}/actions` does, and answers with the task. A task stopped on a question is refused with a line saying it needs an answer, which the tool `answer_task`, the action `answer` or `cavu task answer ID` gives. |
| `cancel_task` | `id` | Records the action `cancel` as `POST /api/tasks/{id}/actions` does, and answers with the task. |
| `answer_task` | `id` and `answer` | Records the action `answer` with that text for a task whose open stop is a question, as `POST /api/tasks/{id}/actions` does, and answers with the task. Any other task is refused with a line naming the stop it has, or that it has none. |
| `list_projects` | none | Answers with each project's name, `repo_url` and `base_branch` and whether it is [runnable](#the-project): `{"projects": [{"name": "web", "repo_url": "https://example.com/web.git", "base_branch": "main", "runnable": true}]}`. |

A tool's answer is its structured content, and the same JSON as its text. A call the server refuses, for what it holds, for the task's state, or for a task or project that does not exist, is a tool result marked `isError` whose text is one line saying why, the line a REST route answers for the same refusal, except the refusal `answer_task` gives, which its row describes; a failure of the server's own is `internal error`. An argument the tool does not take, of the wrong type, or missing, is refused before the tool runs. A request the transport cannot take is refused with an HTTP status and a plain-text line, such as 413 for a body over 1 MiB or 415 for a `Content-Type` other than `application/json`, and a call of a tool that does not exist is a JSON-RPC error. A refused call writes nothing.
