FROM node:24-bookworm-slim

RUN apt-get update \
  && apt-get install -y --no-install-recommends bash ca-certificates git ripgrep curl jq fzf \
  && rm -rf /var/lib/apt/lists/*

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

# allow any user (incl. anonymous uids passed via --user) to use sudo
# NOTE: for apt/system only -- never write to /work as root
RUN apt-get update \
  && apt-get install -y --no-install-recommends sudo \
  && echo 'ALL ALL=(ALL) NOPASSWD:ALL' > /etc/sudoers.d/all-users \
  && chmod 0440 /etc/sudoers.d/all-users \
  && rm -rf /var/lib/apt/lists/*

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
