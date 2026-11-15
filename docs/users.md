# Users, roles and client accounts

The admin token is for bootstrapping and automation. Give each person their own account, so their
actions are attributed to them and their access can be removed on its own.

```bash
armadactl users create --email ops@yourco.com --name "Dana" --role admin         # password read from stdin
armadactl users create --email it@acme.com --role operator --scope customer=acme  # a client account
```

Or use **Users** in the dashboard. People sign in with their email and password.

| Role | Can |
|---|---|
| `viewer` | See everything their scope allows. Change nothing. |
| `operator` | Also edit devices, run actions, create enrollment tokens, and (fleet-wide only) upload releases, run rollouts and manage monitors. |
| `admin` | Everything: users, alert channels and rules, Android apps, backups. Admins are always fleet-wide. |

**Client accounts.** A *scope* limits a user to devices with matching labels (and optionally a channel),
for example `customer=acme`. A scoped user:

- sees only matching devices, their alerts, history and activity. Other devices answer 404, as if they
  didn't exist;
- never sees releases, deployments, enrollment-token lists, monitors, vendor alert rules or fleet-wide
  history, which are shared across customers;
- can set up their own alerts for their devices (see
  [Alerts for client accounts](alerts.md#alerts-for-client-accounts));
- can only create enrollment tokens that put new devices in their scope (the scope labels are forced
  onto the token);
- can't move a device out of their scope by relabelling it, and can't change app licensing (that is
  your anti-redistribution control).

So a customer can watch and manage their own devices, while you decide what software they run.

Set scopes by hand on the Users page, or let your directory do it with
[group mappings](#group-mappings-and-client-accounts).

**Per-client roles.** A person can have a different role for each client. Their access is a list of
*grants*, each a role on some devices (or on the whole fleet). For example:

| Role | Devices |
|---|---|
| viewer | whole fleet |
| operator | `customer=acme` |

This support engineer can look at every device, and fleet-wide pages read-only, but can only reboot,
edit, fetch logs from or enroll Acme's devices. On any device, the highest grant that covers it
applies. Fleet-wide pages (releases, rollouts, monitors, Android apps) need a fleet-wide grant, at
the level the page needs. Set grants with **Access** on the Users page, or:

```bash
armadactl users set sam@vendor.com --grant viewer --grant operator:customer=acme
```

Admin is always fleet-wide. A person whose grants are all for some clients is a client account: no
fleet-wide pages at all.

**Tokens for scripts.** Each user can create personal API tokens (`ut_…`) on **My account** or with
`armadactl me tokens create --name ci`. A token acts as its owner with the same role and scope, and
stops working when it is revoked or the user is disabled or deleted. Pass it to the CLI with `--token`
or `ARMADA_TOKEN`.

**Sessions and safety.**
- Dashboard sessions last 12 hours without activity (`-session-ttl`).
- Changing or resetting a password signs that user out everywhere.
- Disabling a user takes effect on their next request.
- Five failed sign-ins for an email lock that email for 15 minutes; twenty from one address lock the
  address. Behind a reverse proxy, every user shares the proxy's address.
- Passwords are stored as bcrypt hashes; sessions and tokens as SHA-256 hashes.
- Users can't change their own role, disable themselves, or delete themselves, so an admin can't
  lock the last admin out by accident. The admin token keeps working regardless.

## Two-factor sign-in

Each user can turn on two-factor sign-in under **My account**. They scan a QR code with an
authenticator app (Google Authenticator, Microsoft Authenticator, 1Password, Authy and others) and
confirm with a code. After that, signing in takes the password and then a 6-digit code.

- Turning it on gives **10 recovery codes**. Each one signs in once without the phone. Users can
  generate a new set at any time, which voids the old one.
- A code can't be used twice. Each sign-in attempt allows five codes before the password is needed again.
  Wrong codes also count towards the email's lockout.
- Turning it on signs out the user's other sessions, because those only had the password.
- **Lost phone:** an admin clicks **Reset 2FA** on the Users page, or runs
  `armadactl users set <email> --reset-2fa`. That removes the authenticator and any passkeys, and signs the
  user out everywhere.
  Check who is asking before doing this.
- **Require it for everyone:** start the server with `-require-2fa`. Password users who haven't set it
  up can only reach their account page until they do. Single sign-on users are exempt, because their
  identity provider is where MFA belongs.
- Scripts sign in with a personal API token, not a password. With a password, the API also accepts
  `{"email", "password", "code"}` in one request.
- Authenticator secrets are encrypted with `<data-dir>/secrets.key` (included in backups), so a copy of
  the database alone doesn't reveal them.

## Passkeys

Under **My account**, users can add passkeys: Touch ID or Face ID, Windows Hello, Android screen lock,
a password manager's passkeys (iCloud Keychain, Google Password Manager, 1Password, Bitwarden), or a
hardware security key such as a YubiKey. Passkeys can't be phished, because they only work on the
site they were made for.

They work two ways:

- **Passwordless.** **Sign in with a passkey** on the sign-in page needs no email or password. The
  device checks the user's fingerprint, face or PIN, which counts as the second factor, so it
  satisfies `-require-2fa`.
- **As the second step** after a password, instead of an authenticator code. Simple security keys
  without a PIN work this way.

Details:

- Adding a passkey needs a sign-in from the last 15 minutes. A stolen session therefore can't add a
  permanent way in. Adding the first second factor signs out the user's other sessions.
- A security key whose signature counter goes backwards (a sign of cloning) is refused.
- **Reset 2FA** (admin) removes a user's passkeys along with their authenticator app.
- Passkeys belong to the host name in `-public-url` (for example `fleet.example.com`). The dashboard
  must be opened at that address, over HTTPS (or at `localhost` for testing). If you move the server
  to a new domain, users need to add passkeys again. IP addresses can't be used.
- Single sign-on users use their provider's sign-in (which can have its own passkeys), so offboarding
  and group rules at the provider can't be bypassed. Passkey sign-in is off when `-password-login=false`.

## Single sign-on

Armada works with any OpenID Connect provider: Google Workspace, Microsoft Entra ID, Okta, Auth0,
Keycloak, Authentik, JumpCloud and others. For SAML-only providers, put Keycloak or Authentik in
between.

1. Register Armada with your provider as a *web application*, with this redirect URI:
   `https://fleet.example.com/api/v1/auth/sso/callback`
2. Start the server with the provider's issuer and client ID. The secret is read from the
   environment only, so it doesn't show up in `ps`:

   ```bash
   export ARMADA_OIDC_CLIENT_SECRET=...
   armada-server -public-url https://fleet.example.com \
     -oidc-issuer https://accounts.google.com -oidc-client-id 1234.apps.googleusercontent.com \
     -oidc-name Google -oidc-allowed-domains yourco.com
   ```

| Provider | Issuer |
|---|---|
| Google Workspace | `https://accounts.google.com` |
| Microsoft Entra ID | `https://login.microsoftonline.com/<tenant-id>/v2.0` |
| Okta | `https://<your-org>.okta.com` (or a custom authorization server URL) |
| Keycloak | `https://keycloak.example.com/realms/<realm>` |
| Auth0 | `https://<tenant>.auth0.com/` |

The sign-in page then shows **Sign in with Google** (or whatever `-oidc-name` says). Password sign-in
stays available unless you pass `-password-login=false`. The admin token always works, as a
break-glass.

**Who can sign in.** There are three options:

- **Only people you add (default).** Add them on the Users page with no password, or run
  `armadactl users create --email … --role … --sso-only`. The first SSO sign-in with that email links
  the account to their identity at the provider. After that, sign-ins match the provider's stable user
  ID, so a later email change at the provider doesn't matter. Another identity claiming the same email
  is refused.
- **Anyone from your domains:** set `-oidc-create-role viewer` (or `operator`) with
  `-oidc-allowed-domains`. New people get an account at their first sign-in.
- **By group:** use [group mappings](#group-mappings-and-client-accounts). Each sign-in sets the user's
  role and devices from the groups claim (`-oidc-groups-claim`, default `groups`), and people in no
  mapped group can't sign in. Removing someone from the group in your directory is then enough.

Always set `-oidc-allowed-domains` for providers where users can change their own email address (for
example multi-tenant Entra apps). Users who signed in with SSO can't use a password unless an admin
sets one. Their MFA happens at the provider.

## SCIM provisioning

With SCIM, your identity provider manages Armada accounts for you. Assigning someone to the Armada
app creates their account. Changes to their name or email follow. Deactivating or unassigning them
disables them here immediately and ends their sessions. Okta, Microsoft Entra ID, OneLogin, JumpCloud
and others support it.

1. In Armada, go to **Users → Directory sync (SCIM)** and create a token (or run
   `armadactl scim tokens create --name Okta`). Copy the **SCIM base URL**
   (`https://fleet.example.com/scim/v2`) and the token.
2. In your identity provider, turn on provisioning for the Armada app:
   - **Okta:** in the app's *Provisioning* tab, set *SCIM connector base URL*, *Unique identifier field
     for users* = `userName`, and *Authentication mode* = *HTTP Header* with the token. Enable
     *Create*, *Update attributes* and *Deactivate users*, and *Push groups*.
   - **Entra ID:** in the enterprise app, go to *Provisioning*, choose *Automatic*, and enter the
     *Tenant URL* and *Secret token*. The default attribute mappings work (`userPrincipalName` →
     `userName`, `mail` → `emails[type eq "work"].value`).
3. Use SSO for sign-in (see above). Provisioned users have no password.

**Access from groups.** Groups the directory pushes are matched against the
[group mappings](#group-mappings-and-client-accounts). Taking someone out of *Fleet Admins* in the
directory demotes them here at once. While no mappings exist, provisioned users get
`-scim-default-role` (default `viewer`) and admins set their access by hand.

**Existing accounts.** If the directory sends a user whose email already has an Armada account, it
takes that account over: from then on, the directory manages it. With group mappings, the account's
access then follows its groups. **Put existing admins in a group mapped to admin before turning on
sync.** The admin token keeps working if anyone gets locked out.

Details:
- Supported: Users and Groups with create, read, replace, PATCH and delete; `attr eq "value"` filters
  (`userName`, `externalId`, `emails`, `displayName`); paging; and the discovery endpoints. Unknown
  user attributes (title, department and the like) are accepted and ignored.
- Deactivation (`active: false`) keeps the account but disables it. Deleting it in the directory
  deletes it here.
- SCIM tokens can only call `/scim/v2`, not the rest of the API. Revoke them on the Users page. The
  activity log records every directory change as *SCIM (token name)*.

## Group mappings and client accounts

A group mapping gives the members of a directory group a role, and can limit them to some devices.
That lets your directory run client accounts: one group per customer.

| Group | Role | Devices |
|---|---|---|
| Fleet Admins | admin | whole fleet |
| Fleet Ops | operator | whole fleet |
| Acme IT | operator | `customer=acme` |
| Globex IT | viewer | `customer=globex` |

Add them under **Users → Group mappings**, or with the CLI:

```bash
armadactl groups map --group "Acme IT" --role operator --scope customer=acme
armadactl groups map --group "Fleet Ops" --role operator
armadactl groups
```

Groups come from SCIM (for provisioned users) or from the SSO groups claim at each sign-in. The rules:

- **Each group is a grant.** Someone in *Acme IT* and *Globex IT* (an MSP, say) is an operator on
  Acme's devices and a viewer on Globex's. Being an operator for one client never makes them an
  operator for another. On any device, the highest grant covering it applies.
- A fleet-wide group adds access everywhere. *Fleet Ops* plus *Acme IT* makes someone an operator
  everywhere, while *Support (viewer, whole fleet)* plus *Acme IT* lets them see everything and
  operate only Acme's devices.
- **No mapped group, no access.** Once any mapping exists, a directory user in none of the mapped
  groups can't sign in, and their live sessions stop working.
- **Changes apply at once.** Editing a mapping or someone's groups updates every affected account
  immediately. Their role and devices can't be edited by hand in Armada.
- **Removing every mapping fails closed.** People whose access came from groups get no access until an
  admin sets their role and devices by hand, so a client account never falls back to the whole fleet.
- Admin can't be limited to devices, so admin mappings are always fleet-wide.
- When a client account in several groups creates an enrollment token, the token's labels must pick
  one of their customers (for example `customer=globex`).

`-oidc-group-roles` still works. It adds fleet-wide mappings that are shown on the page but can only
be changed with the flag.

**Security details.** The flow is the authorization-code flow with PKCE and a nonce. The callback must
come back to the browser that started it (a cookie check). The session token never appears in a URL;
the browser exchanges a one-time ticket, valid for a minute, for it.

---

[← Armada documentation](../README.md#documentation)
