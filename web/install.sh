#!/bin/sh
# Armada agent installer for Linux (systemd).
#
#   curl -fsSL https://fleet.example.com/install.sh | \
#     sudo ARMADA_SERVER_URL=https://fleet.example.com ARMADA_ENROLLMENT_TOKEN=et_... sh
#
# Downloads the newest armada-agent release for this platform from the
# server, verifies its SHA-256, installs it as a systemd service and enrolls.
set -eu

: "${ARMADA_SERVER_URL:?set ARMADA_SERVER_URL to the Armada server URL}"
: "${ARMADA_ENROLLMENT_TOKEN:?set ARMADA_ENROLLMENT_TOKEN to an enrollment token}"
ARMADA_SERVER_URL=${ARMADA_SERVER_URL%/}
BIN=/usr/local/bin/armada-agent
CONF_DIR=/etc/armada
STATE_DIR=/var/lib/armada

fail() { echo "armada install: $*" >&2; exit 1; }

[ "$(id -u)" = 0 ] || fail "must run as root"
command -v curl >/dev/null || fail "curl is required"
command -v systemctl >/dev/null || fail "systemd is required (or install the agent manually)"

os=$(uname -s | tr 'A-Z' 'a-z')
case "$(uname -m)" in
  x86_64|amd64) arch=amd64 ;;
  aarch64|arm64) arch=arm64 ;;
  armv6l|armv7l) arch=arm ;;
  i386|i686) arch=386 ;;
  riscv64) arch=riscv64 ;;
  *) fail "unsupported architecture $(uname -m)" ;;
esac

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

echo "Downloading armada-agent for $os/$arch..."
curl -fsSL -D "$tmp/headers" -o "$tmp/armada-agent" \
  "$ARMADA_SERVER_URL/api/v1/agent/download?os=$os&arch=$arch" \
  || fail "download failed; has an armada-agent release for $os/$arch been uploaded?"
want=$(grep -i '^x-armada-sha256:' "$tmp/headers" | tr -d '\r' | awk '{print $2}')
if command -v sha256sum >/dev/null; then
  got=$(sha256sum "$tmp/armada-agent" | awk '{print $1}')
else
  got=$(openssl dgst -sha256 -r "$tmp/armada-agent" | awk '{print $1}')
fi
[ -n "$want" ] && [ "$want" = "$got" ] || fail "checksum mismatch (expected $want, got $got)"
chmod 0755 "$tmp/armada-agent"
"$tmp/armada-agent" -version >/dev/null || fail "downloaded agent does not run on this machine"

install -m 0755 "$tmp/armada-agent" "$BIN"
mkdir -p "$CONF_DIR" "$STATE_DIR"
chmod 0700 "$CONF_DIR" "$STATE_DIR"

if [ ! -f "$CONF_DIR/agent.json" ]; then
  umask 077
  cat > "$CONF_DIR/agent.json" <<JSON
{
  "server_url": "$ARMADA_SERVER_URL",
  "enrollment_token": "$ARMADA_ENROLLMENT_TOKEN",
  "state_dir": "$STATE_DIR",
  "auto_update": true,
  "allow_reboot": true,
  "allow_exec": false
}
JSON
  echo "Wrote $CONF_DIR/agent.json"
else
  echo "Keeping existing $CONF_DIR/agent.json"
fi

cat > /etc/systemd/system/armada-agent.service <<'UNIT'
[Unit]
Description=Armada fleet agent
Documentation=https://github.com/nasi-lemak/armada
Wants=network-online.target
After=network-online.target

[Service]
ExecStart=/usr/local/bin/armada-agent -config /etc/armada/agent.json
Restart=always
RestartSec=10
# The agent replaces its own binary on self-update and runs package hooks,
# so it needs root. Narrow this down if your install hooks allow it.
User=root

[Install]
WantedBy=multi-user.target
UNIT

systemctl daemon-reload
systemctl enable armada-agent >/dev/null
systemctl restart armada-agent
echo "armada-agent installed and running. Follow logs with: journalctl -u armada-agent -f"
