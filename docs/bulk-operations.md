# Bulk operations

On the **Devices** page, tick devices (or the header box for everything shown, after filtering), pick
an operation and choose **Apply…**. The same operations are in the CLI (`armadactl bulk`) and the API
(`POST /api/v1/bulk`).

| Operation | Who |
|---|---|
| `reboot`, `restart-agent`, `wake` (check in now), `netcheck`, `lock` (Android) | operators on the devices |
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

## Scheduled actions

A scheduled action runs a bulk operation at a set time, such as a nightly kiosk reboot or a weekly agent
restart. Set them up under **Devices → Scheduled actions**.

- **Operations:** reboot, restart the agent, lock (Android), check in now, or run a command. Commands
  are for fleet-wide operators only.
- **When:** a time of day (`HH:MM`) in a time zone, on chosen weekdays (none means every day). Daylight
  saving changes are handled.
- **Which devices:** a label selector, a channel, or every device you can see. Each run picks its devices
  afresh, so new devices are included.
- **Alerts are held** on the devices for 30 minutes from each run by default, using a maintenance
  window. A reboot won't page anyone. Set the hold to 0 to turn this off.
- **Each run is a bulk operation:** its progress shows on the Bulk page, and the activity log records
  `schedule.ran` (or `schedule.failed`).
- **Missed runs:** if the server was down more than an hour past a run time, the run is recorded as
  missed rather than run late.
- **Run now** tries a schedule immediately. **Pause** keeps it without running.
- **Client accounts:**
  - their operators can schedule actions on their own devices;
  - the schedule acts only on that client's devices, even when set to all devices;
  - the client sees its runs.

```bash
armadactl schedules create --name "Nightly kiosk reboot" --op reboot --label role=kiosk --at 03:00 --tz Asia/Singapore
```

```bash
armadactl schedules create --name "Weekly restart" --op restart-agent --all --at 23:30 --days sat --hold 0
```

```bash
armadactl schedules list
```

```bash
armadactl schedules run|pause|resume|delete <id>
```

---

[← Armada documentation](../README.md#documentation)
