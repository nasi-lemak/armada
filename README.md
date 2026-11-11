# Armada

Armada is a self-hosted fleet management system for Linux devices such as kiosks,
gateways, edge boxes and servers. It keeps an inventory of every device, shows
their health, and **rolls out software updates automatically** with staged canaries,
health checks, automatic rollback and failure-triggered pauses.

It has three parts, all in Go with no external services:

| Component | What it does |
|---|---|
| **`armada-server`** | Control plane: REST API, rollout engine, SQLite storage, signed artifact store, web dashboard |
| **`armada-agent`** | Runs on each device: checks in, reports health, installs updates (including updates to itself), runs remote actions |
| **`armadactl`** | CLI for the admin API |

```
            ┌───────────── armada-server ─────────────┐
 browser ──▶│ dashboard   admin API   rollout engine  │◀── armadactl
            │        SQLite   artifacts   ed25519 key │
            └───────────────────▲─────────────────────┘
                                │ HTTPS: enroll, heartbeat (every 30s),
                                │ download signed artifact, report status
          ┌─────────────┬───────┴─────┬─────────────┐
     armada-agent  armada-agent  armada-agent   …  (each device)
```

## Features

- **Inventory and health.** Hostname, OS/arch, kernel, IPs, uptime, load, memory and disk,
  installed package versions, and online/offline status.
- **Grouping.** Each device has a *channel* (`stable`, `beta`, …) and free-form *labels*
  (`site=sg`, `role=kiosk`). Deployments target any combination of these, or specific device IDs.
- **Auto-update.** Upload a release with `--auto-deploy` and every device on that channel
  updates through a staged rollout without further work.
- **Staged rollouts.** Cumulative stages such as `5% → 25% → 50% → 100%`, each with a soak
  time. Each rollout picks a different random slice of the fleet for its canary
  (deterministic per device).
- **Safety.**
  - On the device: each install runs your health check, and the agent rolls back if it fails.
  - On the server: a rollout **auto-pauses** once its failure rate passes a threshold, so a
    bad release stops at the canary.
- **Agent self-update.** The agent replaces its own binary and restarts in place. If the new
  binary crash-loops, the agent restores the previous one.
- **Any software.** A *package* is anything with an install hook: a tarball, a container
  image tag, a firmware blob or a `.deb`. Hooks get the artifact path and versions as
  environment variables.
- **Signed artifacts.** Every release is ed25519-signed by the server. Agents verify the
  signature and SHA-256 before installing anything. You can pin the key in the agent config.
- **Maintenance windows.** Devices can be limited to installing at certain local times
  (`02:00-05:00`).
- **Remote actions.** Reboot, restart the agent, or run a command. `exec` is off by default
  and each device opts in.
- **Resumable downloads** for devices on poor links (HTTP Range).
- **Activity log** of enrollments, updates, failures, pauses and actions.
- **Web dashboard** built into the server binary (no build step, strict CSP).
- **One-line onboarding** with `curl …/install.sh | sh`.

## Quick start

```bash
make build            # bin/armada-server, bin/armada-agent, bin/armadactl
```

```bash
./bin/armada-server -data-dir ./data
```

On first start the server generates an admin token, prints it, and saves it to
`./data/admin-token`. Open http://localhost:8080 and sign in with it.

Point the CLI at the server:

```bash
export ARMADA_SERVER=http://localhost:8080
```

```bash
export ARMADA_ADMIN_TOKEN=$(cat data/admin-token)
```

Create an enrollment token for new devices:

```bash
./bin/armadactl tokens create --channel stable --label site=sg --ttl 168h
```

Start an agent (here on the same machine, with local state):

```bash
./bin/armada-agent --server http://localhost:8080 --token et_... --state-dir ./agent-state --config ""
```

### Onboard a real device

1. Upload agent binaries so devices can download them:

   ```bash
   make agents VERSION=1.0.0
   ```

   ```bash
   ./bin/armadactl releases upload --package armada-agent --version 1.0.0 --os linux --arch amd64 dist/armada-agent_1.0.0_linux_amd64
   ```

2. On the device, as root:

   ```bash
   curl -fsSL https://fleet.example.com/install.sh | sudo ARMADA_SERVER_URL=https://fleet.example.com ARMADA_ENROLLMENT_TOKEN=et_... sh
   ```

The script downloads the newest agent for the device's architecture, verifies its
checksum, writes `/etc/armada/agent.json` and installs a systemd service. The
dashboard's **Enrollment** page shows this command, filled in, when you create a token.

