# AGENTS.md

## Where you are

You (the pi coding agent) are working **on klank, from inside klank itself**.
This repository was launched via `klank new`, so this very session is an
example of the product you are developing: a klank-managed agent container
running a pi coding-agent session, with this repo bind-mounted at `/work`.

Keep this self-referential nature in mind while working:

- Changes you make to the klank CLI (`klank/cmd/klank`), the agent image
  (`Dockerfile`, `pi-entrypoint.sh`), or klankd (`klank/cmd/klankd`) only take effect
  in **new** sessions/containers — your current container keeps running with
  the previously built image and binaries.
- This container is disposable (`docker run --rm`); only `/work` persists.
  Anything installed elsewhere (apt packages, global npm modules) is
  ephemeral.
- Do not attempt to "manage yourself" with the local `klank` binary; the
  controlling `klank` process runs on the host, not here.

## The harness: pi modified via extensions in this repo

The pi coding agent is **not vanilla** here — it runs a pi harness that is
modified through the extensions in this repository:

- `extensions/` — pi extensions loaded into every klank agent session
  (e.g. `edit-guard.ts`, `hashline.ts` / `hashline-read.ts`,
  `modal-editor.ts`, `user-borders.ts`, and `subagents/`). These alter or
  extend tool behavior, rendering, and agent capabilities beyond upstream pi.
- `klank/bridge.ts` — in-container bridge behavior baked into the image.

Because of this:

- Read the relevant extension source before assuming standard pi behavior —
  tools, edits, and output may be intercepted, guarded, or reformatted by an
  extension in this repo.
- When changing harness behavior, change it **here** (extensions /
  bridge / image), not by hand-patching the installed pi package at runtime.
  Runtime patches vanish when the container is rebuilt.
- When **creating a new extension**, consider how it will interact with
  **vim mode** — the user runs pi in vim mode (normal/insert/visual states)
  all the time. Anything that touches keybindings, editor input, or the TUI
  must play nicely with modal editing: don't swallow keys vim mode needs,
  respect the current mode (e.g. don't hijack keys that differ per mode),
  and test the extension with vim mode enabled rather than assuming default
  (non-modal) input behavior.
- The running pi session does **not** load extensions from this repo — they
  are baked into the image at **`/opt/pi-agent/extensions/`** (the agent dir
  is pinned via `PI_CODING_AGENT_DIR`). To make an extension change testable
  in the **current** session, apply every extension edit to **both** places:

  1. edit the source in `extensions/` (the durable change), and
  2. copy it over the live copy, e.g.
     `sudo cp extensions/foo.ts /opt/pi-agent/extensions/foo.ts`
     (`/opt/pi-agent` is chmod'd a+rwX at build time, but files are owned by
     root, so use `sudo`).

  Then `/reload` inside pi picks the new code up in the same session. The
  runtime copy is ephemeral; the next image build takes the `extensions/`
  source automatically.

## Rebuild the klank binaries when you finish a task

The user runs **klank and klankd built from this repository** on their
local machine: the `klank/` directory is bind-mounted here at `/work`, so
binaries written into the repo appear directly on the user's machine.
Whenever a task touches Go code under `klank/` (`cmd/klank`,
`cmd/klankd`, or any shared package), finish by rebuilding both binaries
so the user actually gets the change:

    cd /work/klank
    go build -o klank ./cmd/klank
    go build -o klankd ./cmd/klankd

(the outputs `klank/klank` and `klank/klankd` are gitignored). Do this
**even if the change seems trivial** and after every related edit in a
task — a fix that never made it into the binary did not ship. Verify
with `go vet ./...` and, when reasonable, a quick smoke test before
building. Pure harness/image work (extensions, `Dockerfile`,
`pi-entrypoint.sh`) does not need this; it ships via the next image
build instead.

## Porting from omp

Features may be **copied from omp** (the oh-my-pi harness by can1357), which
solved many of the same problems our extensions address (fuzzy edit
matching, whitespace/indentation handling, hashline read format, edit-error
recovery). A shallow reference checkout is baked into the image at
**`/tmp/omp`**, owned by user `node` so you can work on it directly:

- Key sources: `crates/pi-edit/src/` (fuzzy match ladder, indentation
  adjustment in `text.rs`, `modes/replace.rs`),
  `packages/coding-agent/src/edit/` (normalize, auto-repair, settings), and
  `packages/coding-agent/src/prompts/tools/read.md` (read-format prompt
  wording).
- The checkout is **pinned to the image build date**; it has full working
  tree but shallow history. Rebuild the image to refresh it, or
  `git -C /tmp/omp fetch --unshallow` in-session for full history.
- /tmp is ephemeral in principle, but the image-layer clone survives for
  the container's lifetime; treat /tmp/omp as read-mostly reference — port
  code into this repo's extensions rather than editing it in place.

## Dockerfile layer discipline

Long-running and network-heavy operations (apt installs, toolchain
downloads, git clones, npm installs) must appear **as early as possible**
in the `Dockerfile`. Docker cache-invalidation is sequential: any changed
layer invalidates every layer after it, so a step that changes frequently
(an extension `COPY`) must never sit above a slow step — otherwise every
harness edit re-downloads Go and re-clones the reference repo.

Ordering rules, in practice:

1. apt layers first (and merged — one `apt-get update` total),
2. then big downloads (Go tarball, fd binary, pi npm install),
3. then rarely-changing remote snapshots (the `/tmp/omp` clone),
4. `COPY` of frequently-edited local files (extensions, entrypoint) last,
   so their cache miss costs nothing but the copy.

## Practical notes

- Named published ports are listed in `/ports` (name, container port, and
  the host-side `127.0.0.1:<host>` address). Use these to expose any local
  service to the user's browser.
- Network access is unrestricted; installing packages (npm, apt, Go modules)
  is fine. Sudo is available.
- Prefer editing files in this repo with the standard tools; the extensions
  here (notably `edit-guard.ts`) may enforce rules on how edits are made.
