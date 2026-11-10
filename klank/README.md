# klank

`klank` is a CLI for running isolated AI coding-agent sessions in Docker
containers on your local machine. Each agent (e.g. a `pi` coding session)
runs inside its own container, so the agent can install packages, run
arbitrary commands and generally make a mess — without touching your host
system. Only the directory you launch from is shared with the container.

## How it works

`klank new`:

1. runs `docker run -d --rm -it` with:
   - `--user <uid>:<gid>` matching your local user, so files created in the
     shared directory keep your ownership;
   - the current working directory bind-mounted at `/work`.
2. saves the container (id, name, image, image hash, tmux pane) in a local
   sqlite database (`~/.klank/klank.db`);
3. attaches your terminal to the container (its entrypoint is the AI agent).

The container is `--rm`: it disappears when the session ends; anything that
should survive lives in `/work` (your project directory).

### tmux integration

Agents live inside tmux panes, and klank records *where* so you can get back
to them later from anywhere:

- **started inside tmux**: the container is attached to the pane you ran
  `klank new` from (plain `docker attach`), and that pane's session name and
  pane id are saved in the database.
- **started outside tmux**: klank creates a new detached tmux session named
  after the agent (`-n <name>`, or the auto-generated agent name), runs
  `docker attach <cid>` inside that session's pane, and then attaches your
  terminal to the session.

The tmux *pane id* (e.g. `%3`) is stored rather than a `session:window.pane`
address because it stays valid when the pane is moved between windows.

`klank attach` brings an agent's pane back to the foreground:

- **inside tmux**: the saved pane is swapped into your current window
  (`tmux swap-pane`), replacing whatever was there.
- **outside tmux**: klank attaches to the agent's tmux session and focuses
  the saved pane.

Agents recorded before tmux integration (no pane saved) fall back to plain
`docker attach`.

## Commands

```
klank new [-n name] [-e KEY=VALUE ...] [image]
                  launch a new agent container and attach to it
klank list        list known agents
                  (agents whose container is no longer present are pruned
                  from the database; if docker itself is unreachable,
                  nothing is pruned)
klank get <id|name>
                  print stored data for one agent
klank attach [<id|name>]
                  bring an agent's pane back to the foreground
                  (without an argument: pick via fzf)
klank send [-w] [--timeout s] <id|name> <text...>
                  send a message to a running agent's pi session
                  (without text arguments, the message is read from
                  stdin; requires the bridge extension in the image)
klank debug query <sql>
                  run a read-only SQL query against the database
klank debug data  show table schemas
```

Global flags: `-d` (debug output to stderr), `-j` / `-o json` (JSON output
for `list`, `get`, `debug`), `-h` (help).

`new` flags:

- `-n, --name <name>` — agent name; when started outside tmux this is also
  the tmux session name. Defaults to `agent001`, `agent002`, ...
- `-e, --env KEY=VALUE` — environment variable for the container
  (repeatable; overrides the configured env file).

`send` flags:

- `-w, --wait` — block until the agent settles after the message (turn
  finished), instead of returning as soon as the message was delivered.
- `--timeout <seconds>` — timeout for `--wait` (default 900).

Examples:

```
klank send agent001 fix the failing test in foo.go
echo "multi-line\nmessage" | klank send agent001
klank send -w agent001 run the test suite
```

### Bridge extension (external messages)

The image ships the `bridge` pi extension (`klank/bridge.ts`, installed
into the extensions dir by the entrypoint). It watches `/tmp/pi-inbox`
inside the container: any `msg-<id>` file dropped there is removed and
injected into the running pi session as if typed by the user, so it shows
up in the TUI transcript like any other message. If the agent is mid-run,
the message is queued as a steering message.

Receipts are written next to the message file:

- `msg-<id>.sent` — delivered to the agent (content: `started` or `queued`)
- `msg-<id>.done` — agent settled after the message
- `msg-<id>.error` — delivery failed

`klank send` uses this to deliver messages and (with `--wait`) polls for
the `.done` receipt. Agents started from older images without the bridge
reject `send` with a write error — rebuild the image and start a new agent.

## Configuration

Config files are TOML, merged lowest to highest priority:

1. built-in defaults (`image = "klank:latest"`)
2. `/etc/klank/config.toml`
3. `~/.config/klank/config.toml` (honors `$XDG_CONFIG_HOME`)
4. `./.klank.toml` (project)
5. environment: `KLANK_IMAGE`
6. CLI arguments

Keys:

- `image` — default container image for `klank new`
- `env_file` — dotenv-style file whose `KEY=VALUE` lines are passed to the
  container (comments, blank lines and `export ` prefixes are supported;
  `-e` flags override it)

## Database

`~/.klank/klank.db` (sqlite), table `agents`:

| column       | meaning                                        |
|--------------|------------------------------------------------|
| `seq`        | auto number, also drives default agent names   |
| `id`         | docker container id                            |
| `name`       | agent name (unique)                            |
| `image`      | image used to launch the container             |
| `image_hash` | image digest at launch time                    |
| `tmux_session` | tmux session hosting the agent pane          |
| `tmux_pane`    | tmux pane id hosting the agent (e.g. `%3`)   |
| `created_at` | unix timestamp                                 |

Inspect it with `klank debug query "SELECT ..."` / `klank debug data`.

## Security model

- The container runs without privileges; your uid/gid is mapped in.
- Only the launch directory is shared (at `/work`).
- The agent has **full network access** from inside the container — klank
  isolates the filesystem, not the network.
- The agent cannot see your host processes, files outside `/work`, or
  credentials (except the API keys you pass explicitly via `-e`).

## Development

```sh
cd klank
go build          # produces ./klank
go vet
```

Notes for AI sessions working on klank:

- Package layout: `main.go` (usage, flags, dispatch), `cli/` (debug/error
  output, text/json printing), `config/` (TOML config + env files),
  `storage/` (sqlite database), `command/` (one file per command plus
  shared `docker.go`/`tmux.go` helpers).
- `docker` and `tmux` are shelled out to via `os/exec`, so logic can be
  tested with stub executables on `PATH` (a fake `docker` script) plus a
  real `tmux`.
- The sqlite driver is pure-Go (`modernc.org/sqlite`); no cgo required.
