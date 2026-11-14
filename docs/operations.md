# Operations runbook

What to do day to day and when something goes wrong. Commands assume the server runs either from
the provided systemd unit ([`deploy/armada-server.service`](../deploy/armada-server.service), data
in `/var/lib/armada-server`) or with Docker Compose
([`deploy/docker-compose.yml`](../deploy/docker-compose.yml), data in the `armada-data` volume).
Adjust paths if yours differ.

## What lives where

The data directory holds everything the server needs:

| File | What it is | If lost |
|---|---|---|
| `armada.db` | The database: devices, releases, rollouts, users, history, alerts | Everything is gone except artifacts |
| `artifacts/` | Uploaded release files | Devices can't download those releases |
| `signing.key` | Signs releases; every agent remembers this key from enrollment and checks each release against it | **Every device rejects new releases** until it is enrolled again |
| `license.key` | Signs Android app licenses | Issued licenses stop verifying |
| `secrets.key` | Encrypts stored secrets (SSO client secret, channel credentials, TOTP seeds) | Those secrets must be entered again; two-factor must be set up again |
| `admin-token` | The generated admin token (if you didn't set one) | A new one is generated at start |

Backups contain all of these except `admin-token`. They hold private keys: treat a backup like a
password. `signing.key` is the one file you can't recover from: keep an extra copy of it offline
(it's 45 bytes), separate from the nightly backups.

## Checking the server is healthy

- **Liveness:** `GET /healthz` returns `ok`. Point your uptime monitor at it (through the same URL
  devices use, so TLS and the proxy are checked too).
- **Logs:**
  - systemd: `journalctl -u armada-server -f`;
  - Docker: `docker compose logs -f armada`.
  - Add `-json-logs` for structured logs.
- **Dashboard:** the **Overview** page shows devices online and health problems. A red banner
  appears there if the server's own TLS certificate is near expiry.
- **Alerts worth having** (Monitoring → Alert rules), each notifying your on-call channel:
  - `server_cert_expiry`: the server's certificate. If it lapses, every device stops checking in.
  - `deployment_paused`: a rollout stopped itself because updates were failing.
  - `device_offline` and `health`: for devices.
  - `cert_expiry`: certificates on devices.

## Backups

Take one nightly, keep several, keep a copy off the server, and **check that each one restores**.
For example, from cron on another machine:

```bash
#!/bin/sh
# /etc/cron.daily/armada-backup
set -e
export ARMADA_SERVER=https://fleet.example.com
export ARMADA_TOKEN=ut_...            # a personal API token of an admin user
f=/backups/armada-$(date +%F).tar.gz
armadactl backup -o "$f"
armada-server -verify-backup "$f"     # restores it in a temporary directory and checks it
find /backups -name 'armada-*.tar.gz' -mtime +30 -delete
```

- **What `-verify-backup` checks:** that the archive is complete, the keys read, and the database
  passes SQLite's integrity check and opens with this version. It also checks every release has its
  artifact.
- **What it prints:** devices, releases, users, and a fingerprint of the signing key. That
  fingerprint should stay the same from one night to the next.
- **When it fails:** it exits non-zero, so cron mails you.
- **Encryption:** to keep backups encrypted at rest, pipe them through `age` or `gpg`. `armadactl backup
  -o -` writes to stdout, and `armada-server -verify-backup -` reads from stdin.

## Restoring

Devices keep their own keys, so a restored server is recognised as soon as it's reachable at the same
address. Nothing needs doing on the devices.

1. **Stop the server.** Keep the old data directory: move it aside, don't delete it.
2. **Restore the newest good backup** into an empty directory. `-restore` refuses a directory that
   isn't empty, checks the result like `-verify-backup`, and removes what it wrote if the check fails.
3. **Start the server** and check it (see below).

systemd (the unit uses `DynamicUser`, so the real directory is under `/var/lib/private`; systemd fixes
file ownership on start):

```bash
systemctl stop armada-server
mv /var/lib/private/armada-server /var/lib/private/armada-server.old
armada-server -data-dir /var/lib/private/armada-server -restore /backups/armada-2026-11-13.tar.gz
systemctl start armada-server
```

Docker Compose (restores into a fresh volume; the old one is kept under another name):

```bash
docker compose down
docker volume create armada-data-old && docker run --rm -v deploy_armada-data:/from -v armada-data-old:/to alpine cp -a /from/. /to/
docker volume rm deploy_armada-data
docker compose run --rm -v /backups/armada-2026-11-13.tar.gz:/backup.tar.gz:ro armada -restore /backup.tar.gz
docker compose up -d
```

(The volume's name is `<project>_armada-data`: check it with `docker volume ls`.)

**After restoring:**
- **Admin token:** sign in with your users. The admin token is whatever you set, or a newly generated
  one in `admin-token`.
- **Check the dashboard:** the device count matches, devices come online within a few heartbeats,
  and **Releases** lists your releases.
- **Anything after the backup is lost:** devices enrolled since the backup must be enrolled again,
  and releases uploaded since must be uploaded again. Devices check in and report their current
  versions, so the dashboard catches up by itself.

**If `signing.key` is lost** (no backup could be restored): the server makes a new key, and every
device rejects releases signed with it, because each agent remembers the key it saw at enrollment.
That's deliberate: it's what stops a stolen server or a man in the middle from pushing software. The
devices must be enrolled again: on Linux, stop the agent, delete its `state_dir`, and start it with an
enrollment token; Android devices need provisioning again. This is why `signing.key` deserves an
offline copy.

## Upgrading

1. **Back up**, and verify the backup (above).
2. **Preview the database changes:** `armada-server -data-dir … -migrate-status` shows what will
   change, without changing anything.
3. **Replace the binary** (or `docker compose pull && docker compose up -d`) and restart.
   - **Automatic backup:** before changing the database schema, the server copies it to
     `armada.db.before-v<N>-<time>`.
   - **Failed upgrade:** if an upgrade step fails, nothing changes and the server doesn't start.
4. **Going back:** stop, restore the `before-v…` copy as `armada.db` (or restore the backup), and
   start the older binary. An older server refuses a database a newer one upgraded; it never
   corrupts it.

## Certificates

- **The server's certificate.**
  - **With `-tls-cert`/`-tls-key`:** renew it as usual (certbot, acme.sh). The server picks up the
    new files by itself within a minute; `systemctl kill -s HUP armada-server` makes it immediate.
    The log says `TLS certificate reloaded`.
  - **Behind a proxy:** renew it on the proxy. Armada only watches it.
- **Certificates on devices:** see *Certificates on devices* in the README. With a renewal policy,
  the only manual step is getting the client to sign the CSRs. If they're slow, the Certificates
  page shows what's waiting and since when, and the policy keeps reminding them.

## Access

- **Rotate the generated admin token:** restart once with `-rotate-admin-token`. The old token stops
  working at once.
- **In production:** sign in with users (SSO, passkeys or two-factor) and run with
  `-disable-admin-token`. If the identity provider is down, remove the flag and restart to get the
  break-glass token back.
- **Someone leaves:** disable them on the Users page (or in your identity provider, if SCIM is set
  up). Their sessions and personal tokens stop working immediately.
- **Locked out of every admin account:** stop the server, start it without `-disable-admin-token`,
  and read `admin-token` from the data directory.

## Incidents

**The server is down.**
- **Devices keep running** what they have, and retry until it's back.
- **What pauses:** update offers and actions, until the server is back. Rollouts resume where they
  stopped.
- **What to do:** check the logs, disk space (`df` on the data directory), and the certificate.
  Then restore if the data is damaged.

**A bad release is going out.**
- **Pause** the rollout (Deployments → Pause, or `armadactl deployments pause <id>`). Devices that
  haven't updated won't.
- **Abort** it to stop it for good.
- **Roll back** devices that already updated: make a new rollout of the previous release with
  **Allow downgrade**.
- **Already happening automatically:** rollouts pause themselves when the failure rate passes the
  limit, and each device rolls back if its health check fails.

**A device is lost or stolen.**
- **Android:** Wipe it (device page → Wipe device, typed confirmation), revoke its app licenses, then
  retire it.
- **Other devices:** retire it. Its key stops working at once.

**A client contract ends.**
- **Their devices:** select them (Devices → filter by `customer=…` → select all) and use bulk
  **Retire** if the hardware is gone, or **Mark as spare** if it comes back to you.
- **Their access:** disable their users or groups, and delete their alert rules, channels, report
  schedules and renewal policies.

**Many devices offline at once.**
- **Check the server first:** the TLS certificate, the proxy, DNS, and that `/healthz` is reachable
  from outside.
- **One site only:** if just one client or location is affected, it's their network, not the server.
  Maintenance windows stop the alerts while it's being fixed.

## Capacity

- **What one server handles:** one server with SQLite handles thousands of devices at a 30-second
  heartbeat. See [scaling.md](scaling.md) for what to change beyond that.
- **Disk growth comes from:**
  - artifacts (delete old releases you no longer deploy);
  - history (`-history-retention`, 90 days by default);
  - the activity log (`-event-retention`, 30 days by default; `8760h` keeps a year for audits).
