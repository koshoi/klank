FROM node:24-bookworm-slim

# Layer discipline: network-heavy / slow steps run FIRST so rebuilds stay
# fast — a cache miss on a late layer (e.g. an extension edit) must never
# re-run the expensive steps above it. Base toolchain and sudo share one
# apt layer: every `apt-get update` re-downloads package indexes, so
# there is exactly one cache-bust point for apt, as early as possible.
# NOTE: sudo is for apt/system only -- never write to /work as root
# Also baked in here: Chromium's runtime library deps + fonts (Debian 12
# package names) so the Playwright browsers installed below can launch.
RUN apt-get update \
  && apt-get install -y --no-install-recommends bash ca-certificates git ripgrep curl jq fzf sudo \
    libnss3 libnspr4 libatk1.0-0 libatk-bridge2.0-0 libcups2 libdrm2 libdbus-1-3 \
    libxkbcommon0 libatspi2.0-0 libxcomposite1 libxdamage1 libxfixes3 libxrandr2 \
    libgbm1 libasound2 libpango-1.0-0 libcairo2 libxi6 libxtst6 libglib2.0-0 \
    fonts-liberation fonts-noto-color-emoji \
    imagemagick fonts-inter fonts-open-sans fonts-roboto \
  && rm -rf /var/lib/apt/lists/* \
  && echo 'ALL ALL=(ALL) NOPASSWD:ALL' > /etc/sudoers.d/all-users \
  && chmod 0440 /etc/sudoers.d/all-users

# Playwright + Chromium browsers for web-app development/testing from inside
# sessions (navigation, clicks, form fills, JS eval, screenshots). Browsers go
# to a shared, writable-by-any-user dir (PLAYWRIGHT_BROWSERS_PATH) instead of
# $HOME/.cache so they work under any runtime uid. Alongside: pixelmatch+pngjs
# (screenshot pixel-diffing), axe-core (accessibility audits — inject
# require('axe-core').source into the page and page.evaluate(axe.run)).
# NODE_PATH makes all of these require-able from anywhere without a local
# npm install (ESM `import` ignores NODE_PATH — use createRequire or CJS).
# Network-heavy: kept early per layer discipline.
ENV PLAYWRIGHT_BROWSERS_PATH=/opt/ms-playwright
ENV NODE_PATH=/usr/local/lib/node_modules
RUN set -eux; \
  npm install -g playwright pixelmatch pngjs axe-core; \
  npx --yes playwright install chromium; \
  chmod -R a+rwX /opt/ms-playwright

# fd: bake in the same static musl binary pi's tools-manager would download
# (sharkdp/fd GitHub release). Resolve the latest version via the GitHub API.
# (The old /releases/latest redirect trick no longer works: GitHub now serves
# that page with 200 instead of 302, so %{redirect_url} comes back empty.)
# Placed in pi's own tools dir ($PI_CODING_AGENT_DIR/bin/fd), which pi checks
# before PATH, so it never re-downloads at session start.
RUN set -eux; \
  mkdir -p /opt/pi-agent/bin; \
  ver="$(curl -fsSL https://api.github.com/repos/sharkdp/fd/releases/latest \
        | jq -r '.tag_name // empty | sub("^v";"")')"; \
  if [ -z "${ver}" ]; then \
    ver="$(curl -fsSL -o /dev/null -w '%{redirect_url}' https://github.com/sharkdp/fd/releases/latest \
          | sed 's|.*/tag/v||')"; \
  fi; \
  echo "fd version: ${ver}"; \
  curl -fsSL "https://github.com/sharkdp/fd/releases/download/v${ver}/fd-v${ver}-x86_64-unknown-linux-musl.tar.gz" \
    | tar -xz -C /opt/pi-agent/bin --strip-components=1 "fd-v${ver}-x86_64-unknown-linux-musl/fd"; \
  chmod 0755 /opt/pi-agent/bin/fd; \
  /opt/pi-agent/bin/fd --version

# golang (hardcoded current latest: go1.27.3)
RUN set -eux; \
  curl -fsSLO "https://go.dev/dl/go1.27.3.linux-amd64.tar.gz"; \
  rm -rf /usr/local/go \
  && tar -C /usr/local -xzf go1.27.3.linux-amd64.tar.gz \
  && rm go1.27.3.linux-amd64.tar.gz
ENV PATH="/usr/local/go/bin:${PATH}"

RUN npm install -g --ignore-scripts @earendil-works/pi-coding-agent

