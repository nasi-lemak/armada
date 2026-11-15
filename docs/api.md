# API

Admin endpoints need `Authorization: Bearer <token>`: the admin token, a dashboard session from
`POST /api/v1/auth/login`, or a personal API token. Each endpoint requires a minimum role, and
client accounts get `403` on fleet-wide endpoints and `404` for devices outside their scope. Agent
endpoints use the per-device key issued at enrollment.

**API reference:** the server generates an OpenAPI 3.1 document from its own route table, so it
always matches the running version.
- **Getting it:** `GET /api/v1/openapi.json`, `armadactl openapi`, or **My account → API reference** in
  the dashboard (searchable, with a download button).
- **What it lists:** every admin endpoint with the least role it needs (`x-armada-min-role`) and
  whether client accounts may use it (`x-armada-client-accounts`).
- **Bodies:** request and response bodies are the JSON forms of the types in
  [`internal/api`](../internal/api/types.go).

## Event webhooks

Event webhooks send activity-log events to another system as they happen: a SIEM, a ticketing tool,
or your own service. Set them up under **Activity → Webhooks**, or with `armadactl webhooks`.

- **Which events:** all of them, or chosen types (`device.enrolled`), or prefixes (`alert.*`).
- **The delivery:** a JSON `POST`:
  ```json
  {"version":"1","id":1234,"time":"2026-11-14T09:00:00Z","type":"device.enrolled","message":"till-7 enrolled (linux/arm64, agent 1.4.2)",
   "actor":"","device":{"id":"dev_…","name":"till-7","channel":"stable","labels":{"customer":"acme"}},"hook":"SIEM"}
  ```
- **Signing:**
  - each delivery carries `X-Armada-Event`, `X-Armada-Timestamp` and
    `X-Armada-Signature: sha256=<hex HMAC-SHA256(secret, timestamp + "." + body)>`;
  - the secret is shown once, when the webhook is created or its secret rotated, and is stored
    encrypted;
  - check the signature, and reject old timestamps.
- **Retries:**
  - each delivery is retried up to 4 times with backoff;
  - `id` is unique, so drop duplicates;
  - after 20 failed deliveries in a row the webhook turns itself off, and the activity log says why;
  - turning it back on resets the count.
- **Client accounts:**
  - their operators can add webhooks of their own;
  - those get only events about that client's devices (the same as their activity log), without
    rollout IDs;
  - they must use `https://` to a public address. The address is checked at connect time, so DNS
    tricks and redirects don't reach internal hosts.
- **What's not sent:** a webhook's own events (`hook.*`), so a failing endpoint can't cause a loop.

```bash
armadactl webhooks create --name SIEM --url https://siem.example.com/armada --types "device.*,alert.*"
```

```bash
armadactl webhooks test <id>
```

```bash
armadactl webhooks list
```

