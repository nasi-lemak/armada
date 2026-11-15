# Android devices

This is for Android devices you **provide to customers**. You control your app's updates instead of Google Play,
and your app can't simply be copied to other phones. Customers can still install other apps.

## How it works

1. **Provisioning.** Before shipping, you factory-reset each device. On the welcome screen you tap six times
   and scan a QR code from the dashboard. Android downloads the Armada agent, checks it against the
   signing-certificate checksum in the QR code, and installs it as **Device Owner**.
2. **Hardware-bound identity.** The agent creates a key inside the device's secure hardware (TEE or
   StrongBox). This key cannot be exported. Android's **key attestation** gives the server a certificate
   chain, signed by Google, proving three things:
   - the key lives in real secure hardware;
   - the bootloader is locked and the OS is unmodified;
   - the key belongs to your agent app, signed with your certificate.
   The server checks the chain against Google's attestation roots and Google's revocation list.
3. **Silent updates.** Rollouts work as on Linux (stages, auto-pause on failures, channels and labels).
   Before installing, the agent checks three things:
   - the server's signature on the release;
   - the file's SHA-256;
   - the APK itself: package name, versionCode, and that it's signed with a certificate you registered.
   It then installs with no user prompt and blocks the app from being uninstalled. USB debugging is
   turned off by default.
4. **Device-bound licenses.** Your app asks the agent for a license through the client library. The agent
   signs the request with the hardware key. The server issues a short-lived license (a standard EdDSA JWT)
   only if all of these hold:
   - the signature comes from the attested key;
   - the calling app is your registered package, signed with your certificate;
   - the device's licenses haven't been revoked.
   The license is also bound to a key that never leaves your app's Keystore (DPoP, RFC 9449). Your
   backend rejects requests without a valid license and proof.

On a phone you didn't provision there's no enrolled agent, so there's no license, and your backend
refuses the app. A re-signed or modified copy of your app fails the signing-certificate check. A license
token copied off a device fails the DPoP check.

## Setup

1. **Build and sign the agent.** Optionally change `applicationId` in `android/agent/build.gradle.kts` to
   your own namespace, and start the server with `-android-agent-package` set to the same value. Build
   `android/agent` with your release signing key (`./gradlew :agent:assembleRelease` with a `signingConfig`),
   or run `make android` for a debug build. Keep that key safe: Android only accepts updates signed with it.
2. **Register the agent and your app** with their signing-certificate SHA-256. You can read it with
   `apksigner verify --print-certs app.apk` or `keytool -list -v -keystore release.jks`.

   ```bash
   armadactl apps set dev.armada.agent --name "Armada agent" --signer AB:CD:...
   ```

   ```bash
   armadactl apps set com.yourco.app --name "Your app" --signer 12:34:... --license --license-ttl 1h
   ```

3. **Upload the agent APK.** Devices download it during provisioning, from a public URL that serves only
   the agent.

   ```bash
   armadactl releases upload --package dev.armada.agent --version 1.0.0 --version-code 1 --os android agent.apk
   ```

4. **Create a setup QR code.** Set `-public-url https://fleet.yourco.com` on the server so devices can
   reach it. Then create a token and the QR code:

   ```bash
   armadactl tokens create --label customer=acme --max-uses 100 --ttl 168h
   ```

   ```bash
   armadactl android qr --token et_... --wifi-ssid factory-wifi --out acme-setup.png
   ```

   Or use **Enrollment → Show Android setup QR** in the dashboard. Labels on the token, such as
   `customer=acme`, end up in every license, so your backend knows which customer a device belongs to.
5. **Ship your app.** Upload each APK with its `versionName` as the version and its `versionCode`, then
   deploy it like any other release:

   ```bash
   armadactl releases upload --package com.yourco.app --version 2.1.0 --version-code 210 --os android --auto-deploy app.apk
   ```

## In your app

Add the `android/client` module (or its AAR) and attach the headers to every backend request, off the
main thread:

```kotlin
val armada = ArmadaLicense(context)
val headers = armada.authHeaders("POST", "https://api.yourco.com/v1/orders")
headers.forEach { (k, v) -> connection.setRequestProperty(k, v) }
// throws LicenseException on devices you didn't provision, or when the license is revoked
```

