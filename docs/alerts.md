# Alerts

Alerts tie a **rule** to one or more **channels**. You can set them up on the dashboard's **Monitoring**
page or with the CLI.

| Condition | Fires when | Resolves when |
|---|---|---|
| `health` | a matching device has a check at `warning` or `critical`. You can limit it to some kinds of check, such as `storage` or `power`. | the checks recover |
| `device_offline` | a device hasn't checked in for N minutes | it checks in |
| `flaky_connectivity` | a device lost its connection at least N times (default 5) within H hours (default 24). The alert says how many were network drops or restarts, and gives the last error. | the drops age out of the window |
| `site_silent` | (central servers) a site server hasn't reported for M minutes (default 30) | it reports again |
| `site_outage` | at least N devices (default 3) **and** P% (default 50) of the devices sharing a label value (`group_by`, default `site`) have been offline for M minutes (default 5). One alert per site: usually its internet, power or proxy is down. | enough of them are back |
| `update_failed` | an update fails or rolls back. All failures in one rollout fold into one alert. | after an hour |
| `deployment_paused` | a rollout auto-paused (or was paused) | it is resumed or aborted |
| `cert_expiry` | a managed certificate on a device is within N days of expiring (default 30; critical within 7) | it's renewed |
| `server_cert_expiry` | the server's own TLS certificate is within N days of expiring (default 21; critical within 7), or devices may not trust it | it's renewed |

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

## Acknowledging alerts

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

## Maintenance

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
  removed after 100 days. Customer reports use it to leave planned downtime out of availability, so
  end a maintenance rather than deleting it.

This is different from the agent's `maintenance_window`, which only limits when a device installs
updates.

## Alerts for client accounts

Client operators set up their own alerts on the **Monitoring** page, or with
`armadactl rules create … --client customer=acme`. For example, they can email their on-call team
when a kiosk goes offline. Your admins manage vendor-wide alerts and can see and change every
client's.

- **A client's rule only ever sees that client's devices.** It matches devices in the client's scope
  (narrowed further by its own labels), however it was written.
- **A client's alerts only go to that client's channels**, and vendor alerts only to vendor channels.
  No notification about one customer's devices can reach another customer.
- Client rules can watch `device_offline`, `health`, `cert_expiry`, `flaky_connectivity` and
  `site_outage` on their own devices. `update_failed` and `deployment_paused` summarise rollouts
  across customers, and `server_cert_expiry` is about the vendor's server, so those stay
  vendor-only.
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

---

[← Armada documentation](../README.md#documentation)
