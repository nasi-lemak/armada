# Device inventory

- **Serial number:** each device reports its hardware serial number:
  - Linux: DMI or device tree (so Raspberry Pis too);
  - macOS;
  - Windows: BIOS;
  - Android: the agent grants itself the permission as device owner.
- **What you record:** on a device's page (or with `armadactl devices set`), its **asset tag**,
  **location**, **on-site contact** and **notes**. Operators on a device can edit them, including a
  client's own operators.
- **Where it shows:** the serial and asset tag appear under each device's name in the list, and in
  customer reports and their CSV.
- **Finding devices:** the device search matches them, along with name, IP and labels.
- **Device list:** **Export CSV** saves the devices shown, with their inventory, as a spreadsheet
  (`armadactl devices list --csv`). The list's filters are kept in the address
  (`#/devices?q=acme&status=offline`), so a view can be bookmarked or sent to a colleague.
- **Quick search:** press **Ctrl+K** (⌘K on a Mac) or **/** anywhere in the dashboard, or click the
  magnifier.
  - It finds devices by name, serial, asset tag, IP, hostname, location, label or ID.
  - It also finds releases, rollouts, users and dashboard pages.
  - It only shows what your account can see: client accounts get their own devices, and users show
    for admins only.
  - The API is `GET /api/v1/search?q=…`.
- **QR asset labels:** print stickers for devices. Use **Print label** on a device's page, or choose
  devices on the Devices page and pick **Print QR labels**.
  - **What a label shows:** a QR code that opens the device's page on a phone (after signing in),
    plus the name, asset tag, serial number and location.
  - **Sheet printers:** labels are laid out on A4 or Letter at 63.5 × 38.1 mm.
  - **Label printers:** one label per page at 62 × 29, 50 × 25 or 100 × 50 mm. Print at actual size.
  - **The link** uses the server's public URL (`-public-url`).
- **Timeline:** each device page has one feed of everything that happened to that device, newest
  first and grouped by day.
  - **What it covers:** actions, lifecycle changes, certificates, health changes and config.
  - **Alerts:** shown as spans, with how long they lasted and who acknowledged them.
  - **Updates:** shown with from and to versions and the error, if any.
  - **Maintenance:** the windows that cover the device.
  - **Using it:** filter it by kind and use **Load more** to go back further.
  - From the CLI: `armadactl devices timeline <id> [--kinds alert,update]`.

```bash
armadactl devices set <id> --asset-tag ACME-00417 --location "Orchard store, till 3" --contact "Mei, +65 5550 1234"
```

## Pre-registration and bulk import

Name and label devices from a spreadsheet, before or after they enroll. Use **Enrollment → Pre-register
and import devices**, or `armadactl import devices.csv`.

```csv
serial,name,channel,labels,asset_tag,location,contact,notes
SN-0001,orchard-till-1,stable,site=sg; role=till,ACME-00417,"Orchard store, till 1","Mei, +65 5550 1234",
```

- **Rows for enrolled devices** update them now. A row finds its device by `serial`, or by a
  `device` column holding its ID or name.
- **Other serials are pre-registered:**
  - when a device reporting that serial enrolls, it takes the name, channel, labels and inventory;
  - each registration is used once;
  - serials match regardless of case and surrounding spaces.
- **Empty cells change nothing.** Labels go in a `labels` column (`site=sg; role=till`) or in columns
  named `label:site`.
- **Preview first:** a dry run (`--dry-run`) shows what each row would do. If any row has an error,
  nothing is changed.
- **Vendor pre-registrations (made by fleet-wide accounts):** they only apply to devices enrolled with
  fleet-wide accounts' tokens. Any device can report any serial number, so a client's device can't
  claim one.
- **The enrollment token still decides the client:**
  - its labels win over a registration's, so a registration can't move a device to another customer;
  - a registration's channel replaces the token's.
- **Client accounts:**
  - their operators can import for their own devices;
  - their registrations only apply to devices that enroll into that client;
  - another client's devices are invisible to their imports.
- **Audit:** the activity log records each device an import changed (`device.imported`) and each
  import (`registration.import`). A device's enrollment entry notes when it used a pre-registration.

```bash
armadactl import devices.csv --dry-run
armadactl import devices.csv
armadactl registrations list
armadactl registrations delete SN-0001
```

---

[← Armada documentation](../README.md#documentation)
