# BLE firmware drivers

The Armada agent flashes peripherals by running a **driver**: any command that
takes a verified firmware image and puts it on one device. These scripts are
starting points for common stacks. Each one depends on a vendor tool, and flags
change between versions, so test the script by hand on one device first.

A driver receives these environment variables:

| Variable | Example |
|---|---|
| `ARMADA_PERIPHERAL_ADDRESS` | `C0:98:E5:49:00:01` |
| `ARMADA_PERIPHERAL_MODEL` | `acme-sensor-v2` |
| `ARMADA_PROTOCOL` | `mcumgr` |
| `ARMADA_FIRMWARE` | path to the image, already checked against the server's signature and SHA-256 |
| `ARMADA_FIRMWARE_VERSION` | `1.4.0` |
| `ARMADA_PREVIOUS_VERSION` | `1.3.2` (empty if unknown) |
| `ARMADA_ADAPTER` | `hci0` |

Exit 0 when the device has accepted the image. The agent then rediscovers the
device, and if discovery reports a firmware version, checks that it's the new
one. If the version doesn't match, the update is marked failed (for example,
when MCUboot reverted a test image).

| Script | Stack | Tool |
|---|---|---|
| `mcumgr-dfu.sh` | Zephyr / MCUboot (SMP over BLE) | [`mcumgr`](https://github.com/apache/mynewt-mcumgr-cli) |
| `nordic-dfu.sh` | Nordic nRF5 SDK Secure DFU | `nrfutil` 6.x with a Nordic dongle |
| `custom-dfu.sh` | Anything else (ESP32, TI, Silicon Labs, in-house) | your vendor's tool |
| `discover-example.sh` | Custom discovery (vendor gateways, serial dongles…) | — |
