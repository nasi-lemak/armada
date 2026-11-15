// Armada dashboard: Certificates on devices.
'use strict';

// ---------- certificates ----------

const RENEWAL_TEXT = { requested: 'making CSR', csr_ready: 'waiting for signed certificate', installing: 'installing', installed: 'installed', failed: 'failed', cancelled: 'cancelled' };
function expiryBadge(c) {
  if (c.days_left === undefined || c.days_left === null) return h('span', { class: 'badge bad' }, h('span', { class: 'dot bad' }), 'unreadable');
  const d = c.days_left;
  const tone = d <= 7 ? 'bad' : d <= 30 ? 'warn' : 'ok';
  return h('span', { class: 'badge ' + tone }, h('span', { class: 'dot ' + tone }), d < 0 ? `expired ${-d}d ago` : `${d} days`);
}
function renewalBadge(r) {
  if (!r) return null;
  const b = badge(r.status);
  b.lastChild.textContent = RENEWAL_TEXT[r.status] || r.status;
  return b;
}
function saveText(name, text, type) {
  const url = URL.createObjectURL(new Blob([text], { type: type || 'application/x-pem-file' }));
  const a = h('a', { href: url, download: name.replace(/[^\w.-]+/g, '-') });
  document.body.append(a); a.click(); a.remove();
  setTimeout(() => URL.revokeObjectURL(url), 10000);
}

