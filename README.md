# klank

`klank` is a tool for running isolated AI coding-agent sessions in Docker
containers on your local machine. Each agent (e.g. a `pi` coding session)
runs inside its own container, so the agent can install packages, run
arbitrary commands and generally make a mess — without touching your host
system. Only the directory you launch from is shared with the container.

The project has three parts that work together:

| Part              | Where                | What it does                                                |
|-------------------|----------------------|-------------------------------------------------------------|
| **klank CLI**     | `klank/` (Go)        | Launches, tracks and communicates with agent containers      |
| **Agent image**   | `Dockerfile` + `pi-entrypoint.sh` | The container image the agents run in               |
| **pi extensions** | `extensions/`, `klank/bridge.ts`  | In-container behavior baked into the image           |

## How the pieces fit together

1. **Build the image** — the `Dockerfile` builds `klank:latest` on top of
   `node:24-bookworm-slim`: the [`pi`
   coding agent](https://www.npmjs.com/package/@earendil-works/pi-coding-agent)
   plus a toolbelt (bash, git, ripgrep, curl, jq, fzf, Go toolchain, sudo),
   the two pi extensions, and the entrypoint script.
2. **Launch an agent** — `klank new` runs `docker run -d --rm -it` with:
   - `--user <uid>:<gid>` matching your local user, so files created in the
     shared directory keep your ownership;
   - the current working directory bind-mounted at `/work`;
   - `-e` environment variables / an env file (typically API keys from the
     project `.env`).
3. **Enter the session** — the container's entrypoint logs the agent in to
   its model provider, installs the extensions, preselects the default
   model, and `exec`s `pi`. Your terminal is attached to the container,
   inside a tmux pane that klank remembers.

The container is `--rm`: it disappears when the session ends; anything that
should survive lives in `/work` (your project directory).

## Agent image details

### `Dockerfile`

- Base: `node:24-bookworm-slim`; installs `pi` globally via npm.
- Adds bash, ca-certificates, git, ripgrep, curl, jq, fzf and a current Go
  toolchain, so agents can build/run Go projects out of the box.
- Copies `klank/bridge.ts`, `extensions/edit-guard.ts`,
  `extensions/hashline.ts`, `extensions/hashline-read.ts` and
  `extensions/subagents/` into `/opt/pi-bridge/` for the entrypoint to
  install.
- Grants passwordless `sudo` to any user (for apt/system work inside the
  container — the agent should still never write to `/work` as root).
- No `CMD`: the entrypoint must run `pi` with no arguments (a CMD argument
  would be forwarded as an initial prompt).

### `pi-entrypoint.sh`

The container entrypoint:

1. Reads `OPENCODE_API_KEY` (or falls back to `OPENAI_API_KEY`) from the
   environment and writes it into the pi `auth.json` (opencode login).
2. Installs the pi extensions (`bridge.ts`, `edit-guard.ts`,
   `hashline.ts`, `hashline-read.ts`, `subagents/`) into the pi extensions
   directory, agent definitions into `~/.pi/agent/agents/` and workflow
   prompt templates into `~/.pi/agent/prompts/`.
3. Preselects the default model from `PI_DEFAULT_MODEL`
   (default: `opencode/glm-5.3-flash`) in `settings.json`.
4. `exec`s `pi`.

### pi extensions

- **`klank/bridge.ts`** — the bridge for external control: runs an HTTP
  server inside the container (port from `KLANK_PORT`, default 8089).
  `klank new` publishes the port on the host's loopback
  (`-p 127.0.0.1:<port>:<port>`) and records it in the database, so the
  klank CLI (or plain curl) can talk to the running pi session:

  - `GET /health` — liveness probe
  - `GET /status` — agent state: busy/idle, model, session, context usage
  - `GET /messages` — recent transcript messages, indexed newest-first
    (`?from=0` offset, `?limit=1`, `?role=user,assistant,tool`,
    `?text=1` flattens content blocks to plain text; `text=0` returns raw
    blocks under a `content` key instead)
  - `POST /message` — body is injected as a user message (queued as a
    steering message when the agent is mid-run); `?wait=1` long-polls until
    the agent settles, which is what powers `klank send -w` with no
    client-side polling

  For compatibility with older klank binaries the legacy file spool
  `/tmp/pi-inbox` is still watched: any `msg-<id>` file dropped there is
  removed and injected exactly like `POST /message`.
- **`extensions/edit-guard.ts`** — snapshot-based edit guardrails (partly
  inspired by omp's hashline editing + stale-tag reveal): tracks a
  content hash of every file the model has read and blocks `edit`/`write`
  calls against files that changed since (catching out-of-band mutations),
  intercepts edit-shaped shell commands (`sed -i`, redirects, …) and points
  the model at the edit tools instead. When a staleness block is locally
  explainable, the message includes a partial-staleness reveal — a line
  diff of the changed regions (idea borrowed from omp's stale-tag reveal) —
  and refreshes the snapshot, so the model can fix its edit from the block
  message alone. Block reasons always explain how to recover (use the shown
  diff, or re-read for large changes), so a blocked call costs one retry,
  not a dead end. Block reasons and snapshots quote the 4-hex file tag that
  `hashline-read.ts` prints in read output, so reads, guard messages and
  snapshots all reference one fingerprint.
- **`extensions/hashline-read.ts`** — overrides the built-in `read` tool with
  an omp hashline-style output format (inspired by the omp / Oh My Pi
  harness, https://github.com/can1357/oh-my-pi — same `[path#TAG]` header
  and numbered-line scheme as omp's read tool, simplified for pi's exact-
  match `edit` tool, which cannot consume tags): text reads start with a
  `[path#TAG]` header (TAG is a 4-hex content fingerprint shared with
  edit-guard — quote it when reporting file state) and every line is
  `N:`-prefixed with its 1-based source line number, stable across
  continuation reads. Line numbers are informational only; the edit tool
  still requires exact `oldText` matching. Image reads keep the built-in
  behavior. Formatting primitives live in `extensions/hashline.ts`.
- **`extensions/subagents/`** — subagent task delegation: registers the
  `subagent` tool, which spawns child `pi` processes (single, parallel
  batch, or sequential chain modes) with isolated context windows. Agents
  are markdown files with frontmatter (`name`, `description`, `tools`,
  `model`) in `~/.pi/agent/agents/`; bundled ones: `scout`, `planner`,
  `reviewer`, `worker`. Children run with `KLANK_SUBAGENT=1`, which tells
  the bridge not to start the control server or watch the inbox (only the
  top-level session consumes `klank send` messages). See
  `extensions/subagents/README.md`.

Receipts for legacy spool messages, written next to the message file:

- `msg-<id>.sent` — delivered to the agent (content: `started` or `queued`)
- `msg-<id>.done` — agent settled after the message
- `msg-<id>.error` — delivery failed

Agents started from older images without the bridge reject `send` with a
write error — rebuild the image and start a new agent. Agents started from
images without the control server fall back to the spool automatically
(port `0` in the database).

Debug an agent's control server directly from the host:

```sh
curl http://127.0.0.1:<port>/status
```

## The klank CLI

### How it works

`klank new` saves the container (id, name, image, image hash, tmux pane) in
a local sqlite database (`~/.klank/klank.db`) and attaches your terminal to
the container.

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

### Commands

```
klank new [-i image] [-e KEY=VALUE ...] [name]
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
klank rename <id|name> <newname>
                  rename an agent (a recorded tmux session name matching
                  the old name is updated in the database; the tmux
                  session itself is left untouched)
klank send [-w] [--timeout s] <id|name> <text...>
                  send a message to a running agent's pi session
                  (without text arguments, the message is read from
                  stdin; requires the bridge extension in the image)
klank status <id|name>
                  show an agent's live state (busy/idle, model, session,
                  context usage) via its control server
klank port <id|name> [<name>]
                  print the local address of a named container port
                  (with a port name: only that address, e.g.
                  127.0.0.1:81; without one: all of them)
klank config [-f] copy the current config (user config, falling back
                  to the system config; a commented starter template
                  when neither exists) to ./.klank.toml for per-project
                  editing (-f overwrites an existing file)
klank debug query <sql>
                  run a read-only SQL query against the database
klank debug data  show table schemas
```

Global flags: `-d` (debug output to stderr), `-j` / `-o json` (JSON output
for `list`, `get`, `debug`), `-h` (help).

`new` flags:

- `-i, --image <image>` — container image. Defaults to the configured
  default image.
- `-e, --env KEY=VALUE` — environment variable for the container
  (repeatable; overrides the configured env file).
- `-p, --port name=container-port` — publish a named container port on
  the local machine (repeatable, e.g. `-p http=80 -p grpc=8000`). Ports
  from the config's `[ports]` section are published automatically;
  `-p` overrides them by name and can add ports not mentioned in
  config. Each is bound to `127.0.0.1` on a free local port as close
  as possible to the container port. Mappings are saved in the agent
  database (see `klank port`) and written to `/ports` inside the
  container, where the system-prompt addendum tells the agent to look
  them up.
- `-f, --file <file>` — stage the file in a scratch directory under
  `/tmp` (named after the first file) and mount that directory at
  `/work` instead of the current one, so the agent works on just the
  given files in isolation. Repeatable (`-f main.go -f util.go`). The
  scratch directory is removed when the agent's container exits; if
  you detach while it still runs, its path is printed. Files must be
  regular files with distinct names. The default agent name is the
  first file's name.

`new` also takes the agent name as a positional argument (`klank new
myagent`); when started outside tmux it is also the tmux session name.
Defaults to `agent001`, `agent002`, ...

`send` flags:

- `-w, --wait` — block until the agent settles after the message (turn
  finished), instead of returning as soon as the message was delivered.
- `--timeout <seconds>` — timeout for `--wait` (default 900).

Examples:

```
klank send agent001 fix the failing test in foo.go
echo "multi-line\nmessage" | klank send agent001
klank send -w agent001 run the test suite
klank status agent001
```

### Configuration

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
  `-e` flags override it). Secrets are staged as `0600` files in a private
  `/tmp/klank-secrets-*` directory on the host, bind-mounted read-write at
  `/secrets` in the container, and wrapped at startup: each file is
  exported as an env var and deleted immediately after it is read, both
  inside the container and on the host (bind mount). Secrets therefore
  never appear in `docker inspect`, process listings, or `docker run`
  arguments, and are gone from disk right after first use.
- `[ports]` — default named container ports published for every new
  agent, for ports that are common across launches:

  ```toml
  [ports]
  http = 80
  grpc = 8000
  ```

  These behave exactly like `-p http=80 -p grpc=8000` on every
  `klank new`; a `-p` flag with the same name overrides the config
  entry for that launch. Run `klank config` to copy your current
  config into `./.klank.toml` as a starting point for per-project
  settings.

### Database

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
docker build -t klank:latest .   # build the agent image
cd klank
go build                         # produces ./klank
go vet
```

Repository layout:

- `klank/` — the CLI. `main.go` (usage, flags, dispatch), `cli/` (debug/error
  output, text/json printing), `config/` (TOML config + env files),
  `storage/` (sqlite database), `command/` (one file per command plus
  shared `docker.go`/`tmux.go` helpers), `bridge.ts` (the pi bridge
  extension, copied into the image).
- `Dockerfile` — the agent image.
- `pi-entrypoint.sh` — container entrypoint.
- `extensions/` — pi extensions baked into the image (`edit-guard.ts`,
  `hashline.ts`, `hashline-read.ts`, `subagents/`).

Notes for AI sessions working on klank:

- `docker` and `tmux` are shelled out to via `os/exec`, so logic can be
  tested with stub executables on `PATH` (a fake `docker` script) plus a
  real `tmux`.
- The sqlite driver is pure-Go (`modernc.org/sqlite`); no cgo required.
