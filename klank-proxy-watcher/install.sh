#!/usr/bin/env bash
# Build and install klank-proxy-watcher as a systemd service on the host.
#
#   ./install.sh <username>     e.g. ./install.sh okoshovetc
#
# Installs:
#   /usr/local/bin/klank-proxy-watcher                    (static binary)
#   /etc/systemd/system/klank-proxy-watcher@.service      (template unit)
# then enables klank-proxy-watcher@<username>.
set -euo pipefail

[[ $# -eq 1 ]] || { echo "usage: $0 <username>" >&2; exit 1; }
user="$1"
id "$user" >/dev/null 2>&1 || { echo "no such user: $user" >&2; exit 1; }
[[ -d "~$user/.klank" || -d "$(eval echo ~$user)/.klank" ]] || {
    echo "warning: $(eval echo ~$user)/.klank does not exist yet (klank creates it on first run)" >&2
}

cd "$(dirname "$0")"

echo "==> building (static, no cgo)"
CGO_ENABLED=0 go build -ldflags="-s -w" -o klank-proxy-watcher .

if ! command -v caddy >/dev/null 2>&1; then
    echo "error: caddy is not installed -- it is a hard dependency (the daemon
  serves the routes and dashboard through it and exits without it)."
    echo "install it first, e.g.:  sudo apt install caddy   (or see https://caddyserver.com)"
    exit 1
fi

echo "==> installing binary and unit"
sudo install -m 0755 klank-proxy-watcher /usr/local/bin/klank-proxy-watcher
sudo install -m 0644 klank-proxy-watcher@.service /etc/systemd/system/

echo "==> enabling klank-proxy-watcher@$user"
sudo systemctl daemon-reload
sudo systemctl enable --now "klank-proxy-watcher@$user.service"

echo "==> done. routes:"
systemctl status "klank-proxy-watcher@$user.service" --no-pager -n 5 || true
journalctl -u "klank-proxy-watcher@$user" -n 20 --no-pager || true
