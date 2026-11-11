// Armada dashboard. Plain JS, no build step. All data is rendered through
// DOM APIs (never innerHTML) so device-reported strings can't inject markup.
'use strict';

// ---------- storage & API ----------

const store = {
  get(k) { try { return localStorage.getItem(k); } catch { return null; } },
  set(k, v) { try { localStorage.setItem(k, v); } catch { /* private mode */ } },
  del(k) { try { localStorage.removeItem(k); } catch { /* ignore */ } },
};
let token = store.get('armada.token') || '';

class APIError extends Error {
  constructor(status, msg) { super(msg); this.status = status; }
}

async function api(method, path, body) {
  const opts = { method, headers: { Authorization: 'Bearer ' + token } };
  if (body instanceof FormData) {
    opts.body = body;
  } else if (body !== undefined) {
    opts.headers['Content-Type'] = 'application/json';
    opts.body = JSON.stringify(body);
  }
  const res = await fetch(path, opts);
  if (res.status === 401) { showLogin('Your session expired or the token is invalid.'); throw new APIError(401, 'unauthorized'); }
  if (!res.ok) {
    let msg = res.statusText;
    try { msg = (await res.json()).error || msg; } catch { /* not JSON */ }
    throw new APIError(res.status, msg);
  }
  if (res.status === 204) return null;
  return res.json();
}

// Upload with progress (fetch has no upload progress events).
function uploadForm(path, form, onProgress) {
  return new Promise((resolve, reject) => {
    const xhr = new XMLHttpRequest();
    xhr.open('POST', path);
    xhr.setRequestHeader('Authorization', 'Bearer ' + token);
    xhr.upload.onprogress = e => e.lengthComputable && onProgress(e.loaded / e.total);
    xhr.onload = () => {
      let data = null;
      try { data = JSON.parse(xhr.responseText); } catch { /* ignore */ }
      if (xhr.status >= 200 && xhr.status < 300) resolve(data);
      else reject(new APIError(xhr.status, (data && data.error) || xhr.statusText));
    };
    xhr.onerror = () => reject(new APIError(0, 'network error'));
    xhr.send(form);
  });
}

// Downloads go through fetch so the admin token stays in a header, not a URL.
async function downloadArtifact(r) {
  const res = await fetch(`/api/v1/releases/${r.id}/artifact`, { headers: { Authorization: 'Bearer ' + token } });
  if (!res.ok) throw new APIError(res.status, 'download failed (HTTP ' + res.status + ')');
  const url = URL.createObjectURL(await res.blob());
  const a = h('a', { href: url, download: `${r.package}-${r.version}` });
  document.body.append(a);
  a.click();
  a.remove();
  setTimeout(() => URL.revokeObjectURL(url), 10000);
}

// ---------- DOM helpers ----------

function h(tag, attrs, ...children) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v === undefined || v === null || v === false) continue;
    if (k === 'class') el.className = v;
    else if (k.startsWith('on')) el.addEventListener(k.slice(2), v);
    else if (k === 'dataset') Object.assign(el.dataset, v);
    else if (v === true) el.setAttribute(k, '');
    else el.setAttribute(k, v);
  }
  append(el, children);
  return el;
}
function append(el, ...children) {
  for (const c of children.flat(Infinity)) {
    if (c === null || c === undefined || c === false) continue;
    el.append(c instanceof Node ? c : document.createTextNode(String(c)));
  }
  return el;
}
function replace(el, ...children) { el.replaceChildren(); return append(el, children); }
const $ = sel => document.querySelector(sel);

function toast(msg, bad) {
  const t = $('#toast');
  t.textContent = msg;
  t.className = 'toast' + (bad ? ' bad' : '');
  clearTimeout(toast.timer);
  toast.timer = setTimeout(() => t.classList.add('hidden'), bad ? 6000 : 3000);
}

async function guard(fn, okMsg) {
  try {
    const r = await fn();
    if (okMsg) toast(okMsg);
    return r;
  } catch (e) {
    if (!(e instanceof APIError && e.status === 401)) toast(e.message || String(e), true);
    return undefined;
  }
}

// ---------- formatting ----------

function ago(ts) {
  if (!ts || ts.startsWith('0001')) return 'never';
  const s = Math.max(0, (Date.now() - new Date(ts).getTime()) / 1000);
  if (s < 60) return Math.floor(s) + 's ago';
  if (s < 3600) return Math.floor(s / 60) + 'm ago';
  if (s < 172800) return Math.floor(s / 3600) + 'h ago';
  return Math.floor(s / 86400) + 'd ago';
}
function fmtTime(ts) {
  const d = new Date(ts);
  if (d.toDateString() === new Date().toDateString()) {
    return d.toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit', second: '2-digit', hour12: false });
  }
  return d.toLocaleDateString(undefined, { month: 'short', day: 'numeric' });
}
function bytes(n) {
  if (!n) return '0 B';
  const u = ['B', 'KiB', 'MiB', 'GiB', 'TiB'];
  const i = Math.min(u.length - 1, Math.floor(Math.log(n) / Math.log(1024)));
  return (n / Math.pow(1024, i)).toFixed(i ? 1 : 0) + ' ' + u[i];
}
function duration(sec) {
  if (!sec) return '0s';
  const d = Math.floor(sec / 86400), hr = Math.floor(sec % 86400 / 3600), m = Math.floor(sec % 3600 / 60);
  return [d && d + 'd', hr && hr + 'h', m && m + 'm'].filter(Boolean).join(' ') || Math.floor(sec) + 's';
}
function sortedEntries(obj) { return Object.entries(obj || {}).sort(([a], [b]) => a.localeCompare(b)); }
function labelChips(labels, onRemove) {
  const e = sortedEntries(labels);
  if (!e.length) return h('span', { class: 'muted' }, '—');
  return h('div', { class: 'chips' }, e.map(([k, v]) =>
    h('span', { class: 'chip' }, k + '=' + v,
      onRemove && h('button', { title: 'Remove label', 'aria-label': 'Remove label ' + k, onclick: () => onRemove(k) }, '×'))));
}
function pkgChips(pkgs) {
  const e = sortedEntries(pkgs);
  if (!e.length) return h('span', { class: 'muted' }, '—');
  return h('div', { class: 'chips' }, e.map(([k, v]) => h('span', { class: 'chip' }, k + ' ' + v)));
}
function parseLabels(s) {
  const out = {};
  for (const part of s.split(/[,\s]+/).filter(Boolean)) {
    const i = part.indexOf('=');
    if (i <= 0) throw new Error('Labels must look like key=value');
    out[part.slice(0, i)] = part.slice(i + 1);
  }
  return out;
}

