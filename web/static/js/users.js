// Armada dashboard: Users (admin).
'use strict';

// ---------- users (admin) ----------

const ROLE_HELP = {
  viewer: 'Read-only',
  operator: 'Manage devices, rollouts and monitors',
  admin: 'Everything, including users, alert channels and signing keys',
};

async function viewUsers(root) {
  const list = h('div', { class: 'card flush' });
  const emailIn = h('input', { type: 'email', required: true, placeholder: 'it@customer.com', 'aria-label': 'Email' });
  const nameIn = h('input', { placeholder: 'optional', 'aria-label': 'Name' });
  const roleIn = h('select', { 'aria-label': 'Role' }, Object.keys(ROLE_HELP).map(r => h('option', { value: r }, r)));
  const roleHint = h('span', { class: 'muted' });
  const scopeIn = h('input', { placeholder: 'empty = whole fleet, or customer=acme', 'aria-label': 'Scope labels' });
  const passIn = h('input', { type: 'password', required: !authConfig.sso, minlength: '10', autocomplete: 'new-password', 'aria-label': 'Initial password',
    placeholder: authConfig.sso ? 'empty: ' + (authConfig.sso_name || 'single sign-on') + ' only' : '' });
  roleIn.value = 'operator';
  const syncRole = () => { roleHint.textContent = ROLE_HELP[roleIn.value]; scopeIn.disabled = roleIn.value === 'admin'; if (scopeIn.disabled) scopeIn.value = ''; };
  roleIn.addEventListener('change', syncRole);

  const mapBox = h('div', { class: 'card' });
  async function refreshMappings() {
    const ms = await api('GET', '/api/v1/group-mappings');
    const gIn = h('input', { required: true, placeholder: 'e.g. Acme IT', 'aria-label': 'Directory group' });
    const rIn = h('select', { 'aria-label': 'Role for group' }, Object.keys(ROLE_HELP).map(r => h('option', { value: r }, r)));
    rIn.value = 'operator';
    const lIn = h('input', { placeholder: 'empty = whole fleet, or customer=acme', 'aria-label': 'Devices for group' });
    const syncAdmin = () => { lIn.disabled = rIn.value === 'admin'; if (lIn.disabled) lIn.value = ''; };
    rIn.addEventListener('change', syncAdmin);
    replace(mapBox, h('h2', {}, 'Group mappings'),
      h('p', { class: 'muted' }, 'Give members of a directory group (from SCIM, or the SSO groups claim) a role, and optionally limit them to some devices: a client account per customer group. Someone in several groups gets each group\'s role on that group\'s devices (operator for one client, viewer for another). Once any mapping exists, directory users in no mapped group can\'t sign in.'),
      ms.length ? table(['Group', 'Role', 'Devices', ''], ms.map(m => h('tr', {},
        h('td', {}, h('strong', {}, m.group), m.from_flag ? h('div', { class: 'muted' }, 'from -oidc-group-roles') : null),
        h('td', {}, m.role),
        h('td', {}, scopeText(m.scope) ? labelChips(m.scope.labels) : h('span', { class: 'muted' }, 'whole fleet')),
        h('td', {}, m.from_flag ? null : h('button', { class: 'btn small danger', onclick: async () => {
          if (confirm(`Remove the mapping for ${m.group}? Its members lose the access it gave at once.`) && await guard(() => api('DELETE', '/api/v1/group-mappings/' + m.id), 'Mapping removed') !== undefined) { refreshMappings(); refresh(); }
        } }, 'Remove'))))) : null,
      h('form', { class: 'inline mt', onsubmit: async e => {
        e.preventDefault();
        let labels;
        try { labels = parseLabels(lIn.value); } catch (err) { return toast(err.message, true); }
        if (await guard(() => api('POST', '/api/v1/group-mappings', { group: gIn.value.trim(), role: rIn.value, scope: { labels } }), 'Mapping saved — members updated')) { refreshMappings(); refresh(); }
      } }, gIn, rIn, lIn, h('button', { class: 'btn', type: 'submit' }, 'Add mapping')));
    syncAdmin();
  }
  const scimBox = h('div', { class: 'card' });
  async function refreshSCIM(created) {
    const toks = await api('GET', '/api/v1/scim/tokens');
    const nameIn = h('input', { placeholder: 'e.g. Okta, Entra ID', 'aria-label': 'SCIM token name' });
    replace(scimBox, h('h2', {}, 'Directory sync (SCIM)'),
      h('p', { class: 'muted' }, 'Let your identity provider (Okta, Microsoft Entra ID, OneLogin, JumpCloud…) create, update and deactivate users here automatically. Deactivated people are signed out at once. The group mappings above decide what each person can do and see.'),
      created ? h('div', { class: 'callout mb' }, h('div', {}, h('strong', {}, 'Give these to your identity provider now'), ' — the token is not shown again.'),
        h('div', { class: 'muted mt' }, 'SCIM base URL (tenant URL)'), h('div', { class: 'secret' }, created.url),
        h('div', { class: 'muted mt' }, 'Secret token (bearer)'), h('div', { class: 'secret' }, created.token)) : null,
      toks.length ? h('ul', { class: 'keylist' }, toks.map(t => h('li', {},
        h('div', {}, h('strong', {}, t.name), h('div', { class: 'muted' }, 'created ' + ago(t.created_at) + (t.last_used_at ? ' · last sync ' + ago(t.last_used_at) : ' · not used yet'))),
        h('button', { class: 'btn small danger', onclick: async () => {
          if (confirm(`Revoke ${t.name}? Your directory stops syncing until you give it a new token.`) && await guard(() => api('DELETE', '/api/v1/scim/tokens/' + t.id), 'Token revoked') !== undefined) refreshSCIM();
        } }, 'Revoke')))) : null,
      h('form', { class: 'inline mt', onsubmit: async e => {
        e.preventDefault();
        const t = await guard(() => api('POST', '/api/v1/scim/tokens', { name: nameIn.value.trim() }));
        if (t) refreshSCIM(t);
      } }, nameIn, h('button', { class: 'btn', type: 'submit' }, 'Create SCIM token')));
  }
  append(root, pageHead('Users', 'People who can sign in to this dashboard and the API'),
    h('div', { class: 'card' }, h('h2', {}, 'Add a user'),
      h('form', { class: 'stack', onsubmit: async e => {
        e.preventDefault();
        let labels;
        try { labels = parseLabels(scopeIn.value); } catch (err) { return toast(err.message, true); }
        const body = { email: emailIn.value.trim(), name: nameIn.value.trim(), role: roleIn.value, scope: { labels }, password: passIn.value };
        if (await guard(() => api('POST', '/api/v1/users', body), body.password ? 'User added — share the password with them privately' : 'User added — they sign in with ' + (authConfig.sso_name || 'single sign-on'))) {
          emailIn.value = nameIn.value = scopeIn.value = passIn.value = '';
          refresh();
        }
      } },
      h('div', { class: 'form-grid' }, h('label', {}, 'Email', emailIn), h('label', {}, 'Name', nameIn), h('label', {}, 'Role', roleIn, roleHint),
        h('label', {}, 'Only devices with labels (client accounts)', scopeIn), h('label', {}, 'Initial password (10+ characters)', passIn)),
      h('p', { class: 'muted' }, 'A scoped user sees only matching devices, their alerts, history and activity. They never see releases, deployments, other customers or fleet-wide settings, and devices they enroll get the scope labels.'),
      h('div', {}, h('button', { class: 'btn primary', type: 'submit' }, 'Add user')))),
    h('div', { class: 'spacer' }), list, h('div', { class: 'spacer' }), mapBox, h('div', { class: 'spacer' }), scimBox);
  syncRole();
  guard(() => refreshSCIM());
  guard(refreshMappings);

  async function refresh() {
    const users = await api('GET', '/api/v1/users');
    const patch = (u, body, msg) => guard(() => api('PATCH', '/api/v1/users/' + u.id, body), msg).then(r => r && refresh());
    replace(list, table(['User', 'Role', 'Devices', 'Last sign-in', ''],
      users.map(u => {
        const self = me.user && me.user.id === u.id;
        const multi = (u.grants || []).length > 0;
        const roleSel = h('select', { 'aria-label': 'Role for ' + u.email, disabled: self || u.group_managed || multi || undefined,
          title: u.group_managed ? 'Set by group mappings' : multi ? 'Per-client roles: use Access' : undefined },
          Object.keys(ROLE_HELP).map(r => h('option', { value: r }, r)));
        roleSel.value = u.role;
        roleSel.addEventListener('change', () => patch(u, { role: roleSel.value }, 'Role changed').then(r => r === undefined && (roleSel.value = u.role)));
        const editorRow = h('tr', { class: 'hidden' }, h('td', { colspan: '5' }, accessEditor(u, gs => patch(u, gs, 'Access updated'), () => editorRow.classList.add('hidden'))));
        return [h('tr', {},
          h('td', {}, h('strong', {}, u.name || u.email), u.name ? h('div', { class: 'muted' }, u.email) : null,
            h('div', { class: 'btn-row mt' }, u.disabled ? badge('disabled') : null,
              u.two_factor ? h('span', { class: 'chip', title: 'Uses an authenticator app' }, '2FA') : null,
              u.passkeys ? h('span', { class: 'chip', title: 'Passkeys or security keys' }, u.passkeys === 1 ? '1 passkey' : u.passkeys + ' passkeys') : null,
              u.sso ? h('span', { class: 'chip', title: 'Has signed in with single sign-on' }, u.no_password ? 'SSO only' : 'SSO') : null,
              u.provisioned ? h('span', { class: 'chip', title: 'Managed by your directory over SCIM: name, active state and (with group mapping) role come from there' }, 'Directory') : null,
              self ? h('span', { class: 'muted' }, 'you') : null)),
          h('td', {}, multi ? h('span', { class: 'muted' }, 'per client') : roleSel),
          h('td', {}, u.no_group_access ? [badge('no access'), h('div', { class: 'muted' }, u.group_managed ? 'in no mapped group' : 'set access by hand')]
            : [multi
              ? h('ul', { class: 'grants' }, u.grants.map(g => h('li', {}, h('strong', {}, g.role), ' ',
                scopeText(g.scope) ? labelChips(g.scope.labels) : h('span', { class: 'muted' }, 'whole fleet'))))
              : scopeText(u.scope) ? labelChips(u.scope.labels) : h('span', { class: 'muted' }, 'whole fleet'),
            u.group_managed ? h('div', { class: 'muted', title: (u.groups || []).join(', ') }, 'from groups') : null]),
          h('td', { class: 'nowrap' }, u.last_login_at ? ago(u.last_login_at) : h('span', { class: 'muted' }, 'never')),
          h('td', {}, self ? null : h('div', { class: 'btn-row' },
            u.group_managed ? null : h('button', { class: 'btn small', title: 'Role per client: which devices this person can see and manage', onclick: () => editorRow.classList.toggle('hidden') }, 'Access'),
            h('button', { class: 'btn small', onclick: () => {
              const pw = prompt(`New password for ${u.email} (10+ characters). Their sessions are signed out.`);
              if (pw) patch(u, { password: pw }, 'Password reset');
            } }, u.no_password ? 'Set password' : 'Reset password'),
            u.two_factor || u.passkeys ? h('button', { class: 'btn small', title: 'For a lost phone or key: removes their authenticator and passkeys and signs them out', onclick: () => {
              if (confirm(`Remove ${u.email}'s authenticator app and passkeys? Do this only after confirming who is asking (lost device). They are signed out everywhere.`)) patch(u, { reset_two_factor: true }, 'Two-factor sign-in reset');
            } }, 'Reset 2FA') : null,
            u.sso ? h('button', { class: 'btn small', title: 'Forget their single sign-on identity; the next sign-in links again by email', onclick: () => {
              if (confirm(`Unlink ${u.email} from their single sign-on identity?`)) patch(u, { unlink_sso: true }, 'Unlinked');
            } }, 'Unlink SSO') : null,
            h('button', { class: 'btn small', onclick: () => patch(u, { disabled: !u.disabled }, u.disabled ? 'Enabled' : 'Disabled — signed out everywhere') },
              u.disabled ? 'Enable' : 'Disable'),
            h('button', { class: 'btn small danger', onclick: async () => {
              if (confirm(`Delete ${u.email}? Their personal API tokens stop working.`) && await guard(() => api('DELETE', '/api/v1/users/' + u.id), 'User deleted') !== undefined) refresh();
            } }, 'Delete')))), editorRow];
      }), 'No users yet. Until you add one, sign in with the admin token.'));
  }
  await guard(refresh);
  return async () => { if (!root.contains(document.activeElement) || document.activeElement === document.body) await refresh(); };
}
