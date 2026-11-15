# Server configuration

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
| `-event-retention` | `ARMADA_EVENT_RETENTION` | `720h` (30 days of activity log; `8760h` for a year) |
| `-history-retention` | `ARMADA_HISTORY_RETENTION` | `2160h` (90 days) |
| `-session-ttl` | `ARMADA_SESSION_TTL` | `12h` (dashboard sign-in, sliding) |
| `-rollout-approval` | `ARMADA_ROLLOUT_APPROVAL` | `second-person` (or `off`) |
| `-unapproved-max-percent` | | `10` (largest first stage allowed before approval) |
| `-trusted-proxies` | `ARMADA_TRUSTED_PROXIES` | none (comma-separated IPs/CIDRs) |
| `-smtp-host`, `-smtp-port`, `-smtp-username`, `-smtp-from` | `ARMADA_SMTP_HOST`, …, `ARMADA_SMTP_PASSWORD` (environment only) | no relay |
| `-report-brand` | `ARMADA_REPORT_BRAND` | `Armada` (name on customer reports and their emails) |
| `-screenshots`, `-screenshot-retention` | `ARMADA_SCREENSHOTS`, `ARMADA_SCREENSHOT_RETENTION` | off, `168h` (remote screenshots; devices must also allow them) |
| `-group-labels` | `ARMADA_GROUP_LABELS` | `customer,project` (label keys the dashboard's picker offers) |
| `-attestation-status-file` | `ARMADA_ATTESTATION_STATUS_FILE` | — (offline copy of Google's attestation revocation list) |
| `-check-public-cert` | `ARMADA_CHECK_PUBLIC_CERT` | `true`: behind a reverse proxy, check hourly the certificate `-public-url` presents |
| `-disable-admin-token` | `ARMADA_DISABLE_ADMIN_TOKEN` | `false` |
| `-rotate-admin-token` | | replace the generated token at startup |
| `-password-login` | `ARMADA_PASSWORD_LOGIN` | `true` |
| `-require-2fa` | `ARMADA_REQUIRE_2FA` | `false` |
| `-oidc-issuer`, `-oidc-client-id` | `ARMADA_OIDC_ISSUER`, `ARMADA_OIDC_CLIENT_ID` | SSO off |
| | `ARMADA_OIDC_CLIENT_SECRET` | (environment only) |
| `-oidc-name`, `-oidc-allowed-domains`, `-oidc-create-role`, `-oidc-group-roles`, `-oidc-groups-claim`, `-oidc-scopes`, `-oidc-redirect-url` | `ARMADA_OIDC_…` | see [Single sign-on](users.md#single-sign-on) |
| `-scim-default-role` | `ARMADA_SCIM_DEFAULT_ROLE` | `viewer`, used only while no group mappings exist (see [SCIM provisioning](users.md#scim-provisioning)) |
| `-push-hold` | `ARMADA_PUSH_HOLD` | `50s` (agent long-poll; keep below your proxy's idle timeout; `0` disables) |
| `-central-url`, `-central-every` | `ARMADA_CENTRAL_URL`, `ARMADA_CENTRAL_EVERY` | — (report to a central Armada server as a site), `5m` |
| | `ARMADA_CENTRAL_TOKEN` | (environment only) the site token from the central server |
| `-backup-dir` | `ARMADA_BACKUP_DIR` | — (automatic backups into this folder; ideally another disk) |
| `-backup-every`, `-backup-keep` | `ARMADA_BACKUP_EVERY` | `24h`, `14` (how often, and how many to keep in each place) |
| `-backup-encrypt-to` | `ARMADA_BACKUP_ENCRYPT_TO` | — (age recipients; required with `-backup-s3-url`); restore with `-backup-identity` |
| `-backup-s3-url`, `-backup-s3-region` | `ARMADA_BACKUP_S3_URL`, `ARMADA_BACKUP_S3_REGION` | — (also upload to S3-compatible storage: `https://host/bucket/prefix/`), `us-east-1` |
| | `ARMADA_BACKUP_S3_ACCESS_KEY`, `ARMADA_BACKUP_S3_SECRET_KEY` | (environment only) |

The data directory holds `armada.db` (SQLite), `artifacts/`, `signing.key`, `license.key`, `secrets.key` and
`admin-token`. **Back it up.** The simplest way is to let the server do it:

```bash
armada-server ... -backup-dir /mnt/backups/armada
```

```bash
age-keygen -o armada-backup.key     # keep this file off the server, somewhere safe
```

```bash
ARMADA_BACKUP_S3_ACCESS_KEY=... ARMADA_BACKUP_S3_SECRET_KEY=... armada-server ... \
  -backup-s3-url https://s3.eu-west-1.amazonaws.com/my-bucket/armada/ -backup-encrypt-to age1...
```

- **Encryption:** a backup holds the server's private keys.
  - `-backup-encrypt-to` (one or more [age](https://age-encryption.org) public keys, or a file of
    them) encrypts each backup before it's written: `armada-backup-….tar.gz.age`.
  - Uploads to S3 require it, and the S3 URL must be https (http only to localhost).
  - Folder backups can use it too.
  - To restore, give the private key:
    `armada-server -restore armada-backup-….tar.gz.age -backup-identity armada-backup.key`. The same
    flag works with `-verify-backup`, and `age -d` decrypts it too.

- **What it does:**
  - makes a backup every day (`-backup-every`);
  - keeps the newest 14 in each place (`-backup-keep`);
  - retries within the hour if one fails.
- **Where backups go:** a folder, S3-compatible storage (AWS S3, MinIO, Wasabi, Backblaze B2; give a
  path-style URL), or both.
- **Monitoring:**
  - the overview warns admins when backups are off, the last one failed, or none has succeeded for
    two periods;
  - **Back up now** retries;
  - `armadactl backup status` and `armadactl backup run` do the same from the command line.
- **The activity log** records each backup (`server.backup`) or failure (`server.backup_failed`).

You can also take one by hand, from anywhere with an admin token:

```bash
armadactl backup -o /backups/armada-$(date +%F).tar.gz
```

The backup is a consistent snapshot taken while the server runs. It contains the database, the signing
keys and every artifact. It holds private keys, so store it like a password.

- **Check a backup:** `armada-server -verify-backup FILE` restores it in a temporary directory and
  checks it, changing nothing. Run it on every nightly backup.
- **Restore:** stop the server, then run `armada-server -data-dir NEW_DIR -restore FILE`. It refuses
  a directory that isn't empty, checks the result, and removes it again if the check fails. Then
  start the server on `NEW_DIR`.
- **`signing.key`:** every agent remembers this key from enrollment, so losing it means every device
  rejects new releases until it's enrolled again. Keep an extra copy offline.

Trying it out on real devices? Start with the [pilot checklist](pilot.md).

See the [operations runbook](operations.md) for backups from cron, restoring with systemd or
Docker, upgrades, access and incidents.

**Monitoring the server.** `GET /metrics` serves Prometheus metrics: devices by status, check-ins,
request latency by route, engine passes, queues, database size, backups and certificate expiry. Give
Prometheus a `-metrics-token` (it reads only the metrics). Scrape config, the metric list and
suggested alert rules are in the [operations runbook](operations.md#prometheus).

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

Deployment options: [`deploy/armada-server.service`](../deploy/armada-server.service)
(systemd), [`Dockerfile`](../Dockerfile) and
[`deploy/docker-compose.yml`](../deploy/docker-compose.yml). In production, put the server
behind TLS, either with `-tls-cert`/`-tls-key` or a reverse proxy.

On Debian or Ubuntu, the `armada-server` package from a [release](development.md#releasing)
installs the server, `armadactl` and the systemd service. Set `ARMADA_PUBLIC_URL` and other
settings in `/etc/default/armada-server`, then `systemctl start armada-server`. The service runs as
a throwaway system user and can only write to `/var/lib/armada-server`: for `-backup-dir` elsewhere,
or TLS files only root can read, add `ReadWritePaths=` or `LoadCredential=` with
`systemctl edit armada-server`.

**The server's certificate.** If it expires, every device stops checking in at once, so Armada
watches it:

- **With `-tls-cert`/`-tls-key`:** the files are re-read when they change, so a renewal (certbot,
  acme.sh, your CA's tooling) needs no restart. `kill -HUP` reloads them at once. A half-finished
  renewal (new certificate, old key) is ignored until the pair matches; the current certificate
  stays in use meanwhile.
- **Behind a reverse proxy:** the server checks hourly the certificate its `-public-url` presents,
  which is what devices see (`-check-public-cert=false` turns this off).
- **Warnings:** from 21 days before expiry, the Overview page shows a warning to fleet-wide accounts
  and the server logs one daily. Add an alert rule with the condition
  **Server's own certificate expiring** (`server_cert_expiry`) to be notified. It also fires if the
  public URL's certificate isn't trusted by standard roots (an incomplete chain, for example).

---

[← Armada documentation](../README.md#documentation)
