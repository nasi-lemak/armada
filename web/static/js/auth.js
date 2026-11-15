// Armada dashboard: Sign-in: password, token, two-factor, single sign-on and passkeys.
'use strict';

// ---------- auth ----------

async function loadMe() {
  me = await api('GET', '/api/v1/me');
  document.body.dataset.role = me.role;
  document.body.dataset.scoped = String(!!me.scoped);
  document.body.dataset.fleetRole = me.fleet_role || '';
  const who = me.user ? (me.user.name || me.user.email) : me.name;
  replace($('#whoami'), who, h('span', { class: 'role muted', title: me.grants && me.grants.length > 1 ? grantsText(me.grants) : '' },
    ' · ' + (me.grants && me.grants.length > 1 ? 'per-client access' : me.role + (me.scoped ? ' · ' + scopesText(me) : ''))));
  return me;
}

function scopeText(sel) {
  if (!sel) return '';
  return [sel.channel && 'channel ' + sel.channel, ...sortedEntries(sel.labels || {}).map(([k, v]) => k + '=' + v)].filter(Boolean).join(' ');
}
// accessEditor edits a user's grants: one row per set of devices.
function accessEditor(u, save, close) {
  const rowsBox = h('div', { class: 'stack' });
  const addRow = (g = { role: 'viewer', scope: {} }) => {
    const roleSel = h('select', { 'aria-label': 'Role' }, Object.keys(ROLE_HELP).map(r => h('option', { value: r }, r)));
    roleSel.value = g.role;
    const labels = h('input', { placeholder: 'empty = whole fleet, or customer=acme', 'aria-label': 'Devices (labels)',
      value: sortedEntries((g.scope && g.scope.labels) || {}).map(([k, v]) => k + '=' + v).join(' ') });
    const sync = () => { labels.disabled = roleSel.value === 'admin'; if (labels.disabled) labels.value = ''; };
    roleSel.addEventListener('change', sync);
    const row = h('div', { class: 'inline' }, roleSel, labels, h('button', { class: 'btn small ghost', type: 'button', onclick: () => row.remove() }, 'Remove'));
    row.read = () => ({ role: roleSel.value, scope: { labels: parseLabels(labels.value) } });
    sync();
    rowsBox.append(row);
  };
  const current = (u.grants || []).length ? u.grants : [{ role: u.role, scope: u.scope || {} }];
  current.forEach(addRow);
  return h('form', { class: 'stack access-editor', onsubmit: e => {
    e.preventDefault();
    let gs;
    try { gs = [...rowsBox.children].map(r => r.read()); } catch (err) { return toast(err.message, true); }
    if (!gs.length) return toast('Give at least one grant', true);
    save(gs.length === 1 ? { grants: [], role: gs[0].role, scope: gs[0].scope } : { grants: gs });
    close();
  } },
  h('div', { class: 'muted' }, `Access for ${u.email}: a role on each set of devices. On a device, the highest matching role applies; a row with no labels covers the whole fleet (and fleet-wide pages such as releases and rollouts).`),
  rowsBox,
  h('div', { class: 'btn-row' }, h('button', { class: 'btn small', type: 'button', onclick: () => addRow() }, 'Add client'),
    h('button', { class: 'btn small primary', type: 'submit' }, 'Save access'),
    h('button', { class: 'btn small ghost', type: 'button', onclick: close }, 'Cancel')));
}

// Per-client access: a role on the devices each grant's scope matches.
function selMatches(sel, d) {
  if (sel.channel && sel.channel !== d.channel) return false;
  return Object.entries(sel.labels || {}).every(([k, v]) => d.labels && k in d.labels && (v === '*' || d.labels[k] === v));
}
function roleForDevice(d) {
  let best = '';
  for (const g of (me && me.grants) || []) {
    const fleet = !g.scope || (!g.scope.channel && !Object.keys(g.scope.labels || {}).length);
    if ((fleet || selMatches(g.scope, d)) && (ROLE_RANK[g.role] || 0) > (ROLE_RANK[best] || 0)) best = g.role;
  }
  return best;
}
function grantsText(gs) {
  return gs.map(g => g.role + ' on ' + (scopeText(g.scope) || 'the whole fleet')).join(' · ');
}
// scopesText describes several scopes (a device in any of them is visible).
function scopesText(m) {
  const list = (m.scopes && m.scopes.length) ? m.scopes : (scopeText(m.scope) ? [m.scope] : []);
  return list.map(scopeText).join(' or ');
}