const STATUS_TONE = {
  online: 'ok', offline: 'bad',
  running: 'info', paused: 'warn', aborted: 'bad', completed: 'ok',
  succeeded: 'ok', failed: 'bad', rolled_back: 'warn', pending: '', downloading: 'info', installing: 'info',
  queued: '', sent: 'info', rejected: 'warn', revoked: 'bad', active: 'ok', attested: 'ok', 'attested (unlocked)': 'warn',
};
function badge(status) {
  const tone = STATUS_TONE[status] || '';
  return h('span', { class: 'badge ' + tone }, h('span', { class: 'dot ' + (tone === 'info' ? '' : tone) }), status.replace('_', ' '));
}

function table(headers, rows, emptyMsg) {
  rows = rows || [];
  if (!rows.length) return h('div', { class: 'empty' }, emptyMsg || 'Nothing here yet.');
  return h('div', { class: 'table-wrap' }, h('table', {},
    h('thead', {}, h('tr', {}, headers.map(x => h('th', {}, x)))),
    h('tbody', {}, rows)));
}

function progressBar(stats, total) {
  const s = stats || {};
  const denom = Math.max(total || 0, s.total || 0, 1);
  const seg = (n, cls) => {
    if (!n) return null;
    const el = h('span', { class: cls, title: n + '' });
    el.style.width = (100 * n / denom) + '%';
    return el;
  };
  return h('div', {},
    h('div', { class: 'bar' },
      seg(s.succeeded, 'ok'), seg(s.failed, 'bad'), seg(s.rolled_back, 'warn'), seg((s.in_progress || 0) + (s.pending || 0), 'info')),
    h('div', { class: 'bar-label' },
      h('span', {}, `${s.succeeded || 0} succeeded`),
      h('span', {}, `${(s.failed || 0) + (s.rolled_back || 0)} failed · ${(s.in_progress || 0) + (s.pending || 0)} in flight`)));
}
function stageStrip(d) {
  const steps = [];
  d.strategy.stages.forEach((p, i) => {
    if (i) steps.push(h('span', { class: 'sep' }));
    steps.push(h('span', { class: 'step' + (i <= d.stage_index ? ' done' : '') + (i === d.stage_index ? ' current' : '') }, p + '%'));
  });
  return h('div', { class: 'stages', title: 'Rollout stages (cumulative share of targeted devices)' }, h('span', { class: 'label' }, 'Stages'), steps);
}
function currentPercent(d) {
  const st = d.strategy.stages;
  return st.length ? st[Math.min(d.stage_index, st.length - 1)] : 100;
}

// ---------- routing ----------

let refreshTimer = null;
let currentRefresh = null;

