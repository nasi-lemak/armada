# Certificates on devices

Applications on your devices often have their own TLS certificates: a Mosquitto broker, a local web
server, a client certificate for a customer's API. When those are issued by the customer's CA, every
renewal means a CSR to them and a certificate back. Armada tracks these certificates and runs that
exchange:

1. **List the certificate in the agent's config** (`managed_certs`). The server can only touch
   certificates listed there, at those paths:

   ```json
   "managed_certs": {
     "mosquitto": {
       "cert": "/etc/mosquitto/certs/server.crt",
       "key": "/etc/mosquitto/certs/server.key",
       "full_chain": true,
       "owner": "mosquitto:mosquitto",
       "reload_command": ["systemctl", "restart", "mosquitto"],
       "verify_address": "127.0.0.1:8883"
     }
   }
   ```

   The agent reports each certificate at every check-in: names, issuer, serial, key type, expiry,
   and whether the key file matches. The **Certificates** page lists them all, soonest to expire
   first, and each device's page shows its own.

2. **Request CSRs.** Tick the certificates and choose **Request CSRs** (or
   `armadactl certs renew --label customer=acme`).
   - **On the device:** each device makes a **new private key**, keeps it in the agent's state
     directory, and sends back only the CSR.
   - **What the CSR asks for:** by default the same subject, names, IP addresses and key type as the
     installed certificate. You can override them, using templates such as `{{.Name}}.acme.example`,
     `{{.Hostname}}`, `{{.IP}}` or `{{index .Labels "site"}}`. Useful for a first certificate, or for
     moving to RSA 2048 when a CA requires it.

3. **Get them signed.** Any of these:
   - **Download CSRs:** a zip with one `.csr` per device and a `manifest.csv`
     (`armadactl certs csrs`).
   - **Email CSRs…:** sent to whoever runs the client's CA, through the server's SMTP relay, with a
     note such as a ticket number.
   - **Client account:** the client's own users sign in, see their devices' CSRs on the Certificates
     page, and download or upload there themselves.

4. **Upload the signed certificates.** Paste them, choose the files, or use
   `armadactl certs upload signed/*.crt`. Any number, in any order, with or without their chains.
   - **Matching:** each certificate is matched to the device whose CSR it answers by its public key,
     so file names don't matter.
   - **Checks before anything is installed:**
     - the certificate is for that CSR's key;
     - it isn't expired;
     - it is for server authentication;
     - every name and IP the CSR asked for is in it;
     - if you included the issuing CA certificates, the chain verifies.
     Anything that fails is listed with the reason and not installed.

5. **Install, with rollback.**
   - **Writing:** the device writes the certificate (with its intermediates if `full_chain`) and the
     new key, keeping the existing file permissions or the `owner`.
   - **Reload and verify:** it runs `reload_command`, and checks that `verify_address` now presents
     the new certificate.
   - **Rollback:** if either step fails, it **puts the previous certificate and key back**, reloads
     again, and the renewal shows as failed with the reason.

## Automatic renewal

A **renewal policy** runs the steps above on its own, up to the signature. Set one up under
**Automatic renewal** on the Certificates page, or with
`armadactl certs policies create --client customer=acme --to pki@acme.example`.

- **Starting:** `renew_days` (default 30) before a certificate expires, the policy starts its
  renewal: the device makes a new key and CSR, with the same names as now (and optionally a new key
  type).
- **Sending:** once CSRs are ready, they're emailed to the policy's recipients, in one email per
  batch with a zip and the policy's note (a contract or ticket number). Without recipients, they wait
  on the Certificates page for the client or your team.
- **Reminding:** while certificates haven't come back, a reminder goes out every `reminder_days`
  (default 7; 0 for none), with the soonest expiry in the subject.
- **Installing:** returned certificates install like any other: matched, checked, installed, verified,
  rolled back on failure.
- **Retrying:** a renewal that failed (for example the device couldn't make the CSR) or that someone
  cancelled is started again after a week. To stop renewing a certificate, narrow or disable the
  policy.
- **Scope:** it covers devices in service. Spares, devices in repair and retired devices are left
  alone. Narrow it with device labels and certificate names (`targets`).
- **Who manages policies:** client operators set up policies for their own client, and admins for
  any client or all of them. Renewals a policy starts are recorded as made by that policy.
- **How often:** policies check every 15 minutes. **Check now** runs one immediately.

```bash
armadactl certs policies create --client customer=acme --days 45 --to pki@acme.example --note "Contract 77"
```

```bash
armadactl certs policies list
```

```bash
armadactl certs policies run <id>
```

**Who can do what:**
- **Seeing certificates:** everyone sees the certificates of the devices they can see.
- **Renewing and installing:** operators on a device (including a client's own operators) can renew
  and upload. Uploading only matches renewals on devices you operate, so one client can't install
  onto another's.

**Alerts and reports:**
- **Alerts:** add an alert rule with the condition **Certificate expiring** (`cert_expiry`, default
  30 days, critical within 7). Client alert rules can use it too.
- **Customer reports** list each device's certificates and when they expire.

**Audit and limits:**
- **Activity log:** requests, CSRs, uploads (with serial and expiry), installs and failures.
- **Cancelling:** a waiting renewal can be cancelled. Its unused key stays on the device until the
  next CSR for that certificate replaces it.
- **Platforms:** Linux and other agents. On Android, apps manage their own certificates.

| `managed_certs` key | Meaning |
|---|---|
| `cert`, `key` | Certificate and private key files (PEM; keys are written as PKCS#8) |
| `chain` | A separate file for intermediates, for services that want one |
| `full_chain` | Write the intermediates after the certificate in `cert` (Mosquitto `certfile`, nginx) |
| `owner` | `user[:group]` for the new files, for services that drop root |
| `reload_command` | Run after writing; on failure the old files are restored |
| `verify_address` | `host:port` that must present the new certificate after reloading |

```bash
armadactl certs list --expiring 30
```

```bash
armadactl certs renew --label customer=acme --target mosquitto
```

```bash
armadactl certs email --label customer=acme --to pki@acme.example --note "Ticket 4411"
```

```bash
armadactl certs upload signed/*.crt
```

---

[← Armada documentation](../README.md#documentation)