async function viewCertificates(root) {
  const search = h('input', { type: 'search', placeholder: 'Search device, service, name, issuer…', 'aria-label': 'Search certificates' });
  const filter = h('select', { 'aria-label': 'Certificate filter' }, [['', 'All certificates'], ['30', 'Expiring within 30 days'],
    ['csr_ready', 'Waiting for a signed certificate'], ['active', 'Renewal in progress'], ['failed', 'Renewal failed']].map(([v, l]) => h('option', { value: v }, l)));
  const list = h('div', { class: 'card flush' });
  const count = h('span', {});
  const selected = new Set();
  let certsList = [];

  // Bulk: request CSRs, download or email the waiting ones.
  const keyType = h('select', { 'aria-label': 'Key type' }, [['', 'Key: same as now'], ['ecdsa-p256', 'ECDSA P-256'], ['ecdsa-p384', 'ECDSA P-384'],
    ['rsa-2048', 'RSA 2048'], ['rsa-3072', 'RSA 3072'], ['rsa-4096', 'RSA 4096']].map(([v, l]) => h('option', { value: v }, l)));
  const cnIn = h('input', { placeholder: 'CN: same as now (or e.g. {{.Name}}.acme.example)', 'aria-label': 'Common name', size: 34 });
  const dnsIn = h('input', { placeholder: 'DNS names: same as now', 'aria-label': 'DNS names', size: 26 });
  const label = h('strong', {});
  const chosen = () => certsList.filter(c => selected.has(c.id));
  const byTarget = cs => cs.reduce((m, c) => ((m[c.target] = m[c.target] || []).push(c.device_id), m), {});
  const requestCSRs = async () => {
    const cs = chosen();
    const busy = cs.filter(c => c.renewal && ['requested', 'csr_ready', 'installing'].includes(c.renewal.status));
    if (!confirm(`Make a new key and CSR on ${cs.length - busy.length} device(s)? The keys stay on the devices.` + (busy.length ? `\n\n${busy.length} already have a renewal in progress and are skipped.` : ''))) return;
    let started = 0; const skipped = [];
    for (const [target, ids] of Object.entries(byTarget(cs))) {
      const body = { selector: { device_ids: ids }, targets: [target], key_type: keyType.value,
        subject: { common_name: cnIn.value.trim() }, dns_names: dnsIn.value.split(/[\s,]+/).filter(Boolean) };
      const r = await guard(() => api('POST', '/api/v1/certificates/renew', body));
      if (!r) return;
      started += r.started.length; skipped.push(...r.skipped);
    }
    toast(`CSRs requested on ${started} device(s)` + (skipped.length ? `; ${skipped.length} skipped: ${skipped[0].reason}` : ''), !started);
    selected.clear(); refresh();
  };
  const waitingQuery = () => {
    const cs = chosen().filter(c => c.renewal && c.renewal.status === 'csr_ready');
    return { cs, q: cs.map(c => 'device_id=' + encodeURIComponent(c.device_id)).join('&') };
  };
  const downloadCSRs = async () => {
    const { cs, q } = selected.size ? waitingQuery() : { cs: certsList.filter(c => c.renewal && c.renewal.status === 'csr_ready'), q: '' };
    if (!cs.length) return toast('None of these are waiting for a signed certificate (request CSRs first).', true);
    await guard(async () => {
      const res = await fetch('/api/v1/certificates/csrs' + (q ? '?' + q : ''), { headers: authHeaders() });
      if (!res.ok) throw new APIError(res.status, (await res.json().catch(() => ({}))).error || 'download failed');
      const url = URL.createObjectURL(await res.blob());
      const a = h('a', { href: url, download: `csrs-${new Date().toISOString().slice(0, 10)}.zip` });
      document.body.append(a); a.click(); a.remove();
      setTimeout(() => URL.revokeObjectURL(url), 10000);
    });
  };
  const emailCSRs = async () => {
    const { cs } = selected.size ? waitingQuery() : { cs: certsList.filter(c => c.renewal && c.renewal.status === 'csr_ready') };
    if (!cs.length) return toast('None of these are waiting for a signed certificate (request CSRs first).', true);
    const to = prompt(`Email ${cs.length} CSR(s) to whoever signs them (comma-separated addresses):`);
    if (!to) return;
    const note = prompt('A note to include (optional), e.g. your ticket number:', '') || '';
    const ids = [...new Set(cs.map(c => c.device_id))];
    if (await guard(() => api('POST', '/api/v1/certificates/email', { selector: { device_ids: ids }, recipients: to.split(/[\s,;]+/).filter(Boolean), note }), 'CSRs emailed')) refresh();
  };
  const bar = h('div', { class: 'bulk-bar need-operator hidden' }, label, keyType, cnIn, dnsIn,
    h('button', { class: 'btn primary', onclick: requestCSRs }, 'Request CSRs'),
    h('button', { class: 'btn', onclick: () => { selected.clear(); render(); } }, 'Clear selection'));

  const policyBox = certPolicies(() => refresh());

  // Upload signed certificates: paste or files, any number, any order.
  const pemIn = h('textarea', { placeholder: '-----BEGIN CERTIFICATE-----\n… paste certificates here (with their chain if you have it), or choose files', 'aria-label': 'Signed certificates', rows: 4 });
  const filesIn = h('input', { type: 'file', multiple: true, accept: '.pem,.crt,.cer,.txt', 'aria-label': 'Certificate files' });
  const uploadOut = h('div', {});
  const upload = async e => {
    e.preventDefault();
    let text = pemIn.value;
    for (const f of filesIn.files) text += '\n' + await f.text();
    if (!text.trim()) return toast('Paste certificates or choose files first.', true);
    const r = await guard(() => api('POST', '/api/v1/certificates/upload', { pem: text }));
    if (!r) return;
    pemIn.value = ''; filesIn.value = '';
    replace(uploadOut, h('div', { class: 'callout stack mt' },
      h('div', {}, h('strong', {}, `${r.installing.length} certificate(s) sent to devices to install`), r.installing.length ? ': ' + r.installing.map(c => `${c.device_name} · ${c.target}`).join(', ') : ''),
      r.rejected.length ? h('div', {}, h('strong', {}, `${r.rejected.length} not used:`), h('ul', {}, r.rejected.map(x => h('li', {}, `${x.subject}${x.serial ? ' (serial ' + x.serial + ')' : ''}: ${x.reason}`)))) : null));
    refresh();
  };
  append(root, pageHead('Certificates', count),
    h('div', { class: 'card mb need-operator' }, h('h2', {}, 'Upload signed certificates'),
      h('p', { class: 'muted' }, 'Each certificate is matched to the device whose CSR it was issued for, checked, and installed there; the service is reloaded and the old certificate restored if anything fails.'),
      h('form', { class: 'stack', onsubmit: upload }, pemIn, h('div', { class: 'inline' }, filesIn, h('button', { class: 'btn primary', type: 'submit' }, 'Upload and install'))),
      uploadOut),
    h('div', { class: 'toolbar' }, search, filter,
      h('button', { class: 'btn need-operator', onclick: downloadCSRs, title: 'The selected waiting CSRs, or all of them' }, 'Download CSRs'),
      h('button', { class: 'btn need-operator', onclick: emailCSRs, title: 'Email the selected waiting CSRs, or all of them' }, 'Email CSRs…')),
    bar, h('div', { class: 'spacer' }), list, policyBox.el);

  function render() {
    const q = search.value.trim().toLowerCase(), f = filter.value;
    const shown = certsList.filter(c => {
      const r = c.renewal;
      if (f === '30' && !(c.days_left === undefined || c.days_left === null || c.days_left <= 30)) return false;
      if (f === 'csr_ready' && !(r && r.status === 'csr_ready')) return false;
      if (f === 'active' && !(r && ['requested', 'csr_ready', 'installing'].includes(r.status))) return false;
      if (f === 'failed' && !(r && r.status === 'failed')) return false;
      const i = c.installed || {};
      return !q || [c.device_name, c.target, i.common_name, i.issuer, ...(i.dns_names || [])].some(v => (v || '').toLowerCase().includes(q));
    });
    for (const id of [...selected]) if (!certsList.some(c => c.id === id)) selected.delete(id);
    bar.classList.toggle('hidden', !selected.size);
    replace(label, `${selected.size} selected`);
    const soon = certsList.filter(c => c.days_left === undefined || c.days_left === null || c.days_left <= 30).length;
    const waiting = certsList.filter(c => c.renewal && c.renewal.status === 'csr_ready').length;
    replace(count, `${certsList.length} managed certificates · ${soon} expiring within 30 days · ${waiting} waiting for a signed certificate`);
    const allBox = h('input', { type: 'checkbox', 'aria-label': 'Select all shown', checked: shown.length > 0 && shown.every(c => selected.has(c.id)),
      onclick: e => { for (const c of shown) e.target.checked ? selected.add(c.id) : selected.delete(c.id); render(); } });
    replace(list, table([h('span', { class: 'need-operator' }, allBox), 'Device', 'Service', 'Certificate', 'Expires', 'Renewal', ''],
      shown.map(c => {
        const i = c.installed, r = c.renewal;
        return h('tr', { class: selected.has(c.id) ? 'selected' : '' },
          h('td', { class: 'need-operator pick' }, h('input', { type: 'checkbox', 'aria-label': 'Select ' + c.device_name + ' ' + c.target, checked: selected.has(c.id),
            onchange: e => { e.target.checked ? selected.add(c.id) : selected.delete(c.id); render(); } })),
          h('td', {}, h('a', { href: '#/devices/' + c.device_id }, c.device_name)),
          h('td', { class: 'mono' }, c.target),
          h('td', {}, i ? [h('div', {}, i.common_name || i.subject), h('div', { class: 'muted' }, [...(i.dns_names || []), ...(i.ip_addresses || [])].join(', ')),
            h('div', { class: 'muted' }, `${i.issuer} · ${i.key_type}`), i.key_matches ? null : h('div', { class: 'bad' }, 'the key file does not match this certificate')]
            : h('span', { class: 'bad' }, c.error || 'no certificate')),
          h('td', { class: 'nowrap' }, expiryBadge(c), i ? h('div', { class: 'muted' }, new Date(i.not_after).toLocaleDateString()) : null),
          h('td', {}, r ? [renewalBadge(r),
            h('div', { class: 'muted' }, r.policy_name ? `automatic (${r.policy_name})` : r.requested_by || '', ' · ', ago(r.updated_at)),
            r.status === 'csr_ready' && r.emailed_at ? h('div', { class: 'muted' }, `CSR emailed ${ago(r.emailed_at)} to ${(r.emailed_to || []).join(', ')}${r.reminded_at ? ' · reminded ' + ago(r.reminded_at) : ''}`) : null,
            r.message ? h('div', { class: 'bad', title: r.message }, r.message.length > 120 ? r.message.slice(0, 120) + '…' : r.message) : null] : h('span', { class: 'muted' }, '—')),
          h('td', {}, h('div', { class: 'btn-row' },
            r && r.status === 'csr_ready' ? h('button', { class: 'btn small', onclick: () => saveText(`${c.device_name}_${c.target}.csr`, r.csr) }, 'CSR') : null,
            r && ['requested', 'csr_ready'].includes(r.status) ? h('button', { class: 'btn small need-operator', onclick: async () => {
              if (confirm(`Cancel the ${c.target} renewal on ${c.device_name}?`) && await guard(() => api('POST', `/api/v1/certificates/${c.device_id}/${c.target}/cancel`), 'Cancelled')) refresh();
            } }, 'Cancel') : null)));
      }), 'No managed certificates yet. List them in the agent\'s managed_certs on each device (see Certificates on devices in the documentation).'));
  }
  search.addEventListener('input', render);
  filter.addEventListener('change', render);
  async function refresh() { certsList = await api('GET', '/api/v1/certificates'); render(); await policyBox.load(); }
  await guard(refresh);
  return async () => { if (!root.contains(document.activeElement) || document.activeElement === document.body) await refresh(); };
}

