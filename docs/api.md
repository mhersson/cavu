# HTTP API

`cavu serve` serves this API on the `listen_address` setting (default `127.0.0.1:7117`), over plain HTTP. Every route is under `/api/`. Request and response bodies are JSON. A request body is read up to 1 MiB, and a field the route does not take is refused. Times are RFC 3339 in UTC. No response holds a secret's value.

## Authentication

Every route but `GET /api/health` and `POST /api/auth/login` needs one of these:

- **An API token**, sent as `Authorization: Bearer <token>`. `cavu token add NAME` creates one and prints it once: `cavu_` followed by 43 URL-safe characters.
- **The session cookie** `cavu_session`, which `POST /api/auth/login` sets. It is `HttpOnly`, `SameSite=Strict` and `Path=/`, and `Secure` when the login came over TLS or carries `X-Forwarded-Proto: https`. A session lasts 30 days after its last use: when a request moves its expiry, which happens at most once a day, the response sets the cookie again with the new expiry.

A request with the cookie whose method is not `GET` or `HEAD` must also carry the header `X-CAVU-CSRF` with any non-empty value. A page served from another port of the same host can make a browser send the cookie, but cannot add this header. A request with a bearer token needs no such header.

A request with a bearer token the server does not know is judged by its cookie.

## Errors

An error is a JSON object with one field, a line saying what went wrong:

```json
{"error": "wrong name or password"}
```

| Status | Meaning |
|--------|---------|
| 400 | The body is not the JSON the route takes (not JSON, the wrong shape, a field the route does not take, or more than one value), or what it holds is refused, such as a project field that holds what a project may not; such a line names the field. |
| 401 | The request has neither a known API token nor a live session cookie, or a login named the wrong name or password. |
| 403 | The request has the session cookie, is not `GET` or `HEAD`, and lacks `X-CAVU-CSRF`. |
| 404 | No route matches the method and the path, or the id in the path names nothing. |
| 409 | The write conflicts with what is stored: another project has the name. |
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

Changes the fields the body names and keeps the others. `switches` merge per key; every other field replaces its whole value. The name cannot change, since the project's secrets are stored under it. An edit of anything a baseline rests on clears the baseline: the repository, the base branch, the setup or verify command, the waiver, the tool versions, the environment or the secret variables. Answers 200 with the project.

### `POST /api/projects/{id}/check`

Asks for an environment check, the one `cavu project check` runs, which the server starts as soon as it can. Answers 202 with the project. `check.requested_at` stays set until a check that started after the request ends; a request made while a check runs is kept, and the check runs again.

### The project

```json
{"id": 1, "name": "web", "repo_url": "https://github.com/acme/web.git", "base_branch": "main",
 "setup_command": "npm ci", "verify_command": "npm test", "verify_waived": false,
 "tool_overrides": {"node": "22"}, "egress_hosts": ["registry.npmjs.org"], "env": {"CI": "1"},
 "secret_vars": ["NPM_TOKEN"],
 "switches": {"continuations": true, "verify_fix": true, "web_tools": false},
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
| `env` | Variables for the setup command. |
| `switches` | Switch name to `true` or `false`. |
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
 "secrets": {"endpoint_credential": true, "forge_token": false}}
```

A value stored as a JSON object is given as that object, and any other as a string. A secret's value is never given.

### `PATCH /api/settings`

Sets settings and instance secrets; either part may be left out:

```json
{"settings": {"model_work": "opus", "effort_work": null}, "secrets": {"forge_token": "..."}}
```

A setting's value is a string, null or a JSON object, which is stored as its text; null or `""` unsets the key. A secret is `endpoint_credential` or `forge_token`, sealed at rest; `""` removes it. Every value is checked before anything is written: a key that cannot be set, a value of another kind or one the server would not read, or another secret refuses the whole request with 400. Answers 200 with the settings as `GET` gives them.
