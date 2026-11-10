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
