# Whole-OS image updates (A/B)

Embedded Linux devices usually update the whole root filesystem rather than
single packages. An A/B updater writes the new image to the inactive slot, and
the device boots into it on the next restart. If the new image fails to boot,
the bootloader's boot counter switches back to the old slot.

Armada drives these updaters through a package with `reboot_required`:

1. `install_command` writes the image to the inactive slot.
2. The agent records the pending update and reboots.
3. On the next start the agent checks `version_command`. If the old version
   booted (the bootloader fell back), the update is reported **rolled back**.
4. It runs `health_command`, then `confirm_command` to mark the slot good. If
   the health check fails, it runs `rollback_command` and reports **rolled back**.

Upload images as releases of a package (for example `os`) whose version is the
image's `VERSION_ID`, and deploy them like any other release. Staged rollouts
and auto-pause apply, which matters more for OS images than for anything else.

These are templates. Flags vary between versions of each tool, so check them on
one device first.

## RAUC

```json
"packages": {
  "os": {
    "install_command":  ["/bin/sh", "-c", "rauc install \"$ARMADA_ARTIFACT\""],
    "reboot_required":  true,
    "version_command":  ["/bin/sh", "-c", ". /etc/os-release && echo $VERSION_ID"],
    "health_command":   ["/usr/local/lib/armada/os-health.sh"],
    "health_retries":   10,
    "health_interval":  "15s",
    "confirm_command":  ["rauc", "status", "mark-good"],
    "rollback_command": ["/bin/sh", "-c", "rauc status mark-bad && systemctl reboot"]
  }
}
```

Commands run without a shell, so the ones that need `$ARMADA_ARTIFACT` are
wrapped in `/bin/sh -c`. Upload the signed `.raucb` bundle; RAUC checks its signature against the
keyring on the device.

## SWUpdate

```json
"os": {
  "install_command":  ["/bin/sh", "-c", "swupdate -v -k /etc/swupdate/public.pem -e stable,${ROOTFS_TARGET:-copy2} -i \"$ARMADA_ARTIFACT\""],
  "reboot_required":  true,
  "version_command":  ["/bin/sh", "-c", ". /etc/os-release && echo $VERSION_ID"],
  "health_command":   ["/usr/local/lib/armada/os-health.sh"],
  "confirm_command":  ["fw_setenv", "upgrade_available", "0"],
  "rollback_command": ["systemctl", "reboot"]
}
```

The confirm step depends on how your bootloader counts boots. With U-Boot's
`bootcount`/`upgrade_available`, clearing `upgrade_available` makes the new
slot permanent, and rebooting without it lets U-Boot fall back.

## Mender (standalone mode)

```json
"os": {
  "install_command":  ["/bin/sh", "-c", "mender-update install \"$ARMADA_ARTIFACT\""],
  "reboot_required":  true,
  "version_command":  ["/bin/sh", "-c", ". /etc/os-release && echo $VERSION_ID"],
  "health_command":   ["/usr/local/lib/armada/os-health.sh"],
  "confirm_command":  ["mender-update", "commit"],
  "rollback_command": ["/bin/sh", "-c", "mender-update rollback && systemctl reboot"]
}
```

Older Mender clients use `mender install`, `mender commit` and `mender rollback`.

## A health script

Check what has to work before you keep an image: your service is up, the
network is reachable, the data partition is mounted. For example:

```sh
#!/bin/sh
set -e
systemctl is-active --quiet myapp
mountpoint -q /data
curl -fsS --max-time 5 http://127.0.0.1:8080/healthz >/dev/null
```
