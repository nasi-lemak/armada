#!/bin/sh
# Zephyr / MCUboot firmware update over BLE using the SMP protocol (mcumgr).
#
# MCUboot's test-then-confirm flow gives you automatic rollback. The new image
# boots once as a "test" image, and if it isn't confirmed (for example because it
# crashes before this script can reach it), MCUboot boots the old image again.
#
# Requires: mcumgr (go install github.com/apache/mynewt-mcumgr-cli/mcumgr@latest).
# Starting point: check the flags against your mcumgr version.
set -eu
CONN="ctlr_name=${ARMADA_ADAPTER:-hci0},peer_id=${ARMADA_PERIPHERAL_ADDRESS}"
mcu() { mcumgr --conntype ble --connstring "$CONN" --timeout 30 "$@"; }

echo "uploading $ARMADA_FIRMWARE_VERSION to $ARMADA_PERIPHERAL_ADDRESS"
mcu image upload "$ARMADA_FIRMWARE"

# The uploaded image sits in slot 1; take its hash.
HASH=$(mcu image list | awk '/slot=1/{f=1} f && /hash:/{print $2; exit}')
[ -n "$HASH" ] || { echo "uploaded image not found in slot 1" >&2; exit 1; }

mcu image test "$HASH"
mcu reset || true          # the device drops the connection as it reboots
sleep 20                   # let MCUboot swap and the new image start

# Confirm only if the new image is the one running now.
ACTIVE=$(mcu image list | awk '/slot=0/{f=1} f && /hash:/{print $2; exit}')
if [ "$ACTIVE" != "$HASH" ]; then
  echo "device is not running the new image (MCUboot reverted it)" >&2
  exit 1
fi
mcu image confirm
echo "confirmed $ARMADA_FIRMWARE_VERSION"