const routes = [
  [/^#?\/?$/, 'overview', viewOverview],
  [/^#\/devices$/, 'devices', viewDevices],
  [/^#\/devices\/([\w-]+)$/, 'devices', viewDevice],
  [/^#\/deployments$/, 'deployments', viewDeployments],
  [/^#\/deployments\/([\w-]+)$/, 'deployments', viewDeployment],
  [/^#\/releases$/, 'releases', viewReleases],
  [/^#\/enroll$/, 'enroll', viewEnroll],
  [/^#\/apps$/, 'apps', viewApps],
  [/^#\/activity$/, 'activity', viewActivity],
];

async function route() {
  if (!token) return showLogin();
  clearInterval(refreshTimer);
  currentRefresh = null;
  const hash = location.hash || '#/';
  for (const [re, nav, view] of routes) {
    const m = hash.match(re);
    if (!m) continue;
    document.querySelectorAll('#nav a').forEach(a => a.classList.toggle('active', a.dataset.route === nav));
    const root = $('#view');
    root.replaceChildren();
    const refresh = await view(root, ...m.slice(1));
    if (typeof refresh === 'function') {
      currentRefresh = refresh;
      refreshTimer = setInterval(() => { if (!document.hidden) guard(refresh); }, 10000);
    }
    return;
  }
  location.hash = '#/';
}

function pageHead(title, sub, ...actions) {
  return h('div', { class: 'page-head' },
    h('div', {}, h('h1', {}, title), sub && h('div', { class: 'sub' }, sub)),
    actions.length ? h('div', { class: 'btn-row' }, actions) : null);
}

// ---------- views ----------

async function viewOverview(root) {
  const stats = h('div', { class: 'grid cols-4' });
  const deps = h('div', {});
  const dist = h('div', {});
  const feed = h('div', {});
  append(root,
    pageHead('Fleet overview'),
    stats,
    h('div', { class: 'grid side mt' },
      h('div', { class: 'stack' },
        h('div', { class: 'card' }, h('h2', {}, 'Active deployments'), deps),
        h('div', { class: 'card' }, h('h2', {}, 'Installed versions'), dist)),
      h('div', { class: 'card flush' }, h('h2', {}, 'Recent activity'), feed)));

  async function refresh() {
    const [sum, active, events] = await Promise.all([
      api('GET', '/api/v1/summary'),
      api('GET', '/api/v1/deployments?status=running,paused'),
      api('GET', '/api/v1/events?limit=15'),
    ]);
    const tile = (label, value, hint, href) => h(href ? 'a' : 'div', { class: 'card stat', href },
      h('div', { class: 'label' }, label), h('div', { class: 'value' }, value), h('div', { class: 'hint' }, hint));
    const pct = sum.devices ? Math.round(100 * sum.online / sum.devices) : 0;
    replace(stats,
      tile('Devices', sum.devices, Object.entries(sum.channels).map(([c, n]) => `${n} ${c}`).join(' · ') || 'none enrolled', '#/devices'),
      tile('Online', sum.online, sum.devices ? pct + '% of fleet' : '—', '#/devices'),
      tile('Offline', sum.offline, sum.offline ? 'needs attention' : 'all reporting', '#/devices'),
      tile('Active rollouts', sum.active_deployments, `${sum.releases} releases available`, '#/deployments'));

    const shown = active.slice(0, 5);
    replace(deps, active.length
      ? [shown.map(d => h('a', { href: '#/deployments/' + d.id, class: 'dep-row' },
        h('div', { class: 'page-head' },
          h('div', {}, h('strong', {}, d.name), ' ', h('span', { class: 'muted mono' }, d.package + ' ' + d.version)),
          badge(d.status)),
        progressBar(d.stats), stageStrip(d))),
        active.length > shown.length ? h('a', { href: '#/deployments', class: 'dep-row' }, `View all ${active.length} active deployments →`) : null]
      : h('div', { class: 'empty' }, 'No rollouts in progress. ', h('a', { href: '#/releases' }, 'Upload a release'), ' to start one.'));

    const pkgs = sortedEntries(sum.package_versions);
    replace(dist, pkgs.length ? pkgs.map(([pkg, versions]) => {
      const total = Object.values(versions).reduce((a, b) => a + b, 0);
      const vs = Object.entries(versions).sort((a, b) => b[1] - a[1]);
      return h('div', { class: 'dist' },
        h('div', { class: 'muted' }, pkg),
        vs.map(([v, n]) => {
          const fill = h('span', { class: 'info' });
          fill.style.width = (100 * n / total) + '%';
          return h('div', { class: 'dist-row' }, h('span', { class: 'name', title: v }, v || '(none)'), h('div', { class: 'bar' }, fill), h('span', { class: 'n' }, n));
        }));
    }) : h('div', { class: 'empty' }, 'No devices yet.'));

    replace(feed, eventFeed(events));
  }
  await guard(refresh);
  return refresh;
}

function eventFeed(events) {
  events = events || [];
  if (!events.length) return h('div', { class: 'empty' }, 'No activity yet.');
  return h('ul', { class: 'feed' }, events.map(e => h('li', {},
    h('time', { datetime: e.time, title: new Date(e.time).toLocaleString() }, fmtTime(e.time)),
    h('div', {}, h('div', {}, e.message), h('div', { class: 'type' }, e.type,
      e.device_id ? [' · ', h('a', { href: '#/devices/' + e.device_id }, 'device')] : null,
      e.deployment_id ? [' · ', h('a', { href: '#/deployments/' + e.deployment_id }, 'deployment')] : null)))));
}

async function viewDevices(root) {
  const search = h('input', { type: 'search', placeholder: 'Search name, ID, IP, label…', 'aria-label': 'Search devices' });
  const status = h('select', { 'aria-label': 'Status filter' }, h('option', { value: '' }, 'All statuses'), h('option', { value: 'online' }, 'Online'), h('option', { value: 'offline' }, 'Offline'));
  const list = h('div', { class: 'card flush' });
  const count = h('span', {});
  append(root, pageHead('Devices', count, h('a', { class: 'btn primary', href: '#/enroll' }, 'Enroll devices')),
    h('div', { class: 'toolbar' }, search, status), h('div', { class: 'spacer' }), list);
  let devices = [];
  function render() {
    const q = search.value.trim().toLowerCase();
    const shown = devices.filter(d => (!status.value || d.status === status.value) && (!q || [
      d.id, d.name, d.channel, d.info.hostname, ...(d.info.ips || []),
      ...Object.entries(d.labels).map(([k, v]) => k + '=' + v),
    ].some(s => (s || '').toLowerCase().includes(q))));
    replace(count, `${devices.length} enrolled · ${devices.filter(d => d.status === 'online').length} online`);
    replace(list, table(['Device', 'Status', 'Channel', 'Platform', 'Packages', 'Labels', 'Last seen'],
      shown.map(d => h('tr', { class: 'clickable', onclick: () => { location.hash = '#/devices/' + d.id; } },
        h('td', {}, h('div', {}, h('strong', {}, d.name)), h('div', { class: 'muted mono' }, d.id),
          d.agent_status ? h('div', { class: 'muted' }, d.agent_status) : null),
        h('td', {}, badge(d.status), d.license_revoked ? h('div', {}, badge('revoked')) : null),
        h('td', {}, d.channel),
        h('td', { class: 'nowrap' }, `${d.info.os || '?'}/${d.info.arch || '?'}`),
        h('td', {}, pkgChips(d.packages)),
        h('td', {}, labelChips(d.labels)),
        h('td', { class: 'nowrap', title: d.last_seen_at }, ago(d.last_seen_at)))),
      devices.length ? 'No devices match.' : 'No devices enrolled yet. Create an enrollment token to add one.'));
  }
  search.addEventListener('input', render);
  status.addEventListener('change', render);
  async function refresh() { devices = await api('GET', '/api/v1/devices'); render(); }
  await guard(refresh);
  search.focus();
  return refresh;
}

async function viewDevice(root, id) {
  const head = h('div', {});
  const info = h('div', { class: 'card' });
  const meta = h('div', { class: 'card' });
  const actions = h('div', { class: 'card' });
  const updates = h('div', { class: 'card flush' });
  const actionLog = h('div', { class: 'card flush' });
  const events = h('div', { class: 'card flush' });
  append(root, head,
    h('div', { class: 'grid side' },
      h('div', { class: 'stack' }, info, updates, actionLog),
      h('div', { class: 'stack' }, meta, actions, events)));
  let dev;

  // Settings form (rendered once so typing isn't clobbered by refresh).
  const nameIn = h('input', { 'aria-label': 'Name' });
  const chanIn = h('input', { 'aria-label': 'Channel' });
  const labelIn = h('input', { placeholder: 'key=value', 'aria-label': 'New label' });
  const labelBox = h('div', {});
  append(meta, h('h2', {}, 'Settings'),
    h('form', { class: 'stack', onsubmit: async e => {
      e.preventDefault();
      if (await guard(() => api('PATCH', '/api/v1/devices/' + id, { name: nameIn.value, channel: chanIn.value }), 'Saved')) refresh();
    } },
    h('div', { class: 'form-grid' }, h('label', {}, 'Name', nameIn), h('label', {}, 'Channel', chanIn)),
    h('button', { class: 'btn', type: 'submit' }, 'Save')),
    h('h2', { class: 'mt' }, 'Labels'), h('div', { class: 'mb' }, labelBox),
    h('form', { class: 'inline', onsubmit: async e => {
      e.preventDefault();
      let set;
      try { set = parseLabels(labelIn.value); } catch (err) { return toast(err.message, true); }
      if (await guard(() => api('PATCH', '/api/v1/devices/' + id, { set_labels: set }))) { labelIn.value = ''; refresh(); }
    } }, labelIn, h('button', { class: 'btn', type: 'submit' }, 'Add')));

  const licenseBox = h('div', {});
  const execIn = h('input', { placeholder: 'e.g. systemctl status myapp', 'aria-label': 'Command' });
  const queue = (type, args) => guard(() => api('POST', `/api/v1/devices/${id}/actions`, { type, args }), `${type} queued; runs at next check-in`).then(r => r && refresh());
  append(actions, h('h2', {}, 'Actions'),
    h('div', { class: 'btn-row' },
      h('button', { class: 'btn', onclick: () => confirm('Reboot this device?') && queue('reboot') }, 'Reboot'),
      h('button', { class: 'btn', onclick: () => queue('restart-agent') }, 'Restart agent')),
    h('form', { class: 'inline mt', onsubmit: e => {
      e.preventDefault();
      const argv = execIn.value.match(/"[^"]*"|'[^']*'|\S+/g);
      if (!argv) return;
      queue('exec', argv.map(a => a.replace(/^(["'])(.*)\1$/, '$2'))).then(() => { execIn.value = ''; });
    } }, execIn, h('button', { class: 'btn', type: 'submit' }, 'Run')),
    h('p', { class: 'muted' }, 'Commands run without a shell, only on devices with allow_exec enabled.'),
    h('hr', { class: 'sep' }),
    licenseBox,
    h('button', { class: 'btn danger', onclick: async () => {
      if (!confirm(`Remove ${dev ? dev.name : id} from the fleet? Its key stops working immediately.`)) return;
      if (await guard(() => api('DELETE', '/api/v1/devices/' + id), 'Device removed') !== undefined) location.hash = '#/devices';
    } }, 'Remove device'));

  async function refresh() {
    const det = await api('GET', '/api/v1/devices/' + id);
    dev = det.device;
    const d = dev, m = d.metrics;
    if (document.activeElement !== nameIn) nameIn.value = d.name;
    if (document.activeElement !== chanIn) chanIn.value = d.channel;
    const android = (d.info.os || '').toLowerCase() === 'android';
    const setRevoked = async revoked => {
      const msg = revoked
        ? `Revoke app licenses for ${d.name}? Your apps on it stop working when their current license expires.`
        : `Reinstate app licenses for ${d.name}?`;
      if (!confirm(msg)) return;
      if (await guard(() => api('PATCH', '/api/v1/devices/' + id, { license_revoked: revoked }), revoked ? 'Licenses revoked' : 'Licenses reinstated')) refresh();
    };
    replace(licenseBox, android || d.attestation ? h('div', { class: 'mb' },
      h('h2', {}, 'App licenses'),
      h('p', { class: 'muted' }, d.license_revoked
        ? 'Revoked: this device gets no new licenses, so your apps stop working on it.'
        : d.attestation ? 'Active: apps on this device can get licenses.' : 'This device has no attested hardware key, so it cannot get licenses.'),
      d.license_revoked
        ? h('button', { class: 'btn', onclick: () => setRevoked(false) }, 'Reinstate licenses')
        : d.attestation ? h('button', { class: 'btn danger', onclick: () => setRevoked(true) }, 'Revoke licenses') : null,
      h('hr', { class: 'sep' })) : null);
    replace(labelBox, labelChips(d.labels, async k => {
      if (await guard(() => api('PATCH', '/api/v1/devices/' + id, { remove_labels: [k] }))) refresh();
    }));
    replace(head, h('div', { class: 'crumb' }, h('a', { href: '#/devices' }, 'Devices'), ' / ', d.id),
      pageHead(d.name, h('span', {}, badge(d.status), d.license_revoked ? [' ', badge('revoked')] : null, ' last seen ', ago(d.last_seen_at),
        d.agent_status ? ' · ' + d.agent_status : '')));
    const usage = (free, total, usedBytes) => {
      if (!total) return h('span', { class: 'muted' }, '—');
      const u = usedBytes || total - free;
      const used = h('span', { class: u / total > 0.9 ? 'bad' : 'info' });
      used.style.width = (100 * u / total) + '%';
      return h('div', {}, h('div', { class: 'bar' }, used), h('div', { class: 'bar-label' }, h('span', {}, bytes(u) + ' used'), h('span', {}, bytes(free) + ' free')));
    };
    replace(info, h('h2', {}, 'System'),
      h('div', { class: 'grid cols-2' },
        h('dl', { class: 'kv' },
          h('dt', {}, 'Hostname'), h('dd', {}, d.info.hostname || '—'),
          h('dt', {}, 'Platform'), h('dd', {}, `${d.info.os}/${d.info.arch}`, d.info.kernel ? ` · ${d.info.kernel}` : ''),
          h('dt', {}, 'IP addresses'), h('dd', { class: 'mono' }, (d.info.ips || []).join(', ') || '—'),
          h('dt', {}, 'Packages'), h('dd', {}, pkgChips(d.packages)),
          h('dt', {}, 'Hardware key'), h('dd', {}, d.attestation
            ? [badge(d.attestation.boot_state === 'verified' && d.attestation.device_locked ? 'attested' : 'attested (unlocked)'),
              ` ${d.attestation.security_level.toUpperCase()}, boot ${d.attestation.boot_state}`,
              d.attestation.os_patch_level ? `, patch ${String(d.attestation.os_patch_level).replace(/^(\d{4})(\d{2}).*/, '$1-$2')}` : '']
            : h('span', { class: 'muted' }, 'none')),
          h('dt', {}, 'Enrolled'), h('dd', {}, new Date(d.enrolled_at).toLocaleString())),
        h('dl', { class: 'kv' },
          h('dt', {}, 'Uptime'), h('dd', {}, duration(m.uptime_seconds)),
          h('dt', {}, 'Load (1m)'), h('dd', {}, `${(m.load1 || 0).toFixed(2)} on ${m.num_cpu || '?'} CPUs`),
          h('dt', {}, 'Memory'), h('dd', {}, usage(m.mem_available_bytes, m.mem_total_bytes)),
          h('dt', {}, 'Disk'), h('dd', {}, usage(m.disk_free_bytes, m.disk_total_bytes, m.disk_used_bytes)))));
    replace(updates, h('h2', {}, 'Update history'), table(['Package', 'Version', 'Status', 'Updated', 'Details'],
      (det.updates || []).map(u => h('tr', {},
        h('td', { class: 'mono' }, u.package),
        h('td', { class: 'mono nowrap' }, (u.from_version || '—') + ' → ' + u.version),
        h('td', {}, badge(u.status)),
        h('td', { class: 'nowrap' }, ago(u.updated_at)),
        h('td', {}, h('a', { href: '#/deployments/' + u.deployment_id }, 'deployment'), u.message ? h('div', { class: 'muted' }, u.message) : null))),
      'This device has not received any updates.'));
    replace(actionLog, h('h2', {}, 'Actions'), table(['Action', 'Status', 'Updated', 'Output'],
      (det.actions || []).map(a => h('tr', {},
        h('td', { class: 'mono' }, a.type, a.args && a.args.length ? ' ' + a.args.join(' ') : ''),
        h('td', {}, badge(a.status)),
        h('td', { class: 'nowrap' }, ago(a.updated_at)),
        h('td', {}, a.output ? h('pre', { class: 'out' }, a.output) : h('span', { class: 'muted' }, a.status === 'queued' || a.status === 'sent' ? 'waiting for device' : '—')))),
      'No actions yet.'));
    replace(events, h('h2', {}, 'Activity'), eventFeed(det.events));
  }
  if (await guard(refresh) === undefined && !dev) replace(root, h('div', { class: 'empty' }, 'Device not found. ', h('a', { href: '#/devices' }, 'Back to devices')));
  return refresh;
}

function strategyFields() {
  const f = {
    stages: h('input', { value: '5,25,50,100', 'aria-label': 'Stages' }),
    soak: h('input', { value: '15', type: 'number', min: '0', 'aria-label': 'Minutes per stage' }),
    maxFail: h('input', { value: '20', type: 'number', min: '0', max: '100', 'aria-label': 'Max failure %' }),
    minSamples: h('input', { value: '3', type: 'number', min: '1', 'aria-label': 'Min samples' }),
    manual: h('input', { type: 'checkbox' }),
    downgrade: h('input', { type: 'checkbox' }),
  };
  f.nodes = [
    h('label', {}, 'Stages (cumulative %)', f.stages),
    h('label', {}, 'Minutes per stage', f.soak),
    h('label', {}, 'Pause above failure %', f.maxFail),
    h('label', {}, 'Min results before judging', f.minSamples),
    h('label', { class: 'check' }, f.manual, 'Advance stages manually'),
    h('label', { class: 'check' }, f.downgrade, 'Allow downgrade (rollback)'),
  ];
  f.value = () => ({
    stages: f.stages.value.split(',').map(s => parseInt(s.trim(), 10)).filter(n => !isNaN(n)),
    stage_duration_seconds: Math.round(parseFloat(f.soak.value || '0') * 60),
    auto_advance: !f.manual.checked,
    max_failure_rate: parseFloat(f.maxFail.value || '0') / 100,
    min_samples: parseInt(f.minSamples.value || '1', 10),
    allow_downgrade: f.downgrade.checked,
  });
  return f;
}

async function viewDeployments(root) {
  const list = h('div', { class: 'card flush' });
  const form = h('div', { class: 'card hidden' });
  const relSel = h('select', { required: true, 'aria-label': 'Release' });
  const nameIn = h('input', { placeholder: 'optional', 'aria-label': 'Name' });
  const chanIn = h('input', { placeholder: 'any', 'aria-label': 'Channel' });
  const labelsIn = h('input', { placeholder: 'site=sg role=kiosk', 'aria-label': 'Labels' });
  const devIn = h('input', { placeholder: 'dev_… (comma separated)', 'aria-label': 'Device IDs' });
  const strat = strategyFields();
  append(form, h('h2', {}, 'New deployment'),
    h('form', { class: 'stack', onsubmit: async e => {
      e.preventDefault();
      let labels;
      try { labels = parseLabels(labelsIn.value); } catch (err) { return toast(err.message, true); }
      const body = {
        name: nameIn.value, release_id: relSel.value, strategy: strat.value(),
        selector: { channel: chanIn.value.trim(), labels, device_ids: devIn.value.split(/[,\s]+/).filter(Boolean) },
      };
      const d = await guard(() => api('POST', '/api/v1/deployments', body), 'Deployment started');
      if (d) location.hash = '#/deployments/' + d.id;
    } },
    h('div', { class: 'form-grid' },
      h('label', { class: 'wide' }, 'Release', relSel),
      h('label', {}, 'Name', nameIn),
      h('label', {}, 'Target channel', chanIn),
      h('label', {}, 'Target labels', labelsIn),
      h('label', {}, 'Target devices', devIn)),
    h('div', { class: 'form-grid' }, strat.nodes),
    h('div', { class: 'btn-row' }, h('button', { class: 'btn primary', type: 'submit' }, 'Start rollout'),
      h('button', { class: 'btn ghost', type: 'button', onclick: () => form.classList.add('hidden') }, 'Cancel'))));

  append(root, pageHead('Deployments', 'Staged rollouts of releases to devices',
    h('button', { class: 'btn primary', onclick: async () => {
      const rels = await guard(() => api('GET', '/api/v1/releases'));
      if (!rels) return;
      if (!rels.length) return toast('Upload a release first.', true);
      replace(relSel, rels.map(r => h('option', { value: r.id }, `${r.package} ${r.version} (${r.channel}${r.os || r.arch ? ', ' + (r.os || 'any') + '/' + (r.arch || 'any') : ''})`)));
      form.classList.remove('hidden');
      relSel.focus();
    } }, 'New deployment')), h('div', { class: 'mb' }, form), list);

  async function refresh() {
    const deps = await api('GET', '/api/v1/deployments');
    replace(list, table(['Deployment', 'Status', 'Stage', 'Progress', 'Created'],
      deps.map(d => h('tr', { class: 'clickable', onclick: () => { location.hash = '#/deployments/' + d.id; } },
        h('td', {}, h('strong', {}, d.name), h('div', { class: 'muted mono' }, d.package + ' ' + d.version)),
        h('td', {}, badge(d.status), d.status_reason ? h('div', { class: 'muted' }, d.status_reason) : null),
        h('td', { class: 'nowrap' }, currentPercent(d) + '%', h('div', { class: 'muted' }, `stage ${Math.min(d.stage_index + 1, d.strategy.stages.length)} of ${d.strategy.stages.length}`)),
        h('td', {}, progressBar(d.stats)),
        h('td', { class: 'nowrap' }, ago(d.created_at)))),
      'No deployments yet.'));
  }
  await guard(refresh);
  return refresh;
}

async function viewDeployment(root, id) {
  const head = h('div', {});
  const summary = h('div', { class: 'card' });
  const devs = h('div', { class: 'card flush' });
  const events = h('div', { class: 'card flush' });
  append(root, head, h('div', { class: 'grid side' }, h('div', { class: 'stack' }, summary, devs), events));

  const op = async (name, msg) => { if (await guard(() => api('POST', `/api/v1/deployments/${id}/${name}`), msg)) refresh(); };

  async function refresh() {
    const det = await api('GET', '/api/v1/deployments/' + id);
    const d = det.deployment, r = det.release, s = d.stats;
    const last = d.stage_index >= d.strategy.stages.length - 1;
    const buttons = [];
    if (d.status === 'running') buttons.push(h('button', { class: 'btn', onclick: () => op('pause', 'Paused') }, 'Pause'));
    if (d.status === 'paused') buttons.push(h('button', { class: 'btn primary', onclick: () => op('resume', 'Resumed') }, 'Resume'));
    if (d.status === 'running' && !last) buttons.push(h('button', { class: 'btn', onclick: () => op('advance', 'Advanced') }, 'Advance to ' + d.strategy.stages[d.stage_index + 1] + '%'));
    if ((d.status === 'running' || d.status === 'paused') && (s.failed || s.rolled_back)) buttons.push(h('button', { class: 'btn', onclick: () => op('retry', 'Failed devices will retry') }, 'Retry failed'));
    const open = d.status === 'running' || d.status === 'paused';
    if (open && last) buttons.push(h('button', { class: 'btn', title: 'Stop offering this release, including to devices enrolled later', onclick: () => confirm('Mark this rollout complete? It stops offering the release to devices that have not updated yet.') && op('complete', 'Marked complete') }, 'Complete'));
    if (open) buttons.push(h('button', { class: 'btn danger', onclick: () => confirm('Abort this rollout? Devices that already updated keep the new version.') && op('abort', 'Aborted') }, 'Abort'));
    replace(head, h('div', { class: 'crumb' }, h('a', { href: '#/deployments' }, 'Deployments'), ' / ', d.id),
      pageHead(d.name, h('span', {}, badge(d.status), ' ', d.status_reason || ''), ...buttons));

    const sel = d.selector;
    const selParts = [sel.channel && 'channel ' + sel.channel, ...sortedEntries(sel.labels).map(([k, v]) => k + '=' + v),
      sel.device_ids && sel.device_ids.length && sel.device_ids.length + ' specific device(s)'].filter(Boolean);
    replace(summary,
      h('div', { class: 'grid cols-2' },
        h('dl', { class: 'kv' },
          h('dt', {}, 'Release'), h('dd', { class: 'mono' }, `${r.package} ${r.version}`),
          h('dt', {}, 'Targets'), h('dd', {}, selParts.join(', ') || 'all devices', h('div', { class: 'muted' }, `${det.targeted} device(s) match · ${det.eligible} in current stage`)),
          h('dt', {}, 'SHA-256'), h('dd', { class: 'mono' }, r.sha256.slice(0, 16) + '…')),
        h('dl', { class: 'kv' },
          h('dt', {}, 'Stage'), h('dd', {}, `${currentPercent(d)}% · started ${ago(d.stage_started_at)}`),
          h('dt', {}, 'Advance'), h('dd', {}, d.strategy.auto_advance ? `automatically every ${duration(d.strategy.stage_duration_seconds)}` : 'manually'),
          h('dt', {}, 'Safety'), h('dd', {}, `pause above ${Math.round(d.strategy.max_failure_rate * 100)}% failures (after ${d.strategy.min_samples} results)`))),
      h('div', { class: 'mt' }, stageStrip(d), h('div', { class: 'spacer' }), progressBar(s, det.targeted)));

    replace(devs, h('h2', {}, 'Devices'), table(['Device', 'From', 'Status', 'Updated', 'Details'],
      (det.updates || []).map(u => h('tr', { class: 'clickable', onclick: () => { location.hash = '#/devices/' + u.device_id; } },
        h('td', { class: 'mono' }, u.device_id),
        h('td', { class: 'mono' }, u.from_version || '—'),
        h('td', {}, badge(u.status)),
        h('td', { class: 'nowrap' }, ago(u.updated_at)),
        h('td', { class: 'muted' }, u.message || ''))),
      d.status === 'paused' ? 'Rollout is paused.' : 'No device has been offered this release yet. Devices pick it up on their next check-in.'));
    replace(events, h('h2', {}, 'Timeline'), eventFeed(det.events));
  }
  await guard(refresh);
  return refresh;
}

async function viewReleases(root) {
  const list = h('div', { class: 'card flush' });
  const pkgIn = h('input', { required: true, placeholder: 'myapp or armada-agent', 'aria-label': 'Package' });
  const verIn = h('input', { required: true, placeholder: '1.4.2', 'aria-label': 'Version' });
  const chanIn = h('input', { value: 'stable', 'aria-label': 'Channel' });
  const osIn = h('select', { 'aria-label': 'OS' }, ['', 'linux', 'android', 'darwin', 'windows', 'freebsd'].map(o => h('option', { value: o }, o || 'any')));
  const vcIn = h('input', { type: 'number', min: '1', placeholder: 'e.g. 210', 'aria-label': 'Android versionCode' });
  const vcLabel = h('label', { class: 'hidden' }, 'Android versionCode', vcIn);
  osIn.addEventListener('change', () => vcLabel.classList.toggle('hidden', osIn.value !== 'android'));
  const archIn = h('select', { 'aria-label': 'Architecture' }, ['', 'amd64', 'arm64', 'arm', '386', 'riscv64'].map(o => h('option', { value: o }, o || 'any')));
  const notesIn = h('textarea', { placeholder: 'What changed (optional)', 'aria-label': 'Notes' });
  const fileIn = h('input', { type: 'file', required: true, 'aria-label': 'Artifact' });
  const autoIn = h('input', { type: 'checkbox', checked: true });
  const progress = h('div', { class: 'bar hidden' }, h('span', { class: 'info' }));
  const submit = h('button', { class: 'btn primary', type: 'submit' }, 'Upload');

  append(root, pageHead('Releases', 'Signed artifacts that can be rolled out'),
    h('div', { class: 'card' }, h('h2', {}, 'Upload a release'),
      h('form', { class: 'stack', onsubmit: async e => {
        e.preventDefault();
        const fd = new FormData();
        fd.append('package', pkgIn.value.trim());
        fd.append('version', verIn.value.trim());
        fd.append('channel', chanIn.value.trim());
        fd.append('os', osIn.value);
        fd.append('arch', archIn.value);
        if (osIn.value === 'android' && vcIn.value) fd.append('version_code', vcIn.value);
        fd.append('notes', notesIn.value);
        fd.append('auto_deploy', autoIn.checked ? 'true' : 'false');
        fd.append('file', fileIn.files[0]);
        submit.disabled = true;
        progress.classList.remove('hidden');
        const fill = progress.firstChild;
        try {
          const res = await uploadForm('/api/v1/releases', fd, p => { fill.style.width = (p * 100) + '%'; });
          toast(`Uploaded ${res.release.package} ${res.release.version}` + (res.deployment ? ' — rollout started' : ''));
          verIn.value = ''; fileIn.value = ''; notesIn.value = '';
          if (res.deployment) location.hash = '#/deployments/' + res.deployment.id; else refresh();
        } catch (err) {
          if (err.status === 401) showLogin(); else toast(err.message, true);
        } finally {
          submit.disabled = false;
          progress.classList.add('hidden');
          fill.style.width = '0';
        }
      } },
      h('div', { class: 'form-grid' },
        h('label', {}, 'Package', pkgIn), h('label', {}, 'Version', verIn), h('label', {}, 'Channel', chanIn),
        h('label', {}, 'OS', osIn), h('label', {}, 'Architecture', archIn), vcLabel,
        h('label', { class: 'wide' }, 'Artifact file', fileIn),
        h('label', { class: 'wide' }, 'Notes', notesIn)),
      h('label', { class: 'check' }, autoIn, 'Automatically roll out to every device on this channel (staged, auto-pauses on failures)'),
      progress,
      h('div', {}, submit))),
    h('div', { class: 'spacer' }),
    list);

  async function refresh() {
    const rels = await api('GET', '/api/v1/releases');
    replace(list, table(['Release', 'Channel', 'Platform', 'Size', 'Uploaded', ''],
      rels.map(r => h('tr', {},
        h('td', {}, h('strong', { class: 'mono' }, r.package + ' ' + r.version), r.notes ? h('div', { class: 'muted' }, r.notes) : null,
          h('div', { class: 'muted mono', title: r.sha256 }, 'sha256 ' + r.sha256.slice(0, 16) + '…')),
        h('td', {}, r.channel),
        h('td', { class: 'nowrap' }, (r.os || 'any') + '/' + (r.arch || 'any')),
        h('td', { class: 'nowrap' }, bytes(r.size)),
        h('td', { class: 'nowrap' }, ago(r.created_at)),
        h('td', {}, h('div', { class: 'btn-row' },
          h('button', { class: 'btn small', onclick: () => guard(() => downloadArtifact(r)) }, 'Download'),
          h('button', { class: 'btn small danger', onclick: async () => {
            if (!confirm(`Delete ${r.package} ${r.version}?`)) return;
            if (await guard(() => api('DELETE', '/api/v1/releases/' + r.id), 'Release deleted') !== undefined) refresh();
          } }, 'Delete'))))),
      'No releases yet.'));
  }
  await guard(refresh);
  return refresh;
}

// Android setup: a QR code scanned on a factory-reset device's welcome screen
// installs the Armada agent as Device Owner and enrolls it with this token.
function androidQR(enrollmentToken) {
  const ssid = h('input', { placeholder: 'optional', 'aria-label': 'Wi-Fi network' });
  const pass = h('input', { type: 'password', placeholder: 'optional', 'aria-label': 'Wi-Fi password' });
  const out = h('div', {});
  return h('div', { class: 'stack mt' },
    h('div', {}, h('strong', {}, 'Android devices'), ' — on a new or factory-reset device, tap the welcome screen six times and scan:'),
    h('form', { class: 'inline', onsubmit: async e => {
      e.preventDefault();
      const r = await guard(() => api('POST', '/api/v1/android/provisioning', {
        enrollment_token: enrollmentToken, wifi_ssid: ssid.value.trim(), wifi_password: pass.value,
      }));
      if (!r) return;
      $('#toast').classList.add('hidden'); // clear any earlier error
      replace(out, h('div', { class: 'stack' },
        h('img', { src: 'data:image/png;base64,' + r.qr_png, alt: 'Android provisioning QR code', class: 'qr', width: '256', height: '256' }),
        (r.warnings || []).map(w => h('div', { class: 'muted' }, '⚠ ' + w)),
        h('details', {}, h('summary', {}, 'QR payload'), h('pre', { class: 'out' }, JSON.stringify(JSON.parse(r.payload), null, 2)))));
    } },
    h('label', {}, 'Wi-Fi network', ssid), h('label', {}, 'Wi-Fi password', pass),
    h('button', { class: 'btn', type: 'submit' }, 'Show Android setup QR')),
    out);
}

async function viewApps(root) {
  const list = h('div', { class: 'card flush' });
  const pkgIn = h('input', { required: true, placeholder: 'com.yourco.app', 'aria-label': 'Package' });
  const nameIn = h('input', { placeholder: 'optional', 'aria-label': 'App name' });
  const signersIn = h('textarea', { required: true, placeholder: 'SHA-256 of the signing certificate, e.g. from: keytool -list -v -keystore release.jks\nAB:CD:…', 'aria-label': 'Signers' });
  const licIn = h('input', { type: 'checkbox', checked: true });
  const ttlIn = h('input', { type: 'number', min: '1', value: '60', 'aria-label': 'License lifetime (minutes)' });
  const fill = a => {
    pkgIn.value = a.package; nameIn.value = a.name; signersIn.value = a.signers.join('\n');
    licIn.checked = a.licensing; ttlIn.value = Math.round(a.license_ttl_seconds / 60);
    pkgIn.focus();
  };
  append(root, pageHead('Apps', 'Android apps Armada installs, and licenses to enrolled devices only'),
    h('div', { class: 'card' }, h('h2', {}, 'Register or update an app'),
      h('form', { class: 'stack', onsubmit: async e => {
        e.preventDefault();
        const body = {
          name: nameIn.value.trim(), licensing: licIn.checked,
          license_ttl_seconds: Math.round(parseFloat(ttlIn.value || '60') * 60),
          signers: signersIn.value.split(/[\s,]+/).filter(Boolean),
        };
        if (await guard(() => api('PUT', '/api/v1/apps/' + encodeURIComponent(pkgIn.value.trim()), body), 'App saved')) refresh();
      } },
      h('div', { class: 'form-grid' },
        h('label', {}, 'Package name', pkgIn), h('label', {}, 'Display name', nameIn),
        h('label', {}, 'License lifetime (minutes)', ttlIn),
        h('label', { class: 'wide' }, 'Signing certificate SHA-256 (one per line)', signersIn)),
      h('label', { class: 'check' }, licIn, 'Issue licenses: the app only works on devices you enrolled'),
      h('p', { class: 'muted' }, 'Register the Android agent here too (without licensing) so devices can verify its updates and the setup QR can be generated.'),
      h('div', {}, h('button', { class: 'btn primary', type: 'submit' }, 'Save')))),
    h('div', { class: 'spacer' }), list);

  async function refresh() {
    const apps = await api('GET', '/api/v1/apps');
    replace(list, table(['App', 'Signing certificates', 'Licensing', ''],
      apps.map(a => h('tr', {},
        h('td', {}, h('strong', { class: 'mono' }, a.package), a.name ? h('div', { class: 'muted' }, a.name) : null),
        h('td', { class: 'mono' }, a.signers.map(sg => h('div', { title: sg }, sg.slice(0, 16) + '…'))),
        h('td', {}, a.licensing ? [badge('active'), h('div', { class: 'muted' }, `${Math.round(a.license_ttl_seconds / 60)} min licenses`)] : h('span', { class: 'muted' }, 'off')),
        h('td', {}, h('div', { class: 'btn-row' },
          h('button', { class: 'btn small', onclick: () => fill(a) }, 'Edit'),
          h('button', { class: 'btn small danger', onclick: async () => {
            if (!confirm(`Unregister ${a.package}? Devices will stop receiving its updates and licenses.`)) return;
            if (await guard(() => api('DELETE', '/api/v1/apps/' + encodeURIComponent(a.package)), 'App removed') !== undefined) refresh();
          } }, 'Remove'))))),
      'No apps registered yet.'));
  }
  await guard(refresh);
  return refresh;
}

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

  append(root, pageHead('Enrollment', 'Tokens that let new devices join the fleet'),
    h('div', { class: 'card' }, h('h2', {}, 'Create enrollment token'),
      h('form', { class: 'stack', onsubmit: async e => {
        e.preventDefault();
        let labels;
        try { labels = parseLabels(labelsIn.value); } catch (err) { return toast(err.message, true); }
        const t = await guard(() => api('POST', '/api/v1/tokens', {
          description: descIn.value, channel: chanIn.value.trim(), labels,
          max_uses: parseInt(usesIn.value || '0', 10), ttl_seconds: parseInt(ttlIn.value, 10),
        }));
        if (!t) return;
        const origin = location.origin;
        replace(created, h('div', { class: 'callout stack' },
          h('div', {}, h('strong', {}, 'Copy this token now'), ' — it will not be shown again.'),
          h('div', { class: 'secret' }, t.token),
          h('div', {}, 'Run on a device (Linux, as root):'),
          h('div', { class: 'secret' }, `curl -fsSL ${origin}/install.sh | sudo ARMADA_SERVER_URL=${origin} ARMADA_ENROLLMENT_TOKEN=${t.token} sh`),
          h('div', { class: 'muted' }, 'or run the agent directly:'),
          h('div', { class: 'secret' }, `armada-agent --server ${origin} --token ${t.token}`),
          androidQR(t.token)));
        refresh();
      } },
      h('div', { class: 'form-grid' },
        h('label', {}, 'Description', descIn), h('label', {}, 'Channel', chanIn), h('label', {}, 'Labels for new devices', labelsIn),
        h('label', {}, 'Max uses (0 = unlimited)', usesIn), h('label', {}, 'Expires', ttlIn)),
      h('div', {}, h('button', { class: 'btn primary', type: 'submit' }, 'Create token'))),
      h('div', { class: 'mt' }, created)),
    h('div', { class: 'spacer' }), list);

  async function refresh() {
    const toks = await api('GET', '/api/v1/tokens');
    const now = Date.now();
    replace(list, table(['Token', 'State', 'Channel', 'Labels', 'Uses', 'Expires', ''],
      toks.map(t => {
        const state = t.revoked ? 'revoked' : t.expires_at && new Date(t.expires_at) < now ? 'expired' : t.max_uses && t.uses >= t.max_uses ? 'used up' : 'active';
        return h('tr', {},
          h('td', {}, h('div', {}, t.description || h('span', { class: 'muted' }, 'untitled')), h('div', { class: 'muted mono' }, t.id)),
          h('td', {}, h('span', { class: 'badge ' + (state === 'active' ? 'ok' : '') }, state)),
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

async function viewActivity(root) {
  const feed = h('div', { class: 'card flush' });
  append(root, pageHead('Activity', 'Everything that happened across the fleet'), feed);
  async function refresh() { replace(feed, eventFeed(await api('GET', '/api/v1/events?limit=200'))); }
  await guard(refresh);
  return refresh;
}

// ---------- auth ----------

function showLogin(msg) {
  clearInterval(refreshTimer);
  $('#app').classList.add('hidden');
  $('#login').classList.remove('hidden');
  const err = $('#login-error');
  err.textContent = msg || '';
  err.classList.toggle('hidden', !msg);
  $('#login-token').focus();
}

$('#login-form').addEventListener('submit', async e => {
  e.preventDefault();
  token = $('#login-token').value.trim();
  const res = await fetch('/api/v1/summary', { headers: { Authorization: 'Bearer ' + token } }).catch(() => null);
  if (!res || !res.ok) {
    token = '';
    return showLogin(res ? 'That token was not accepted.' : 'Could not reach the server.');
  }
  store.set('armada.token', token);
  $('#login-token').value = '';
  $('#login').classList.add('hidden');
  $('#app').classList.remove('hidden');
  route();
});

$('#logout').addEventListener('click', () => {
  token = '';
  store.del('armada.token');
  showLogin();
});

window.addEventListener('hashchange', route);
document.addEventListener('visibilitychange', () => { if (!document.hidden && currentRefresh) guard(currentRefresh); });

if (token) {
  $('#app').classList.remove('hidden');
  route();
} else {
  showLogin();
}