let tokenMode = false;
let challenge = ''; // set while waiting for a second-factor code
let challengeMethods = []; // "code" and/or "passkey"
let authConfig = { password_login: true, sso: false, passkeys: false };
const passkeysSupported = () => !!(window.PublicKeyCredential && navigator.credentials);
function syncLoginMode() {
  const pw = authConfig.password_login;
  $('#login-sso').classList.toggle('hidden', !authConfig.sso || tokenMode || !!challenge);
  const pkLogin = authConfig.passkeys && passkeysSupported();
  $('#login-passkey').classList.toggle('hidden', !pkLogin || tokenMode || !!challenge);
  $('#login-or').classList.toggle('hidden', !(authConfig.sso || pkLogin) || !pw || tokenMode || !!challenge);
  $('#login-2fa-passkey').classList.toggle('hidden', !challengeMethods.includes('passkey') || !passkeysSupported());
  $('#login-code-box').classList.toggle('hidden', !challengeMethods.includes('code') && challengeMethods.includes('passkey'));
  $('#login-user').classList.toggle('hidden', tokenMode || !pw || !!challenge);
  $('#login-2fa').classList.toggle('hidden', !challenge);
  $('#login-admin').classList.toggle('hidden', !tokenMode);
  const passkeyOnlyStep = !!challenge && !challengeMethods.includes('code');
  $('#login-form button[type=submit]').classList.toggle('hidden', (!tokenMode && !pw && !challenge) || passkeyOnlyStep);
  $('#login-switch').textContent = tokenMode ? (pw ? 'Sign in with email instead' : 'Back') : 'Sign in with a token instead';
  $('#login-switch').classList.toggle('hidden', !!challenge);
}
fetch('/api/v1/auth/config').then(r => r.ok ? r.json() : null).then(c => {
  if (!c) return;
  authConfig = c;
  if (c.sso_name) $('#login-sso').textContent = 'Sign in with ' + c.sso_name;
  syncLoginMode();
}).catch(() => {});
$('#login-switch').addEventListener('click', () => {
  tokenMode = !tokenMode;
  syncLoginMode();
  (tokenMode ? $('#login-token') : $('#login-email')).focus();
});

function showLogin(msg) {
  clearInterval(refreshTimer);
  $('#app').classList.add('hidden');
  $('#login').classList.remove('hidden');
  const err = $('#login-error');
  err.textContent = msg || '';
  err.classList.toggle('hidden', !msg);
  syncLoginMode();
  (challenge ? $('#login-code') : tokenMode ? $('#login-token') : $('#login-email')).focus();
}

// Finish signing in with a session or API token.
async function signedIn(candidate) {
  const res = await fetch('/api/v1/me', { headers: { Authorization: 'Bearer ' + candidate } }).catch(() => null);
  if (!res || !res.ok) return showLogin(res ? 'That token was not accepted.' : 'Could not reach the server.');
  token = candidate;
  challenge = '';
  challengeMethods = [];
  store.set('armada.token', token);
  $('#login-token').value = $('#login-password').value = $('#login-code').value = '';
  $('#login').classList.add('hidden');
  $('#app').classList.remove('hidden');
  me = null;
  groupings = null; // another account may see other customers
  route();
}

$('#login-form').addEventListener('submit', async e => {
  e.preventDefault();
  const post = (path, body) => fetch(path, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) }).catch(() => null);
  if (challenge) {
    const code = $('#login-code').value.trim();
    if (!code) return showLogin('Enter the code from your authenticator app.');
    const res = await post('/api/v1/auth/2fa', { challenge, code });
    if (!res) return showLogin('Could not reach the server.');
    if (!res.ok) {
      const msg = (await res.json().catch(() => ({}))).error || '';
      if (msg.includes('expired')) { challenge = ''; return showLogin('That took too long or too many codes were wrong. Enter your password again.'); }
      $('#login-code').value = '';
      return showLogin('That code is not valid. Codes change every 30 seconds.');
    }
    return signedIn((await res.json()).token);
  }
  if (tokenMode) {
    const t = $('#login-token').value.trim();
    if (!t) return showLogin('Enter a token.');
    return signedIn(t);
  }
  const email = $('#login-email').value.trim(), password = $('#login-password').value;
  if (!email || !password) return showLogin('Enter your email and password.');
  const res = await post('/api/v1/auth/login', { email, password });
  if (!res) return showLogin('Could not reach the server.');
  if (res.status === 429) return showLogin('Too many failed attempts. Wait 15 minutes and try again.');
  if (res.status === 403) return showLogin('Password sign-in is turned off. Use single sign-on.');
  if (!res.ok) return showLogin('Wrong email or password.');
  const lr = await res.json();
  if (lr.two_factor_required) {
    challenge = lr.challenge;
    challengeMethods = lr.two_factor_methods || ['code'];
    $('#login-password').value = '';
    return showLogin();
  }
  signedIn(lr.token);
});

