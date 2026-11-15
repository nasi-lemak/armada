// Armada dashboard: My account.
'use strict';

// ---------- my account ----------

// Two-factor sign-in: set up with an authenticator app, recovery codes, turn off.
function twoFactorCard() {
  const card = h('div', { class: 'card' });
  const codeInput = label => h('input', { inputmode: 'numeric', autocomplete: 'one-time-code', maxlength: '11', placeholder: '123456', 'aria-label': label });
  const showCodes = codes => {
    const text = codes.join('\n');
    return h('div', { class: 'callout mt' },
      h('div', {}, h('strong', {}, 'Save these recovery codes now'), ' — they are shown only once. Each one signs you in a single time if you lose your phone.'),
      h('div', { class: 'codes' }, codes.map(c => h('span', {}, c))),
      h('div', { class: 'btn-row' },
        h('button', { class: 'btn small', type: 'button', onclick: () => navigator.clipboard.writeText(text).then(() => toast('Copied'), () => toast('Copy failed; select the codes instead', true)) }, 'Copy'),
        h('button', { class: 'btn small', type: 'button', onclick: () => {
          const url = URL.createObjectURL(new Blob([`Armada recovery codes for ${me.user.email}\n\n${text}\n`], { type: 'text/plain' }));
          const a = h('a', { href: url, download: 'armada-recovery-codes.txt' });
          document.body.append(a); a.click(); a.remove();
          setTimeout(() => URL.revokeObjectURL(url), 10000);
        } }, 'Download')));
  };
  const afterChange = async () => { me = null; await guard(loadMe); };
  const render = () => {
    if (!me.user.two_factor) {
      replace(card, h('h2', {}, 'Two-factor sign-in'),
        h('p', { class: 'muted' }, 'Off. Turn it on so a stolen password alone can\'t get into your account. You\'ll need an authenticator app such as Google Authenticator, Microsoft Authenticator, 1Password or Authy.'),
        h('div', {}, h('button', { class: 'btn primary', onclick: start }, 'Set up two-factor sign-in')));
      return;
    }
    const code = codeInput('Current code');
    const out = h('div', {});
    replace(card, h('h2', {}, 'Two-factor sign-in ', badge('enabled')),
      h('p', { class: 'muted' }, 'You enter a code from your authenticator app after your password.'),
      h('form', { class: 'stack', onsubmit: e => e.preventDefault() },
        h('label', {}, 'Code from your app (needed to change these settings)', code),
        h('div', { class: 'btn-row' },
          h('button', { class: 'btn', type: 'button', onclick: async () => {
            const r = await guard(() => api('POST', '/api/v1/me/2fa/recovery-codes', { code: code.value }));
            if (r) { code.value = ''; replace(out, showCodes(r.codes)); }
          } }, 'New recovery codes'),
          h('button', { class: 'btn danger', type: 'button', onclick: async () => {
            if (!confirm('Turn off two-factor sign-in? Your password alone will be enough to sign in.')) return;
            if (await guard(() => api('POST', '/api/v1/me/2fa/disable', { code: code.value }), 'Two-factor sign-in turned off') === undefined) return;
            await afterChange(); render();
          } }, 'Turn off'))),
      out);
  };
  const start = async () => {
    const st = await guard(() => api('POST', '/api/v1/me/2fa/setup'));
    if (!st) return;
    const code = codeInput('Code from the app');
    replace(card, h('h2', {}, 'Set up two-factor sign-in'),
      h('div', { class: 'qr-box' },
        h('img', { src: 'data:image/png;base64,' + st.qr_png, alt: 'QR code for your authenticator app', width: '200', height: '200' }),
        h('div', { class: 'stack' },
          h('div', {}, h('strong', {}, '1. '), 'Scan the code with your authenticator app.'),
          h('div', { class: 'muted' }, 'Can\'t scan? Enter this key:'),
          h('div', { class: 'secret' }, st.secret.replace(/(.{4})/g, '$1 ').trim()),
          h('div', {}, h('strong', {}, '2. '), 'Enter the 6-digit code it shows.'),
          h('form', { class: 'inline', onsubmit: async e => {
            e.preventDefault();
            const r = await guard(() => api('POST', '/api/v1/me/2fa/enable', { code: code.value }));
            if (!r) return;
            await afterChange();
            render();
            card.append(showCodes(r.codes));
            if (location.hash === '#/account') toast('Two-factor sign-in is on');
          } }, code, h('button', { class: 'btn primary', type: 'submit' }, 'Turn on')),
          h('div', {}, h('button', { class: 'btn ghost small', type: 'button', onclick: render }, 'Cancel')))));
    code.focus();
  };
  render();
  return card;
}