// certPolicies is the automatic-renewal section of the Certificates page.
function certPolicies(onRun) {
  const isAdmin = !!me && me.fleet_role === 'admin';
  const opScopes = ((me && me.grants) || []).filter(g => (ROLE_RANK[g.role] || 0) >= ROLE_RANK.operator && scopeText(g.scope)).map(g => g.scope);
  const canManage = isAdmin || opScopes.length > 0;
  const canEdit = sc => isAdmin || (scopeText(sc) !== '' && opScopes.some(c => scopeText(c) === scopeText(sc)));
  const listBox = h('div', { class: 'card flush' });
  const pFor = isAdmin
    ? h('input', { placeholder: 'customer=acme (empty = every client)', 'aria-label': 'Policy for' })
    : h('select', { 'aria-label': 'Policy for' }, opScopes.map((sc, i) => h('option', { value: String(i) }, scopeText(sc))));
  const pName = h('input', { placeholder: 'optional', 'aria-label': 'Policy name' });
  const pDays = h('input', { type: 'number', min: '1', max: '365', value: '30', 'aria-label': 'Days before expiry' });
  const pTargets = h('input', { placeholder: 'all, or mosquitto nginx', 'aria-label': 'Certificates' });
  const pLabels = h('input', { placeholder: 'all devices, or site=sg', 'aria-label': 'Devices (labels)' });
  const pKey = h('select', { 'aria-label': 'New key type' }, [['', 'Same as now'], ['ecdsa-p256', 'ECDSA P-256'], ['ecdsa-p384', 'ECDSA P-384'],
    ['rsa-2048', 'RSA 2048'], ['rsa-3072', 'RSA 3072'], ['rsa-4096', 'RSA 4096']].map(([v, l]) => h('option', { value: v }, l)));
  const pTo = h('input', { placeholder: 'pki@customer.com (empty: collect CSRs here)', 'aria-label': 'Email CSRs to' });
  const pRemind = h('input', { type: 'number', min: '0', max: '90', value: '7', 'aria-label': 'Remind every (days)' });
  const pNote = h('input', { placeholder: 'optional, e.g. contract or ticket number', 'aria-label': 'Note in emails' });
  const form = h('div', { class: 'card mb' + (canManage ? '' : ' hidden') }, h('form', { class: 'stack', onsubmit: async e => {
    e.preventDefault();
    let scope, labels;
    try {
      scope = isAdmin ? { labels: parseLabels(pFor.value) } : opScopes[parseInt(pFor.value || '0', 10)];
      labels = parseLabels(pLabels.value);
    } catch (err) { return toast(err.message, true); }
    const body = { name: pName.value.trim(), client_scope: scope, selector: { labels }, targets: pTargets.value.split(/[\s,]+/).filter(Boolean),
      renew_days: parseInt(pDays.value || '30', 10), key_type: pKey.value, recipients: pTo.value.split(/[\s,;]+/).filter(Boolean),
      reminder_days: parseInt(pRemind.value || '0', 10), note: pNote.value.trim() };
    if (await guard(() => api('POST', '/api/v1/cert-policies', body), 'Automatic renewal set up')) { pName.value = ''; pTo.value = ''; pNote.value = ''; load(); }
  } },
  h('div', { class: 'form-grid' }, h('label', { class: isAdmin || opScopes.length > 1 ? '' : 'hidden' }, 'For', pFor), h('label', {}, 'Name', pName),
    h('label', {}, 'Start this many days before expiry', pDays), h('label', {}, 'Certificates', pTargets), h('label', {}, 'Devices', pLabels),
    h('label', {}, 'New key', pKey), h('label', { class: 'wide' }, 'Email CSRs to', pTo), h('label', {}, 'Remind every (days, 0 = never)', pRemind),
    h('label', { class: 'wide' }, 'Note in emails', pNote)),
  h('div', {}, h('button', { class: 'btn primary', type: 'submit' }, 'Set up automatic renewal'))));
  const el = h('div', {}, h('div', { class: 'page-head mt' }, h('div', {}, h('h2', {}, 'Automatic renewal'),
    h('div', { class: 'muted' }, 'Starts renewals before certificates expire, emails the CSRs to whoever signs them, and reminds them until the certificates come back. Returned certificates install as usual.'))),
    form, listBox);
  async function load() {
    const pols = await api('GET', '/api/v1/cert-policies');
    replace(listBox, table(['Policy', 'For', 'When', 'CSRs go to', 'Last checked', ''], pols.map(p => h('tr', {},
      h('td', {}, h('strong', {}, p.name), ' ', badge(p.enabled ? 'enabled' : 'disabled'), p.created_by ? h('div', { class: 'muted' }, 'by ' + p.created_by) : null),
      h('td', {}, scopeText(p.client_scope) || h('span', { class: 'muted' }, 'every client'),
        scopeText(p.selector) ? h('div', { class: 'muted' }, scopeText(p.selector)) : null,
        (p.targets || []).length ? h('div', { class: 'muted mono' }, p.targets.join(', ')) : null),
      h('td', {}, `${p.renew_days} days before expiry`, p.key_type ? h('div', { class: 'muted' }, 'new key: ' + p.key_type) : null),
      h('td', {}, (p.recipients || []).length ? [p.recipients.join(', '), h('div', { class: 'muted' }, p.reminder_days ? `reminder every ${p.reminder_days} days` : 'no reminders')]
        : h('span', { class: 'muted' }, 'collected here')),
      h('td', { class: 'nowrap' }, p.last_run_at ? ago(p.last_run_at) : h('span', { class: 'muted' }, 'not yet')),
      h('td', {}, !canEdit(p.client_scope) ? null : h('div', { class: 'btn-row' },
        h('button', { class: 'btn small', onclick: async () => {
          const r = await guard(() => api('POST', `/api/v1/cert-policies/${p.id}/run`));
          if (r) { toast(`Checked: ${r.started} renewal(s) started, ${r.emailed} CSR(s) emailed, ${r.reminded} reminded`); onRun(); }
        } }, 'Check now'),
        h('button', { class: 'btn small', onclick: async () => {
          if (await guard(() => api('PUT', '/api/v1/cert-policies/' + p.id, { ...p, enabled: !p.enabled }))) load();
        } }, p.enabled ? 'Disable' : 'Enable'),
        h('button', { class: 'btn small danger', onclick: async () => {
          if (confirm(`Delete ${p.name}? Renewals it started carry on.`) && await guard(() => api('DELETE', '/api/v1/cert-policies/' + p.id)) !== undefined) load();
        } }, 'Delete'))))),
    'No automatic renewal set up. Certificates are renewed when someone requests CSRs.'));
  }
  return { el, load };
}
