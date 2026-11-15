# Cloud, on-prem and offline sites

A device only ever makes outbound HTTPS requests to the Armada server's URL. Updates, certificates,
actions and logs all go through that one connection. Nothing connects to the device, and it needs no
other internet host. So the server can run wherever your devices can reach it:

- **Cloud:** the server has a public address and devices use their own internet connection.
- **On-prem:** run the server inside the customer's network, and devices reach it over the LAN.
  Everything works without internet access:
  - releases are uploaded to that server (dashboard or `armadactl` over the LAN);
  - the server serves agent installs and updates itself.

  A few optional extras need a reachable service. Point them at internal ones:

  | Feature | On a network without internet |
  |---|---|
  | Alert and report email | an internal SMTP relay (`-smtp-host`) |
  | Slack, webhooks, event webhooks | internal endpoints |
  | Single sign-on | an internal identity provider (`-oidc-issuer`) |
  | The server's TLS certificate | your internal CA; give agents its certificate with `ca_cert` |
  | Android key-attestation revocation list | copy Google's list in by hand: `-attestation-status-file` (re-read when it changes) |
- **A site without internet, managed from the cloud:** devices that can't reach the internet
  directly can reach a cloud server through a proxy at the site.
  - **Linux, macOS and Windows agents:** set `HTTPS_PROXY` for the agent's service. `NO_PROXY` is
    honoured too.
  - **Android:** set a global HTTP proxy, as device owner or through your MDM.
- **A fully air-gapped site** (nothing leaves the network): run a separate Armada server there, and
  watch it from your central one with snapshot files (below).

## One view of several servers (Sites)

When customers have their own on-prem Armada servers, or you run one per region, a **central** Armada
server can watch them all.

**Setting it up:**
1. On the central server, go to **Sites → Add a site**, or run `armadactl sites add "Orchard store"`.
   You get a site token (shown once).
2. Start the site's server with `-central-url https://central.example.com` and
   `ARMADA_CENTRAL_TOKEN=<token>`.

**What the site sends:**
- **The snapshot:** every 5 minutes (`-central-every`) it pushes its devices online, health issues,
  firing alerts, software behind, certificates due, disconnections, customers and projects, backup
  status and its own dashboard's address.
- **How it connects:** outbound HTTPS only, so it works behind NAT and through `HTTPS_PROXY`. If the
  central server can't be reached, the site just tries again later.
- **Checking it:** `armadactl site status` on the site.

**The Sites page** on the central server shows every site side by side:
- the totals across them;
- each site's numbers and details;
- a link to open its own dashboard;
- **reporting** or **not reporting** (no report for three intervals).

**Alerts:** a `site_silent` alert rule (default 30 minutes) notifies you when a site stops reporting.
Its server may be down, or cut off.

**Security:**
- the token only lets a server send reports, and nothing else;
- each report (and each exported snapshot) is signed with the site server's own private key;
- the central server pins that key on the first report, so a copied token alone can't impersonate
  the site;
- after rebuilding a site server, make a **New token**;
- `-central-url` must be https, because the token travels with every report.

**Air-gapped sites:**
1. On the site, run `armadactl site export -o snapshot.json`.
2. Carry the file over, and run `armadactl sites import <id> snapshot.json`, or use **Import
   snapshot** on the Sites page. Imported sites show as *imported by hand* and never alert as silent.

**What a site sends is a summary.** Devices are still managed on each site's own server.

**Devices keep working while offline:**
- the agent keeps running its health checks;
- it queues update results;
- it retries with backoff, up to every 5 minutes;
- interrupted downloads resume.

When the connection is back, it reports everything, including how long it was cut off.

---

[← Armada documentation](../README.md#documentation)
