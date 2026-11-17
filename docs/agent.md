# Agent configuration

`/etc/armada/agent.json` (see [`deploy/agent.example.json`](../deploy/agent.example.json)):

```json
{
  "server_url": "https://fleet.example.com",
  "enrollment_token": "et_...",
  "state_dir": "/var/lib/armada",
  "signing_public_key": "",
  "auto_update": true,
  "maintenance_window": "02:00-05:00",
  "allow_reboot": true,
  "allow_exec": false,
  "allow_logs": true,
  "log_files": { "myapp": "/var/log/myapp/app.log" },
  "packages": {
    "myapp": {
      "install_command":  ["/opt/myapp/hooks/install.sh"],
      "health_command":   ["/opt/myapp/hooks/health.sh"],
      "health_retries":   5,
      "health_interval":  "5s",
      "rollback_command": ["/opt/myapp/hooks/rollback.sh"],
      "version_command":  ["cat", "/opt/myapp/VERSION"]
    }
  }
}
```

| Key | Default | Meaning |
|---|---|---|
| `server_url` | required | Server URL (flag `--server`, env `ARMADA_SERVER_URL`) |
| `enrollment_token` | | Needed only for the first run (flag `--token`, env `ARMADA_ENROLLMENT_TOKEN`) |
| `state_dir` | `/var/lib/armada` | Device identity, downloads, kept artifacts |
| `signing_public_key` | key received at enrollment | Pins the release-signing key (get it from `GET /api/v1/signing-key`) |
| `ca_cert` | system roots | PEM bundle for a private CA |
| `auto_update` | `true` | When `false`, the agent only reports that updates are available |
| `maintenance_window` | any time | Local-time windows such as `02:00-05:00,13:00-14:00`; windows may cross midnight |
| `allow_reboot` / `allow_exec` | `true` / `false` | Which remote actions this device accepts |
| `allow_screenshots` | `false` | Accept remote screenshots (the server needs `-screenshots` too) |
| `screenshot_command` | auto | A command that writes a PNG of the screen to stdout, for setups the agent can't capture by itself |
| `allow_logs` | `true` | Whether the server may read the systemd journal and the files in `log_files` |
| `log_files` | none | Log files the server may ask for, by name: `{"myapp": "/var/log/myapp/app.log"}`. No other files can be read. |
| `disable_push` | `false` | Turn off the long-poll connection; actions then wait for the next heartbeat |
| `disable_delta` | `false` | Always download updates in full instead of reusing chunks of the installed version |
| `managed_config` | none | Directories configuration profiles may write files into, by name, each with an optional `reload_command` |
| `managed_certs` | none | Certificates the server may renew by CSR, by name: files, owner, `reload_command`, `verify_address` (see [Certificates on devices](certificates.md)) |
| `allow_remote_settings` | `true` | Whether `agent` profiles may override `maintenance_window` and `auto_update` |
| `reboot_command` | `["systemctl","reboot"]` | |
| `command_timeout` | `10m` | Applies to hooks and exec actions |

## Resource use

Measured on Linux (amd64) with the default settings: a 30-second check-in and push enabled.

| | |
|---|---|
| Binary | about 8 MB |
| Memory, idle | about 13 MB resident |
| Memory, installing a 300 MB update | peaks at about 12 MB (full download) and 16 MB (delta update): downloads stream to disk |
| CPU, idle | about 0.3 seconds an hour |
| Disk | the binary, a small state file, and update downloads while they install |
| Data, idle | about 140 KB an hour, roughly 100 MB a month, before TLS and TCP/IP overhead |

Most of the data is check-ins, which carry health results. The agent compresses them once the server
says it accepts compressed bodies (servers from this version on; older servers get them plain),
which halves the upload. To use less on metered links:

- **Check in less often:** `-heartbeat-interval` on the server (for example `2m` quarters the data).
  Actions still arrive at once over the push connection.
- **Delta updates** (on by default) download only the parts of a release that changed.

## Package hooks

Hooks are argv arrays run **without a shell**. They receive:

| Variable | Value |
|---|---|
| `ARMADA_PACKAGE` | package name |
| `ARMADA_VERSION` | version being installed |
| `ARMADA_ARTIFACT` | path to the verified artifact |
| `ARMADA_PREVIOUS_VERSION` | version installed before this update |
| `ARMADA_PREVIOUS_ARTIFACT` | path to the previous artifact (kept for rollback) |

The update flow is: download, verify the SHA-256 and signature, run `install_command`,
then run `health_command` (retried). If install or the health check fails, the agent runs
`rollback_command` and reports `rolled_back`, or `failed` if there was nothing to roll back
to. [`examples/myapp/`](../examples/myapp) has a complete set of hooks for a tarball deployed
by symlink switching.

**Whole-OS (A/B) image updates** with RAUC, SWUpdate or Mender use `reboot_required: true`. The agent
reboots after `install_command`. On the next start it checks which version booted, runs the health
check, then `confirm_command` (for example `rauc status mark-good`). If the bootloader fell back, or the
health check fails, the update is reported as rolled back. See [`examples/os-image/`](../examples/os-image).

`version_command` must print exactly the version string you upload (for example `2.1.0`).
Without it, the agent reports the last version it installed.

## Self-update safety

The agent stages the new binary and runs `-version` on it to check that it starts and is
the expected version. It then swaps the binary (keeping `armada-agent.prev`) and re-execs.
The new process confirms the update after its first successful heartbeat. If the new
binary starts three times without completing a heartbeat, the agent restores the previous
binary and reports `rolled_back`.

---

[← Armada documentation](../README.md#documentation)