// ---------- passkeys (WebAuthn) ----------

const b64u = {
  dec: s => Uint8Array.from(atob(s.replace(/-/g, '+').replace(/_/g, '/') + '==='.slice((s.length + 3) % 4)), c => c.charCodeAt(0)).buffer,
  enc: buf => btoa(String.fromCharCode(...new Uint8Array(buf))).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, ''),
};
function creationOptions(o) {
  const pk = { ...o.publicKey, challenge: b64u.dec(o.publicKey.challenge), user: { ...o.publicKey.user, id: b64u.dec(o.publicKey.user.id) } };
  pk.excludeCredentials = (pk.excludeCredentials || []).map(c => ({ ...c, id: b64u.dec(c.id) }));
  return { publicKey: pk };
}
function requestOptions(o) {
  const pk = { ...o.publicKey, challenge: b64u.dec(o.publicKey.challenge) };
  pk.allowCredentials = (pk.allowCredentials || []).map(c => ({ ...c, id: b64u.dec(c.id) }));
  return { publicKey: pk };
}
function credentialJSON(c) {
  const r = c.response, out = { id: c.id, rawId: b64u.enc(c.rawId), type: c.type, authenticatorAttachment: c.authenticatorAttachment || undefined,
    clientExtensionResults: c.getClientExtensionResults ? c.getClientExtensionResults() : {}, response: { clientDataJSON: b64u.enc(r.clientDataJSON) } };
  if (r.attestationObject) {
    out.response.attestationObject = b64u.enc(r.attestationObject);
    if (r.getTransports) out.response.transports = r.getTransports();
  } else {
    out.response.authenticatorData = b64u.enc(r.authenticatorData);
    out.response.signature = b64u.enc(r.signature);
    if (r.userHandle) out.response.userHandle = b64u.enc(r.userHandle);
  }
  return out;
}
// passkeyError turns a browser WebAuthn failure into something readable.
function passkeyError(e) {
  if (e && e.name === 'NotAllowedError') return 'The passkey request was cancelled or timed out.';
  if (e && e.name === 'InvalidStateError') return 'This passkey is already registered.';
  if (e && e.name === 'SecurityError') return 'Passkeys need the dashboard on HTTPS (or localhost) at the server\'s public URL.';
  return (e && e.message) || String(e);
}
async function postJSON(path, body) {
  const res = await fetch(path, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body || {}) });
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new APIError(res.status, data.error || res.statusText);
  return data;
}
$('#login-passkey').addEventListener('click', async () => {
  try {
    const cr = await postJSON('/api/v1/auth/passkey/begin');
    const cred = await navigator.credentials.get(requestOptions(cr.options));
    const lr = await postJSON('/api/v1/auth/passkey/finish', { ceremony: cr.ceremony, credential: credentialJSON(cred) });
    signedIn(lr.token);
  } catch (e) { showLogin(e instanceof APIError ? e.message : passkeyError(e)); }
});
$('#login-2fa-passkey').addEventListener('click', async () => {
  try {
    const cr = await postJSON('/api/v1/auth/2fa/passkey/begin', { challenge });
    const cred = await navigator.credentials.get(requestOptions(cr.options));
    const lr = await postJSON('/api/v1/auth/2fa/passkey/finish', { challenge, ceremony: cr.ceremony, credential: credentialJSON(cred) });
    signedIn(lr.token);
  } catch (e) {
    if (e instanceof APIError && e.message.includes('expired')) { challenge = ''; challengeMethods = []; }
    showLogin(e instanceof APIError ? e.message : passkeyError(e));
  }
});

