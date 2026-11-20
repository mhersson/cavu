# Running the server

`cavu serve` drives every started task, as `task run` drives one, watches each task's pull request until the task is done or cancelled ([The pull request](tasks.md#the-pull-request)), and serves the web app and the HTTP API of [api.md](api.md) on the `listen_address` setting (default `127.0.0.1:7117`): open `http://127.0.0.1:7117/` in a browser. Set the user first, with the password in a file, then start it:

```
cavu user set NAME < password.txt
cavu serve
```

It prints the address it listens on. On the page, sign in as that user; a CAVU with no project opens at its setup ([Getting started](../README.md#getting-started)). A client of the API logs in with `POST /api/auth/login`, or sends an API token from `cavu token add NAME`. An MCP client gets the tools that create, show, list, edit, run, answer and cancel tasks and list the projects at `http://HOST:PORT/mcp` over the streamable HTTP transport, with the header `Authorization: Bearer TOKEN` and a token from `cavu token add NAME`. While it runs, `task run`, `project check` and `replay` leave the driving to it, and `doctor` is refused. Ctrl-C stops it, and the next `cavu serve` carries on with the tasks it was running.

The web app fits a phone: at 720 pixels wide or less it shows four tabs at the bottom, **Needs you**, **New task**, **Status** and **More**, in place of the navigation along the top, and it carries a web manifest and icons for a phone's home screen. By default CAVU listens on `127.0.0.1`, which only the computer it runs on reaches. For a phone on your network, set `listen_address` to a host and a port of that computer, such as `0.0.0.0:7117` for every address it has, start `cavu serve` again, and open `http://ADDRESS:7117/` on the phone with the computer's address on your network:

```
cavu settings set listen_address 0.0.0.0:7117
```

CAVU serves plain HTTP, so what passes between the phone and CAVU, the password at sign-in included, travels unencrypted: do this only on a network you trust, or put a proxy that serves HTTPS in front of CAVU ([Behind a proxy or a tunnel](#behind-a-proxy-or-a-tunnel)).

To keep it running without a terminal, install it as a service of your login with `cavu service install`. It writes `~/.config/systemd/user/cavu.service` on Linux, a systemd user unit, or `~/Library/LaunchAgents/com.github.mhersson.cavu.plist` on macOS, a launchd agent, and starts the service from it at once and again when it fails. The file names the `cavu` binary that installed it by its absolute path and sets your `PATH`, so the service finds the `docker` or `podman` your shell finds; `CAVU_DATA_DIR`, the absolute path of the data directory chosen as for every verb (`--data-dir`, else `CAVU_DATA_DIR`, else `$XDG_DATA_HOME/cavu`, else `~/.local/share/cavu`); `DOCKER_HOST`, only when your Docker context's endpoint is not `unix:///var/run/docker.sock`; and `HTTPS_PROXY`, `HTTP_PROXY`, `NO_PROXY`, their lower-case forms, `SSL_CERT_FILE` and `SSL_CERT_DIR`, each only when your shell sets it. The macOS agent also sets `HOME`. CAVU's secrets stay in the data directory, where `cavu secret set` stored them; the file is readable by you alone, since a proxy address in it may hold a password. As in a terminal, set the user and build the image first. On Linux, `--linger` also runs `loginctl enable-linger`, for a host nobody stays logged in to.

```
cavu service install
cavu service uninstall
```

Read the log with `journalctl --user -u cavu` on Linux, or in `~/Library/Logs/cavu.log` on macOS. Running `cavu service install` again rewrites the file when it changed and restarts the service. `cavu service uninstall` stops the service and removes its file, and keeps the data directory. To run `cavu doctor`, which a running server refuses, stop the service first with `systemctl --user stop cavu` on Linux or `launchctl bootout gui/$(id -u)/com.github.mhersson.cavu` on macOS, and start it again afterwards with `systemctl --user start cavu` or `launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.github.mhersson.cavu.plist`.

## Behind a proxy or a tunnel

A proxy or a tunnel in front of CAVU gives it an https address, and can make it reachable away from home. CAVU needs this of it:

- It reaches CAVU at `listen_address`. The default, `127.0.0.1:7117`, serves a proxy on the same computer; one elsewhere needs an address of that computer it can reach, set as for a phone above.
- It serves HTTPS, redirects plain HTTP there, and sends `X-Forwarded-Proto: https`, on which CAVU marks the session cookie `Secure`.
- It passes the page's live stream, `GET /api/events`, on as it comes and leaves it open, with no buffering and no request timeout. CAVU answers it with `X-Accel-Buffering: no` and sends a comment line on it every 15 seconds.
- It gives CAVU one address at home and away: CAVU's sign-in, its notifications and its app on a home screen each belong to the address they were set up on.

A CAVU the internet reaches belongs behind an access layer that admits only you, such as an identity-aware proxy. CAVU's sign-in limiter counts failed sign-ins by the user name and the connection's address, and behind a proxy every connection comes from the proxy's address: from the third failure in a row, sign-ins with that name wait 1 second, twice as long after each further failure, up to 5 minutes. So anyone who reaches the page and knows your user name can keep you out. A sign-in that succeeds, or a restart of `cavu serve`, clears the count. The page links its web manifest with `crossorigin="use-credentials"`, so that it is asked for with the page's cookies.

API tokens, which an MCP client needs, are made only on CAVU's computer, with `cavu token add NAME`, also while `cavu serve` runs.

## Notifications

CAVU can notify a browser when a task comes to need you. Turn them on in each browser you want them in: in **Settings**, under **This browser**, press **Turn on** and allow them when the browser asks; **Turn off** there stops them. They need CAVU on an https address ([Behind a proxy or a tunnel](#behind-a-proxy-or-a-tunnel)). On an iPhone or iPad, add CAVU to the Home Screen first and turn them on in the app it adds: Apple's documentation gives Web Push to Home Screen web apps on iOS and iPadOS 16.4 or later.

A notification's title is what the task needs, `Needs an answer`, `Ready for review`, `Stopped` or `A change to take`, then its project, as in `Ready for review · web`; its text is the task's title, and a tap opens the task's page. The browser in which a CAVU page has the focus gets none; every other browser with them on does.

Only `cavu serve` sends them, each need once, to the browsers that have them on when it finds the need. A need that comes while it is stopped, such as one of a task that `cavu task run` drives, is sent when it next starts, if the task still needs you.

## Upgrading

Each build of `cavu` has its own version and sandbox image, and `cavu serve` starts only on its own. To move to a new release, or to a newer copy of this repository:

1. Replace `cavu`. With a release, download the new archive and its `checksums.txt` into an empty directory, check and unpack it there as in [Getting started](../README.md#getting-started), then move the new `cavu` over the old one with `mv`: on Linux, a copy over a running binary fails with `Text file busy`, while a move replaces it and a running server carries on. From source, run `make install`.
2. Build the new image with `cavu image build`, or `make image` from source. It runs beside a running server, which goes on with the old image.
3. Restart the server: `cavu service install` restarts the service on the new binary; without the service, stop `cavu serve` and start it again.

A service that starts before its image is built stops with a line in its log naming `cavu image build`, and starts again every 10 seconds until the image is there.
