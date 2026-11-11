#!/bin/sh
# Custom discovery: print one JSON object per peripheral, either one per line
# or as a JSON array. Use this for vendor gateways, serial dongles, Zigbee or
# LoRa bridges, or BLE stacks other than BlueZ. Lines that aren't JSON are
# ignored, so log output is harmless.
#
# Fields: address (required), model (required unless set in the agent config),
# name, protocol, firmware_version, battery_percent, rssi, details (object).
cat <<'JSON'
{"address": "C0:98:E5:49:00:01", "name": "Freezer 3", "model": "acme-temp-v2", "protocol": "mcumgr", "firmware_version": "1.3.2", "battery_percent": 81, "rssi": -67}
{"address": "24:0A:C4:12:34:56", "name": "Door 1", "model": "acme-door", "protocol": "esp-ota", "firmware_version": "0.9.4"}
JSON
