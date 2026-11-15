// Armada dashboard: Enrollment tokens and pre-registration.
'use strict';

async function viewEnroll(root) {
  const list = h('div', { class: 'card flush' });
  const created = h('div', {});
  const descIn = h('input', { placeholder: 'e.g. Singapore kiosks', 'aria-label': 'Description' });
  const chanIn = h('input', { value: 'stable', 'aria-label': 'Channel' });
  const labelsIn = h('input', { placeholder: 'site=sg role=kiosk', 'aria-label': 'Labels' });
  const usesIn = h('input', { type: 'number', min: '0', value: '0', 'aria-label': 'Max uses' });
  const ttlIn = h('select', { 'aria-label': 'Expiry' },
    [['0', 'Never'], ['3600', '1 hour'], ['86400', '1 day'], ['604800', '7 days'], ['2592000', '30 days']].map(([v, l]) => h('option', { value: v }, l)));
  ttlIn.value = '604800';
  const spareIn = h('input', { type: 'checkbox' });
  const fleetOperator = (ROLE_RANK[me.fleet_role] || 0) >= ROLE_RANK.operator;

  append(root, pageHead('Enrollment', 'Tokens that let new devices join the fleet'),
    h('div', { class: 'card' }, h('h2', {}, 'Create enrollment token'),
      h('form', { class: 'stack', onsubmit: async e => {
        e.preventDefault();
        let labels;
        try { labels = parseLabels(labelsIn.value); } catch (err) { return toast(err.message, true); }
        const t = await guard(() => api('POST', '/api/v1/tokens', {
          description: descIn.value, channel: chanIn.value.trim(), labels,
          max_uses: parseInt(usesIn.value || '0', 10), ttl_seconds: parseInt(ttlIn.value, 10),
          lifecycle: spareIn.checked ? 'spare' : 'active',
        }));
        if (!t) return;
        const origin = location.origin;
        replace(created, h('div', { class: 'callout stack' },
          h('div', {}, h('strong', {}, 'Copy this token now'), ' — it will not be shown again.'),
          t.lifecycle === 'spare' ? h('div', {}, 'Devices enrolled with it join as ', h('strong', {}, 'spares'), ': they get updates but no alerts until you put them in service or use them as a replacement.') : null,
          h('div', { class: 'secret' }, t.token),
          h('div', {}, 'Run on a device — Linux or macOS, in a terminal:'),
          h('div', { class: 'secret' }, `curl -fsSL ${origin}/install.sh | sudo ARMADA_SERVER_URL=${origin} ARMADA_ENROLLMENT_TOKEN=${t.token} sh`),
          h('div', {}, 'Windows, in PowerShell run as Administrator:'),
          h('div', { class: 'secret' }, `$env:ARMADA_SERVER_URL='${origin}'; $env:ARMADA_ENROLLMENT_TOKEN='${t.token}'; irm ${origin}/install.ps1 | iex`),
          h('div', { class: 'muted' }, 'or run the agent directly:'),
          h('div', { class: 'secret' }, `armada-agent --server ${origin} --token ${t.token}`),
          androidQR(t.token)));
        refresh();
      } },
      h('div', { class: 'form-grid' },
        h('label', {}, 'Description', descIn), h('label', {}, 'Channel', chanIn), h('label', {}, 'Labels for new devices', labelsIn),
        h('label', {}, 'Max uses (0 = unlimited)', usesIn), h('label', {}, 'Expires', ttlIn)),
      fleetOperator ? h('label', { class: 'check' }, spareIn, 'Enroll as spares (stock for replacements: updates, no alerts)') : null,
      h('div', {}, h('button', { class: 'btn primary', type: 'submit' }, 'Create token'))),
      h('div', { class: 'mt' }, created)),
    h('div', { class: 'spacer' }), list);
  const regs = registrationsCard();
  append(root, h('div', { class: 'spacer' }), regs.el);

  if ((ROLE_RANK[me.fleet_role] || 0) < ROLE_RANK.operator) {
    list.classList.add('hidden');
    created.before(h('p', { class: 'muted' }, 'Devices enrolled with these tokens are added to your account automatically.'));
    await guard(regs.refresh);
    return regs.refresh;
  }
  async function refresh() {
    await regs.refresh();
    const toks = await api('GET', '/api/v1/tokens');
    const now = Date.now();
    replace(list, table(['Token', 'State', 'Enrolls as', 'Channel', 'Labels', 'Uses', 'Expires', ''],
      toks.map(t => {
        const state = t.revoked ? 'revoked' : t.expires_at && new Date(t.expires_at) < now ? 'expired' : t.max_uses && t.uses >= t.max_uses ? 'used up' : 'active';
        return h('tr', {},
          h('td', {}, h('div', {}, t.description || h('span', { class: 'muted' }, 'untitled')), h('div', { class: 'muted mono' }, t.id)),
          h('td', {}, h('span', { class: 'badge ' + (state === 'active' ? 'ok' : '') }, state)),
          h('td', {}, t.lifecycle === 'spare' ? lifecycleBadge(t) : 'in service'),
          h('td', {}, t.channel),
          h('td', {}, labelChips(t.labels)),
          h('td', { class: 'num' }, t.uses + (t.max_uses ? ' / ' + t.max_uses : '')),
          h('td', { class: 'nowrap' }, t.expires_at ? new Date(t.expires_at).toLocaleDateString() : 'never'),
          h('td', {}, state === 'active' ? h('button', { class: 'btn small danger', onclick: async () => {
            if (await guard(() => api('DELETE', '/api/v1/tokens/' + t.id), 'Token revoked') !== undefined) refresh();
          } }, 'Revoke') : null));
      }), 'No enrollment tokens yet.'));
  }
  await guard(refresh);
  return refresh;
}

