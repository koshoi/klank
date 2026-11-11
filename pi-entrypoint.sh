#!/bin/bash
# pi-entrypoint.sh - container entrypoint for pi coding agent.
#
# Reads env vars on startup, logs into opencode with the provided API key,
# and preselects the default model before exec'ing pi.
#
# env:
#   OPENCODE_API_KEY    opencode API key (falls back to OPENAI_API_KEY)
#   PI_DEFAULT_MODEL    default model as "provider/model" (default: opencode/glm-5.3-flash)
set -euo pipefail

PI_DIR="${PI_CODING_AGENT_DIR:-$HOME/.pi/agent}"
AUTH_FILE="$PI_DIR/auth.json"
SETTINGS_FILE="$PI_DIR/settings.json"
DEFAULT_MODEL="${PI_DEFAULT_MODEL:-opencode/glm-5.3-flash}"

api_key="${OPENCODE_API_KEY:-${OPENAI_API_KEY:-}}"

mkdir -p "$PI_DIR"

# --- login: write opencode api key into auth.json (merge if it exists) ---
if [[ -n "$api_key" ]]; then
  if command -v jq >/dev/null 2>&1; then
    if [[ -f "$AUTH_FILE" ]]; then
      jq --arg key "$api_key" '.opencode = {"type": "api_key", "key": $key}' \
        "$AUTH_FILE" > "$AUTH_FILE.tmp" && mv "$AUTH_FILE.tmp" "$AUTH_FILE"
    else
      jq -n --arg key "$api_key" '{opencode: {"type": "api_key", "key": $key}}' \
        > "$AUTH_FILE"
    fi
  else
    # no jq available: overwrite (single-provider setup)
    printf '{"opencode":{"type":"api_key","key":"%s"}}' "$api_key" > "$AUTH_FILE"
  fi
  chmod 600 "$AUTH_FILE"
  echo "[pi-entrypoint] opencode login configured"
else
  echo "[pi-entrypoint] no OPENCODE_API_KEY/OPENAI_API_KEY set; skipping login" >&2
fi

# --- install the klank bridge extension (external message inbox) ---
# Lets `klank send` drop messages into /tmp/pi-inbox inside the container;
# the extension injects them into the running pi session as user messages.
BRIDGE_SRC=/opt/pi-bridge/bridge.ts
if [[ -f "$BRIDGE_SRC" ]]; then
  mkdir -p "$PI_DIR/extensions"
  cp -f "$BRIDGE_SRC" "$PI_DIR/extensions/bridge.ts"
  echo "[pi-entrypoint] klank bridge extension installed"
fi

# --- install the edit-guard extension (snapshot edit guardrails) ---
# Also installs hashline.ts (shared fingerprint/formatting primitives) and
# hashline-read.ts (read override that emits [path#TAG] + N:-numbered lines);
# edit-guard.ts imports ./hashline.ts, so all three must live side by side.
GUARD_SRC=/opt/pi-bridge/edit-guard.ts
if [[ -f "$GUARD_SRC" ]]; then
  mkdir -p "$PI_DIR/extensions"
  cp -f "$GUARD_SRC" "$PI_DIR/extensions/edit-guard.ts"
  for ext in hashline.ts hashline-read.ts; do
    if [[ -f "/opt/pi-bridge/$ext" ]]; then
      cp -f "/opt/pi-bridge/$ext" "$PI_DIR/extensions/$ext"
    fi
  done
  echo "[pi-entrypoint] edit-guard extension installed"
fi

# --- install the subagents extension (task delegation to child pi processes) ---
SUB_SRC=/opt/pi-bridge/subagents
if [[ -d "$SUB_SRC" ]]; then
  mkdir -p "$PI_DIR/extensions/subagents" "$PI_DIR/agents" "$PI_DIR/prompts"
  cp -f "$SUB_SRC/index.ts" "$SUB_SRC/agents.ts" "$PI_DIR/extensions/subagents/"
  cp -f "$SUB_SRC"/agents/*.md "$PI_DIR/agents/"
  cp -f "$SUB_SRC"/prompts/*.md "$PI_DIR/prompts/"
  echo "[pi-entrypoint] subagents extension installed"
fi

# --- install the user-borders extension (ASCII rules around user messages) ---
BORDERS_SRC=/opt/pi-bridge/user-borders.ts
if [[ -f "$BORDERS_SRC" ]]; then
  cp -f "$BORDERS_SRC" "$PI_DIR/extensions/user-borders.ts"
  echo "[pi-entrypoint] user-borders extension installed"
fi

# --- preselect default model in settings.json (merge if it exists) ---
provider="${DEFAULT_MODEL%%/*}"
model="${DEFAULT_MODEL#*/}"
if [[ -f "$SETTINGS_FILE" ]]; then
  jq --arg p "$provider" --arg m "$model" \
    '.defaultProvider = $p | .defaultModel = $m' \
    "$SETTINGS_FILE" > "$SETTINGS_FILE.tmp" && mv "$SETTINGS_FILE.tmp" "$SETTINGS_FILE"
else
  jq -n --arg p "$provider" --arg m "$model" \
    '{defaultProvider: $p, defaultModel: $m}' > "$SETTINGS_FILE"
fi
echo "[pi-entrypoint] default model: $provider/$model"

exec pi "$@"