# omp harness reference checkout: upstream omp (oh-my-pi) solved many of the
# same problems our extensions face (fuzzy edit matching, hashline read
# format, edit recovery). Shallow-cloned at build time so the source is
# available inside sessions at /tmp/omp for reading and porting. Owned by
# user `node` (uid 1000, the runtime user) so in-session edits/tools work
# on it without sudo; a+rX so any --user uid can still read it.
# NOTE: pinned to whatever HEAD was at build time — rebuild the image to
# refresh, or `git -C /tmp/omp fetch --unshallow` in-session for history.
# Kept BEFORE the extension COPYs: those change with every harness edit and
# must not force a re-clone of the reference checkout on each rebuild.
RUN set -eux; \
  git clone --depth 1 https://github.com/can1357/oh-my-pi /tmp/omp; \
  chown -R node:node /tmp/omp; \
  chmod -R a+rX /tmp/omp


# pi agent dir, pinned so config can be baked in at build time.
# Readable/writable by any runtime user (auth.json, settings.json, sessions
# are written here at startup).
# NOTE: single-user image -- do not rely on /opt/pi-agent for isolation
# between container users.
ENV PI_CODING_AGENT_DIR=/opt/pi-agent
RUN mkdir -p /opt/pi-agent/extensions && chmod -R a+rwX /opt/pi-agent

# klank bridge extension: lets `klank send` inject user messages into a
# running pi TUI session
COPY klank/bridge.ts /opt/pi-agent/extensions/bridge.ts

# edit-guard extension: snapshot-based edit guardrails (staleness detection,
# bash intent interceptor)
COPY extensions/edit-guard.ts /opt/pi-agent/extensions/edit-guard.ts

# hashline extensions: shared fingerprint primitives + the `read` override
# that emits [path#TAG] headers and N:-numbered lines (edit-guard.ts imports
# ./hashline.ts, so both must be installed alongside it)
COPY extensions/hashline.ts /opt/pi-agent/extensions/hashline.ts
COPY extensions/hashline-read.ts /opt/pi-agent/extensions/hashline-read.ts

# fuzzy-edit extension: whitespace-insensitive fallback for `edit` (tab vs
# space mismatches) with re-indentation and whitespace-specific errors
# (edit-guard.ts imports its resolveEditSpans, so both must be installed)
COPY extensions/fuzzy-edit.ts /opt/pi-agent/extensions/fuzzy-edit.ts

# subagents extension: task delegation to isolated `pi` subprocesses
# (single/parallel/chain modes, agents defined as markdown frontmatter);
# its agent defs and prompt templates go into the agent dir too
COPY extensions/subagents /opt/pi-agent/extensions/subagents/
COPY extensions/subagents/agents/ /opt/pi-agent/agents/
COPY extensions/subagents/prompts/ /opt/pi-agent/prompts/

# modal editor extension: vim-style modal editing (normal/insert modes,
# hjkl navigation, mode indicator on the editor border)
COPY extensions/modal-editor.ts /opt/pi-agent/extensions/modal-editor.ts

# user-borders extension: ASCII rules around user messages (keeps prompts
# visually distinct from model output on color-poor terminals)
COPY extensions/user-borders.ts /opt/pi-agent/extensions/user-borders.ts

# keybindings: up/down arrows scroll the transcript instead of browsing
# prompt history (fullscreen TUI mode)
COPY keybindings.json /opt/pi-agent/keybindings.json

# system-prompt addendum: appended to pi's system prompt
COPY APPEND_SYSTEM.md /opt/pi-agent/APPEND_SYSTEM.md

# serve-static: zero-dep static file server for frontend testing inside
# sessions (usage: serve-static [dir] [port])
COPY bin/serve-static.mjs /usr/local/bin/serve-static
RUN chmod +x /usr/local/bin/serve-static

# entrypoint: auto-login to opencode from env + preselect default model
COPY pi-entrypoint.sh /usr/local/bin/pi-entrypoint.sh
RUN chmod +x /usr/local/bin/pi-entrypoint.sh

# default model preselected by the entrypoint (provider/model)
ENV PI_DEFAULT_MODEL=opencode/glm-5.3-flash


WORKDIR /work
ENTRYPOINT ["pi-entrypoint.sh"]
# NOTE: no CMD here. A CMD arg would be forwarded as `exec pi "$@"`, making
# pi run as `pi pi` — and a positional arg to pi is an initial prompt, which
# shows up pre-typed in the TUI input box. No CMD => bare `pi`.
