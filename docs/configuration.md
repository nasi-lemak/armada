# Configuration profiles

A profile is configuration the server keeps applied on every matching device (by labels and/or
channel). Change it once and devices pick it up within seconds. Connected devices are woken, and the
rest pick it up at their next heartbeat. Each device reports what it applied, so the **Config** page
shows how many devices have applied each profile, are still pending, or failed.

| Kind | Devices | What it does |
|---|---|---|
| `files` | Linux and other agents | Writes files into a directory the agent's `managed_config` names, then runs that entry's reload command |
| `agent` | Linux and other agents | Overrides `maintenance_window` and/or `auto_update` |
| `android_app` | Android | An app's managed configuration (see [App settings](android.md#app-settings-and-os-updates)) |
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

---

[← Armada documentation](../README.md#documentation)
