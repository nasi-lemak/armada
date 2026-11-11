#!/bin/sh
# Nordic nRF5 SDK Secure DFU over BLE.
#
# ARMADA_FIRMWARE must be a DFU package (.zip) made with
# `nrfutil pkg generate` and signed with your DFU key. Upload that zip as the
# release. The device's bootloader checks the signature itself.
#
# Requires: nrfutil 6.x (pip install nrfutil) and a Nordic nRF52 dongle running
# the connectivity firmware, on NRF_DONGLE_PORT. Starting point: check the
# flags against your nrfutil version. Newer nrfutil releases ship BLE DFU as a
# separate command.
set -eu
PORT="${NRF_DONGLE_PORT:-/dev/ttyACM0}"
nrfutil dfu ble -ic NRF -pkg "$ARMADA_FIRMWARE" -p "$PORT" -a "$ARMADA_PERIPHERAL_ADDRESS" -f
