# Peripherals: BLE sensors and other devices

Devices that can't run an agent can be managed through a **gateway**: any Linux agent near them, such as
a Raspberry Pi or an industrial gateway. Each peripheral becomes a device in the fleet, linked to its
gateway. It has its own firmware version, battery, signal strength, health, labels, update history and
alerts. Staged rollouts, auto-pause and offline alerts work for peripherals just like for other devices.

**Discovery** (`peripherals.discovery` in the agent config):
- `bluez` scans with the Linux Bluetooth stack. It matches devices by name prefix, service UUID,
  manufacturer ID or address. It reads battery level from the Battery Service and, optionally, the
  firmware version from the standard Device Information Service.
- `command` runs any program that prints JSON. Use it for vendor gateways, USB or serial dongles, or
  Zigbee, Thread and LoRa bridges ([example](../examples/ble/discover-example.sh)).

**Firmware updates:**
1. Upload the image as a release with `--os ble` and the hardware **model** as the package name.
   Firmware is only ever offered to peripherals of exactly that model.

   ```bash
   armadactl releases upload --package acme-temp-v2 --version 1.4.0 --os ble firmware.bin
   ```

2. Deploy it like any release.
3. The gateway checks the image's signature and SHA-256, then runs the **driver** for the device's
   protocol.
4. It then rediscovers the device and checks that it reports the new version. If the bootloader rolled
   back, the update is marked failed.

A gateway flashes one device at a time.

**Drivers** are commands, so any stack works. [`examples/ble/`](../examples/ble) has starting points:
- **MCUboot/Zephyr** over SMP (`mcumgr`), using test-then-confirm, so a bad image is reverted by the
  bootloader;
- **Nordic Secure DFU** (`nrfutil`);
- a template for **ESP32, TI, Silicon Labs or in-house** OTA tools.

See [`deploy/gateway.example.json`](../deploy/gateway.example.json) for a complete gateway config.

**Testing status:** the gateway logic, the server side and the discovery-command path are tested end to
end with simulated devices, including a bootloader rollback and a sensor of the wrong model. The BlueZ
scanner and the example driver scripts haven't been run against real Bluetooth hardware yet, so pilot
them with one gateway first.

---

[← Armada documentation](../README.md#documentation)