### Ship an update

```bash
./bin/armadactl releases upload --package myapp --version 2.1.0 --auto-deploy myapp-2.1.0.tar.gz
```

This creates a deployment to the `stable` channel using the default strategy:
5% → 25% → 50% → 100%, 15 minutes per stage, pausing above 20% failures. Follow it with:

```bash
./bin/armadactl deployments list
```

## Concepts

**Package.** A named piece of software on a device, such as `myapp`. The agent reports the
installed version of each package. `armada-agent` is a built-in package: releases of it
update the agent itself.

**Release.** An uploaded artifact for `(package, version, os?, arch?)` on a channel. The
server stores it, hashes it and signs it.

**Deployment.** Rolls a release out to the devices matching a *selector*
(channel, labels and/or device IDs). Its *strategy* sets the stages, the soak time,
auto/manual advance, the failure threshold, and whether downgrades are allowed.

How devices are chosen:

- A device is in the current stage if `hash(deployment, device) mod 100` is below the
  stage percentage.
- When several deployments target the same package, the **newest one that includes the
  device in its current stage wins**. A new canary never fights an older full rollout, and
  a rollback deployment takes precedence over the release it replaces.
- Devices are never downgraded unless the deployment sets `allow_downgrade`.
- A device that failed a deployment is not offered it again until you **Retry failed**.

Deployment lifecycle:

```
running ──(failure rate > limit)──▶ paused ──resume──▶ running
   │  advance (soak time elapsed, or manual)                │
   └──────────────▶ complete / abort ◀──────────────────────┘
```

A running deployment keeps offering its release to devices that enroll later.
**Complete** retires it. **Resume** counts the existing failures as acknowledged, so it
does not pause again immediately.

**Rolling back a bad release:** abort its deployment, then deploy the previous release
with `--allow-downgrade`.

## Agent configuration

`/etc/armada/agent.json` (see [`deploy/agent.example.json`](deploy/agent.example.json)):

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
| `reboot_command` | `["systemctl","reboot"]` | |
| `command_timeout` | `10m` | Applies to hooks and exec actions |

### Package hooks

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
to. [`examples/myapp/`](examples/myapp) has a complete set of hooks for a tarball deployed
by symlink switching.

`version_command` must print exactly the version string you upload (for example `2.1.0`).
Without it, the agent reports the last version it installed.

### Self-update safety

The agent stages the new binary and runs `-version` on it to check that it starts and is
the expected version. It then swaps the binary (keeping `armada-agent.prev`) and re-execs.
The new process confirms the update after its first successful heartbeat. If the new
binary starts three times without completing a heartbeat, the agent restores the previous
binary and reports `rolled_back`.

## CLI

```
armadactl status
armadactl devices list [--channel C] [--label K=V] [--status online|offline]
armadactl devices get <id>
armadactl devices set <id> [--name N] [--channel C] [--label K=V]... [--unlabel K]...
armadactl devices action <id> reboot|restart-agent
armadactl devices action <id> exec -- systemctl status myapp
armadactl devices delete <id>
armadactl tokens create|list|revoke
armadactl releases upload|list|delete
armadactl deployments create --release <id> [--channel C] [--label K=V] [--device ID]
                             [--stages 5,25,100] [--stage-duration 15m] [--max-failure-rate 0.2]
                             [--min-samples 3] [--manual] [--allow-downgrade]
armadactl deployments list|get|pause|resume|advance|retry|complete|abort
armadactl events [--device ID] [--deployment ID]
```

Add `--json` to any command for machine-readable output.

## Server configuration

| Flag | Env | Default |
|---|---|---|
| `-listen` | `ARMADA_LISTEN` | `:8080` |
| `-data-dir` | `ARMADA_DATA_DIR` | `./data` |
| `-admin-token` | `ARMADA_ADMIN_TOKEN` | generated into `<data-dir>/admin-token` |
| `-tls-cert`, `-tls-key` | `ARMADA_TLS_CERT`, `ARMADA_TLS_KEY` | plain HTTP |
| `-heartbeat-interval` | `ARMADA_HEARTBEAT_INTERVAL` | `30s` (sent to agents) |
| `-offline-after` | `ARMADA_OFFLINE_AFTER` | `2m` |
| `-update-timeout` | `ARMADA_UPDATE_TIMEOUT` | `1h` (stuck installs become failures) |
| `-default-stages` | `ARMADA_DEFAULT_STAGES` | `5,25,50,100` |
| `-default-stage-duration` | `ARMADA_DEFAULT_STAGE_DURATION` | `15m` |
| `-default-max-failure-rate` | | `0.2` |
| `-max-artifact-mb` | | `2048` |

