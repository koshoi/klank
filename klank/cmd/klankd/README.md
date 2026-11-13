# klankd

A daemon that gives every service inside a klank container a stable
name from the host, e.g.:

    http.myproject.klank.localhost  ->  127.0.0.1:1024

instead of ad-hoc `127.0.0.1:<port>` addresses. It also serves a
dashboard at `http://klank.localhost`: an expandable list of agents
whose ports are clickable links to the mapped URLs.

The daemon **runs and supervises caddy itself** — there is no separate
caddy service to install or manage. One process is the entire deployment.

## How it works

Klank records every named port (`klank new -p name=container-port`) in
`~/.klank/klank.db` (`agents` table, JSON `ports` column). The daemon:

1. polls that database read-only every 750ms and regenerates, next to the
   Caddyfile:
   - the **Caddyfile** — one `http://<port-name>.<agent>.<domain>`
     reverse-proxy site block per route, plus a dashboard site block
     (`file_server` at the domain root). The explicit `http://` scheme
     matters: without it caddy serves HTTPS with auto-redirect and
     local TLS certs, which breaks these machine-local names.
   - **`index.html`** — server-rendered expandable agent list (native
     `<details>`, works without JS); an inline script re-fetches
     `klank-agents.json` every 5s so new agents appear without a
     reload, preserving expanded/collapsed state
   - **`klank-agents.json`** — the JSON feed the page polls
2. runs caddy as a **supervised child process**: started after the
   first sync, restarted (2s backoff) if it dies, reloaded via
   SIGUSR1 when the generated Caddyfile changes (caddy re-reads its
   config file on that signal — no admin API, no `caddy reload`)

Writes are compare-then-atomic-rename, so idle polls do nothing. The
database is opened `mode=ro` and klank runs it in WAL mode, so polling
never blocks klank and vice versa. Transient errors (db busy, caddy
restart) are logged and retried; the daemon only stops on
SIGINT/SIGTERM (stopping caddy gracefully first) or a crash.

## Install & run

Build and run the binary yourself (from the repository root; klank and
klankd live in one Go module under `klank/`):

    cd klank
    CGO_ENABLED=0 go build -ldflags="-s -w" -o klankd ./cmd/klankd
    ./klankd

Requires `caddy` (hard dependency — the daemon exits if it's
missing). The binary is fully static (pure-Go sqlite driver, built
with CGO_ENABLED=0; no runtime dependencies).

Only one process may bind port 80 — if a system caddy service is
running, stop it first.

To grant the supervised caddy access to ports 80/443 while running
unprivileged, use e.g. `setcap` on the caddy binary or run the daemon
under a wrapper that grants `CAP_NET_BIND_SERVICE`.

## Configuration

Flags (see `-h`): `-i` poll interval, `-f` Caddyfile path (dashboard
files are written next to it), `-d` domain suffix, `-caddy` path to
the caddy binary, `-db` database path, `-open-command` command to open
diagram URLs on the host, `-http-port` listen port (see below),
`--once` (sync once, no caddy).
Defaults: poll 750ms, `~/.klank/Caddyfile`, domain from
`$KLANK_PROXY_DOMAIN` or `klank.localhost`.

### Running unprivileged (recommended over sudo)

The daemon needs no root if nothing it starts binds port 80. Two ways
to free the daemon (and its supervised caddy) from port 80:

1. `setcap 'cap_net_bind_service=+ep' $(which caddy)` — the supervised
   caddy keeps binding 80; the daemon runs as your user. Simplest.
2. **Root proxy in front** — run a minimal root caddy (or any port-80
   reverse proxy) that forwards to the daemon's caddy on an unprivileged
   port, and tell klankd about it:

       # root Caddyfile, e.g. /etc/caddy/klank-front:80
       :80 {
       	reverse_proxy 127.0.0.1:8080
       }

       ./klankd -http-port 8080

   The generated site addresses become `http://<name>:8080` (listen
   port only — matching stays by hostname, and the port-80 proxy
   forwards a portless Host header untouched). Because the daemon now
   runs as the desktop user, it inherits `DISPLAY`/`DBUS` and
   `show_diagram` browser-opening just works — running the daemon via
   sudo does not, since root has no path to the user's graphical
   session (the spawned opener exits immediately with status 3).

The dashboard's links are always rendered on port 80 (via the front
proxy) regardless of `-http-port`.

### Running under sudo

If the daemon must run as root (e.g. because caddy needs to bind port
80), pass `-u <user>` so everything else still behaves like a user
run:

    sudo ./klankd -u $USER

`-u` accepts a username, a numeric uid, or `uid:gid` / `user:group`.
It makes the daemon resolve `~/.klank` under that user's home, chown
the Caddyfile and dashboard files to that user (never root), and point
the supervised caddy's `HOME`/`USER` there. Caddy itself keeps the
daemon's privileges so it can still bind port 80.

`*.klank.localhost` (including `klank.localhost` itself) resolves to
127.0.0.1 out of the box on most modern Linux systems and browsers. For
a custom suffix, set `KLANK_PROXY_DOMAIN` in the environment and add a
one-time dnsmasq wildcard (`address=/myproject.local/127.0.0.1`) on
the host. The dashboard is then served at the domain root itself.

## Agent outbox (show_diagram)

Every agent's control server (bridge.ts) exposes an agent→host push
channel: the agent enqueues typed messages (POST /outbox), the daemon
streams them over SSE (GET /events) and acknowledges what it handled
(POST /ack). The daemon keeps one SSE consumer per running agent,
reconciled with the database on every poll tick; a handler that fails
is not acked, so the bridge redelivers on the next reconnect.

Agents whose bridge predates the outbox (old image, container not
rebuilt) have no `/events` endpoint; their consumer disables itself on
the first 404 instead of retrying forever and flooding the log. It
comes back automatically once the agent's port changes — i.e. its
container was recreated with a current image.

The first message type is `show_diagram`: the agent sends mermaid
source, the daemon encodes it into a mermaid.live URL
(`https://mermaid.live/edit#<base64url(JSON state)>`) and opens it in
a browser on the host — `-open-command` if given, else `firefox
--new-window` (its own window per diagram, not a tab in the existing
one). The outcome (URL, or a failure note) is delivered back
to the container (POST /inbox) so the agent-side `show_diagram` tool
can report it to the model.

Delivery is **at-least-once**: an item that was handled but whose
deliver or ack failed (e.g. the bridge was briefly unreachable) is
replayed on the next connect and handled again — for `show_diagram`
that means a second browser tab, which is accepted. Mermaid payloads
larger than 200 KB are rejected by the bridge (very large mermaid.live
URLs fail to restore silently in some browsers).

`-open-command` is split on spaces, so paths/arguments containing
spaces (e.g. `/Applications/Google Chrome.app/...`) are not supported;
use a wrapper script for those.

## Schema coupling

The daemon reads klank's database directly (the `agents` table shape
and the JSON format of the `ports` column). klank and klankd share one
Go module: port mappings are decoded into `klankdb.PortMapping`, defined
in a cli-free leaf package (`klank/klankdb`) so a schema change that
affects the daemon fails the build instead of drifting silently — while
the daemon's import graph stays free of `cli.Fatal`'s `os.Exit`, so no
library error can ever kill the polling loop.

After upgrading klank on a live machine, it is still worth verifying the
watcher end to end:

    klank new -p smoke=9999 -n watcher-check   # inside any project
    curl -s http://smoke.watcher-check.klank.localhost || echo "watcher out of sync — restart klankd with the new binary"
