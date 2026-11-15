# Customer reports

The **Reports** page summarises a client's devices (or the whole fleet) over a period, ready to send
to the customer:

- **Summary:** devices in service, spare and in repair; average availability, and how many devices
  are below the 99% target; alerts by severity and the mean time to resolve them; updates installed
  and failed; replacements; maintenance windows.
- **Per device:** availability, downtime, time in maintenance, alerts, disconnections, updates
  installed and failed, and health now. Devices are sorted least available first.
- **Alerts** that started in the period, with how long each lasted.
- **Updates** by release, **replacements and lifecycle changes**, and **maintenance windows** that
  covered the devices.
- **Software:** per package, how many devices run the newest release for their channel and platform,
  and which don't (as of when the report is made).
- **Disconnections** in the period: how many, network drops versus restarts, the devices that drop
  out most with their last error, and a per-device count. The CSV gains `disconnections`,
  `network_drops` and `restarts` columns.
- **Certificates:** each device's managed certificates, soonest to expire first.

**How the numbers are measured:**
- **Availability** is the share of time a device was checking in, **outside planned maintenance**.
  Each device's history has an `online` sample for every 5-minute period it checked in, measured per
  hour.
- **What counts as maintenance:** windows that hold alerts, one-off or recurring. Their time is left
  out of availability, and downtime during them isn't counted. Updates-only windows (alerts not held)
  don't excuse downtime.
- **Hours with maintenance:** history records how much of each hour a device was up, not exactly
  when. So in an hour with maintenance, down time counts against the maintenance first.
- **Downtime and time in maintenance** are shown per device. Maintenance windows are also listed.
- **Keeping maintenance in reports:** ended maintenance is kept 100 days so reports can use it.
  Deleting a maintenance also removes it from reports, so to keep the record, end it instead.
- **Average availability** is over devices in service. Spares and devices in repair are listed but
  not averaged.
- **Mean time to resolve** leaves out alerts that closed because their device left service.
- **Alerts** in the report are device alerts (`health`, `device_offline`). Rollout alerts cover
  several customers, so they show up as failed updates instead.
- **Retention limits:** history is kept 90 days, alerts 100 days and the activity log 30 days by
  default. If a period goes back further, the report notes what's missing.

**Who sees what:** everyone can report on the devices they can see. A client account gets only its
own devices, whatever labels it asks for. Fleet-wide accounts can report on any client
(`label=customer=acme`) or the whole fleet.

**Formats:** JSON (`GET /api/v1/reports`), **CSV** (one row per device), and a self-contained
**printable HTML** page. To get a PDF, open the page and print it.

```bash
armadactl report --label customer=acme --period last-month
```

```bash
armadactl report --label customer=acme --from 2026-10-01 --to 2026-12-31 --tz Asia/Singapore --format html -o acme-q4.html
```

## Scheduled reports

Have a report emailed **weekly** (Monday to Sunday) or **monthly**, in the client's time zone. It
goes out an hour after the period ends, as an HTML email with the device table attached as CSV. Each
period is sent once; failures retry hourly, show on the Reports page and in the activity log.
**Send now** emails the last complete period straight away.

- Emails go through the server's SMTP relay (`-smtp-host` and so on).
- `-report-brand "Your Company"` puts your name on reports and their subject lines.
- Client operators schedule reports for their own client; admins for any client or the whole fleet.
  At most 20 recipients each.

```bash
armadactl report schedules create --client customer=acme --period monthly --tz Asia/Singapore --to it@acme.example --to ops@acme.example
```

```bash
armadactl report schedules list
```

```bash
armadactl report schedules send <id>
```

---

[← Armada documentation](../README.md#documentation)