The data directory holds `armada.db` (SQLite), `artifacts/`, `signing.key` and
`admin-token`. **Back it up.** Losing `signing.key` means agents that pinned the old key
reject new releases.

Deployment options: [`deploy/armada-server.service`](deploy/armada-server.service)
(systemd), [`Dockerfile`](Dockerfile) and
[`deploy/docker-compose.yml`](deploy/docker-compose.yml). In production, put the server
behind TLS, either with `-tls-cert`/`-tls-key` or a reverse proxy.

## API

All admin endpoints need `Authorization: Bearer <admin token>`. Agent endpoints use the
per-device key issued at enrollment.

| Method & path | Purpose |
|---|---|
| `GET /api/v1/summary` | Fleet counts and version distribution |
| `GET /api/v1/devices` (`?channel=&label=k=v&status=`) | List devices |
| `GET/PATCH/DELETE /api/v1/devices/{id}` | Device detail (with updates, actions, events), edit name/channel/labels, remove |
| `GET/POST /api/v1/devices/{id}/actions` | List or queue `reboot`, `restart-agent`, `exec` |
| `GET/POST /api/v1/tokens`, `DELETE /api/v1/tokens/{id}` | Enrollment tokens |
| `GET/POST /api/v1/releases`, `GET/DELETE /api/v1/releases/{id}`, `GET …/artifact` | Releases (upload is `multipart/form-data`) |
| `GET/POST /api/v1/deployments`, `GET /api/v1/deployments/{id}` | Deployments |
| `POST /api/v1/deployments/{id}/{pause,resume,advance,retry,complete,abort}` | Control a rollout |
| `GET /api/v1/events` | Activity log |
| `GET /api/v1/signing-key` | Release-signing public key |
| `POST /api/v1/agent/enroll`, `/heartbeat`, `/updates`, `/actions/{id}/result`, `GET /api/v1/agent/releases/{id}/artifact` | Agent protocol |
| `GET /install.sh`, `GET /api/v1/agent/download?os=&arch=` | Public bootstrap (agent binary only) |

## Security model

- Admin and device credentials are random 256-bit secrets. The server stores only their
  SHA-256 hashes. Deleting a device revokes its key immediately.
- Enrollment tokens can expire, have a use limit, and be revoked.
- A device can download only the artifacts it has been offered. The only public download
  is the newest `armada-agent` binary, which `install.sh` uses.
- Releases are signed with a server-held ed25519 key over
  `(package, version, sha256)`. An artifact therefore can't be replayed under another
  name or version.
- `exec` actions are disabled unless a device sets `allow_exec: true`.
- The dashboard sends a strict Content-Security-Policy and renders all device-supplied
  text as text.

## Development

```bash
make vet test         # gofmt, go vet, unit and end-to-end tests
```

```bash
make test-race        # same with the race detector (needs cgo)
```

The end-to-end tests run a real server and agent together. They cover enrollment, a
package update, a failed health check with rollback, the auto-pause, remote actions, a
self-update with the restart simulated, and crash-loop rollback.
`internal/server/server_test.go` simulates a 200-device fleet through a staged rollout.

Layout:

```
cmd/armada-server   server entrypoint
cmd/armada-agent    agent entrypoint
cmd/armadactl       CLI
internal/api        wire types shared by everything
internal/rollout    pure rollout logic: targeting, staging, version ordering, health evaluation
internal/store      SQLite persistence
internal/server     HTTP API, agent protocol, rollout engine
internal/agent      agent: enrollment, heartbeat, updates, self-update, actions
web/                embedded dashboard and install.sh
deploy/, examples/  service units, compose file, sample agent config and hooks
```

## Limitations and next steps

- A single server with SQLite comfortably handles thousands of devices at a 30s
  heartbeat. Larger fleets would need Postgres and several server replicas.
- There is one admin token, with no per-user accounts, roles or audit attribution.
- Agents poll; there is no push channel, so actions run on the next check-in (30s by default).
- There are no delta updates; every update downloads the full artifact.
- The agent's metrics are richest on Linux. Other platforms report the basics.