| Method & path | Purpose |
|---|---|
| `GET /api/v1/summary` | Fleet counts and version distribution |
| `GET /api/v1/devices` (`?channel=&label=k=v&status=&lifecycle=`) | List devices (retired ones only with `lifecycle=retired` or `all`) |
| `GET/PATCH/DELETE /api/v1/devices/{id}` | Device detail (with updates, actions, events), edit name/channel/labels/inventory, remove |
| `GET/POST /api/v1/devices/{id}/actions` | List or queue `reboot`, `restart-agent`, `netcheck`, `screenshot`, `exec`, `logs`; on Android `lock`, `wipe` |
| `GET /api/v1/actions/{id}/attachment` | A screenshot action's image (operators on the device) |
| `POST /api/v1/devices/{id}/wake` | Ask a device to check in now |
| `POST /api/v1/bulk` (`operation`, `selector` or `all`, `dry_run`, `expect`) | Bulk operation: preview, then apply with the previewed count |
| `GET /api/v1/bulk`, `GET /api/v1/bulk/{id}` | Recent bulk operations; per-device progress |
| `POST /api/v1/devices/{id}/lifecycle` (`{"state": "spare\|repair\|active\|retired", "note": …}`) | Change a device's lifecycle state (retiring is final) |
| `POST /api/v1/devices/{id}/replace` (`{"replacement_id": …, "old_state": "repair"}`) | Replace a device (RMA): the replacement takes over name, channel and labels |
| `GET/POST /api/v1/config-profiles`, `GET/PUT/DELETE /api/v1/config-profiles/{id}` | Configuration profiles (with compliance counts) |
| `GET/POST /api/v1/tokens`, `DELETE /api/v1/tokens/{id}` | Enrollment tokens (`"lifecycle": "spare"` enrolls devices as spares) |
| `GET/POST /api/v1/releases`, `GET/DELETE /api/v1/releases/{id}`, `GET …/artifact` | Releases (upload is `multipart/form-data`) |
| `GET/POST /api/v1/deployments`, `GET /api/v1/deployments/{id}` | Deployments |
| `POST /api/v1/deployments/{id}/{approve,pause,resume,advance,retry,complete,abort}` | Control a rollout (`approve`: someone other than its creator) |
| `GET /api/v1/events` | Activity log. Filters: `type` (comma list; `prefix.*`), `actor` (`system` for none), `q`, `since`/`until` (date or RFC 3339; dates use `tz`), `device_id`, `deployment_id`. Paging: `limit` (up to 1000), `before_id`. `format=csv` exports up to 50,000 entries |
| `GET /api/v1/events/types` | Event types recorded, for filter menus |
| `GET/POST /api/v1/event-hooks`, `PUT/DELETE /api/v1/event-hooks/{id}`, `POST /api/v1/event-hooks/{id}/test` | Event webhooks |
| `GET /api/v1/backup`, `GET /api/v1/backup/status`, `POST /api/v1/backup/run` | Download a backup; automatic backups' status; make one now (admins) |
| `GET/POST /api/v1/sites`, `DELETE /api/v1/sites/{id}`, `POST /api/v1/sites/{id}/token`, `POST /api/v1/sites/{id}/import` | Sites reporting to this central server (admins) |
| `POST /api/v1/sites/report` | A site server's report (site token) |
| `GET /api/v1/site/snapshot`, `GET /api/v1/site/central` | This server's snapshot (for export), and how its reports to the central one are doing |
| `GET /api/v1/groupings` | Customers and projects (the `-group-labels` keys) among your devices, for the picker |
| `GET /api/v1/insights?range=7d` | What needs attention among the devices in view: software behind, certificates, disconnections |
| `GET /api/v1/devices/{id}/outages` | A device's outages, with cause (network or restart) and the agent's last error |
| `GET /api/v1/openapi.json` | OpenAPI 3.1 document of the admin API |
| `GET /api/v1/labels?ids=dev_a,dev_b` | Asset label data: name, serial, asset tag, device URL and its QR code as an SVG path |
| `GET/POST /api/v1/scheduled-actions`, `PUT/DELETE /api/v1/scheduled-actions/{id}`, `POST /api/v1/scheduled-actions/{id}/run` | Scheduled actions |
| `GET /api/v1/registrations`, `POST /api/v1/registrations/import` (CSV body; `dry_run=1`), `DELETE /api/v1/registrations/{serial}` | Pre-registration and bulk import |
| `GET /api/v1/devices/{id}/timeline` | A device's timeline. `kinds` (alert, update, action, lifecycle, cert, health, config, maintenance, other), `limit` (up to 200), `cursor` (the previous page's `next`) |
| `GET /api/v1/signing-key` | Release-signing public key |
| `POST /api/v1/agent/enroll`, `/heartbeat`, `/updates`, `/actions/{id}/result`, `GET /api/v1/agent/releases/{id}/artifact`, `GET /api/v1/agent/wait`, `GET /api/v1/agent/releases/{id}/chunks` | Agent protocol (`wait` is the push long-poll; `chunks` the delta-update index) |
| `GET /install.sh`, `GET /install.ps1`, `GET /api/v1/agent/download?os=&arch=&package=` | Public bootstrap (agent binaries and APK only) |
| `GET/PUT/DELETE /api/v1/apps[/{package}]` | Android apps: signing certificates and licensing |
| `POST /api/v1/android/provisioning` | Android setup QR code for an enrollment token |
| `POST /api/v1/agent/challenge`, `POST /api/v1/agent/license` | Attestation and license protocol |
| `GET /api/v1/license/jwks` | Public keys for verifying licenses |
| `GET/POST /api/v1/monitors`, `PUT/DELETE /api/v1/monitors/{id}` | Program health checks |
| `GET/POST /api/v1/channels`, `PUT/DELETE /api/v1/channels/{id}`, `POST …/{id}/test` | Notification channels (`client_scope` for a client's) |
| `GET/POST /api/v1/alert-rules`, `PUT/DELETE /api/v1/alert-rules/{id}` | Alert rules (`client_scope` for a client's) |
| `GET /api/v1/alerts[?status=firing]` | Firing and recent alerts |
| `GET /api/v1/certificates` (`?expiring_within=30&renewal=csr_ready&label=`) | Managed certificates on devices, with renewals |
| `POST /api/v1/certificates/renew` | New key and CSR on the devices (`selector`, `targets`, optional subject/names/key type) |
| `GET /api/v1/certificates/csrs`, `POST /api/v1/certificates/email` | Waiting CSRs as a zip, or emailed |
| `POST /api/v1/certificates/upload` (`{"pem": …}`) | Signed certificates: matched by key, checked, installed |
| `POST /api/v1/certificates/{device}/{target}/cancel` | Cancel a waiting renewal |
| `GET/POST /api/v1/cert-policies`, `PUT/DELETE …/{id}`, `POST …/{id}/run` | Automatic renewal policies |
| `GET /api/v1/reports?label=k=v&period=last-month\|this-month\|last-week\|last-7d\|last-30d` (or `from=&to=`, `tz=`, `format=json\|csv\|html`) | Customer report |
| `GET/POST /api/v1/report-schedules`, `DELETE …/{id}`, `POST …/{id}/send` | Scheduled report emails |
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

---

[← Armada documentation](../README.md#documentation)
