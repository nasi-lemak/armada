# Device lifecycle

Every device has a lifecycle state, shown as a badge in the device list and on its page:

| State | Meaning | Updates | Alerts | Can check in |
|---|---|---|---|---|
| `active` | In service (new devices start here, unless their token enrolls spares) | yes | yes | yes |
| `spare` | In stock, ready to replace a device | yes, so it's current when needed | no | yes |
| `repair` | Out of service: RMA, at the bench | no | no | yes |
| `retired` | Decommissioned for good | no | no | **no** |

**Enrolling stock as spares.** Create the enrollment token with **Enroll as spares** on the
Enrollment page (or `armadactl tokens create --spare`). Devices that enroll with it start as spares.
They get updates so they're current when needed, but they don't alert or count as offline while
they sit in a box. Use such a token for the warehouse batch, and a normal one for devices going
straight to a customer. Spare tokens need a fleet-wide operator, like other lifecycle changes. The
device's lifecycle note records which token enrolled it.

```bash
armadactl tokens create --spare --description "warehouse batch 7" --max-uses 50
```

When a device leaves service (spare, repair, retired or removed), its open alerts close without a
"resolved" notification. The device didn't recover, it just stopped being watched. Spares and devices
in repair don't count as offline in the fleet overview.

**Replacing a device (RMA).** On the broken device's page, choose **Replace…** and pick a spare (or
any active device). In one step:

- The replacement takes over the old device's **name, channel and labels**. With them come its
  client, configuration profiles, alert rules, rollouts and maintenance by labels. It checks in
  straight away to pick up its configuration.
- The old device is renamed "… (replaced)" and **loses its labels**, so it leaves the client's
  account: the customer can't see or act on a unit sitting at your repair bench. By default it goes
  to `repair`. You can choose `spare` or `retired` instead.
- Both devices link to each other, and the activity log records the swap and its note
  (for example the RMA number).
- Each device keeps its own key and its own history.

**Retiring a device** is final:
- **Key destroyed:** it can never check in again, and its live connection drops.
- **Licenses revoked:** its app licenses are revoked and its hardware key is forgotten.
- **Records kept:** its record, events and history stay. It leaves the device list, counts, rollouts
  and alerts. Show retired devices with the **Retired** filter or `?lifecycle=retired`.
- **Using the hardware again:** enroll it again. It joins as a new device.
- **Android:** wipe the device first (**Wipe device**), then retire it.

Removing a device instead (`DELETE`) also deletes its history.

```bash
armadactl devices lifecycle <id> spare
```

```bash
armadactl devices lifecycle <id> repair --note "RMA 4411: cracked screen"
```

```bash
armadactl devices replace <broken-id> --with <spare-id> --note "RMA 4411"
```

```bash
armadactl devices retire <id> --yes --note "end of life"
```

```bash
armadactl devices list --lifecycle spare
```

Lifecycle changes need a fleet-wide operator, because they move devices between clients and revoke
credentials. Client accounts see the state of their devices.

---

[← Armada documentation](../README.md#documentation)