// Passkeys on My account.
function passkeysCard() {
  const card = h('div', { class: 'card' });
  const nameIn = h('input', { placeholder: 'e.g. MacBook, iPhone, YubiKey', 'aria-label': 'Passkey name', maxlength: '64' });
  const list = h('div', {});
  const note = h('div', {});
  async function refresh() {
    const ps = await api('GET', '/api/v1/me/passkeys');
    replace(list, ps.length ? h('ul', { class: 'keylist' }, ps.map(p => h('li', {},
      h('div', {}, h('strong', {}, p.name), h('div', { class: 'muted' }, (p.synced ? 'Synced passkey' : 'Device-bound key') + ' · added ' + ago(p.created_at) +
        (p.last_used_at ? ' · used ' + ago(p.last_used_at) : ' · not used yet'))),
      h('button', { class: 'btn small danger', onclick: async () => {
        if (!confirm(`Remove the passkey "${p.name}"? You won't be able to sign in with it any more.`)) return;
        if (await guard(() => api('DELETE', '/api/v1/me/passkeys/' + p.id), 'Passkey removed') === undefined) return;
        me = null; await guard(loadMe); refresh();
      } }, 'Remove')))) : h('p', { class: 'muted' }, 'No passkeys yet.'));
  }
  const add = async e => {
    e.preventDefault();
    replace(note);
    try {
      const cr = await postJSONAuth('/api/v1/me/passkeys/register/begin');
      const cred = await navigator.credentials.create(creationOptions(cr.options));
      await postJSONAuth('/api/v1/me/passkeys/register/finish', { ceremony: cr.ceremony, name: nameIn.value.trim(), credential: credentialJSON(cred) });
      nameIn.value = '';
      toast('Passkey added');
      me = null; await guard(loadMe);
      if (location.hash === '#/account') route(); else refresh();
    } catch (err) {
      if (err instanceof APIError && err.status === 403 && /sign in again/.test(err.message)) {
        replace(note, h('div', { class: 'callout mt' }, err.message + ' ',
          h('button', { class: 'btn small', type: 'button', onclick: () => $('#logout').click() }, 'Sign out and back in')));
      } else replace(note, h('p', { class: 'error' }, err instanceof APIError ? err.message : passkeyError(err)));
    }
  };
  append(card, h('h2', {}, 'Passkeys'),
    h('p', { class: 'muted' }, 'Sign in with your fingerprint, face or device PIN instead of a password, or use a passkey or security key as your second step after the password. Passkeys can\'t be phished: they only work on this site.'),
    list,
    passkeysSupported()
      ? h('form', { class: 'inline mt', onsubmit: add }, nameIn, h('button', { class: 'btn', type: 'submit' }, 'Add a passkey'))
      : h('p', { class: 'muted' }, 'This browser doesn\'t support passkeys.'),
    note);
  guard(refresh);
  return card;
}
// postJSONAuth is postJSON with the session token (for account changes).
async function postJSONAuth(path, body) {
  const res = await fetch(path, { method: 'POST', headers: authHeaders({ 'Content-Type': 'application/json' }), body: JSON.stringify(body || {}) });
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new APIError(res.status, data.error || res.statusText);
  return data;
}

// Single sign-on comes back to #/sso?ticket=… (or ?error=…).
async function finishSSO() {
  const q = new URLSearchParams(location.hash.replace(/^#\/sso\??/, ''));
  history.replaceState(null, '', location.pathname + '#/');
  if (q.get('error')) return showLogin(q.get('error'));
  const res = await fetch('/api/v1/auth/sso/exchange', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ ticket: q.get('ticket') || '' }) }).catch(() => null);
  if (!res || !res.ok) return showLogin('Single sign-on did not complete. Try again.');
  signedIn((await res.json()).token);
}

$('#logout').addEventListener('click', () => {
  challenge = '';
  // Sessions are ended server-side too; tokens just stop being remembered here.
  if (token.startsWith('st_')) fetch('/api/v1/auth/logout', { method: 'POST', headers: { Authorization: 'Bearer ' + token } }).catch(() => {});
  token = '';
  me = null;
  groupings = null;
  store.del('armada.token');
  showLogin();
});
