#!/bin/sh
# Armada agent installer for Linux (systemd) and macOS (launchd).
#
#   curl -fsSL https://fleet.example.com/install.sh | \
#     sudo ARMADA_SERVER_URL=https://fleet.example.com ARMADA_ENROLLMENT_TOKEN=et_... sh
#
# Downloads the newest armada-agent release for this platform from the
# server, verifies its SHA-256, installs it as a service (started at boot,
# restarted if it stops) and enrolls. For Windows, use install.ps1.
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
os=$(uname -s | tr 'A-Z' 'a-z')
case "$os" in
  linux) command -v systemctl >/dev/null || fail "systemd is required (or install the agent manually)" ;;
  darwin) ;;
  *) fail "unsupported system $os (install the agent manually)" ;;
esac
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
elif command -v shasum >/dev/null; then
  got=$(shasum -a 256 "$tmp/armada-agent" | awk '{print $1}')
else
  got=$(openssl dgst -sha256 -r "$tmp/armada-agent" | awk '{print $1}')
fi
[ -n "$want" ] && [ "$want" = "$got" ] || fail "checksum mismatch (expected $want, got $got)"
chmod 0755 "$tmp/armada-agent"
"$tmp/armada-agent" -version >/dev/null || fail "downloaded agent does not run on this machine"

mkdir -p "$(dirname "$BIN")" # not there on a new Mac
install -m 0755 "$tmp/armada-agent" "$BIN"
mkdir -p "$CONF_DIR" "$STATE_DIR"
chmod 0700 "$CONF_DIR" "$STATE_DIR"

log_files=
[ "$os" = darwin ] && log_files=',
  "log_files": { "agent": "/var/log/armada-agent.log" }'
if [ ! -f "$CONF_DIR/agent.json" ]; then
  umask 077
  cat > "$CONF_DIR/agent.json" <<JSON
{
  "server_url": "$ARMADA_SERVER_URL",
  "enrollment_token": "$ARMADA_ENROLLMENT_TOKEN",
  "state_dir": "$STATE_DIR",
  "auto_update": true,
  "allow_reboot": true,
  "allow_exec": false$log_files
}
JSON
  echo "Wrote $CONF_DIR/agent.json"
else
  echo "Keeping existing $CONF_DIR/agent.json"
fi

if [ "$os" = darwin ]; then
  # launchd keeps the agent running and starts it at boot. It logs to its
  # own file (kept under 10 MB); launchd's file only catches crashes.
  plist=/Library/LaunchDaemons/com.armada.agent.plist
  mkdir -p /Library/LaunchDaemons
  cat > "$plist" <<'PLIST'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>com.armada.agent</string>
  <key>ProgramArguments</key>
  <array>
    <string>/usr/local/bin/armada-agent</string>
    <string>-config</string><string>/etc/armada/agent.json</string>
    <string>-log-file</string><string>/var/log/armada-agent.log</string>
  </array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>ThrottleInterval</key><integer>10</integer>
  <key>StandardErrorPath</key><string>/var/log/armada-agent.stderr.log</string>
</dict>
</plist>
PLIST
  chown root:wheel "$plist"
  chmod 0644 "$plist"
  launchctl bootout system/com.armada.agent 2>/dev/null || true
  launchctl bootstrap system "$plist" || fail "launchctl could not start the agent"
  echo "armada-agent installed and running. Logs: /var/log/armada-agent.log"
  exit 0
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
