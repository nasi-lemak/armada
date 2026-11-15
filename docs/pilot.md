# Pilot checklist

A pilot proves Armada on real devices before a wider rollout. Plan on two to four weeks, with 3–10
devices. If you have both kinds of customer, use one cloud site and one on-prem site.

Most of Armada is covered by automated tests, and much of it has run against a real agent. These
parts have only been exercised in simulation, so the pilot should prove them first:

| What | Why it needs real hardware |
|---|---|
| Disconnection tracking (network vs restart) | It needs real cable pulls, Wi-Fi drops and power cuts |
| Health data during outages | It needs an agent cut off for a while |
| Network check on Android | It has been built, but not run on a phone or kiosk |
| Screenshots | grim, ImageMagick, scrot, macOS and Windows capture need a real display |
| Backups to real S3 or MinIO | The signing matches AWS's published examples, but nothing has been uploaded to a real bucket |
| Sites (central + on-prem server) | It needs two real networks and their firewalls and proxies |
| Updates and rollback on your own images | It needs your packages and hooks |

## 1. Set up

**Server:**
1. Install the server where devices can reach it, with a real TLS certificate (see
   [operations.md](operations.md)).
2. Turn on automatic backups, to another disk and to S3 or MinIO:
   ```bash
   armada-server -data-dir /var/lib/armada -public-url https://fleet.example.com \
     -backup-dir /mnt/backups/armada -backup-s3-url https://s3.example.com/backups/armada/
   ```
3. Add a user for each person in the pilot, then turn off the admin token (`-disable-admin-token`).
4. Create alert rules:
   - `device_offline`;
   - `health` (critical);
   - `flaky_connectivity` (3 drops in 24 h, while piloting);
   - `site_outage` (group by `site`);
   - `cert_expiry`;
   - with a central server, `site_silent`.

   Send them to a channel people actually read.

**Devices:**
- **Labels:** make an enrollment token per customer and site, e.g.
  `--label customer=acme --label site=orchard --label project=tills`.
- **Pre-register:** import the pilot devices' serial numbers with names and asset tags, so they
  come up named. Print their QR labels.
- **Agent config:** give each device the agent with the `allow_*` settings you want. For
  screenshots, set `allow_screenshots: true` and start the server with `-screenshots`.

## 2. Scenarios to run

Tick each one off, and note anything surprising.

**Connectivity:**
- [ ] Unplug a device's network for 5 minutes. Expect:
  - it shows offline;
  - when plugged back in, its timeline has a *network* disconnection with the agent's error;
  - its health charts have no gap.
- [ ] Power a device off for 5 minutes. Expect a *restart* disconnection.
- [ ] Block the server at a site's firewall for 10 minutes. Expect:
  - the `site_outage` alert fires once, for that site;
  - it resolves when the firewall opens.
- [ ] Run **Network check** on a healthy device, then on one whose DNS you've broken. The second
  report should stop at DNS and say so.
- [ ] On a site behind a proxy, set `HTTPS_PROXY` for the agent. Expect check-ins to work, and the
  network check to report the proxy.

**Updates:**
- [ ] Upload a new version of your app. In *Needs attention*, **Update these** starts a rollout.
  Watch it go through its stages.
- [ ] Upload a deliberately broken build. Expect:
  - the rollout pauses at its failure threshold;
  - devices roll back;
  - `update_failed` and `deployment_paused` alerts fire.
- [ ] Update the agent itself on one device.

**Certificates:**
- [ ] On a device with a managed certificate, request a renewal. Send the CSR to whoever signs it,
  upload the signed certificate, and check the service picked it up.

**Day-to-day:**
- [ ] Schedule a nightly reboot for one device. Expect:
  - it runs;
  - alerts are held while it runs;
  - the run shows on the Bulk page.
- [ ] Take a screenshot of a kiosk, if you enabled screenshots.
- [ ] Send a customer report for the pilot period. Read it as the customer would.
- [ ] Sign in as a client account. Check it only ever sees its own devices, in every page and in
  quick search.

**Recovery:**
- [ ] Restore last night's backup onto a spare machine (`armada-server -restore`). Check it starts,
  and that its signing-key fingerprint matches.
- [ ] Check `armadactl backup status` shows recent backups in both places.

**With an on-prem site:**
- [ ] Register it on the central server's Sites page, and start its server with the site token.
  It should show as *reporting*.
- [ ] Stop the on-prem server for 40 minutes. Expect `site_silent` to fire.
- [ ] For an air-gapped site, export a snapshot (`armadactl site export`) and import it centrally.

## 3. Before going wider

- **Look over what the pilot recorded:**
  - the activity log, the disconnections per device, and any alert that fired without a real
    cause, which is a rule to tune;
  - whether health checks need thresholds for your hardware.
- **Measure your server:** run the load test on your server's hardware with your expected device
  count (see [operations.md](operations.md#capacity)).
- **Write down the settings that worked:**
  - agent config, labels, alert rules;
  - maintenance windows for updates;
  - who gets which role.

  The next sites start from these.
