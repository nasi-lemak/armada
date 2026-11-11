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
  who made it.
- **Web dashboard** built into the server binary (no build step, strict CSP).
- **One-line onboarding** with `curl …/install.sh | sh`.
- **Android devices you provide to customers** (see [below](#android-devices)):
  - Updates to your app go out silently, without Google Play.
  - Your app only runs on devices you enrolled. A copied APK won't work anywhere else, because your
    backend requires a license bound to the device's secure hardware.

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
- **Email:** SMTP with STARTTLS on 587 or implicit TLS on 465.

```bash
armadactl channels create --name oncall --type webhook --url https://events.pagerduty.com/v2/enqueue --template "$(cat pd.tmpl)"
```

```bash
armadactl rules create --name "Storage failing" --condition health --min-severity warning --kind storage --channel <id>
```

Set `-public-url` on the server so notifications link to the device page. Secrets aren't returned by
the API.

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
- never sees releases, deployments, enrollment-token lists, monitors, alert rules or fleet-wide history,
  which are shared across customers;
- can only create enrollment tokens that put new devices in their scope (the scope labels are forced
  onto the token);
- can't move a device out of their scope by relabelling it, and can't change app licensing (that is
  your anti-redistribution control).

So a customer can watch and manage their own devices, while you decide what software they run.

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
armadactl whoami
armadactl users list|create|set|delete
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

The data directory holds `armada.db` (SQLite), `artifacts/`, `signing.key`, `license.key` and
`admin-token`. **Back it up**, for example nightly from cron:

```bash
armadactl backup -o /backups/armada-$(date +%F).tar.gz
```

The backup is a consistent snapshot taken while the server runs. It contains the database, the signing
keys and every artifact. It holds private keys, so store it like a password. To restore, stop the
server, extract the archive into an empty data directory, and start the server with the same admin
token. Losing `signing.key` means agents that pinned the old key
reject new releases.

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
| `GET /install.sh`, `GET /api/v1/agent/download?os=&arch=&package=` | Public bootstrap (agent binaries and APK only) |
| `GET/PUT/DELETE /api/v1/apps[/{package}]` | Android apps: signing certificates and licensing |
| `POST /api/v1/android/provisioning` | Android setup QR code for an enrollment token |
| `POST /api/v1/agent/challenge`, `POST /api/v1/agent/license` | Attestation and license protocol |
| `GET /api/v1/license/jwks` | Public keys for verifying licenses |
| `GET/POST /api/v1/monitors`, `PUT/DELETE /api/v1/monitors/{id}` | Program health checks |
| `GET/POST /api/v1/channels`, `PUT/DELETE /api/v1/channels/{id}`, `POST …/{id}/test` | Notification channels |
| `GET/POST /api/v1/alert-rules`, `PUT/DELETE /api/v1/alert-rules/{id}` | Alert rules |
| `GET /api/v1/alerts[?status=firing]` | Firing and recent alerts |
| `GET /api/v1/devices/{id}/history?range=24h\|7d\|30d\|90d` | A device's check and metric history (raw or hourly) |
| `GET /api/v1/history/fleet?range=…` | Devices online, health problems and firing alerts over time |
| `GET /api/v1/history/checks`, `GET /api/v1/history/compare?check=…&label=…` | One check across devices (worst eight) |
| `GET /api/v1/backup` | Full backup (`.tar.gz`: database snapshot, keys, artifacts) |
| `POST /api/v1/auth/login`, `POST /api/v1/auth/logout` | Email and password sign-in; returns a session token |
| `GET /api/v1/me`, `POST /api/v1/me/password` | Who is signed in (role, scope); change password |
| `GET/POST /api/v1/me/tokens`, `DELETE /api/v1/me/tokens/{id}` | Personal API tokens |
| `GET/POST /api/v1/users`, `PATCH/DELETE /api/v1/users/{id}` | Users (admin) |

## Security model

- Admin, session, personal and device credentials are random 256-bit secrets. The server stores
  only their SHA-256 hashes. Deleting a device revokes its key immediately. User passwords are
  bcrypt hashes.
- Roles and client scopes are enforced by the server on every request. The dashboard only hides
  what a user can't use.
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
internal/attestation Android key attestation verifier (Google roots, revocation list)
pkg/license         license issuing and verification (import this in your backend)
android/agent       Android Device Owner agent (Kotlin)
android/client      Android client library for your app
web/                embedded dashboard and install.sh
deploy/, examples/  service units, compose file, sample agent config and hooks
```

## Limitations and next steps

- A single server with SQLite comfortably handles thousands of devices at a 30s
  heartbeat. Larger fleets would need Postgres and several server replicas.
- There is no single sign-on (OIDC/SAML) or two-factor sign-in yet. Put the dashboard behind your
  identity-aware proxy if you need either.
- Agents poll; there is no push channel, so actions run on the next check-in (30s by default).
- There are no delta updates; every update downloads the full artifact.
- The agent's metrics are richest on Linux. Other platforms report the basics.
- On Android, actions are limited to reboot and restarting the agent (`exec` isn't supported).
- Android devices can't yet act as BLE gateways. That needs Nordic's Android DFU and mcumgr libraries
  in the Android agent.
- On macOS and Windows, the built-in checks cover SMART and disk space only; temperature, power and
  battery checks are Linux-only. Program monitors work everywhere.
