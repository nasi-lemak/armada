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
- **Remote actions, in seconds.** Reboot, restart the agent, run a command, or ask for a check-in.
  Each agent holds a long-poll connection, so actions reach it within a second or two, even behind
  NAT, without opening ports. `exec` is off by default and each device opts in.
- **Remote logs.** Fetch recent systemd journal lines (any unit) or named log files from a device,
  filtered by time and text, from the dashboard or `armadactl devices logs`.
- **Delta updates.** Devices download only the parts of a release that differ from the version they
  have (content-defined chunks), from any older version, with no diffs to prepare. Resumable full
  downloads for everything else.
- **Activity log** of enrollments, updates, failures, pauses and actions.
- **Health monitoring** (see [below](#health-monitoring)):
  - Hardware checks run automatically. They cover SSD/HDD SMART data, NVMe wear, eMMC wear, SD-card
    failure signs, RAID, temperature, power and under-voltage, battery, disk space and memory.
  - You can add checks for your programs, defined on the server: systemd unit, process, HTTP, TCP, or any
    Nagios-style command.
- **Alerts** to any webhook (with a body template and signature, for PagerDuty, Teams, Opsgenie and
  similar), Slack or email. They cover devices going offline, unhealthy checks, failed updates and paused
  rollouts.
- **BLE and other peripherals** (see [below](#peripherals-ble-sensors-and-other-devices)): any agent can be a
  gateway that discovers nearby devices and rolls out their firmware. Drivers exist for MCUboot/Zephyr,
  Nordic DFU, or your own tool.
- **Users, roles and client accounts** (see [below](#users-roles-and-client-accounts)): viewer, operator
  and admin roles, and accounts limited to one customer's devices. Every change in the activity log names
  who made it. Sign in with single sign-on (Google, Microsoft, Okta or any OpenID Connect provider,
  with roles from groups), passkeys (passwordless, or as a second step), and authenticator codes.
  Two-factor sign-in can be required. SCIM provisioning lets your directory create and deactivate
  accounts automatically.
- **Web dashboard** built into the server binary (no build step, strict CSP).
- **One-line onboarding** with `curl …/install.sh | sh`.
- **Android devices you provide to customers** (see [below](#android-devices)):
  - Updates to your app go out silently, without Google Play.
  - Your app only runs on devices you enrolled. A copied APK won't work anywhere else, because your
    backend requires a license bound to the device's secure hardware.
  - App settings per customer or site, OS update timing, and remote lock and wipe.
- **Configuration profiles** (see [below](#configuration-profiles)): files, agent settings and Android app
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
5% → 25% → 50% → 100%, 15 minutes per stage, pausing above 20% failures. The 5% canary starts right
away. Going further needs a second person to approve it (see [rollout approval](#security-model)):

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

## Android devices

This is for Android devices you **provide to customers**. You control your app's updates instead of Google Play,
and your app can't simply be copied to other phones. Customers can still install other apps.

### How it works

1. **Provisioning.** Before shipping, you factory-reset each device. On the welcome screen you tap six times
   and scan a QR code from the dashboard. Android downloads the Armada agent, checks it against the
   signing-certificate checksum in the QR code, and installs it as **Device Owner**.
2. **Hardware-bound identity.** The agent creates a key inside the device's secure hardware (TEE or
   StrongBox). This key cannot be exported. Android's **key attestation** gives the server a certificate
   chain, signed by Google, proving three things:
   - the key lives in real secure hardware;
   - the bootloader is locked and the OS is unmodified;
   - the key belongs to your agent app, signed with your certificate.
   The server checks the chain against Google's attestation roots and Google's revocation list.
3. **Silent updates.** Rollouts work as on Linux (stages, auto-pause on failures, channels and labels).
   Before installing, the agent checks three things:
   - the server's signature on the release;
   - the file's SHA-256;
   - the APK itself: package name, versionCode, and that it's signed with a certificate you registered.
   It then installs with no user prompt and blocks the app from being uninstalled. USB debugging is
   turned off by default.
4. **Device-bound licenses.** Your app asks the agent for a license through the client library. The agent
   signs the request with the hardware key. The server issues a short-lived license (a standard EdDSA JWT)
   only if all of these hold:
   - the signature comes from the attested key;
   - the calling app is your registered package, signed with your certificate;
   - the device's licenses haven't been revoked.
   The license is also bound to a key that never leaves your app's Keystore (DPoP, RFC 9449). Your
   backend rejects requests without a valid license and proof.

On a phone you didn't provision there's no enrolled agent, so there's no license, and your backend
refuses the app. A re-signed or modified copy of your app fails the signing-certificate check. A license
token copied off a device fails the DPoP check.

### Setup

1. **Build and sign the agent.** Optionally change `applicationId` in `android/agent/build.gradle.kts` to
   your own namespace, and start the server with `-android-agent-package` set to the same value. Build
   `android/agent` with your release signing key (`./gradlew :agent:assembleRelease` with a `signingConfig`),
   or run `make android` for a debug build. Keep that key safe: Android only accepts updates signed with it.
2. **Register the agent and your app** with their signing-certificate SHA-256. You can read it with
   `apksigner verify --print-certs app.apk` or `keytool -list -v -keystore release.jks`.

   ```bash
   armadactl apps set dev.armada.agent --name "Armada agent" --signer AB:CD:...
   ```

   ```bash
   armadactl apps set com.yourco.app --name "Your app" --signer 12:34:... --license --license-ttl 1h
   ```

3. **Upload the agent APK.** Devices download it during provisioning, from a public URL that serves only
   the agent.

   ```bash
   armadactl releases upload --package dev.armada.agent --version 1.0.0 --version-code 1 --os android agent.apk
   ```

4. **Create a setup QR code.** Set `-public-url https://fleet.yourco.com` on the server so devices can
   reach it. Then create a token and the QR code:

   ```bash
   armadactl tokens create --label customer=acme --max-uses 100 --ttl 168h
   ```

   ```bash
   armadactl android qr --token et_... --wifi-ssid factory-wifi --out acme-setup.png
   ```

   Or use **Enrollment → Show Android setup QR** in the dashboard. Labels on the token, such as
   `customer=acme`, end up in every license, so your backend knows which customer a device belongs to.
5. **Ship your app.** Upload each APK with its `versionName` as the version and its `versionCode`, then
   deploy it like any other release:

   ```bash
   armadactl releases upload --package com.yourco.app --version 2.1.0 --version-code 210 --os android --auto-deploy app.apk
   ```

### In your app

Add the `android/client` module (or its AAR) and attach the headers to every backend request, off the
main thread:

```kotlin
val armada = ArmadaLicense(context)
val headers = armada.authHeaders("POST", "https://api.yourco.com/v1/orders")
headers.forEach { (k, v) -> connection.setRequestProperty(k, v) }
// throws LicenseException on devices you didn't provision, or when the license is revoked
```

### In your backend

Go, using `github.com/nasi-lemak/armada/pkg/license`:

```go
v, err := license.NewVerifierFromJWKSURL(ctx, "https://fleet.yourco.com/api/v1/license/jwks", "com.yourco.app")
http.Handle("/v1/", v.Middleware("https://api.yourco.com", apiHandler))
// inside handlers: claims, _ := license.ClaimsFromContext(r.Context())
// claims.DeviceID(), claims.Device.Labels["customer"]
```

Other languages: a license is a standard JWT (`alg: EdDSA`, `typ: armada-license+jwt`, `iss: armada`,
`aud: <your package>`), and the verification keys are at `/api/v1/license/jwks`. If the license has a
`cnf.jkt` claim, also verify the `DPoP` header as described in RFC 9449. For example, in Node with `jose`:

```js
const JWKS = createRemoteJWKSet(new URL("https://fleet.yourco.com/api/v1/license/jwks"));
const { payload } = await jwtVerify(token, JWKS, { issuer: "armada", audience: "com.yourco.app", typ: "armada-license+jwt" });
```

### Revoking a device

Click **Revoke licenses** on the device page, or run:

```bash
armadactl devices revoke <device-id>
```

The device gets no new licenses, so your app stops working on it within the license lifetime. The
default lifetime is 1 hour and you can set it per app. Reinstating restores it.

### Lost or stolen devices

**Lock screen** locks the device now. **Wipe device** factory-resets it: everything is erased and the
device leaves the fleet until it is set up again with the QR code. The dashboard asks you to type the
device name; the API needs `["confirm=<device ID>"]`, and the device checks that ID again before
wiping.

```bash
armadactl devices action <id> lock
armadactl devices action <id> wipe confirm=<id>
```

Revoking licenses as well stops your app working on it even if the wipe never arrives.

### App settings and OS updates

Use [configuration profiles](#configuration-profiles):

- **Android app settings** (`android_app`) set an app's *managed configuration*: the values your app
  reads with `RestrictionsManager.getApplicationRestrictions()`. Use them for the backend URL, feature
  flags or kiosk options per customer or site, without rebuilding the APK.
- **Android OS updates** (`android_system_update`) set the system update policy: install automatically,
  only during a daily window (for example `02:00-04:00` device time), or postpone for up to 30 days.

### What this does and doesn't protect against

- **Protects against:** installing a copy of your APK on any other phone; running a modified or
  re-signed APK; cloning a device's data onto another device (the key can't be exported); unlocked or
  rooted devices enrolling (verified boot is required); and a stolen license being replayed from
  elsewhere (DPoP).
- **Doesn't protect against:** someone who patches your app to skip the license check *and* re-creates
  your backend's functionality. That's why the check has to be enforced by your backend. If your app
  works fully offline, copy protection can only make copying hard, not impossible.
- **Rollback:** Android doesn't let apps install an older versionCode. To undo a bad release, ship a
  fixed build with a higher versionCode. The rollout's automatic pause stops a bad release at the
  canary devices first.
- **Testing status:** the Android code compiles and passes Android Lint and its unit tests. The
  server-side attestation verifier is tested against real device attestations from Google's sample
  repo. The full flow has **not yet been run on a physical device**, so pilot it on one device before
  provisioning a batch.

### Server flags for Android

| Flag | Default | Meaning |
|---|---|---|
| `-android-agent-package` | `dev.armada.agent` | Agent application ID (attested keys must come from it) |
| `-public-url` | from request | URL devices use to reach the server; put it behind HTTPS |
| `-android-require-strongbox` | off | Require keys in a dedicated security chip, not just the TEE |
| `-android-allow-unverified-boot` | off | Accept unlocked bootloaders (development devices only) |
| `-android-allow-unattested` | off | Accept enrollments without attestation (such devices can't get licenses) |
| `-android-allow-adb` | off | Don't disable USB debugging |
| `-attestation-status-url` | Google's list | Revocation list for attestation keys; set it to empty to disable |

## Health monitoring

Every agent reports health checks with its heartbeat, and checks in early when a status changes.
Each device gets an overall health of `ok`, `warning` or `critical`, shown in the dashboard. Every
status change is recorded in the activity log.

### Built-in hardware checks

These run on Linux. On macOS, Windows and the BSDs, the agent checks SSD/HDD SMART health (through
`smartctl --scan-open`) and disk space.


| Check | What it reads | Flags |
|---|---|---|
| SATA/SAS SSD & HDD | SMART via `smartctl` (install `smartmontools`) | failed self-assessment, reallocated/pending/uncorrectable sectors, SSD wear |
| NVMe | SMART/health log via `smartctl` | % of rated life used, spare capacity, media errors, critical warnings |
| eMMC | the chip's own wear estimate (`life_time`, `pre_eol_info`) | ≥80% of rated life used, reserve blocks running out |
| SD cards | no wear reporting exists on consumer cards, so it watches for failure signs | kernel I/O and filesystem errors, filesystems the kernel remounted read-only |
| Kernel log | I/O and filesystem errors per disk (`/dev/kmsg`) | any errors in the last 24h (≥20 is critical) |
| Read-only filesystems | mounts that should be writable (per `/etc/fstab`, or remounted by the kernel after errors) | critical |
| RAID | `/proc/mdstat` | degraded or rebuilding arrays |
| Disk space / inodes | every mounted filesystem | 85% / 95% (configurable) |
| Memory | available memory | below 10% / 5% |
| Temperature | thermal zones | 80°C / 90°C (configurable) |
| Power | Raspberry Pi firmware throttling flags | under-voltage now or since boot, throttling |
| Battery | power-supply class | battery health problems, low charge on battery power |

Android agents report battery health, charge and temperature; device thermal status; storage space; and
memory. Android doesn't let apps read flash wear. Thresholds and exceptions go in the agent config's
`health` section; see [`deploy/gateway.example.json`](deploy/gateway.example.json).

### Program checks (monitors)

Monitors are defined once on the server. Every device matching the monitor's labels runs them:

```bash
armadactl monitors create --name kiosk-web --type http --target http://127.0.0.1:8080/healthz --contains '"db":"up"' --label role=kiosk
```

```bash
armadactl monitors create --name app-service --type systemd --target myapp.service
```

```bash
armadactl monitors create --name queue --type command -- /usr/lib/nagios/plugins/check_procs -c 1:5 -C worker
```

The types are `http` (status code, body content, latency), `tcp`, `systemd`, `process` and `command`. A
command follows the Nagios plugin convention: exit 0 is ok, 1 is warning, 2 is critical, and the first
line of output is the message. That means existing Nagios and Icinga plugins work unchanged.

Command checks run as root, so they only run on devices whose agent config has `allow_exec: true`
(elsewhere they report "unknown"), and only admins can create or change them.

### History and charts

Every check's status and value is kept as history:
- **Device page:** a chart for each measured value (temperature, disk and storage wear, memory, load,
  response times, battery, signal…). Each chart has a crosshair tooltip, arrow-key navigation and a
  table view.
- **Status over time:** a timeline for every check, labelled with the share of time it was ok. An
  `online` row shows when the device was checking in, so gaps are downtime.
- **Overview:** the fleet's online devices and health problems over time.
- **Ranges:** 24 hours, 7, 30 or 90 days. Ranges longer than the raw retention show hourly averages,
  with the min–max range shaded.

**Comparing one check across devices:** on the overview, pick a check, optionally filtered by labels.
Armada charts the eight devices that need attention most: the highest values, or the least time ok. Use
**Download CSV** on any history view to export it. From the CLI:

```bash
armadactl history <device-id> --range 7d
```

```bash
armadactl history compare --check "temperature cpu-thermal" --label site=sg --csv > temps.csv
```

Storage stays small: one sample per check every 5 minutes is kept for 3 days, then hourly summaries
(min, average, max and worst status) for 90 days. A device with a dozen checks levels off at about
2 MB in total, because older data is dropped as it ages out (measured: ~33 bytes per raw sample and ~51
bytes per hourly summary). Tune it with `-history-resolution`, `-history-raw-retention` and `-history-retention`
(`-history-resolution 0` disables history).

## Configuration profiles

A profile is configuration the server keeps applied on every matching device (by labels and/or
channel). Change it once and devices pick it up within seconds. Connected devices are woken, and the
rest pick it up at their next heartbeat. Each device reports what it applied, so the **Config** page
shows how many devices have applied each profile, are still pending, or failed.

| Kind | Devices | What it does |
|---|---|---|
| `files` | Linux and other agents | Writes files into a directory the agent's `managed_config` names, then runs that entry's reload command |
| `agent` | Linux and other agents | Overrides `maintenance_window` and/or `auto_update` |
| `android_app` | Android | An app's managed configuration (see [App settings](#app-settings-and-os-updates)) |
| `android_system_update` | Android | OS update policy: automatic, windowed or postpone |

**Files** are safe by construction:

- The agent only writes into directories listed in its own config. The server can't name a path:

  ```json
  "managed_config": {
    "kiosk": { "dir": "/etc/kiosk", "reload_command": ["systemctl", "reload", "kiosk"] }
  }
  ```

- Files are written atomically. If the reload command fails, the previous files are put back and
  reloaded, and the device reports the error.
- Files a profile no longer lists, or whose profile is deleted, are removed.
- Content can be a template filled in per device: `{{.Device.Name}}`, `{{.Device.ID}}`,
  `{{.Device.Channel}}`, `{{index .Device.Labels "site"}}`, `{{.Device.Info.Hostname}}`.

**Which profile applies.** When several enabled profiles of the same kind and target match a device,
the highest `priority` wins (then the most recently changed). Profiles are not merged, so give a
site- or customer-specific profile a higher priority than the fleet-wide one.

Manage them in the dashboard, or keep them in version control and apply them with the CLI:

```json
[
  { "name": "Kiosk settings", "kind": "files", "target": "kiosk",
    "selector": { "labels": { "customer": "acme" } },
    "files": [
      { "name": "kiosk.ini", "content_file": "kiosk.ini", "template": true },
      { "name": "secrets.env", "content_file": "acme.env", "mode": "0600" }
    ] },
  { "name": "Night installs", "kind": "agent", "agent": { "maintenance_window": "01:00-04:00" } },
  { "name": "Kiosk app", "kind": "android_app", "target": "com.example.kiosk",
    "values": { "server_url": "https://api.example.com", "kiosk_mode": true } },
  { "name": "OS updates at night", "kind": "android_system_update",
    "system_update": { "type": "windowed", "window_start": "02:00", "window_end": "04:00" } }
]
```

```bash
armadactl config apply -f profiles.json   # creates or updates by name
armadactl config list
```

Profile contents are stored in the server database in plain text (and in backups). Don't put secrets
there that the server's administrators shouldn't see.

## Alerts

Alerts tie a **rule** to one or more **channels**. You can set them up on the dashboard's **Monitoring**
page or with the CLI.

| Condition | Fires when | Resolves when |
|---|---|---|
| `health` | a matching device has a check at `warning` or `critical`. You can limit it to some kinds of check, such as `storage` or `power`. | the checks recover |
| `device_offline` | a device hasn't checked in for N minutes | it checks in |
| `update_failed` | an update fails or rolls back. All failures in one rollout fold into one alert. | after an hour |
| `deployment_paused` | a rollout auto-paused (or was paused) | it is resumed or aborted |

Alerts notify when they fire and when their severity escalates. They can repeat every N minutes, and
notify again when they resolve. Delivery is retried, and failures show up in the activity log.

**Channels:**
- **Webhook:** any URL, with custom headers. Set a secret and every request carries
  `X-Armada-Signature: sha256=<HMAC of "timestamp.body">` and `X-Armada-Timestamp`. Without a body
  template, the body is Armada's JSON notification. With one, it's a Go template, so you can fit
  PagerDuty, Opsgenie, Microsoft Teams, Discord, Telegram or your own API. For example, PagerDuty Events v2:

  ```
  {"routing_key":"<key>","event_action":"{{if eq .Status "resolved"}}resolve{{else}}trigger{{end}}",
   "dedup_key":"{{.AlertID}}","payload":{"summary":{{json .Summary}},"severity":"{{.Severity}}","source":"{{.DeviceName}}"}}
  ```

  Template fields: `.Status`, `.Severity`, `.Rule`, `.Condition`, `.Summary`, `.DeviceID`, `.DeviceName`,
  `.DeviceLabels`, `.DeploymentID`, `.URL`, `.StartedAt`, `.ResolvedAt`, `.Count`, `.AlertID`.
  Helper functions: `json`, `upper`, `lower`.
- **Slack:** an incoming webhook URL.
- **Email:** SMTP with STARTTLS on 587 or implicit TLS on 465. Either name a server per channel, or
  leave the server out to use the server's relay (`-smtp-host`, `-smtp-port`, `-smtp-username`,
  `-smtp-from`, and `ARMADA_SMTP_PASSWORD` in the environment).

```bash
armadactl channels create --name oncall --type webhook --url https://events.pagerduty.com/v2/enqueue --template "$(cat pd.tmpl)"
```

```bash
armadactl rules create --name "Storage failing" --condition health --min-severity warning --kind storage --channel <id>
```

Set `-public-url` on the server so notifications link to the device page. Secrets aren't returned by
the API.

### Acknowledging alerts

Acknowledge a firing alert to say someone is on it, from the **Monitoring** page or with
`armadactl alerts ack <id> --note "replacing the disk"`.

- **Repeat notifications stop** until the alert resolves.
- **Getting worse still notifies.** If the severity escalates (warning to critical), the alert
  notifies as usual and the acknowledgement is cleared.
- `armadactl alerts unack <id>` brings repeats back.
- **Who can acknowledge:** anyone with the operator role on the alert's device. A client operator can
  acknowledge their own devices' alerts. Rollout alerts (`update_failed`, `deployment_paused`) need a
  fleet-wide operator.
- The activity log records who acknowledged an alert and their note.

### Maintenance

Maintenance marks devices as expected to be down or noisy, for example while you replace a screen,
move a store, or reboot a site every Sunday night. While a maintenance covers a device:

- **Alerts are held.** No new alerts fire for it. Alerts already firing neither repeat nor resolve, so
  nobody gets a "resolved" in the middle of the work.
- **When it ends, alerting resumes.** A problem still present then (the device is still offline, the
  disk still full) fires right away, and alerts that cleared during the work resolve.
- **Updates are paused.** Rollouts don't offer it new updates, and the held updates go out when the
  maintenance ends. An install already under way finishes.
- You can turn either effect off (`suppress_alerts: false` or `pause_updates: false`, or
  `--updates-only` and `--alerts-only` in the CLI).
- The device shows a **maintenance** badge, with the end time, in the device list and on its page.
  Alerts held this way are marked on the Monitoring page.

A maintenance targets devices by ID, labels or channel. It is either:

- **One-off:** a start (default now) and an end, at most 30 days apart. On a device's page,
  **Start maintenance** covers just that device for a chosen time.
- **Recurring:** daily windows on chosen days in a time zone, for example `02:00-04:00` on Sundays in
  `Asia/Singapore`. It applies only inside the windows, optionally until an end date.

```bash
armadactl maintenance start --device <id> --for 2h --note "replacing the screen"
```

```bash
armadactl maintenance schedule --label site=orchard --start "2026-12-01 01:00" --end "2026-12-01 05:00" --tz Asia/Singapore
```

```bash
armadactl maintenance schedule --label customer=acme --windows 02:00-04:00 --days sun --tz Asia/Singapore
```

```bash
armadactl maintenance list
```

```bash
armadactl maintenance end <id>
```

**Who can plan maintenance:**
- **Fleet-wide operators and admins:** any devices.
- **Client operators:** their own client's devices only. A client's maintenance only ever covers that
  client's devices, whatever its labels say.
- **Visibility:** clients see their own maintenance, but not vendor maintenance in the list. Their
  devices still show the badge.
- **Activity log:** creating, ending and deleting maintenance are recorded.
- **Retention:** ended maintenance stays listed for a day (`?all=1` or `--all` for longer), and is
  removed after 30 days.

This is different from the agent's `maintenance_window`, which only limits when a device installs
updates.

### Alerts for client accounts

Client operators set up their own alerts on the **Monitoring** page, or with
`armadactl rules create … --client customer=acme`. For example, they can email their on-call team
when a kiosk goes offline. Your admins manage vendor-wide alerts and can see and change every
client's.

- **A client's rule only ever sees that client's devices.** It matches devices in the client's scope
  (narrowed further by its own labels), however it was written.
- **A client's alerts only go to that client's channels**, and vendor alerts only to vendor channels.
  No notification about one customer's devices can reach another customer.
- Client rules can watch `device_offline` and `health`. `update_failed` and `deployment_paused`
  summarise rollouts across customers, so they stay vendor-only.
- Everyone with access to a client sees its rules and channels. Its operators can change them, and
  its viewers can only look. Someone covering several clients chooses which one each rule is for.
- **The server sends client notifications for them, so they are limited:**
  - **Webhooks and Slack:** public `https://` addresses only. The address is checked at connect time
    on the resolved IP, so a DNS name pointing inside your network is refused too, and redirects are
    checked the same way. Client requests never use the server's proxy.
  - **Body templates:** client webhooks send the standard JSON notification; custom body templates
    are vendor-only.
  - **Email:** goes through your SMTP relay. Clients choose recipients only, not a mail server. Without
    `-smtp-host`, clients can't create email channels.

## Device lifecycle

Every device has a lifecycle state, shown as a badge in the device list and on its page:

| State | Meaning | Updates | Alerts | Can check in |
|---|---|---|---|---|
| `active` | In service (every new device starts here) | yes | yes | yes |
| `spare` | In stock, ready to replace a device | yes, so it's current when needed | no | yes |
| `repair` | Out of service: RMA, at the bench | no | no | yes |
| `retired` | Decommissioned for good | no | no | **no** |

When a device leaves service (spare, repair, retired or removed), its open alerts close without a
"resolved" notification. The device didn't recover, it just stopped being watched. Spares and devices
in repair don't count as offline in the fleet overview.

**Replacing a device (RMA).** On the broken device's page, choose **Replace…** and pick a spare (or
any active device). In one step:

- The replacement takes over the old device's **name, channel and labels**. With them come its
  client, configuration profiles, alert rules, rollouts and maintenance by labels. It checks in
  straight away to pick up its configuration.
- The old device is renamed "… (replaced)" and **loses its labels**, so it leaves the client's
  account: the customer can't see or act on a unit sitting at your repair bench. By default it goes
  to `repair`. You can choose `spare` or `retired` instead.
- Both devices link to each other, and the activity log records the swap and its note
  (for example the RMA number).
- Each device keeps its own key and its own history.

**Retiring a device** is final:
- **Key destroyed:** it can never check in again, and its live connection drops.
- **Licenses revoked:** its app licenses are revoked and its hardware key is forgotten.
- **Records kept:** its record, events and history stay. It leaves the device list, counts, rollouts
  and alerts. Show retired devices with the **Retired** filter or `?lifecycle=retired`.
- **Using the hardware again:** enroll it again. It joins as a new device.
- **Android:** wipe the device first (**Wipe device**), then retire it.

Removing a device instead (`DELETE`) also deletes its history.

```bash
armadactl devices lifecycle <id> spare
```

```bash
armadactl devices lifecycle <id> repair --note "RMA 4411: cracked screen"
```

```bash
armadactl devices replace <broken-id> --with <spare-id> --note "RMA 4411"
```

```bash
armadactl devices retire <id> --yes --note "end of life"
```

```bash
armadactl devices list --lifecycle spare
```

Lifecycle changes need a fleet-wide operator, because they move devices between clients and revoke
credentials. Client accounts see the state of their devices.

## Bulk operations

On the **Devices** page, tick devices (or the header box for everything shown, after filtering), pick
an operation and choose **Apply…**. The same operations are in the CLI (`armadactl bulk`) and the API
(`POST /api/v1/bulk`).

| Operation | Who |
|---|---|
| `reboot`, `restart-agent`, `wake` (check in now), `lock` (Android) | operators on the devices |
| `set_labels`, `remove_labels`, `set_channel` | operators on the devices |
| Start maintenance (dashboard; or `armadactl maintenance start` with several `--device`) | operators on the devices |
| `exec` (run a command) | fleet-wide operators |
| `lifecycle` (spare, repair, active, retired) | fleet-wide operators |

`wipe` and `logs` are one device at a time.

**How it works:**
- **Preview first.** A dry run says how many devices match, which ones the operation applies to, and
  which are skipped and why. For example, `lock` skips non-Android devices, and a client operator's
  relabel skips any change that would move a device out of their account. Nothing changes.
- **The run must match the preview.** The real request carries the number of devices the preview
  matched (`expect`). If the matching devices have changed since, nothing happens and you preview
  again. An empty selector needs `all: true`. Together these stop a typo from hitting the whole fleet.
- **Same rules as one device at a time.** Each device is checked exactly as if you changed it on its
  own: your role on it, its platform and lifecycle, and client scope. Retired devices are never
  included.
- **Progress for actions.** Reboots and commands are tracked per device (queued, sent, succeeded,
  failed) on the operation's page in the dashboard, or with `armadactl bulk get <id>`.
- **Paced check-ins.** Devices are woken about 50 a second, so a large batch doesn't hit the server
  at once.
- **Audit.** The activity log records each operation (`bulk.<operation>`) and the usual per-device
  events. Operations are kept for 90 days. Client accounts see their own operations; fleet-wide
  accounts see all.

```bash
armadactl bulk reboot --label site=orchard
```

That only previews. Add `--yes` to apply:

```bash
armadactl bulk reboot --label site=orchard --yes
```

```bash
armadactl bulk exec --label role=kiosk --yes -- systemctl restart myapp
```

```bash
armadactl bulk label floor=2 --device dev_a --device dev_b --yes
```

```bash
armadactl bulk lifecycle spare --label customer=acme --note "contract ended" --yes
```

```bash
armadactl bulk get <id>
```

## Peripherals: BLE sensors and other devices

Devices that can't run an agent can be managed through a **gateway**: any Linux agent near them, such as
a Raspberry Pi or an industrial gateway. Each peripheral becomes a device in the fleet, linked to its
gateway. It has its own firmware version, battery, signal strength, health, labels, update history and
alerts. Staged rollouts, auto-pause and offline alerts work for peripherals just like for other devices.

**Discovery** (`peripherals.discovery` in the agent config):
- `bluez` scans with the Linux Bluetooth stack. It matches devices by name prefix, service UUID,
  manufacturer ID or address. It reads battery level from the Battery Service and, optionally, the
  firmware version from the standard Device Information Service.
- `command` runs any program that prints JSON. Use it for vendor gateways, USB or serial dongles, or
  Zigbee, Thread and LoRa bridges ([example](examples/ble/discover-example.sh)).

**Firmware updates:**
1. Upload the image as a release with `--os ble` and the hardware **model** as the package name.
   Firmware is only ever offered to peripherals of exactly that model.

   ```bash
   armadactl releases upload --package acme-temp-v2 --version 1.4.0 --os ble firmware.bin
   ```

2. Deploy it like any release.
3. The gateway checks the image's signature and SHA-256, then runs the **driver** for the device's
   protocol.
4. It then rediscovers the device and checks that it reports the new version. If the bootloader rolled
   back, the update is marked failed.

A gateway flashes one device at a time.

**Drivers** are commands, so any stack works. [`examples/ble/`](examples/ble) has starting points:
- **MCUboot/Zephyr** over SMP (`mcumgr`), using test-then-confirm, so a bad image is reverted by the
  bootloader;
- **Nordic Secure DFU** (`nrfutil`);
- a template for **ESP32, TI, Silicon Labs or in-house** OTA tools.

See [`deploy/gateway.example.json`](deploy/gateway.example.json) for a complete gateway config.

**Testing status:** the gateway logic, the server side and the discovery-command path are tested end to
end with simulated devices, including a bootloader rollback and a sensor of the wrong model. The BlueZ
scanner and the example driver scripts haven't been run against real Bluetooth hardware yet, so pilot
them with one gateway first.

## Users, roles and client accounts

The admin token is for bootstrapping and automation. Give each person their own account, so their
actions are attributed to them and their access can be removed on its own.

```bash
armadactl users create --email ops@yourco.com --name "Dana" --role admin         # password read from stdin
armadactl users create --email it@acme.com --role operator --scope customer=acme  # a client account
```

Or use **Users** in the dashboard. People sign in with their email and password.

| Role | Can |
|---|---|
| `viewer` | See everything their scope allows. Change nothing. |
| `operator` | Also edit devices, run actions, create enrollment tokens, and (fleet-wide only) upload releases, run rollouts and manage monitors. |
| `admin` | Everything: users, alert channels and rules, Android apps, backups. Admins are always fleet-wide. |

**Client accounts.** A *scope* limits a user to devices with matching labels (and optionally a channel),
for example `customer=acme`. A scoped user:

- sees only matching devices, their alerts, history and activity. Other devices answer 404, as if they
  didn't exist;
- never sees releases, deployments, enrollment-token lists, monitors, vendor alert rules or fleet-wide
  history, which are shared across customers;
- can set up their own alerts for their devices (see
  [Alerts for client accounts](#alerts-for-client-accounts));
- can only create enrollment tokens that put new devices in their scope (the scope labels are forced
  onto the token);
- can't move a device out of their scope by relabelling it, and can't change app licensing (that is
  your anti-redistribution control).

So a customer can watch and manage their own devices, while you decide what software they run.

Set scopes by hand on the Users page, or let your directory do it with
[group mappings](#group-mappings-and-client-accounts).

**Per-client roles.** A person can have a different role for each client. Their access is a list of
*grants*, each a role on some devices (or on the whole fleet). For example:

| Role | Devices |
|---|---|
| viewer | whole fleet |
| operator | `customer=acme` |

This support engineer can look at every device, and fleet-wide pages read-only, but can only reboot,
edit, fetch logs from or enroll Acme's devices. On any device, the highest grant that covers it
applies. Fleet-wide pages (releases, rollouts, monitors, Android apps) need a fleet-wide grant, at
the level the page needs. Set grants with **Access** on the Users page, or:

```bash
armadactl users set sam@vendor.com --grant viewer --grant operator:customer=acme
```

Admin is always fleet-wide. A person whose grants are all for some clients is a client account: no
fleet-wide pages at all.

**Tokens for scripts.** Each user can create personal API tokens (`ut_…`) on **My account** or with
`armadactl me tokens create --name ci`. A token acts as its owner with the same role and scope, and
stops working when it is revoked or the user is disabled or deleted. Pass it to the CLI with `--token`
or `ARMADA_TOKEN`.

**Sessions and safety.**
- Dashboard sessions last 12 hours without activity (`-session-ttl`).
- Changing or resetting a password signs that user out everywhere.
- Disabling a user takes effect on their next request.
- Five failed sign-ins for an email lock that email for 15 minutes; twenty from one address lock the
  address. Behind a reverse proxy, every user shares the proxy's address.
- Passwords are stored as bcrypt hashes; sessions and tokens as SHA-256 hashes.
- Users can't change their own role, disable themselves, or delete themselves, so an admin can't
  lock the last admin out by accident. The admin token keeps working regardless.

### Two-factor sign-in

Each user can turn on two-factor sign-in under **My account**. They scan a QR code with an
authenticator app (Google Authenticator, Microsoft Authenticator, 1Password, Authy and others) and
confirm with a code. After that, signing in takes the password and then a 6-digit code.

- Turning it on gives **10 recovery codes**. Each one signs in once without the phone. Users can
  generate a new set at any time, which voids the old one.
- A code can't be used twice. Each sign-in attempt allows five codes before the password is needed again.
  Wrong codes also count towards the email's lockout.
- Turning it on signs out the user's other sessions, because those only had the password.
- **Lost phone:** an admin clicks **Reset 2FA** on the Users page, or runs
  `armadactl users set <email> --reset-2fa`. That removes the authenticator and any passkeys, and signs the
  user out everywhere.
  Check who is asking before doing this.
- **Require it for everyone:** start the server with `-require-2fa`. Password users who haven't set it
  up can only reach their account page until they do. Single sign-on users are exempt, because their
  identity provider is where MFA belongs.
- Scripts sign in with a personal API token, not a password. With a password, the API also accepts
  `{"email", "password", "code"}` in one request.
- Authenticator secrets are encrypted with `<data-dir>/secrets.key` (included in backups), so a copy of
  the database alone doesn't reveal them.

### Passkeys

Under **My account**, users can add passkeys: Touch ID or Face ID, Windows Hello, Android screen lock,
a password manager's passkeys (iCloud Keychain, Google Password Manager, 1Password, Bitwarden), or a
hardware security key such as a YubiKey. Passkeys can't be phished, because they only work on the
site they were made for.

They work two ways:

- **Passwordless.** **Sign in with a passkey** on the sign-in page needs no email or password. The
  device checks the user's fingerprint, face or PIN, which counts as the second factor, so it
  satisfies `-require-2fa`.
- **As the second step** after a password, instead of an authenticator code. Simple security keys
  without a PIN work this way.

Details:

- Adding a passkey needs a sign-in from the last 15 minutes. A stolen session therefore can't add a
  permanent way in. Adding the first second factor signs out the user's other sessions.
- A security key whose signature counter goes backwards (a sign of cloning) is refused.
- **Reset 2FA** (admin) removes a user's passkeys along with their authenticator app.
- Passkeys belong to the host name in `-public-url` (for example `fleet.example.com`). The dashboard
  must be opened at that address, over HTTPS (or at `localhost` for testing). If you move the server
  to a new domain, users need to add passkeys again. IP addresses can't be used.
- Single sign-on users use their provider's sign-in (which can have its own passkeys), so offboarding
  and group rules at the provider can't be bypassed. Passkey sign-in is off when `-password-login=false`.

### Single sign-on

Armada works with any OpenID Connect provider: Google Workspace, Microsoft Entra ID, Okta, Auth0,
Keycloak, Authentik, JumpCloud and others. For SAML-only providers, put Keycloak or Authentik in
between.

1. Register Armada with your provider as a *web application*, with this redirect URI:
   `https://fleet.example.com/api/v1/auth/sso/callback`
2. Start the server with the provider's issuer and client ID. The secret is read from the
   environment only, so it doesn't show up in `ps`:

   ```bash
   export ARMADA_OIDC_CLIENT_SECRET=...
   armada-server -public-url https://fleet.example.com \
     -oidc-issuer https://accounts.google.com -oidc-client-id 1234.apps.googleusercontent.com \
     -oidc-name Google -oidc-allowed-domains yourco.com
   ```

| Provider | Issuer |
|---|---|
| Google Workspace | `https://accounts.google.com` |
| Microsoft Entra ID | `https://login.microsoftonline.com/<tenant-id>/v2.0` |
| Okta | `https://<your-org>.okta.com` (or a custom authorization server URL) |
| Keycloak | `https://keycloak.example.com/realms/<realm>` |
| Auth0 | `https://<tenant>.auth0.com/` |

The sign-in page then shows **Sign in with Google** (or whatever `-oidc-name` says). Password sign-in
stays available unless you pass `-password-login=false`. The admin token always works, as a
break-glass.

**Who can sign in.** There are three options:

- **Only people you add (default).** Add them on the Users page with no password, or run
  `armadactl users create --email … --role … --sso-only`. The first SSO sign-in with that email links
  the account to their identity at the provider. After that, sign-ins match the provider's stable user
  ID, so a later email change at the provider doesn't matter. Another identity claiming the same email
  is refused.
- **Anyone from your domains:** set `-oidc-create-role viewer` (or `operator`) with
  `-oidc-allowed-domains`. New people get an account at their first sign-in.
- **By group:** use [group mappings](#group-mappings-and-client-accounts). Each sign-in sets the user's
  role and devices from the groups claim (`-oidc-groups-claim`, default `groups`), and people in no
  mapped group can't sign in. Removing someone from the group in your directory is then enough.

Always set `-oidc-allowed-domains` for providers where users can change their own email address (for
example multi-tenant Entra apps). Users who signed in with SSO can't use a password unless an admin
sets one. Their MFA happens at the provider.

### SCIM provisioning

With SCIM, your identity provider manages Armada accounts for you. Assigning someone to the Armada
app creates their account. Changes to their name or email follow. Deactivating or unassigning them
disables them here immediately and ends their sessions. Okta, Microsoft Entra ID, OneLogin, JumpCloud
and others support it.

1. In Armada, go to **Users → Directory sync (SCIM)** and create a token (or run
   `armadactl scim tokens create --name Okta`). Copy the **SCIM base URL**
   (`https://fleet.example.com/scim/v2`) and the token.
2. In your identity provider, turn on provisioning for the Armada app:
   - **Okta:** in the app's *Provisioning* tab, set *SCIM connector base URL*, *Unique identifier field
     for users* = `userName`, and *Authentication mode* = *HTTP Header* with the token. Enable
     *Create*, *Update attributes* and *Deactivate users*, and *Push groups*.
   - **Entra ID:** in the enterprise app, go to *Provisioning*, choose *Automatic*, and enter the
     *Tenant URL* and *Secret token*. The default attribute mappings work (`userPrincipalName` →
     `userName`, `mail` → `emails[type eq "work"].value`).
3. Use SSO for sign-in (see above). Provisioned users have no password.

**Access from groups.** Groups the directory pushes are matched against the
[group mappings](#group-mappings-and-client-accounts). Taking someone out of *Fleet Admins* in the
directory demotes them here at once. While no mappings exist, provisioned users get
`-scim-default-role` (default `viewer`) and admins set their access by hand.

**Existing accounts.** If the directory sends a user whose email already has an Armada account, it
takes that account over: from then on, the directory manages it. With group mappings, the account's
access then follows its groups. **Put existing admins in a group mapped to admin before turning on
sync.** The admin token keeps working if anyone gets locked out.

Details:
- Supported: Users and Groups with create, read, replace, PATCH and delete; `attr eq "value"` filters
  (`userName`, `externalId`, `emails`, `displayName`); paging; and the discovery endpoints. Unknown
  user attributes (title, department and the like) are accepted and ignored.
- Deactivation (`active: false`) keeps the account but disables it. Deleting it in the directory
  deletes it here.
- SCIM tokens can only call `/scim/v2`, not the rest of the API. Revoke them on the Users page. The
  activity log records every directory change as *SCIM (token name)*.

### Group mappings and client accounts

A group mapping gives the members of a directory group a role, and can limit them to some devices.
That lets your directory run client accounts: one group per customer.

| Group | Role | Devices |
|---|---|---|
| Fleet Admins | admin | whole fleet |
| Fleet Ops | operator | whole fleet |
| Acme IT | operator | `customer=acme` |
| Globex IT | viewer | `customer=globex` |

Add them under **Users → Group mappings**, or with the CLI:

```bash
armadactl groups map --group "Acme IT" --role operator --scope customer=acme
armadactl groups map --group "Fleet Ops" --role operator
armadactl groups
```

Groups come from SCIM (for provisioned users) or from the SSO groups claim at each sign-in. The rules:

- **Each group is a grant.** Someone in *Acme IT* and *Globex IT* (an MSP, say) is an operator on
  Acme's devices and a viewer on Globex's. Being an operator for one client never makes them an
  operator for another. On any device, the highest grant covering it applies.
- A fleet-wide group adds access everywhere. *Fleet Ops* plus *Acme IT* makes someone an operator
  everywhere, while *Support (viewer, whole fleet)* plus *Acme IT* lets them see everything and
  operate only Acme's devices.
- **No mapped group, no access.** Once any mapping exists, a directory user in none of the mapped
  groups can't sign in, and their live sessions stop working.
- **Changes apply at once.** Editing a mapping or someone's groups updates every affected account
  immediately. Their role and devices can't be edited by hand in Armada.
- **Removing every mapping fails closed.** People whose access came from groups get no access until an
  admin sets their role and devices by hand, so a client account never falls back to the whole fleet.
- Admin can't be limited to devices, so admin mappings are always fleet-wide.
- When a client account in several groups creates an enrollment token, the token's labels must pick
  one of their customers (for example `customer=globex`).

`-oidc-group-roles` still works. It adds fleet-wide mappings that are shown on the page but can only
be changed with the flag.

**Security details.** The flow is the authorization-code flow with PKCE and a nonce. The callback must
come back to the browser that started it (a cookie check). The session token never appears in a URL;
the browser exchanges a one-time ticket, valid for a minute, for it.

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
| `allow_logs` | `true` | Whether the server may read the systemd journal and the files in `log_files` |
| `log_files` | none | Log files the server may ask for, by name: `{"myapp": "/var/log/myapp/app.log"}`. No other files can be read. |
| `disable_push` | `false` | Turn off the long-poll connection; actions then wait for the next heartbeat |
| `disable_delta` | `false` | Always download updates in full instead of reusing chunks of the installed version |
| `managed_config` | none | Directories configuration profiles may write files into, by name, each with an optional `reload_command` |
| `allow_remote_settings` | `true` | Whether `agent` profiles may override `maintenance_window` and `auto_update` |
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

**Whole-OS (A/B) image updates** with RAUC, SWUpdate or Mender use `reboot_required: true`. The agent
reboots after `install_command`. On the next start it checks which version booted, runs the health
check, then `confirm_command` (for example `rauc status mark-good`). If the bootloader fell back, or the
health check fails, the update is reported as rolled back. See [`examples/os-image/`](examples/os-image).

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
armadactl devices list [--channel C] [--label K=V] [--status online|offline] [--lifecycle spare|repair|retired|all]
armadactl devices get <id>
armadactl devices set <id> [--name N] [--channel C] [--label K=V]... [--unlabel K]...
armadactl devices action <id> reboot|restart-agent
armadactl devices action <id> exec -- systemctl status myapp
armadactl devices logs <id> [--unit myapp.service | --file myapp] [--lines 200] [--since 1h] [--grep error]
armadactl devices wake <id>
armadactl devices delete <id>
armadactl devices lifecycle <id> active|spare|repair [--note N]
armadactl devices retire <id> --yes [--note N]
armadactl devices replace <id> --with <new-id> [--old-state repair|spare|retired] [--note N]
armadactl tokens create|list|revoke
armadactl releases upload|list|delete
armadactl deployments create --release <id> [--channel C] [--label K=V] [--device ID]
                             [--stages 5,25,100] [--stage-duration 15m] [--max-failure-rate 0.2]
                             [--min-samples 3] [--manual] [--allow-downgrade]
armadactl deployments list|get|approve|pause|resume|advance|retry|complete|abort
armadactl events [--device ID] [--deployment ID]
armadactl alerts [--firing] | alerts ack <id> [--note N] | alerts unack <id>
armadactl bulk reboot|restart-agent|wake|lock|exec|label|unlabel|set-channel|lifecycle <devices> [--yes]
armadactl bulk list | bulk get <id>
armadactl maintenance list|start|schedule|end|delete   (start --device ID --for 2h; schedule --windows 02:00-04:00 --days sun)
armadactl config list|get|apply -f FILE|enable|disable|delete
armadactl groups [list|map --group G --role R [--scope K=V]...|unmap G]
armadactl scim tokens [list|create --name N|revoke ID]
armadactl whoami
armadactl users list|create|set|delete      (create --sso-only; set --grant ROLE[:K=V], --reset-2fa, --unlink-sso)
armadactl me password
armadactl me tokens [list|create --name N|revoke ID]
```

Authenticate with `--token` or `ARMADA_TOKEN`: a personal token, or the admin token.

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
| `-history-resolution` | `ARMADA_HISTORY_RESOLUTION` | `5m` (0 disables history) |
| `-history-raw-retention` | `ARMADA_HISTORY_RAW_RETENTION` | `72h` |
| `-history-retention` | `ARMADA_HISTORY_RETENTION` | `2160h` (90 days) |
| `-session-ttl` | `ARMADA_SESSION_TTL` | `12h` (dashboard sign-in, sliding) |
| `-rollout-approval` | `ARMADA_ROLLOUT_APPROVAL` | `second-person` (or `off`) |
| `-unapproved-max-percent` | | `10` (largest first stage allowed before approval) |
| `-trusted-proxies` | `ARMADA_TRUSTED_PROXIES` | none (comma-separated IPs/CIDRs) |
| `-smtp-host`, `-smtp-port`, `-smtp-username`, `-smtp-from` | `ARMADA_SMTP_HOST`, …, `ARMADA_SMTP_PASSWORD` (environment only) | no relay |
| `-disable-admin-token` | `ARMADA_DISABLE_ADMIN_TOKEN` | `false` |
| `-rotate-admin-token` | | replace the generated token at startup |
| `-password-login` | `ARMADA_PASSWORD_LOGIN` | `true` |
| `-require-2fa` | `ARMADA_REQUIRE_2FA` | `false` |
| `-oidc-issuer`, `-oidc-client-id` | `ARMADA_OIDC_ISSUER`, `ARMADA_OIDC_CLIENT_ID` | SSO off |
| | `ARMADA_OIDC_CLIENT_SECRET` | (environment only) |
| `-oidc-name`, `-oidc-allowed-domains`, `-oidc-create-role`, `-oidc-group-roles`, `-oidc-groups-claim`, `-oidc-scopes`, `-oidc-redirect-url` | `ARMADA_OIDC_…` | see [Single sign-on](#single-sign-on) |
| `-scim-default-role` | `ARMADA_SCIM_DEFAULT_ROLE` | `viewer`, used only while no group mappings exist (see [SCIM provisioning](#scim-provisioning)) |
| `-push-hold` | `ARMADA_PUSH_HOLD` | `50s` (agent long-poll; keep below your proxy's idle timeout; `0` disables) |

The data directory holds `armada.db` (SQLite), `artifacts/`, `signing.key`, `license.key`, `secrets.key` and
`admin-token`. **Back it up**, for example nightly from cron:

```bash
armadactl backup -o /backups/armada-$(date +%F).tar.gz
```

The backup is a consistent snapshot taken while the server runs. It contains the database, the signing
keys and every artifact. It holds private keys, so store it like a password. To restore, stop the
server, extract the archive into an empty data directory, and start the server with the same admin
token. Losing `signing.key` means agents that pinned the old key
reject new releases.

**Upgrading.** Replace the binary and restart. On start, the server brings the database schema up to
date:
- It first copies the database next to itself as `armada.db.before-v<N>-<time>`. If anything goes
  wrong, stop the server, put that file back as `armada.db`, and run the previous version. Delete these
  copies once you're happy.
- Each schema change is applied in a transaction. A failed one leaves the database untouched at its
  previous version, and the server doesn't start.
- To see what an upgrade will do without changing anything, run
  `armada-server -data-dir ./data -migrate-status`.
- An older armada-server refuses to open a database a newer one has upgraded, instead of corrupting
  it. To go back to an older version, restore the `before-v…` copy.

Deployment options: [`deploy/armada-server.service`](deploy/armada-server.service)
(systemd), [`Dockerfile`](Dockerfile) and
[`deploy/docker-compose.yml`](deploy/docker-compose.yml). In production, put the server
behind TLS, either with `-tls-cert`/`-tls-key` or a reverse proxy.

## API

Admin endpoints need `Authorization: Bearer <token>`: the admin token, a dashboard session from
`POST /api/v1/auth/login`, or a personal API token. Each endpoint requires a minimum role, and
client accounts get `403` on fleet-wide endpoints and `404` for devices outside their scope. Agent
endpoints use the per-device key issued at enrollment.

| Method & path | Purpose |
|---|---|
| `GET /api/v1/summary` | Fleet counts and version distribution |
| `GET /api/v1/devices` (`?channel=&label=k=v&status=&lifecycle=`) | List devices (retired ones only with `lifecycle=retired` or `all`) |
| `GET/PATCH/DELETE /api/v1/devices/{id}` | Device detail (with updates, actions, events), edit name/channel/labels, remove |
| `GET/POST /api/v1/devices/{id}/actions` | List or queue `reboot`, `restart-agent`, `exec`, `logs`; on Android `lock`, `wipe` |
| `POST /api/v1/devices/{id}/wake` | Ask a device to check in now |
| `POST /api/v1/bulk` (`operation`, `selector` or `all`, `dry_run`, `expect`) | Bulk operation: preview, then apply with the previewed count |
| `GET /api/v1/bulk`, `GET /api/v1/bulk/{id}` | Recent bulk operations; per-device progress |
| `POST /api/v1/devices/{id}/lifecycle` (`{"state": "spare\|repair\|active\|retired", "note": …}`) | Change a device's lifecycle state (retiring is final) |
| `POST /api/v1/devices/{id}/replace` (`{"replacement_id": …, "old_state": "repair"}`) | Replace a device (RMA): the replacement takes over name, channel and labels |
| `GET/POST /api/v1/config-profiles`, `GET/PUT/DELETE /api/v1/config-profiles/{id}` | Configuration profiles (with compliance counts) |
| `GET/POST /api/v1/tokens`, `DELETE /api/v1/tokens/{id}` | Enrollment tokens |
| `GET/POST /api/v1/releases`, `GET/DELETE /api/v1/releases/{id}`, `GET …/artifact` | Releases (upload is `multipart/form-data`) |
| `GET/POST /api/v1/deployments`, `GET /api/v1/deployments/{id}` | Deployments |
| `POST /api/v1/deployments/{id}/{approve,pause,resume,advance,retry,complete,abort}` | Control a rollout (`approve`: someone other than its creator) |
| `GET /api/v1/events` | Activity log |
| `GET /api/v1/signing-key` | Release-signing public key |
| `POST /api/v1/agent/enroll`, `/heartbeat`, `/updates`, `/actions/{id}/result`, `GET /api/v1/agent/releases/{id}/artifact`, `GET /api/v1/agent/wait`, `GET /api/v1/agent/releases/{id}/chunks` | Agent protocol (`wait` is the push long-poll; `chunks` the delta-update index) |
| `GET /install.sh`, `GET /api/v1/agent/download?os=&arch=&package=` | Public bootstrap (agent binaries and APK only) |
| `GET/PUT/DELETE /api/v1/apps[/{package}]` | Android apps: signing certificates and licensing |
| `POST /api/v1/android/provisioning` | Android setup QR code for an enrollment token |
| `POST /api/v1/agent/challenge`, `POST /api/v1/agent/license` | Attestation and license protocol |
| `GET /api/v1/license/jwks` | Public keys for verifying licenses |
| `GET/POST /api/v1/monitors`, `PUT/DELETE /api/v1/monitors/{id}` | Program health checks |
| `GET/POST /api/v1/channels`, `PUT/DELETE /api/v1/channels/{id}`, `POST …/{id}/test` | Notification channels (`client_scope` for a client's) |
| `GET/POST /api/v1/alert-rules`, `PUT/DELETE /api/v1/alert-rules/{id}` | Alert rules (`client_scope` for a client's) |
| `GET /api/v1/alerts[?status=firing]` | Firing and recent alerts |
| `POST /api/v1/alerts/{id}/ack` (`{"note": …}`), `POST …/{id}/unack` | Acknowledge a firing alert, or undo it |
| `GET/POST /api/v1/maintenance`, `POST …/{id}/end`, `DELETE …/{id}` | Maintenance (`?all=1` includes long-ended ones) |
| `GET /api/v1/devices/{id}/history?range=24h\|7d\|30d\|90d` | A device's check and metric history (raw or hourly) |
| `GET /api/v1/history/fleet?range=…` | Devices online, health problems and firing alerts over time |
| `GET /api/v1/history/checks`, `GET /api/v1/history/compare?check=…&label=…` | One check across devices (worst eight) |
| `GET /api/v1/backup` | Full backup (`.tar.gz`: database snapshot, keys, artifacts) |
| `POST /api/v1/auth/login`, `POST /api/v1/auth/logout` | Email and password sign-in; returns a session token, or a two-factor challenge |
| `POST /api/v1/auth/2fa` | Second step: challenge and authenticator or recovery code |
| `GET /api/v1/auth/config` | Which sign-in methods are on (public) |
| `GET /api/v1/auth/sso/login`, `GET …/callback`, `POST …/exchange` | Single sign-on (browser flow) |
| `POST /api/v1/me/2fa/{setup,enable,disable,recovery-codes}` | Manage your own two-factor sign-in (dashboard sessions only) |
| `POST /api/v1/auth/passkey/{begin,finish}` | Passwordless passkey sign-in (WebAuthn) |
| `POST /api/v1/auth/2fa/passkey/{begin,finish}` | Passkey as the second step after a password |
| `GET /api/v1/me/passkeys`, `POST …/register/{begin,finish}`, `DELETE …/{id}` | Manage your passkeys |
| `GET /api/v1/me`, `POST /api/v1/me/password` | Who is signed in (role, scope); change password |
| `GET/POST /api/v1/me/tokens`, `DELETE /api/v1/me/tokens/{id}` | Personal API tokens |
| `GET/POST /api/v1/users`, `PATCH/DELETE /api/v1/users/{id}` | Users (admin); `grants` sets per-client roles |
| `GET/POST /api/v1/scim/tokens`, `DELETE …/{id}` | SCIM tokens (admin) |
| `GET/POST /api/v1/group-mappings`, `PUT/DELETE …/{id}` | Group mappings: group → role and devices (admin) |
| `/scim/v2/Users`, `/scim/v2/Groups`, `/scim/v2/ServiceProviderConfig`, `…/ResourceTypes`, `…/Schemas` | SCIM 2.0 for your identity provider (SCIM token) |

## Security model

**Production checklist.**

1. Set `-public-url https://fleet.example.com`. Single sign-on and passkeys stay off without it, and
   links are never built from request headers.
2. Behind a reverse proxy or load balancer, set `-trusted-proxies` to its addresses. Otherwise every
   user shares the proxy's address for sign-in throttling (the server logs a warning when it sees
   this).
3. Add admin users (ideally with SSO, passkeys or 2FA), then start with `-disable-admin-token`.
4. Keep `-rollout-approval second-person` (the default), so one stolen operator account can't push
   code to the whole fleet.

How it works:

- Admin, session, personal and device credentials are random 256-bit secrets. The server stores
  only their SHA-256 hashes. Deleting a device revokes its key immediately. User passwords are
  bcrypt hashes. Authenticator secrets are encrypted (AES-GCM) with `secrets.key`; recovery codes
  are stored as hashes. Passkeys store only public keys.
- Roles and client scopes are enforced by the server on every request. The dashboard only hides
  what a user can't use.
- Enrollment tokens can expire, have a use limit, and be revoked.
- A device can download only the artifacts it has been offered. The only public download
  is the newest `armada-agent` binary, which `install.sh` uses.
- Releases are signed with a server-held ed25519 key over
  `(package, version, sha256)`. An artifact therefore can't be replayed under another
  name or version.
- **Rollout approval.** A release is code that runs as root on devices. With `-rollout-approval
  second-person` (the default), every rollout stops after its first stage until someone other than
  the person who started it approves it: **Approve** on the deployment page, or `armadactl
  deployments approve <id>`.
  - Before approval, a rollout runs only its first stage, and only if that stage is at most
    `-unapproved-max-percent` (10%). Rollouts that start bigger (for example a single 100% stage)
    wait in `awaiting_approval` and offer nothing.
  - Uploads with `--auto-deploy` still canary automatically and then wait.
  - The admin token counts as one person, so it can't approve its own rollouts.
  - Use `-rollout-approval off` for a one-person team.
- **Client alert channels can't reach your network.** Client webhooks go only to public HTTPS
  addresses, checked when connecting, and client email only through your relay (see
  [Alerts for client accounts](#alerts-for-client-accounts)).
- **Code execution on devices is opt-in on the device and limited to fleet-wide staff.**
  - `exec` actions and **command** health checks both need `allow_exec: true` in the agent config.
    Without it, command checks report "unknown" and never run.
  - Only admins can create or change command checks.
  - `exec` and Android `wipe` need the operator role on the whole fleet. Per-client operators can
    reboot, restart the agent, lock, fetch logs and ask for a check-in on their devices.
- `logs` actions read only the systemd journal and the files a device lists in `log_files`. Set
  `allow_logs: false` to refuse them. Client operators can read logs from their own devices.
- **Sign-in throttling** counts failures per email *and* address (5 in 15 minutes), per address (20),
  and per email from anywhere (50). A stranger therefore can't lock a known user out with a few bad
  guesses, and spreading a guessing attack over many addresses still hits a limit.
  `X-Forwarded-For`, `-Proto` and `-Host` are only believed from `-trusted-proxies`.
- **Admin token.** It bypasses SSO, 2FA and SCIM rules, so:
  - each new address using it is recorded in the activity log (`auth.admin_token`);
  - `-disable-admin-token` turns it off once an enabled admin user exists (the server refuses to start
    otherwise, so you can't lock yourself out);
  - `-rotate-admin-token` replaces the generated token.
- Dashboard sessions are kept in the browser's localStorage. The strict Content-Security-Policy
  below is the protection against scripts reading them.
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

**CI** (`.github/workflows/ci.yml`) runs on every push to `main` and every pull request:

| Job | What it checks |
|---|---|
| Go (go.mod and stable) | `gofmt`, `go vet`, `go mod tidy`, all tests with the race detector, agent cross-compiles for every platform, server and CLI builds for macOS, Windows and FreeBSD |
| Vulnerability scan | `govulncheck` with the latest Go release |
| Dashboard | JavaScript syntax, then a browser smoke test (`web/e2e/smoke.cjs`): signs in, opens every page as an admin, and checks a client account sees only its customer's devices and no fleet pages. It fails on any JavaScript error. |
| Android | agent APK and client library build, unit tests, Android lint |

Run the dashboard smoke test against a local server with:

```bash
cd web/e2e && npm ci && npx playwright install chromium
node smoke.cjs http://localhost:8080 "$(cat ../../data/admin-token)"
```

Build releases with the **current Go release** rather than the minimum in `go.mod`. Many security
fixes land in the standard library, and the vulnerability scan uses the latest Go for that reason.

**Changing the database schema.** Schema changes are numbered migrations in
`internal/store/migrations.go`. Each is applied once, in order, inside a transaction, and recorded in
the `schema_migrations` table.
- Add a new migration at the end. Never edit a released one; `TestMigrationsAreFrozen` fails if you
  do. Then add its fingerprint (printed by the test) to the list in that test.
- `TestLegacyDatabaseUpgrades` upgrades a real database written by the first release
  (`internal/store/testdata`) and checks it ends up with exactly the same tables, columns and indexes as
  a new one.

Layout:

```
cmd/armada-server   server entrypoint
cmd/armada-agent    agent entrypoint
cmd/armadactl       CLI
internal/api        wire types shared by everything
internal/rollout    pure rollout logic: targeting, staging, version ordering, health evaluation
internal/chunk      content-defined chunking for delta updates
internal/store      SQLite persistence and numbered schema migrations
web/e2e             dashboard browser smoke test (run by CI)
internal/server     HTTP API, agent protocol, rollout engine
internal/agent      agent: enrollment, heartbeat, updates, self-update, actions
internal/attestation Android key attestation verifier (Google roots, revocation list)
pkg/license         license issuing and verification (import this in your backend)
android/agent       Android Device Owner agent (Kotlin)
android/client      Android client library for your app
web/                embedded dashboard and install.sh
deploy/, examples/  service units, compose file, sample agent config and hooks
```

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