// registrationsCard: import a CSV that updates enrolled devices and
// pre-registers the rest by serial number, and list what's waiting.
const IMPORT_TEMPLATE = 'serial,name,channel,labels,asset_tag,location,contact,notes\n' +
  'SN-0001,orchard-till-1,stable,site=sg; role=till,ACME-00417,"Orchard store, till 1","Mei, +65 5550 1234",\n';
function registrationsCard() {
  const fileIn = h('input', { type: 'file', accept: '.csv,text/csv', 'aria-label': 'CSV file' });
  const text = h('textarea', { rows: 5, class: 'mono', placeholder: IMPORT_TEMPLATE, 'aria-label': 'CSV' });
  const report = h('div', {});
  const list = h('div', {});
  const applyBtn = h('button', { class: 'btn primary hidden', type: 'button' }, 'Apply');
  fileIn.addEventListener('change', async () => {
    const f = fileIn.files[0];
    if (f) { text.value = await f.text(); replace(report); applyBtn.classList.add('hidden'); }
  });
  text.addEventListener('input', () => { replace(report); applyBtn.classList.add('hidden'); });
  const send = dry => api('POST', '/api/v1/registrations/import' + (dry ? '?dry_run=1' : ''), new Blob([text.value], { type: 'text/csv' }));
  const ACTION = { updated: ['ok', 'updates device'], registered: ['info', 'pre-registers'], unchanged: ['', 'no change'], error: ['bad', 'error'] };
  const showReport = res => {
    const sum = [res.updated && `${res.updated} device${res.updated === 1 ? '' : 's'} updated`, res.registered && `${res.registered} serial${res.registered === 1 ? '' : 's'} pre-registered`,
      res.errors && `${res.errors} error${res.errors === 1 ? '' : 's'}`].filter(Boolean).join(' · ') || 'nothing to change';
    replace(report, h('div', { class: res.errors ? 'callout bad-callout stack' : 'callout stack' },
      h('div', {}, h('strong', {}, res.applied ? 'Imported: ' : res.errors ? 'Nothing was changed: fix these rows first. ' : 'Preview: '), sum),
      table(['Line', 'Device', 'Result', 'Changes'], res.rows.map(r => h('tr', {},
        h('td', { class: 'num' }, r.line),
        h('td', {}, r.device_id ? h('a', { href: '#/devices/' + r.device_id }, r.device) : h('span', { class: 'mono' }, r.serial || '—')),
        h('td', {}, h('span', { class: 'badge ' + ACTION[r.action][0] }, ACTION[r.action][1])),
        h('td', { class: r.error ? 'error' : 'muted' }, r.error || (r.changes || []).join(', ') || '—'))))));
    applyBtn.classList.toggle('hidden', res.applied || !!res.errors || !(res.updated + res.registered));
  };
  applyBtn.onclick = async () => {
    const res = await guard(() => send(false));
    if (res) { showReport(res); if (res.applied) { text.value = ''; fileIn.value = ''; refresh(); } }
  };
  const el = h('div', { class: 'card stack' },
    h('h2', {}, 'Pre-register and import devices'),
    h('p', { class: 'muted' }, 'A spreadsheet with one row per device, by serial number (or a ', h('code', {}, 'device'), ' column with its ID or name). ',
      'Rows for enrolled devices update them now. Other serials wait: a device that enrolls with that serial gets its name, channel, labels and inventory. ',
      'The enrollment token\'s labels still win. Empty cells change nothing. Labels go in one ', h('code', {}, 'labels'), ' column (', h('code', {}, 'site=sg; role=till'),
      ') or in columns named ', h('code', {}, 'label:site'), '.'),
    h('div', { class: 'btn-row' }, fileIn, h('button', { class: 'btn ghost small', type: 'button', onclick: () => saveText('armada-import-template.csv', IMPORT_TEMPLATE, 'text/csv') }, 'Download template')),
    text,
    h('div', { class: 'btn-row' }, h('button', { class: 'btn', type: 'button', onclick: async () => {
      if (!text.value.trim()) return toast('Choose a CSV file or paste one', true);
      const res = await guard(() => send(true));
      if (res) showReport(res);
    } }, 'Preview'), applyBtn),
    report,
    h('h2', { class: 'mt' }, 'Pre-registered serials'), list);
  async function refresh() {
    const rs = await api('GET', '/api/v1/registrations');
    replace(list, table(['Serial', 'Name', 'Channel', 'Labels', 'Inventory', 'Status', ''], rs.map(r => h('tr', {},
      h('td', { class: 'mono' }, r.serial),
      h('td', {}, r.name || h('span', { class: 'muted' }, '—')),
      h('td', {}, r.channel || h('span', { class: 'muted' }, 'from token')),
      h('td', {}, labelChips(r.labels)),
      h('td', { class: 'muted' }, r.inventory ? [r.inventory.asset_tag, r.inventory.location].filter(Boolean).join(' · ') : '—'),
      h('td', {}, r.claimed_by ? ['used by ', h('a', { href: '#/devices/' + r.claimed_by }, 'device'), ' ', h('span', { class: 'muted' }, ago(r.claimed_at))]
        : h('span', { class: 'badge info' }, 'waiting')),
      h('td', {}, h('button', { class: 'btn small ghost', onclick: async () => {
        if (await guard(() => api('DELETE', '/api/v1/registrations/' + encodeURIComponent(r.serial)), 'Removed') !== undefined) refresh();
      } }, 'Remove')))), 'No serials are pre-registered.'));
  }
  return { el, refresh };
}
