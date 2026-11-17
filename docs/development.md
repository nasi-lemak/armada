# Development

```bash
make vet test         # gofmt, go vet, unit and end-to-end tests
```

```bash
make test-race        # same with the race detector (needs cgo)
```

The end-to-end tests run a real server and agent together. They cover enrollment, a
package update, a failed health check with rollback, the auto-pause, remote actions, a
self-update with the restart simulated, and crash-loop rollback.
`internal/server/server_test.go` simulates a 200-device fleet through a staged rollout.

**CI** (`.github/workflows/ci.yml`) runs on every push to `main` and every pull request, or by hand
(**Run workflow**). Every job runs `scripts/ci.sh`, so you can run exactly the same checks on your
own machine, without spending CI minutes:

```bash
make ci
```

- **Go toolchains:** it runs the Go job twice, like CI. Once with the Go version `go.mod` names, and
  once with the newest Go release. Both are fetched automatically through `GOTOOLCHAIN`.
- **Then:** the vulnerability scan, the upgrade test from older releases, the dashboard smoke test
  and the Android build.
- **Missing tools:** a job whose tools aren't installed (Node.js, a JDK, the Android SDK) is reported
  as skipped. `CI_STRICT=1` makes that a failure.
- **One job only:** `scripts/ci.sh go --tidy`, `vulncheck`, `upgrade`, `dashboard` or `android`.
- **Settings:**
  - `CI_STABLE_GO=go1.x.y` picks the "newest" Go;
  - `ARMADA_PLAYWRIGHT=$(npm root -g)/playwright` uses an installed Playwright and browser instead
    of installing one;
  - `JAVA_HOME` picks the JDK (CI uses 17).

The CI jobs:

| Job | What it checks |
|---|---|
| Go (go.mod and stable) | `gofmt`, `go vet`, `go mod tidy`, all tests with the race detector, agent cross-compiles for every platform, server and CLI builds for macOS, Windows and FreeBSD |
| Vulnerability scan | `govulncheck` with the latest Go release |
| Dashboard | JavaScript syntax, then a browser smoke test (`web/e2e/smoke.cjs`): signs in, opens every page as an admin, and checks a client account sees only its customer's devices and no fleet pages. It fails on any JavaScript error. |
| Android | agent APK and client library build, unit tests, Android lint |

Run the dashboard smoke test against a local server with:

```bash
cd web/e2e && npm ci && npx playwright install chromium
node smoke.cjs http://localhost:8080 "$(cat ../../data/admin-token)"
```

## Releasing

```bash
make release VERSION=1.4.0
```

`tools/release` builds everything into `dist/1.4.0/` with the Go toolchain alone:

- `armada-agent` for Linux (amd64, arm64, armv7, 386, riscv64), macOS (Intel, Apple silicon) and
  Windows (amd64, arm64); `armada-server` and `armadactl` for Linux, macOS and Windows;
- `.deb` packages: `armada-agent` for each Linux architecture and `armada-server` (with `armadactl`)
  for amd64 and arm64. They install systemd services; the agent package writes its config from
  `ARMADA_SERVER_URL` and `ARMADA_ENROLLMENT_TOKEN` if they are set, and the server reads its
  settings from `/etc/default/armada-server` (kept across upgrades). Purging the agent removes its
  config and state; purging the server keeps its database;
- `dependencies.txt`, the Go modules in each program, and `SHA256SUMS`.

Pre-releases (`1.4.0-rc1`) become Debian versions that sort before the release (`1.4.0~rc1`).
Package timestamps come from the last commit, so the same commit builds the same packages.

Then upload the agents to your server with `armadactl releases upload-agents dist/1.4.0`.

Pushing a tag such as `v1.4.0` runs `.github/workflows/release.yml`: the Go checks, then this build,
published as a GitHub release with all the files (marked a pre-release when the version has a `-`).
It runs only for tags.

There are no `.rpm` packages yet.

Build releases with the **current Go release** rather than the minimum in `go.mod`. Many security
fixes land in the standard library, and the vulnerability scan uses the latest Go for that reason.

**Changing the database schema.** Schema changes are numbered migrations in
`internal/store/migrations.go`. Each is applied once, in order, inside a transaction, and recorded in
the `schema_migrations` table.
- Add a new migration at the end. Never edit a released one; `TestMigrationsAreFrozen` fails if you
  do. Then add its fingerprint (printed by the test) to the list in that test.
- `TestLegacyDatabaseUpgrades` upgrades a real database written by the first release
  (`internal/store/testdata`) and checks it ends up with exactly the same tables, columns and indexes as
  a new one.
