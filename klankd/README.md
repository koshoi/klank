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

Build and run the binary yourself:

    CGO_ENABLED=0 go build -ldflags="-s -w" -o klankd .
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
the caddy binary, `-db` database path, `--once` (sync once, no caddy).
Defaults: poll 750ms, `~/.klank/Caddyfile`, domain from
`$KLANK_PROXY_DOMAIN` or `klank.localhost`.

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
127.0.0.1 out of the box on systemd-resolved and modern browsers. For
a custom suffix, set `KLANK_PROXY_DOMAIN` in the environment and add a
one-time dnsmasq wildcard (`address=/myproject.local/127.0.0.1`) on
the host. The dashboard is then served at the domain root itself.

## Caveat: schema coupling

**The binary hardcodes klank's storage internals** — the `agents` table
shape and the JSON format of the `ports` column (mirrored in
`portMapping` in `main.go`, kept in sync with `storage.PortMapping`).
It is compiled separately from klank, so if a klank upgrade ever changes
that schema, the watcher will not fail at compile time: it logs parsing
warnings and routes stop updating silently.

After upgrading klank, verify the watcher still tracks it:

    klank new -p smoke=9999 -n watcher-check   # inside any project
    curl -s http://smoke.watcher-check.klank.localhost || echo "watcher out of sync — check portMapping in main.go"
