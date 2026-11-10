FROM node:24-bookworm-slim

RUN apt-get update \
  && apt-get install -y --no-install-recommends bash ca-certificates git ripgrep curl jq fzf \
  && rm -rf /var/lib/apt/lists/*

# golang (hardcoded current latest: go1.27.3)
RUN set -eux; \
  curl -fsSLO "https://go.dev/dl/go1.27.3.linux-amd64.tar.gz"; \
  rm -rf /usr/local/go \
  && tar -C /usr/local -xzf go1.27.3.linux-amd64.tar.gz \
  && rm go1.27.3.linux-amd64.tar.gz
ENV PATH="/usr/local/go/bin:${PATH}"

RUN npm install -g --ignore-scripts @earendil-works/pi-coding-agent

# klank bridge extension: lets `klank send` inject user messages into a
# running pi TUI session (the entrypoint installs it into the extensions dir)
COPY klank/bridge.ts /opt/pi-bridge/bridge.ts

# edit-guard extension: snapshot-based edit guardrails (staleness detection,
# bash intent interceptor) — installed into the extensions dir by the entrypoint
COPY extensions/edit-guard.ts /opt/pi-bridge/edit-guard.ts

# entrypoint: auto-login to opencode from env + preselect default model
COPY pi-entrypoint.sh /usr/local/bin/pi-entrypoint.sh
RUN chmod +x /usr/local/bin/pi-entrypoint.sh

# default model preselected by the entrypoint (provider/model)
ENV PI_DEFAULT_MODEL=opencode/glm-5.3-flash

# allow any user (incl. anonymous uids passed via --user) to use sudo
# NOTE: for apt/system only -- never write to /work as root
RUN apt-get update \
  && apt-get install -y --no-install-recommends sudo \
  && echo 'ALL ALL=(ALL) NOPASSWD:ALL' > /etc/sudoers.d/all-users \
  && chmod 0440 /etc/sudoers.d/all-users \
  && rm -rf /var/lib/apt/lists/*

WORKDIR /work
ENTRYPOINT ["pi-entrypoint.sh"]
# NOTE: no CMD here. A CMD arg would be forwarded as `exec pi "$@"`, making
# pi run as `pi pi` — and a positional arg to pi is an initial prompt, which
# shows up pre-typed in the TUI input box. No CMD => bare `pi`.
