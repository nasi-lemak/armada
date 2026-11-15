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
| **Android agent** (`android/agent`) | Device Owner app for Android devices you ship to customers: silent app updates and device-bound app licenses |
| **Android client** (`android/client`) | Small library your Android app uses to get its license and sign requests to your backend |

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
  (deterministic per device). Rollouts can be scheduled and limited to time windows
  (for example weeknights 22:00-05:00 in the customer's time zone).
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
- **Install windows.** Devices can be limited to installing at certain local times
  (`02:00-05:00`, the agent's `maintenance_window`).
- **Maintenance mode.** Put devices into planned maintenance, one-off or recurring: their alerts are
  held and rollouts skip them. Firing alerts can be **acknowledged**, which stops repeat notifications.
- **Device lifecycle.** Track spares and devices in repair, swap a broken device for a spare in one
  step (RMA), and retire devices for good while keeping their history.
- **Bulk operations.** Reboot, run a command, relabel, change channel, start maintenance or change
  lifecycle on many devices at once. You preview first, each device is checked on its own, and
  progress is tracked per device.
- **Certificates on devices.** Track the TLS certificates your applications use (Mosquitto, web
  servers…) and renew them with your clients' CAs: a new key and CSR are made on each device (keys
  never leave it), the CSRs go to the client (download, email, or their own login), and the signed
  certificates are matched, checked and installed with automatic rollback. Renewal policies start
  all this before expiry on their own, and remind the client until the certificates come back.
- **Customer reports.** Availability, alerts, updates, replacements and maintenance for a client
  over a week, a month or any dates. View them in the dashboard, download them as CSV or a printable
  page, or have them emailed every week or month.
- **Remote actions, in seconds.** Reboot, restart the agent, run a command, or ask for a check-in.
  Each agent holds a long-poll connection, so actions reach it within a second or two, even behind
  NAT, without opening ports. `exec` is off by default and each device opts in.
- **Remote logs.** Fetch recent systemd journal lines (any unit) or named log files from a device,
  filtered by time and text, from the dashboard or `armadactl devices logs`.
- **Delta updates.** Devices download only the parts of a release that differ from the version they
  have (content-defined chunks), from any older version, with no diffs to prepare. Resumable full
  downloads for everything else.
- **Activity log** of enrollments, updates, failures, pauses and actions, with who did each:
  - filter by type, who did it, text and dates;
  - export as CSV;
  - filters are kept in the address (`#/activity?type=device.*&actor=system`).
- **Day-to-day tools:**
  - [quick search](docs/inventory.md) (Ctrl+K);
  - a timeline on each device;
  - [CSV import and pre-registration by serial](docs/inventory.md#pre-registration-and-bulk-import);
  - a [customer/project picker](docs/customers.md), with a *Needs attention* view: software
    not yet updated, certificates to renew, and disconnections;
  - [scheduled actions](docs/bulk-operations.md#scheduled-actions), such as nightly reboots;
  - printable [QR asset labels](docs/inventory.md);
  - [event webhooks](docs/api.md#event-webhooks);
  - an [OpenAPI reference](docs/api.md) generated from the server.
- **Health monitoring** ([details](docs/health.md)):
  - Hardware checks run automatically. They cover SSD/HDD SMART data, NVMe wear, eMMC wear, SD-card
    failure signs, RAID, temperature, power and under-voltage, battery, disk space and memory.
  - You can add checks for your programs, defined on the server: systemd unit, process, HTTP, TCP, or any
    Nagios-style command.
- **Alerts** to any webhook (with a body template and signature, for PagerDuty, Teams, Opsgenie and
  similar), Slack or email. They cover devices going offline, unhealthy checks, failed updates and paused
  rollouts.
- **BLE and other peripherals** ([details](docs/peripherals.md)): any agent can be a
  gateway that discovers nearby devices and rolls out their firmware. Drivers exist for MCUboot/Zephyr,
  Nordic DFU, or your own tool.
- **Users, roles and client accounts** ([details](docs/users.md)): viewer, operator
  and admin roles, and accounts limited to one customer's devices. Every change in the activity log names
  who made it. Sign in with single sign-on (Google, Microsoft, Okta or any OpenID Connect provider,
  with roles from groups), passkeys (passwordless, or as a second step), and authenticator codes.
  Two-factor sign-in can be required. SCIM provisioning lets your directory create and deactivate
  accounts automatically.
- **Web dashboard** built into the server binary (no build step, strict CSP).
- **One-line onboarding** on Linux and macOS (`curl …/install.sh | sh`) and Windows
  (`irm …/install.ps1 | iex`), installing the agent as a service.
- **Android devices you provide to customers** ([details](docs/android.md)):
  - Updates to your app go out silently, without Google Play.
  - Your app only runs on devices you enrolled. A copied APK won't work anywhere else, because your
    backend requires a license bound to the device's secure hardware.
  - App settings per customer or site, OS update timing, and remote lock and wipe.
- **Configuration profiles** ([details](docs/configuration.md)): files, agent settings and Android app
  settings kept applied on matching devices, with per-device templates, rollback on failed reload, and
  compliance reporting.

## Quick start

```bash
make build            # bin/armada-server, bin/armada-agent, bin/armadactl
```

```bash
./bin/armada-server -data-dir ./data
```

On first start the server generates an admin token, prints it, and saves it to
`./data/admin-token`. Open http://localhost:8080, choose **Sign in with a token instead**, and
paste it. Then add a user for yourself under **Users**, and sign in with email and password from then on.

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

2. On the device. Linux or macOS:

   ```bash
   curl -fsSL https://fleet.example.com/install.sh | sudo ARMADA_SERVER_URL=https://fleet.example.com ARMADA_ENROLLMENT_TOKEN=et_... sh
   ```

   Windows, in PowerShell run as Administrator:

   ```powershell
   $env:ARMADA_SERVER_URL='https://fleet.example.com'; $env:ARMADA_ENROLLMENT_TOKEN='et_...'; irm https://fleet.example.com/install.ps1 | iex
   ```

The script downloads the newest agent for the device's system and architecture (upload a release for
each: `--os darwin`, `--os windows`), verifies its checksum, writes the config and starts the agent
as a service that runs at boot and is restarted if it stops. The dashboard's **Enrollment** page shows
these commands, filled in, when you create a token. Running the script again updates the agent and
keeps the config.

| | Linux | macOS | Windows |
|---|---|---|---|
| Service | systemd `armada-agent` | launchd `com.armada.agent` | service `armada-agent` |
| Binary | `/usr/local/bin/armada-agent` | `/usr/local/bin/armada-agent` | `C:\Program Files\Armada\armada-agent.exe` |
| Config | `/etc/armada/agent.json` | `/etc/armada/agent.json` | `C:\ProgramData\Armada\agent.json` |
| State | `/var/lib/armada` | `/var/lib/armada` | `C:\ProgramData\Armada\state` |
| Logs | `journalctl -u armada-agent` | `/var/log/armada-agent.log` | `C:\ProgramData\Armada\agent.log` |
| Reboot action | `systemctl reboot` | `shutdown -r now` | `shutdown /r /t 0` |

On macOS and Windows the agent's log is also available from the dashboard (device → Logs, file
`agent`). The config and state directories are readable only by root (SYSTEM and Administrators on
Windows), since the device key is kept there. On Windows, `armada-agent.exe -install-service -config
<file>` installs or updates the service and `-uninstall-service` removes it; self-updates and the
restart action exit and rely on the service's restart-on-failure setting, which `-install-service`
sets.

### Ship an update

```bash
./bin/armadactl releases upload --package myapp --version 2.1.0 --auto-deploy myapp-2.1.0.tar.gz
```

This creates a deployment to the `stable` channel using the default strategy:
5% → 25% → 50% → 100%, 15 minutes per stage, pausing above 20% failures. The 5% canary starts right
away. Going further needs a second person to approve it (see [rollout approval](docs/security.md)):

```bash
./bin/armadactl deployments approve <deployment-id>
```

Follow it with:

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
scheduled ──(start time, or resume)──▶ running
running ──(failure rate > limit)──▶ paused ──resume──▶ running
   │  advance (soak time elapsed, or manual)                │
   └──────────────▶ complete / abort ◀──────────────────────┘
```

A running deployment keeps offering its release to devices that enroll later.
**Complete** retires it. **Resume** counts the existing failures as acknowledged, so it
does not pause again immediately.

**Rolling back a bad release:** abort its deployment, then deploy the previous release
with `--allow-downgrade`.

**Scheduling.** A deployment can start later and only roll out at certain times:

```bash
armadactl deployments create --release rel_... --start-at "2026-12-01 02:00" \
  --windows 22:00-05:00 --days mon,tue,wed,thu,fri --tz Asia/Singapore
```

- It stays `scheduled` until `--start-at`. **Resume** starts it early, **Abort** cancels it.
- Outside its windows it offers no new installs and doesn't advance stages; installs already
  under way finish. An overnight window belongs to the day it starts on, so `22:00-05:00` on
  Friday covers early Saturday too.
- Devices also honour their own `maintenance_window`, in their local time.

**Delta updates.** Nothing to set up. For every release of 1 MB or more, the server publishes a list of
the artifact's *content-defined chunks*: pieces whose boundaries follow the content, so an edit only
changes the chunks around it. A device that already has an earlier version of the package splits that
copy the same way, then downloads only the chunks it lacks, using range requests. For agent updates
the running binary is the starting copy.

- It works from any older version, and the server keeps no per-version diffs.
- The rebuilt file is checked against the release's SHA-256 and signature like any download. If
  anything is off, or more than 70% would have to be fetched anyway, the agent downloads in full.
- The device's update progress shows what it saved, e.g. *delta update: downloaded 3.1 MB of 412 MB
  (99% reused)*.
- Savings depend on the format. Uncompressed tarballs, disk and filesystem images, and binaries share
  most chunks between versions. Compressed archives (`.tar.gz`, `.zip`, APKs) change throughout, so
  they gain little. Ship `.tar` and let the link compress, or use formats built for this, such as RAUC
  bundles with casync, or squashfs with small blocks.
- Splitting a file runs at roughly 100-200 MB/s on a modern CPU, and slower on small boards.
- Set `disable_delta: true` in the agent config to always download in full.

## Documentation

Each topic has its own page. Start with [Quick start](#quick-start) and [Concepts](#concepts) above.

| Topic | |
|---|---|
| [Android devices](docs/android.md) | Android devices you ship to customers: silent app updates, device-bound licenses, provisioning |
| [Health monitoring](docs/health.md) | Built-in health checks and program monitors |
| [Configuration profiles](docs/configuration.md) | Configuration profiles: files and app settings pushed to devices, OS updates |
| [Alerts](docs/alerts.md) | Alert rules, channels (webhook, Slack, email), acknowledging, maintenance, alerts for client accounts |
| [Device inventory](docs/inventory.md) | Serials, asset tags, locations, pre-registration, CSV import and export, QR labels |
| [Customers and projects](docs/customers.md) | The customer/project picker, Needs attention, disconnections, remote screenshots, network checks |
| [Cloud, on-prem and offline sites](docs/sites.md) | Cloud, on-prem and offline sites; one view of several servers |
| [Device lifecycle](docs/lifecycle.md) | Spares, repair, RMA replacement, retirement |
| [Bulk operations](docs/bulk-operations.md) | Actions on many devices at once, and scheduled actions |
| [Certificates on devices](docs/certificates.md) | Certificates on devices: inventory, renewal, policies |
| [Customer reports](docs/reports.md) | Customer reports: availability, software, certificates, disconnections |
| [Peripherals: BLE sensors and other devices](docs/peripherals.md) | BLE sensors behind gateways and their firmware |
| [Users, roles and client accounts](docs/users.md) | Users, roles, client accounts, single sign-on, passkeys, 2FA, SCIM |
| [Agent configuration](docs/agent.md) | Agent configuration reference |
| [CLI](docs/cli.md) | armadactl |
| [Server configuration](docs/server.md) | Server flags, automatic backups, Prometheus metrics, upgrading, the server's certificate |
| [API](docs/api.md) | The REST API, the generated API reference, event webhooks |
| [Security model](docs/security.md) | Security model |
| [Development](docs/development.md) | Building, testing, the layout of the code |
| [Operations runbook](docs/operations.md) | Runbook: monitoring, backups, restoring, upgrades, incidents, capacity |
| [Pilot checklist](docs/pilot.md) | Checklist for a pilot on real devices |
| [Scaling and high availability (design)](docs/scaling.md) | Beyond one server |

## Limitations and next steps

- A single server with SQLite comfortably handles thousands of devices at a 30s
  heartbeat. Running several replicas on Postgres is designed but not built; see
  [docs/scaling.md](docs/scaling.md).
- Passkey users have no recovery codes unless they also set up an authenticator app. Losing every
  passkey means asking an admin for a reset. SAML needs a broker such as Keycloak.
- Push wakes one device at a time (actions, logs, check-in). New rollouts still start at each
  device's next heartbeat, which spreads the load. Logs are a snapshot, not a live tail.
- Android app updates always download the full APK (APKs are compressed, so deltas save little).
- The agent's metrics are richest on Linux. Other platforms report the basics.
- On Android, actions are reboot, restart the agent, lock and wipe (`exec` and `logs` aren't
  supported: Android doesn't let apps read other apps' logs).
- Android devices can't yet act as BLE gateways. That needs Nordic's Android DFU and mcumgr libraries
  in the Android agent.
- On macOS and Windows, the built-in checks cover SMART and disk space only; temperature, power and
  battery checks are Linux-only. Program monitors work everywhere.
