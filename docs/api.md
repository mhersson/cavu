# HTTP API

`cavu serve` serves this API on the `listen_address` setting (default `127.0.0.1:7117`), over plain HTTP: the REST routes under `/api/`, and the [MCP tools](#mcp) at `/mcp`. Under `/api/`, request and response bodies are JSON, but for the event stream's. A request body is read up to 1 MiB, and a field the route does not take is refused. Times are RFC 3339 in UTC. No response holds a secret's value.

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
| 400 | The body is not the JSON the route takes (not JSON, the wrong shape, a field the route does not take, or more than one value), or what it holds is refused, such as a project field that holds what a project may not, which the line names, or an action the task's state makes meaningless; or the query names a project or a status that does not exist; or a query value or header that must be a whole number is not one. |
| 401 | The request has neither a known API token nor a live session cookie, or, at `/mcp`, no known API token; or a login named the wrong name or password. |
| 403 | The request has the session cookie, is not `GET` or `HEAD`, and lacks `X-CAVU-CSRF`. |
| 404 | No route matches the method and the path, the id in the path names nothing, or the task the query names does not exist. |
| 409 | The write conflicts with what is stored: another project has the name, or an event was recorded on the task between an action's read and its write. Nothing was written; send it again. |
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

Answers 200 with every project, ordered by name:

```json
{"projects": [{"id": 1, "name": "web", "...": "..."}]}
```

### `POST /api/projects`

Creates a project. The body holds `name`, `repo_url` and any of the other fields a request sets (see [The project](#the-project)); `base_branch` is `main` when left out. Answers 201 with the project, 400 when a field holds what a project may not, or 409 when another project has the name.

### `GET /api/projects/{id}`

Answers 200 with the project.

### `PATCH /api/projects/{id}`

Changes the fields the body names and keeps the others: a field left out or sent as null keeps its value, unlike a setting, which null unsets. `switches`, `caps` and `secrets` merge per key; every other field replaces its whole value, so `""`, `[]` or `{}` empties one that may be empty. The name cannot change, since the project's secrets are stored under it. An edit of anything a baseline rests on clears the baseline: the repository, the base branch, the setup or verify command, the waiver, the tool versions, the environment or the secret variables. Answers 200 with the project.

### `POST /api/projects/{id}/check`

Asks for an environment check, the one `cavu project check` runs, which the server starts as soon as it can. Answers 202 with the project. `check.requested_at` stays set until a check that started after the request ends; a request made while a check runs is kept, and the check runs again.

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
 "runnable": true,
 "baseline": {"commit": "4b825dc", "passed": true, "at": "2030-01-02T03:04:05Z", "output": ""},
 "check": {"requested_at": null, "tools": ["node"], "ignored_tools": [], "failed_step": "",
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
| `secrets` | Variable to value, for the setup command. Each value is sealed as the secret `project.<name>.<VAR>` and the variable added to `secret_vars`; an empty value removes both. It is never given back. |

The others are the server's:

| Field | Meaning |
|-------|---------|
| `id` | The project's id. |
| `secret_vars` | The variables whose values are sealed. |
| `switches` | Every switch and its state, the defaults included. |
| `runnable` | Whether the project's tasks may start: the baseline passed, or verify is waived. |
| `baseline` | The verify command's result on the base commit at the newest check that ran it, or null. |
| `check` | The newest check that ended: the tools it found and ignored, the step that failed, the verify command the repository suggests, the error it met (`fault`), the base commit and the output. `requested_at` is the time of a check asked for and not yet answered, or null. |

### `GET /api/settings`

Answers 200 with every key `cavu settings set` takes whose value is not empty, and whether each instance secret is set:

```json
{"settings": {"endpoint_mode": "subscription", "model_work": "sonnet"},
 "secrets": {"endpoint_credential": true, "forge_token": false, "forge_app_key": false}}
```

A value stored as a JSON object is given as that object, and any other as a string. A secret's value is never given.

### `PATCH /api/settings`

Sets settings and instance secrets; either part may be left out:

```json
{"settings": {"model_work": "opus", "effort_work": null}, "secrets": {"forge_token": "..."}}
```

A setting's value is a string, null or a JSON object, which is stored as its text; null or `""` unsets the key. A secret is `endpoint_credential`, `forge_token` or `forge_app_key`, sealed at rest; `""` removes it. Every value is checked before anything is written: a key that cannot be set, a value of another kind or one the server would not read, or another secret refuses the whole request with 400. Answers 200 with the settings as `GET` gives them.

### `GET /api/tasks`

Answers 200 with every task but a replay's shadow tasks, oldest first:

```json
{"tasks": [{"id": 7, "project": "web", "title": "Add a footer", "status": "blocked", "stop_reason": "question",
            "waiting": "", "created_at": "2030-01-02T03:04:05Z"}]}
```

The query `project=NAME` keeps the tasks of one project, and `status=STATUS` the tasks with one status; a project that does not exist, or a status that is not one of [the task's](#the-task), is 400. `stop_reason` is the open stop's reason, else `""`; the other fields are the task's.

### `POST /api/tasks`

Creates a draft task, with the requester `api`:

```json
{"project": "web", "title": "Add a footer", "body": "Below the fold, on every page.", "depends_on": [3], "start": true}
```

`title` is required: at most 200 characters, with no line break or other control character. `depends_on` names the tasks that must be done before this one runs. With `start` set to `true` the task is started as well, which its project must allow (the project's `runnable`); when it does not, nothing is created. Answers 201 with the task, or 400 when the project does not exist, a field holds what a task may not, a dependency does not exist, or the project is not runnable for a start.

### `GET /api/tasks/{id}`

Answers 200 with the task.

### `PATCH /api/tasks/{id}`

Changes the `title`, `body` or `depends_on` the body names and keeps the others: a field left out or sent as null keeps its value, unlike a setting, which null unsets. `""` empties the body; `depends_on` replaces the whole list, and `[]` removes every dependency. A task can be edited until its first run starts. An edit after that is 400, and so is a title or dependency that `POST` refuses, or a dependency that is the task itself or leads back to it. Answers 200 with the task.

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

Only `answer` reads `text`. Answers 200 with the task, 400 for an unknown action or one that is refused, or 409 when an event was recorded on the task between the action's read and its write, such as the server's own next step; nothing is recorded then, and the request may be sent again.

### The task

```json
{"id": 7, "project": "web", "title": "Add a footer", "body": "Below the fold, on every page.", "requester": "api",
 "created_at": "2030-01-02T03:04:05Z", "status": "blocked", "waiting": "", "depends_on": [3],
 "stop": {"reason": "question", "explanation": "Which pages should the footer link to?", "action": "answer", "resets_at": null},
 "questions": [{"question": "Which pages should the footer link to?", "suggested_answer": ""},
               {"question": "Should it show the year?", "suggested_answer": ""}],
 "review": {"rounds": 1, "failed": "",
            "open": [{"severity": "important", "where": "footer.html:10", "what": "The footer covers the cookie banner.",
                      "why": "A visitor cannot dismiss the banner."}],
            "minor": []},
 "change": null, "turns": 33, "duration_ms": 226000, "cost_estimate_usd": 0.325, "cost_missing_sessions": 0,
 "budget": {"turns": 33, "max_turns": 1000, "minutes": 4, "max_minutes": 300, "usd": 0.29, "max_usd": 5}}
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
| `waiting` | Why a task that would act does not, such as a dependency that is not done, a usage limit that has not reset, another task's stop on the model endpoint, or every slot taken; for a task whose pull request CAVU watches, what the watch waits for, or the error of the last look at the pull request and when the next look comes; else `""`. |
| `depends_on` | The ids of the tasks that must be done before this one runs. |
| `stop` | The open stop, or null: its `reason` (`question`, `environment`, `verify_failed`, `budget`, `timeout`, `no_change`, `agent_error`, `usage_limit`, `endpoint`, `sandbox`, `cancelled`, `dependency`, `push_rejected`, `ci_failed`, `ci_timeout`, `no_checks`, `conflict`, `taken_over` or `changes_requested`), a one-line `explanation`, the one `action` that clears it (`answer`, `retry`, or `none` for a stop that a clock ends or that stays), and `resets_at`, the time a usage limit resets, else null. |
| `questions` | While the open stop is `question`, every question the agent asked, of which the explanation holds the first, each with the `suggested_answer` the agent would assume, `""` where it gave none; else `[]`. |
| `review` | The newest run's review, or null until a review gives a verdict or fails to give one: `rounds`, the fix rounds after its findings since a person last acted; `failed`, why the newest review gave no verdict, else `""`; `open`, the critical and important findings left open; and `minor`, the minor ones, which never start a fix round. A finding has a `severity` (`critical`, `important` or `minor`), `where`, `what` and `why`. A review that a usage limit, a timeout or the like cut short shows as the stop instead. |
| `change` | The newest exported change, or null: its `head_commit`, the number of `files_changed`, `diff_stat`, git's summary of the diff: a line per file, a long list cut short, then the totals, and `flagged`, the paths a person should know of before a merge, at most 20 in each list and `[]` for none: `workflows`, a path under `.github/workflows/`; `instructions`, a file named `AGENTS.md` or `CLAUDE.md`, or a path under a `.claude/` directory, in any directory; and `large`, a file the change adds that is over 1 MiB. The first two name a path the change deletes too, and a rename counts as deleting the old path and adding the new one. |
| `turns` | The turns of all the task's agent sessions. |
| `duration_ms` | The milliseconds from the task's start to its newest event other than the agent's progress, 0 before it starts. |
| `cost_estimate_usd` | The sum of the costs recorded for the task, an estimate from list prices, not a bill. |
| `cost_missing_sessions` | How many of the task's sessions and reviews ended without a cost, which `cost_estimate_usd` leaves out. |
| `budget` | What the task used since a person last started, answered, retried or cancelled it, of what it may: `turns` of `max_turns`, and `minutes` of `max_minutes`, the time its sessions and reviews ran, in whole minutes. While a money cap on the task is in force, which is only in key mode, `usd` of `max_usd`, its spend at `model_prices`; else both are left out. A task the daily cap holds says so in `waiting`. |

### `GET /api/tasks/{id}/events`

Answers 200 with a page of the task's events, oldest first:

```json
{"events": [{"id": 42, "task_id": 7, "run_id": 3, "...": "..."}]}
```

The query `after=ID` starts the page after the event with that id, and `limit=N` sets how many events the page holds at most: 200 when it is left out or 0, and never more than 1000. A client pages by sending the id of the last event it got as `after`, until a page comes back empty. An `after` or `limit` that is not a whole number is 400.

### `GET /api/events`

Answers 200 with a stream of [server-sent events](https://html.spec.whatwg.org/multipage/server-sent-events.html) (`Content-Type: text/event-stream`) that carries the events of every task as they are recorded, or those of one task with the query `task=ID`. Each event is one message: `id:` the event's id, `data:` the event as one line of JSON, then a blank line.

```
id: 42
data: {"id":42,"task_id":7,"run_id":3,"at":"2030-01-02T03:04:05Z","type":"agent_text","actor":"agent","payload":{"text":"Adding the footer."}}

```

The stream starts after the event named by the header `Last-Event-ID`, else by the query `after=ID`, else after the newest event when the request arrives. It opens with a message that holds only that id (`id: 41` and a blank line) and carries no event. A client that reconnects with the id it got last, as a browser's `EventSource` does, misses no event and gets none twice, even when it drops before its first event. The server looks for new events once a second and sends them in id order, and sends a comment line, `:`, every 15 seconds. The stream's answer carries `Cache-Control: no-store` and `X-Accel-Buffering: no`, which keeps a proxy such as nginx from holding the events back. The stream ends when the client leaves or the server stops. A `task` that does not exist is 404; a `task`, or the `Last-Event-ID` or `after` the stream starts after, that is not a whole number is 400.

### The event

An event is one fact in a task's history, recorded once and never changed:

```json
{"id": 42, "task_id": 7, "run_id": 3, "at": "2030-01-02T03:04:05Z", "type": "agent_text", "actor": "agent",
 "payload": {"text": "Adding the footer."}}
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

## MCP

`POST /mcp` serves six tools to an MCP client over the streamable HTTP transport. It takes only an API token, as `Authorization: Bearer <token>`: a request without a token the server knows is 401 with the JSON error above, whatever cookie it carries, and no request there needs `X-CAVU-CSRF`. The server is stateless: it keeps no session between requests and sets no `Mcp-Session-Id`. It advertises the tools and nothing else: no `listChanged`, no logging, no resources or prompts, so a client has no reason to open a stream and the server holds none. It answers each call with one JSON message, and any method but `POST` with 405.

A tool that does what a REST route does calls that route's code, so it refuses what the route refuses, and every tool gives a task as [the task](#the-task) shows it:

| Tool | Input | What it does |
|------|-------|--------------|
| `create_task` | `project`, `title`, and optionally `body`, `depends_on` and `start` | Creates a task as `POST /api/tasks` does, with the requester `mcp`, and answers with the task. |
| `get_task` | `id` | Answers with the task. |
| `list_tasks` | optionally `project` and `status` | Answers with the list `GET /api/tasks` gives for that query. |
| `run_task` | `id` | Records the action `start` for a task with no open stop, else the action `retry`, as `POST /api/tasks/{id}/actions` does, and answers with the task. A task stopped on a question is refused with a line saying it needs an answer, which the action `answer` or `cavu task answer ID` gives. |
| `cancel_task` | `id` | Records the action `cancel` as `POST /api/tasks/{id}/actions` does, and answers with the task. |
| `list_projects` | none | Answers with each project's name and whether it is [runnable](#the-project): `{"projects": [{"name": "web", "runnable": true}]}`. |

A tool's answer is its structured content, and the same JSON as its text. A call the server refuses, for what it holds, for the task's state, or for a task or project that does not exist, is a tool result marked `isError` whose text is one line saying why, the line a REST route answers for the same refusal; a failure of the server's own is `internal error`. An argument the tool does not take, of the wrong type, or missing, is refused before the tool runs. A request the transport cannot take is refused with an HTTP status and a plain-text line, such as 413 for a body over 1 MiB or 415 for a `Content-Type` other than `application/json`, and a call of a tool that does not exist is a JSON-RPC error. A refused call writes nothing.
