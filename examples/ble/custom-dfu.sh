#!/bin/sh
# Template for any other stack: ESP32 (esp_ble_ota or your own OTA service),
# TI OAD, Silicon Labs OTA, or an in-house protocol.
#
# Replace the command below with your vendor tool. Exit non-zero on failure;
# its output (last lines) appears in the dashboard.
set -eu
echo "flashing $ARMADA_PERIPHERAL_MODEL $ARMADA_PERIPHERAL_ADDRESS: ${ARMADA_PREVIOUS_VERSION:-?} -> $ARMADA_FIRMWARE_VERSION"
exec your-ota-tool --device "$ARMADA_PERIPHERAL_ADDRESS" --image "$ARMADA_FIRMWARE"
