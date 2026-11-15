# Security model

**Production checklist.**

1. Set `-public-url https://fleet.example.com`. Single sign-on and passkeys stay off without it, and
   links are never built from request headers.
2. Behind a reverse proxy or load balancer, set `-trusted-proxies` to its addresses. Otherwise every
   user shares the proxy's address for sign-in throttling (the server logs a warning when it sees
   this).
3. Add admin users (ideally with SSO, passkeys or 2FA), then start with `-disable-admin-token`.
4. Keep `-rollout-approval second-person` (the default), so one stolen operator account can't push
   code to the whole fleet.

How it works:

- Admin, session, personal and device credentials are random 256-bit secrets. The server stores
  only their SHA-256 hashes. Deleting a device revokes its key immediately. User passwords are
  bcrypt hashes. Authenticator secrets are encrypted (AES-GCM) with `secrets.key`; recovery codes
  are stored as hashes. Passkeys store only public keys.
- Roles and client scopes are enforced by the server on every request. The dashboard only hides
  what a user can't use.
- Enrollment tokens can expire, have a use limit, and be revoked.
- A device can download only the artifacts it has been offered. The only public download
  is the newest `armada-agent` binary, which `install.sh` uses.
- Releases are signed with a server-held ed25519 key over
  `(package, version, sha256)`. An artifact therefore can't be replayed under another
  name or version.
- **Rollout approval.** A release is code that runs as root on devices. With `-rollout-approval
  second-person` (the default), every rollout stops after its first stage until someone other than
  the person who started it approves it: **Approve** on the deployment page, or `armadactl
  deployments approve <id>`.
  - Before approval, a rollout runs only its first stage, and only if that stage is at most
    `-unapproved-max-percent` (10%). Rollouts that start bigger (for example a single 100% stage)
    wait in `awaiting_approval` and offer nothing.
  - Uploads with `--auto-deploy` still canary automatically and then wait.
  - The admin token counts as one person, so it can't approve its own rollouts.
  - Use `-rollout-approval off` for a one-person team.
- **Client alert channels can't reach your network.** Client webhooks go only to public HTTPS
  addresses, checked when connecting, and client email only through your relay (see
  [Alerts for client accounts](alerts.md#alerts-for-client-accounts)).
- **Code execution on devices is opt-in on the device and limited to fleet-wide staff.**
  - `exec` actions and **command** health checks both need `allow_exec: true` in the agent config.
    Without it, command checks report "unknown" and never run.
  - Only admins can create or change command checks.
  - `exec` and Android `wipe` need the operator role on the whole fleet. Per-client operators can
    reboot, restart the agent, lock, fetch logs and ask for a check-in on their devices.
- `logs` actions read only the systemd journal and the files a device lists in `log_files`. Set
  `allow_logs: false` to refuse them. Client operators can read logs from their own devices.
- **Sign-in throttling** counts failures per email *and* address (5 in 15 minutes), per address (20),
  and per email from anywhere (50). A stranger therefore can't lock a known user out with a few bad
  guesses, and spreading a guessing attack over many addresses still hits a limit.
  `X-Forwarded-For`, `-Proto` and `-Host` are only believed from `-trusted-proxies`.
- **Admin token.** It bypasses SSO, 2FA and SCIM rules, so:
  - each new address using it is recorded in the activity log (`auth.admin_token`);
  - `-disable-admin-token` turns it off once an enabled admin user exists (the server refuses to start
    otherwise, so you can't lock yourself out);
  - `-rotate-admin-token` replaces the generated token.
- Dashboard sessions are kept in the browser's localStorage. The strict Content-Security-Policy
  below is the protection against scripts reading them.
- The dashboard sends a strict Content-Security-Policy and renders all device-supplied
  text as text.

---

[← Armada documentation](../README.md#documentation)