- `make upgrade-test` (also part of `make ci`) builds older releases from git, fills each with data
  through its API (devices, releases, a rollout, a client account, alert rules, events), then starts
  this build on the same data directory. It checks that everything is still there, that device keys,
  enrollment tokens and passwords still work, that newer features work on the upgraded data, that a
  second start changes nothing, and that the old release refuses the upgraded database. Add each
  release's tag to `DEFAULT_REFS` in `scripts/upgrade-test.sh`.

Layout:

```
cmd/armada-server   server entrypoint
cmd/armada-agent    agent entrypoint
cmd/armadactl       CLI
internal/api        wire types shared by everything
internal/rollout    pure rollout logic: targeting, staging, version ordering, health evaluation
internal/chunk      content-defined chunking for delta updates
internal/store      SQLite persistence and numbered schema migrations
internal/upgradetest  upgrade test from older releases (scripts/upgrade-test.sh)
web/e2e             dashboard browser smoke test (run by CI)
internal/server     HTTP API, agent protocol, rollout engine
internal/agent      agent: enrollment, heartbeat, updates, self-update, actions
internal/attestation Android key attestation verifier (Google roots, revocation list)
pkg/license         license issuing and verification (import this in your backend)
android/agent       Android Device Owner agent (Kotlin)
android/client      Android client library for your app
web/static          the dashboard: index.html, style.css, charts.js, and js/ (one file per area,
                    plain scripts loaded in order: core.js first, main.js with routing and startup last)
web/                embedded dashboard and the install scripts (install.sh, install.ps1)
deploy/, examples/  service units, compose file, sample agent config and hooks
docs/               documentation (go test ./docs checks every link)
```


## Accessibility

The dashboard smoke test runs accessibility checks (`web/e2e/a11y.cjs`) on the sign-in page and
every page it opens, as an admin and as a client account, in light and dark mode. It fails on:

- a form field, button or link without a name a screen reader can announce, or an image without
  `alt`;
- a page without exactly one `h1`, or a skipped heading level;
- text below 4.5:1 contrast against its background (3:1 for large text);
- Tab not moving focus, or a focused element without a visible focus ring;
- no `lang` on the page.

Status colors have separate shades for text (`--ok-text`, `--warn-text`, `--bad-text`): the bright
ones are for dots and charts and are too light to read as text on a light background.

## Testing on Windows, macOS and Linux

`internal/platformtest` installs the agent the way a customer would and checks that it enrolls,
restarts when asked from the dashboard (the service manager must bring it back, with a new
process), updates itself to a new release, and (on Windows) uninstalls with `-uninstall-service`:

- Windows: `install.ps1` and the Windows service;
- macOS: `install.sh` and launchd;
- Linux: `install.sh` and systemd.

It changes the machine it runs on, so it only runs when asked. The **Platforms** workflow
(Actions → Platforms → Run workflow) runs it on GitHub's Windows, macOS and Linux machines; run it
before a release. On a spare machine of your own, as a user with password-less `sudo` (or as
Administrator on Windows):

```bash
ARMADA_PLATFORM_TEST=install go test -v -run TestPlatform ./internal/platformtest
```

`ARMADA_PLATFORM_TEST=direct` runs the same checks with the agent as a plain process, changing
nothing (Linux and macOS).

## Fuzz testing

Everything a device sends, and everything the server accepts before sign-in, has a fuzz test: Go's
fuzzer feeds it millions of generated inputs looking for crashes and internal errors.

| Target | Input |
|---|---|
| `FuzzHeartbeat` | Check-ins posted by an enrolled device, through the real handler; the device's pages must still load after each |
| `FuzzPublicEndpoints` | Bodies sent to enrollment, sign-in, two-factor, passkey, single sign-on, site report and Android provisioning endpoints |
| `FuzzParseKeyDescription` | The Android attestation record in a device's certificate |
| `FuzzCompare` | Versions devices report: the order must stay consistent |
| `FuzzParse` (certs) | Certificates, requests and keys, uploaded or reported |
| `FuzzParseImport`, `FuzzParseLabelList` | CSV imports and label lists |
| `FuzzParseTimelineCursor`, `FuzzParseEventTime` | Query parameters |
| `FuzzVerifySnapshot` | Site reports |
| `FuzzParseSmart`, `FuzzParseReports` (agent) | smartctl output and Bluetooth discovery output |
| `FuzzSplit` | Delta-update chunking: chunks must add up to the input exactly |
| `FuzzVerify` (license) | License tokens your backend receives from apps |
| `FuzzParseWindows` | Maintenance windows |

`go test ./...` runs each target's seed inputs. `make fuzz` (or `FUZZTIME=5m scripts/fuzz.sh
FuzzHeartbeat`) fuzzes for real. An input that fails is saved under the package's `testdata/fuzz/`;
commit it with the fix.

---

[← Armada documentation](../README.md#documentation)
