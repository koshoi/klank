# AGENTS.md

## Where you are

You (the pi coding agent) are working **on klank, from inside klank itself**.
This repository was launched via `klank new`, so this very session is an
example of the product you are developing: a klank-managed agent container
running a pi coding-agent session, with this repo bind-mounted at `/work`.

Keep this self-referential nature in mind while working:

- Changes you make to the klank CLI (`klank/`), the agent image
  (`Dockerfile`, `pi-entrypoint.sh`), or klankd (`klankd/`) only take effect
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

## Practical notes

- Named published ports are listed in `/ports` (name, container port, and
  the host-side `127.0.0.1:<host>` address). Use these to expose any local
  service to the user's browser.
- Network access is unrestricted; installing packages (npm, apt, Go modules)
  is fine. Sudo is available.
- Prefer editing files in this repo with the standard tools; the extensions
  here (notably `edit-guard.ts`) may enforce rules on how edits are made.
