# Health monitoring

Every agent reports health checks with its heartbeat, and checks in early when a status changes.
Each device gets an overall health of `ok`, `warning` or `critical`, shown in the dashboard. Every
status change is recorded in the activity log.

## Built-in hardware checks

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
`health` section; see [`deploy/gateway.example.json`](../deploy/gateway.example.json).

## Program checks (monitors)

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

## History and charts

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

---

[← Armada documentation](../README.md#documentation)
