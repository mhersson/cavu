# HTTP API

`cavu serve` serves this API on the `listen_address` setting (default `127.0.0.1:7117`), over plain HTTP. Every route is under `/api/`. Request and response bodies are JSON. A request body is read up to 1 MiB, and a field the route does not take is refused.

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
| 400 | The body is not the JSON the route takes: not JSON, the wrong shape, a field the route does not take, or more than one value. |
| 401 | The request has neither a known API token nor a live session cookie, or a login named the wrong name or password. |
| 403 | The request has the session cookie, is not `GET` or `HEAD`, and lacks `X-CAVU-CSRF`. |
| 404 | No route matches the method and the path. |
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