async function viewAccount(root) {
  const tokens = h('div', { class: 'card flush' });
  const created = h('div', {});
  append(root, pageHead('My account', [me.user ? me.user.email : me.name, ' · ', me.grants && me.grants.length > 1 ? grantsText(me.grants) : me.role + (me.scoped ? ' · devices with ' + scopesText(me) : ' · whole fleet')]));
  if (!me.user) {
    append(root, h('div', { class: 'card' }, h('p', {}, 'You are signed in with the server admin token. ',
      atLeast('admin') ? [h('a', { href: '#/users' }, 'Add a user'), ' for each person, so actions are attributed to them and access can be revoked individually.'] : null)));
    return;
  }
  if (me.two_factor_setup_required) {
    append(root, h('div', { class: 'banner' }, h('strong', {}, 'Set up two-factor sign-in to continue. '),
      'This server requires a second step for password sign-in: add a passkey or an authenticator app. It takes a minute.'),
      h('div', { class: 'grid cols-2' }, passkeysCard(), twoFactorCard()));
    return;
  }
  const cur = h('input', { type: 'password', required: true, autocomplete: 'current-password', 'aria-label': 'Current password' });
  const nw = h('input', { type: 'password', required: true, minlength: '10', autocomplete: 'new-password', 'aria-label': 'New password' });
  const tokName = h('input', { required: true, placeholder: 'e.g. CI pipeline', 'aria-label': 'Token name' });
  const ssoName = authConfig.sso_name || 'single sign-on';
  append(root,
    h('div', { class: 'grid cols-2' },
      me.user.no_password
        ? h('div', { class: 'card' }, h('h2', {}, 'Sign-in'),
          h('p', {}, `You sign in with ${ssoName}. Your password and two-factor settings are managed there.`))
        : twoFactorCard(),
      me.user.no_password ? null : passkeysCard(),
      me.user.no_password ? null : h('div', { class: 'card' }, h('h2', {}, 'Change password'),
        h('form', { class: 'stack', onsubmit: async e => {
          e.preventDefault();
          if (await guard(() => api('POST', '/api/v1/me/password', { current: cur.value, new: nw.value })) === undefined) return;
          // Changing the password signs out every session, including this one.
          token = ''; me = null; store.del('armada.token');
          showLogin('Password changed. Sign in again with the new password.');
        } },
        h('label', {}, 'Current password', cur), h('label', {}, 'New password (10+ characters)', nw),
        h('div', {}, h('button', { class: 'btn primary', type: 'submit' }, 'Change password')))),
      h('div', { class: 'card' }, h('h2', {}, 'Personal API tokens'),
        h('p', { class: 'muted' }, 'For scripts and armadactl (--token). A token acts as you, with your role and scope. See the ', h('a', { href: '#/api' }, 'API reference'), '.'),
        h('form', { class: 'inline', onsubmit: async e => {
          e.preventDefault();
          const t = await guard(() => api('POST', '/api/v1/me/tokens', { name: tokName.value.trim() }));
          if (!t) return;
          tokName.value = '';
          replace(created, h('div', { class: 'callout mt' },
            h('div', {}, h('strong', {}, 'Copy this token now'), ' — it will not be shown again.'),
            h('div', { class: 'secret' }, t.token)));
          refresh();
        } }, tokName, h('button', { class: 'btn', type: 'submit' }, 'Create')),
        created)),
    h('div', { class: 'spacer' }), tokens);

  async function refresh() {
    const toks = await api('GET', '/api/v1/me/tokens');
    replace(tokens, table(['Token', 'Created', 'Last used', ''],
      toks.map(t => h('tr', {},
        h('td', {}, h('strong', {}, t.name), h('div', { class: 'muted mono' }, t.id)),
        h('td', { class: 'nowrap' }, ago(t.created_at)),
        h('td', { class: 'nowrap' }, t.last_used_at ? ago(t.last_used_at) : h('span', { class: 'muted' }, 'never')),
        h('td', {}, h('button', { class: 'btn small danger', onclick: async () => {
          if (confirm(`Revoke ${t.name}?`) && await guard(() => api('DELETE', '/api/v1/me/tokens/' + t.id), 'Token revoked') !== undefined) refresh();
        } }, 'Revoke')))),
      'No personal tokens.'));
  }
  await guard(refresh);
}