## In your backend

Go, using `github.com/nasi-lemak/armada/pkg/license`:

```go
v, err := license.NewVerifierFromJWKSURL(ctx, "https://fleet.yourco.com/api/v1/license/jwks", "com.yourco.app")
http.Handle("/v1/", v.Middleware("https://api.yourco.com", apiHandler))
// inside handlers: claims, _ := license.ClaimsFromContext(r.Context())
// claims.DeviceID(), claims.Device.Labels["customer"]
```

Other languages: a license is a standard JWT (`alg: EdDSA`, `typ: armada-license+jwt`, `iss: armada`,
`aud: <your package>`), and the verification keys are at `/api/v1/license/jwks`. If the license has a
`cnf.jkt` claim, also verify the `DPoP` header as described in RFC 9449. For example, in Node with `jose`:

```js
const JWKS = createRemoteJWKSet(new URL("https://fleet.yourco.com/api/v1/license/jwks"));
const { payload } = await jwtVerify(token, JWKS, { issuer: "armada", audience: "com.yourco.app", typ: "armada-license+jwt" });
```

## Revoking a device

Click **Revoke licenses** on the device page, or run:

```bash
armadactl devices revoke <device-id>
```

The device gets no new licenses, so your app stops working on it within the license lifetime. The
default lifetime is 1 hour and you can set it per app. Reinstating restores it.

## Lost or stolen devices

**Lock screen** locks the device now. **Wipe device** factory-resets it: everything is erased and the
device leaves the fleet until it is set up again with the QR code. The dashboard asks you to type the
device name; the API needs `["confirm=<device ID>"]`, and the device checks that ID again before
wiping.

```bash
armadactl devices action <id> lock
armadactl devices action <id> wipe confirm=<id>
```

Revoking licenses as well stops your app working on it even if the wipe never arrives.

## App settings and OS updates

Use [configuration profiles](configuration.md):

- **Android app settings** (`android_app`) set an app's *managed configuration*: the values your app
  reads with `RestrictionsManager.getApplicationRestrictions()`. Use them for the backend URL, feature
  flags or kiosk options per customer or site, without rebuilding the APK.
- **Android OS updates** (`android_system_update`) set the system update policy: install automatically,
  only during a daily window (for example `02:00-04:00` device time), or postpone for up to 30 days.

## What this does and doesn't protect against

- **Protects against:** installing a copy of your APK on any other phone; running a modified or
  re-signed APK; cloning a device's data onto another device (the key can't be exported); unlocked or
  rooted devices enrolling (verified boot is required); and a stolen license being replayed from
  elsewhere (DPoP).
- **Doesn't protect against:** someone who patches your app to skip the license check *and* re-creates
  your backend's functionality. That's why the check has to be enforced by your backend. If your app
  works fully offline, copy protection can only make copying hard, not impossible.
- **Rollback:** Android doesn't let apps install an older versionCode. To undo a bad release, ship a
  fixed build with a higher versionCode. The rollout's automatic pause stops a bad release at the
  canary devices first.
- **Testing status:** the Android code compiles and passes Android Lint and its unit tests. The
  server-side attestation verifier is tested against real device attestations from Google's sample
  repo. The full flow has **not yet been run on a physical device**, so pilot it on one device before
  provisioning a batch.

## Server flags for Android

| Flag | Default | Meaning |
|---|---|---|
| `-android-agent-package` | `dev.armada.agent` | Agent application ID (attested keys must come from it) |
| `-public-url` | from request | URL devices use to reach the server; put it behind HTTPS |
| `-android-require-strongbox` | off | Require keys in a dedicated security chip, not just the TEE |
| `-android-allow-unverified-boot` | off | Accept unlocked bootloaders (development devices only) |
| `-android-allow-unattested` | off | Accept enrollments without attestation (such devices can't get licenses) |
| `-android-allow-adb` | off | Don't disable USB debugging |
| `-attestation-status-url` | Google's list | Revocation list for attestation keys; set it to empty to disable |
| `-attestation-status-file` | — | Servers without internet: a copy of that list, re-read when it changes |

---

[← Armada documentation](../README.md#documentation)
