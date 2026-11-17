# Customers and projects

Label devices with their customer and project (`customer=acme`, `project=kiosk-rollout`).
Enrollment tokens can add the labels, or use an import or bulk labels.

## The picker

The bar under the top menu lists the customers and projects your devices have, with their device
counts.

- **Choosing one filters every page** to those devices:
  - the overview tiles and the *Needs attention* section;
  - the device list, search and bulk actions;
  - the rollouts list (rollouts that target any of those devices);
  - the activity log, alerts, certificates and reports.
- **It stays in the address** (`/?customer=acme&project=tills#/devices`), so a view can be bookmarked
  or sent to a colleague.
- **What it doesn't filter:**
  - a device opened by its link still opens;
  - the fleet history charts always show the whole fleet, and say so.
- **It narrows what you see, not what you may do:**
  - client accounts are still limited to their own devices, whatever they pick;
  - the API takes the same filter as the header `X-Armada-View: customer=acme;project=tills`.
- **Other label keys:** to offer different keys, start the server with `-group-labels client,site`
  (default `customer,project`).

## Needs attention

The overview shows, for the devices in view:

- **Software not on the latest version:**
  - per package, how many devices run the newest release for their channel and platform;
  - which don't, with installed → latest versions and any update already in progress;
  - **Update these** (fleet-wide operators) starts a staged rollout of the newest release to just
    those devices, one rollout per release, with the usual stages and safety checks.
- **Certificates to renew:**
  - expired;
  - expiring within 30 days;
  - waiting for the client to sign a CSR;
  - renewal failed.
- **Disconnections** over the last 24 hours, 7 days or 30 days:
  - how many, on how many devices, and how long they were offline in all;
  - the devices that drop out most;
  - which devices are offline right now.

## How disconnections are tracked

When a device checks in again after losing contact, the server records an outage: when it started
and ended, and why.

- **Network:** the device stayed up but couldn't reach the server. Its uptime spans the gap.
- **Restart:** the device rebooted or lost power during the gap. Its uptime is shorter.
- **Error detail:** the agents (Linux, macOS, Windows and Android) note their failed attempts. The
  next check-in reports when the trouble started and the last error, e.g. `lookup fleet.example.com:
  no such host` or `i/o timeout`.
- **Short drops count too:** drops shorter than the offline threshold are recorded as long as the
  agent noticed them.
- **Where outages show:**
  - on the device's timeline (*Connectivity*);
  - at `GET /api/v1/devices/{id}/outages`;
  - in the overview.
- **Retention:** outages are kept as long as health history (90 days by default).
- **When an outage starts:** with the agent's report, it starts at the agent's first failed attempt,
  which is the next heartbeat after the connection went. Without one, it starts at the last check-in.

## Health data during outages

While an agent can't reach the server, it keeps one reading every 5 minutes: its check results,
load, memory and disk.
- **Where readings are kept:** they're saved on the device, so a restart doesn't lose them. The
  Linux/macOS/Windows agents keep up to a week of readings; Android keeps a day.
- **When the server is back:** the agent uploads them, 100 per heartbeat, and the server stores them
  at their original times. The device's charts then show what happened during the outage, at full
  resolution and in the hourly roll-ups.
- **What isn't filled in:** the `online` series. Availability still shows the device as unreachable,
  because it was.

## Remote screenshots

Take a screenshot of a kiosk or signage screen from its device page (**Screenshot**). It's useful when
a customer says "the screen looks wrong".

**Privacy controls.** Every one of these must allow it:
- **The server:** it's started with `-screenshots` (off by default).
- **The device:** its agent config has `allow_screenshots: true` (off by default), so whoever runs
  the device has agreed to it.
- **The person asking:** taking or viewing one needs the operator role on that device. The activity
  log records each request and who made it.
- **Retention:** images are deleted after `-screenshot-retention` (7 days by default). They aren't
  included in backups.

**How the agent captures the screen:**
- **Linux:** `grim` (Wayland), ImageMagick `import` or `scrot` (X11, `DISPLAY=:0` by default), or
  `gnome-screenshot`.
- **macOS:** `screencapture`. The agent needs the Screen Recording permission.
- **Windows:** PowerShell. A service running in session 0 can't see the desktop, so run the agent in
  the user's session, or use `screenshot_command`.
- **Kiosks that run as another user:** set `screenshot_command`, e.g.
  `["sudo","-u","kiosk","env","DISPLAY=:0","import","-window","root","png:-"]`.
- **Android:** not supported. It only allows screen capture after someone accepts a prompt on the
  device.

The image appears under the device's **Actions**: choose **Show screenshot**, and click the image to
download it.

## Network check

**Network check** on a device's page tests the route from the device to the server, step by step. It's
also available for many devices from the device list, as a scheduled action, or with
`armadactl devices netcheck <id>`, which waits and prints the report.

- **What it checks:**
  - interfaces and default gateway (pinged on Linux);
  - proxy;
  - DNS;
  - TCP to the server or proxy;
  - the TLS handshake, with the certificate, its expiry and whether the device trusts it;
  - three HTTPS round trips to `/healthz`.
- **Android** also reports the network type (Wi-Fi, mobile, ethernet) and whether Android sees
  internet access there.
- **The result:** a readable report under **Actions**. It stops at the first step that fails and says
  what that usually means, e.g. *the device doesn't trust the server's certificate; with a private
  CA, set `ca_cert`*.
- **Who can run it:** client operators too, since it changes nothing on the device.

```
Server      https://fleet.example.com
Interfaces  eth0 192.168.1.20/24
Gateway     192.168.1.1 · ping: 3 packets transmitted, 3 received, 0% packet loss
Proxy       none: connecting directly
DNS         fleet.example.com → 203.0.113.10 (12 ms)
TCP         server fleet.example.com:443 connected in 35 ms
TLS         handshake 80 ms · TLS 1.3 · fleet.example.com · expires 2027-01-01 (47 days) · trusted
HTTPS       /healthz 200 in 95 ms (3 of 3 tries: 88 ms, 95 ms, 102 ms)

Result: OK. The server is reachable.
```


## Offboarding a customer

When a customer leaves, **Users → Customer offboarding** (or `armadactl offboard`) exports what
Armada holds about them and then removes it in one step. A customer is its labels, e.g.
`customer=acme`; anything whose scope includes all of them belongs to the customer.

1. **Preview** (`armadactl offboard plan --label customer=acme`) lists what would go:
   - devices with those labels;
   - accounts limited to that customer (deleted), and accounts that also have access to other
     customers (they lose only this one);
   - group mappings, enrollment tokens, alert rules, notification channels, event webhooks,
     pre-registrations, scheduled actions, maintenance windows, report schedules and certificate
     policies made for the customer.
2. **Export** (`armadactl offboard export --label customer=acme`) downloads a zip:
   - `devices/`: one file per device with its inventory, updates, alerts, disconnections,
     certificates and hourly health history (as far back as history is kept);
   - `activity.csv`: activity about the customer's devices or by its accounts, with each event's
     hash;
   - `accounts.json`, `settings.json` and `manifest.json`.

   Passwords, device keys, tokens and webhook secrets are never included.
3. **Offboard** (`armadactl offboard run --label customer=acme --confirm customer=acme`): you type
   the labels to confirm. Devices are deleted, or kept as retired records with `--retire` (the
   checkbox). Either way their keys stop working.

Only fleet admins can do this. The export and the offboarding are recorded in the activity log
(`customer.exported`, `customer.offboarded`, and each device removed).

---

[← Armada documentation](../README.md#documentation)
