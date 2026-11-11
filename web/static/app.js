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
  online: 'ok', offline: 'bad', ok: 'ok', warning: 'warn', critical: 'bad', unknown: '', firing: 'bad', resolved: 'ok',
  enabled: 'ok', disabled: '',
  running: 'info', paused: 'warn', aborted: 'bad', completed: 'ok',
  succeeded: 'ok', failed: 'bad', rolled_back: 'warn', pending: '', downloading: 'info', installing: 'info',
  queued: '', sent: 'info', rejected: 'warn', revoked: 'bad', active: 'ok', attested: 'ok', 'attested (unlocked)': 'warn',
};
function healthRank(s) { return { ok: 0, unknown: 1, warning: 2, critical: 3 }[s] ?? 1; }

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
  [/^#\/monitoring$/, 'monitoring', viewMonitoring],
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
  const fleetHist = fleetHistorySection();
  const stats = h('div', { class: 'grid cols-4' });
  const deps = h('div', {});
  const dist = h('div', {});
  const feed = h('div', {});
  append(root,
    pageHead('Fleet overview'),
    stats,
    fleetHist.el,
    h('div', { class: 'grid side mt' },
      h('div', { class: 'stack' },
        h('div', { class: 'card' }, h('h2', {}, 'Active deployments'), deps),
        h('div', { class: 'card' }, h('h2', {}, 'Installed versions'), dist)),
      h('div', { class: 'card flush' }, h('h2', {}, 'Recent activity'), feed)));

  async function refresh() {
    fleetHist.load();
    const [sum, active, events] = await Promise.all([
      api('GET', '/api/v1/summary'),
      api('GET', '/api/v1/deployments?status=running,paused'),
      api('GET', '/api/v1/events?limit=15'),
    ]);
    const tile = (label, value, hint, href) => h(href ? 'a' : 'div', { class: 'card stat', href },
      h('div', { class: 'label' }, label), h('div', { class: 'value' }, value), h('div', { class: 'hint' }, hint));
    const pct = sum.devices ? Math.round(100 * sum.online / sum.devices) : 0;
    replace(stats,
      tile('Devices', sum.devices, [Object.entries(sum.channels).map(([c, n]) => `${n} ${c}`).join(' · ') || 'none enrolled',
        sum.peripherals ? ` · ${sum.peripherals} peripherals` : ''].join(''), '#/devices'),
      tile('Online', sum.online, sum.devices ? `${pct}% · ${sum.offline} offline` : '—', '#/devices'),
      tile('Health issues', sum.health_critical + sum.health_warning,
        sum.health_critical + sum.health_warning ? `${sum.health_critical} critical · ${sum.health_warning} warning` : 'all healthy', '#/devices'),
      tile('Firing alerts', sum.firing_alerts, sum.firing_alerts ? 'see Monitoring' : 'nothing firing', '#/monitoring'),
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
  const healthSel = h('select', { 'aria-label': 'Health filter' }, ['', 'critical', 'warning', 'unknown', 'ok'].map(v => h('option', { value: v }, v ? 'Health: ' + v : 'Any health')));
  const kindSel = h('select', { 'aria-label': 'Kind filter' }, [['', 'All kinds'], ['agent', 'Agents'], ['peripheral', 'Peripherals']].map(([v, l]) => h('option', { value: v }, l)));
  const list = h('div', { class: 'card flush' });
  const count = h('span', {});
  append(root, pageHead('Devices', count, h('a', { class: 'btn primary', href: '#/enroll' }, 'Enroll devices')),
    h('div', { class: 'toolbar' }, search, status, healthSel, kindSel), h('div', { class: 'spacer' }), list);
  let devices = [];
  function render() {
    const q = search.value.trim().toLowerCase();
    const names = Object.fromEntries(devices.map(d => [d.id, d.name]));
    const shown = devices.filter(d => (!status.value || d.status === status.value) && (!healthSel.value || d.health === healthSel.value) &&
      (!kindSel.value || (kindSel.value === 'peripheral') === !!d.parent_id) && (!q || [
      d.id, d.name, d.channel, d.info.hostname, d.info.model, d.info.machine_id, ...(d.info.ips || []),
      ...Object.entries(d.labels).map(([k, v]) => k + '=' + v),
    ].some(s => (s || '').toLowerCase().includes(q))));
    replace(count, `${devices.length} enrolled · ${devices.filter(d => d.status === 'online').length} online`);
    replace(list, table(['Device', 'Status', 'Health', 'Channel', 'Platform', 'Packages', 'Labels', 'Last seen'],
      shown.map(d => h('tr', { class: 'clickable', onclick: () => { location.hash = '#/devices/' + d.id; } },
        h('td', {}, h('div', {}, h('strong', {}, d.name)), h('div', { class: 'muted mono' }, d.id),
          d.parent_id ? h('div', { class: 'muted' }, 'via ', names[d.parent_id] || d.parent_id) : null,
          d.agent_status ? h('div', { class: 'muted' }, d.agent_status) : null),
        h('td', {}, badge(d.status), d.license_revoked ? h('div', {}, badge('revoked')) : null),
        h('td', {}, badge(d.health || 'ok')),
        h('td', {}, d.channel),
        h('td', { class: 'nowrap' }, d.parent_id ? `BLE · ${d.info.model}` : `${d.info.os || '?'}/${d.info.arch || '?'}`),
        h('td', {}, pkgChips(d.packages)),
        h('td', {}, labelChips(d.labels)),
        h('td', { class: 'nowrap', title: d.last_seen_at }, ago(d.last_seen_at)))),
      devices.length ? 'No devices match.' : 'No devices enrolled yet. Create an enrollment token to add one.'));
  }
  search.addEventListener('input', render);
  [status, healthSel, kindSel].forEach(el => el.addEventListener('change', render));
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
  const healthCard = h('div', { class: 'card flush' });
  const periphCard = h('div', { class: 'card flush hidden' });
  const hist = historySection(id);
  append(root, head,
    h('div', { class: 'grid side' },
      h('div', { class: 'stack' }, info, healthCard, periphCard, updates, actionLog),
      h('div', { class: 'stack' }, meta, actions, events)),
    hist.el);
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
    hist.load();
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
    if (d.parent_id) {
      replace(info, h('h2', {}, 'Peripheral'), h('dl', { class: 'kv' },
        h('dt', {}, 'Gateway'), h('dd', {}, h('a', { href: '#/devices/' + d.parent_id }, d.parent_id)),
        h('dt', {}, 'Address'), h('dd', { class: 'mono' }, d.info.machine_id),
        h('dt', {}, 'Model'), h('dd', { class: 'mono' }, d.info.model),
        h('dt', {}, 'Update method'), h('dd', {}, d.info.protocol || '—'),
        h('dt', {}, 'Firmware'), h('dd', { class: 'mono' }, d.packages[d.info.model] || 'unknown'),
        h('dt', {}, 'Battery'), h('dd', {}, d.metrics.battery_percent ? d.metrics.battery_percent + '%' : '—'),
        h('dt', {}, 'Signal'), h('dd', {}, d.metrics.rssi ? d.metrics.rssi + ' dBm' : '—'),
        h('dt', {}, 'First seen'), h('dd', {}, new Date(d.enrolled_at).toLocaleString())));
    } else replace(info, h('h2', {}, 'System'),
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
    const checks = [...(d.checks || [])].sort((a, b) => healthRank(b.status) - healthRank(a.status) || a.name.localeCompare(b.name));
    replace(healthCard, h('h2', {}, 'Health ', badge(d.health || 'ok')), table(['Status', 'Check', 'Details', 'Checked'],
      checks.map(c => h('tr', {},
        h('td', {}, badge(c.status)),
        h('td', {}, h('div', { class: 'mono' }, c.name), h('div', { class: 'muted' }, c.kind)),
        h('td', {}, c.message, c.value !== undefined && c.value !== null ? h('div', { class: 'muted' }, `${c.value} ${c.unit || ''}`) : null,
          c.details && Object.keys(c.details).length ? h('div', { class: 'muted mono small' },
            sortedEntries(c.details).filter(([, v]) => v !== '').map(([k, v]) => `${k}=${v}`).join(' ')) : null),
        h('td', { class: 'nowrap' }, ago(c.checked_at)))),
      d.parent_id ? 'No health data from this peripheral.' : 'No health checks reported yet. Agents report hardware checks automatically; add program checks under Monitoring.'));
    const periphs = det.peripherals || [];
    periphCard.classList.toggle('hidden', periphs.length === 0);
    replace(periphCard, h('h2', {}, `Peripherals (${periphs.length})`), table(['Device', 'Status', 'Health', 'Model', 'Firmware', 'Battery', 'Signal', 'Last seen'],
      periphs.map(p => h('tr', { class: 'clickable', onclick: () => { location.hash = '#/devices/' + p.id; } },
        h('td', {}, h('strong', {}, p.name), h('div', { class: 'muted mono' }, p.info.machine_id)),
        h('td', {}, badge(p.status)), h('td', {}, badge(p.health || 'ok')),
        h('td', { class: 'mono' }, p.info.model), h('td', { class: 'mono' }, p.packages[p.info.model] || '—'),
        h('td', {}, p.metrics.battery_percent ? p.metrics.battery_percent + '%' : '—'),
        h('td', {}, p.metrics.rssi ? p.metrics.rssi + ' dBm' : '—'),
        h('td', { class: 'nowrap' }, ago(p.last_seen_at))))));
    actions.classList.toggle('hidden', !!d.parent_id);
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

// ---------- monitoring: alerts, monitors, rules, channels ----------

function kvLines(text) {
  const out = {};
  for (const line of text.split('\n').map(l => l.trim()).filter(Boolean)) {
    const i = line.indexOf(':');
    if (i <= 0) throw new Error('Headers must look like "Name: value"');
    out[line.slice(0, i).trim()] = line.slice(i + 1).trim();
  }
  return out;
}

async function viewMonitoring(root) {
  const alertsBox = h('div', { class: 'card flush' });
  const monitorsBox = h('div', { class: 'card flush' });
  const rulesBox = h('div', { class: 'card flush' });
  const channelsBox = h('div', { class: 'card flush' });
  let channels = [];

  // --- monitor form ---
  const mName = h('input', { required: true, placeholder: 'kiosk-web', 'aria-label': 'Monitor name' });
  const mType = h('select', { 'aria-label': 'Check type' }, ['http', 'tcp', 'systemd', 'process', 'command'].map(t => h('option', { value: t }, t)));
  const mTarget = h('input', { placeholder: 'http://127.0.0.1:8080/healthz', 'aria-label': 'Target' });
  const mCommand = h('input', { placeholder: '/usr/lib/nagios/plugins/check_load -w 5 -c 10', 'aria-label': 'Command' });
  const mContains = h('input', { placeholder: 'optional', 'aria-label': 'Must contain' });
  const mInterval = h('input', { type: 'number', min: '10', value: '60', 'aria-label': 'Interval seconds' });
  const mLabels = h('input', { placeholder: 'all devices, or role=kiosk', 'aria-label': 'Monitor labels' });
  const targetHint = { http: 'URL', tcp: 'host:port', systemd: 'unit, e.g. myapp.service', process: 'process name', command: '' };
  const syncType = () => {
    mTarget.closest('label').classList.toggle('hidden', mType.value === 'command');
    mCommand.closest('label').classList.toggle('hidden', mType.value !== 'command');
    mTarget.placeholder = targetHint[mType.value];
  };
  mType.addEventListener('change', syncType);

  // --- rule form ---
  const rName = h('input', { required: true, placeholder: 'Storage failing', 'aria-label': 'Rule name' });
  const rCond = h('select', { 'aria-label': 'Condition' }, [['health', 'Health check failing'], ['device_offline', 'Device offline'],
    ['update_failed', 'Update failed'], ['deployment_paused', 'Rollout paused']].map(([v, l]) => h('option', { value: v }, l)));
  const rSev = h('select', { 'aria-label': 'Minimum severity' }, ['critical', 'warning'].map(v => h('option', { value: v }, v)));
  const rKinds = h('input', { placeholder: 'any, or storage power temperature', 'aria-label': 'Check kinds' });
  const rOffline = h('input', { type: 'number', min: '1', value: '10', 'aria-label': 'Offline minutes' });
  const rLabels = h('input', { placeholder: 'all devices, or site=sg', 'aria-label': 'Rule labels' });
  const rRepeat = h('input', { type: 'number', min: '0', value: '0', 'aria-label': 'Repeat minutes' });
  const rResolved = h('input', { type: 'checkbox', checked: true });
  const rChannels = h('div', { class: 'chips' });
  const syncCond = () => {
    rSev.closest('label').classList.toggle('hidden', rCond.value !== 'health');
    rKinds.closest('label').classList.toggle('hidden', rCond.value !== 'health');
    rOffline.closest('label').classList.toggle('hidden', rCond.value !== 'device_offline');
  };
  rCond.addEventListener('change', syncCond);

  // --- channel form ---
  const cName = h('input', { required: true, placeholder: 'Ops on-call', 'aria-label': 'Channel name' });
  const cType = h('select', { 'aria-label': 'Channel type' }, [['webhook', 'Webhook (any service)'], ['slack', 'Slack'], ['email', 'Email (SMTP)']].map(([v, l]) => h('option', { value: v }, l)));
  const cURL = h('input', { placeholder: 'https://events.pagerduty.com/v2/enqueue', 'aria-label': 'URL' });
  const cSecret = h('input', { type: 'password', placeholder: 'optional, signs each request', 'aria-label': 'Signing secret' });
  const cHeaders = h('textarea', { placeholder: 'Authorization: Bearer …\n(one header per line, optional)', 'aria-label': 'Headers' });
  const cTemplate = h('textarea', { placeholder: 'optional Go template, e.g. {"text": {{json .Summary}}}', 'aria-label': 'Body template' });
  const cSlack = h('input', { placeholder: 'https://hooks.slack.com/services/…', 'aria-label': 'Slack webhook URL' });
  const cHost = h('input', { placeholder: 'smtp.example.com', 'aria-label': 'SMTP host' });
  const cPort = h('input', { type: 'number', value: '587', 'aria-label': 'SMTP port' });
  const cUser = h('input', { placeholder: 'optional', 'aria-label': 'SMTP user' });
  const cPass = h('input', { type: 'password', placeholder: 'optional', 'aria-label': 'SMTP password' });
  const cFrom = h('input', { placeholder: 'armada@example.com', 'aria-label': 'From' });
  const cTo = h('input', { placeholder: 'ops@example.com, oncall@example.com', 'aria-label': 'To' });
  const webhookFields = h('div', { class: 'form-grid' }, h('label', { class: 'wide' }, 'URL', cURL), h('label', {}, 'Signing secret', cSecret),
    h('label', { class: 'wide' }, 'Headers', cHeaders), h('label', { class: 'wide' }, 'Body template', cTemplate));
  const slackFields = h('div', { class: 'form-grid hidden' }, h('label', { class: 'wide' }, 'Slack incoming webhook URL', cSlack));
  const emailFields = h('div', { class: 'form-grid hidden' }, h('label', {}, 'SMTP host', cHost), h('label', {}, 'Port', cPort),
    h('label', {}, 'Username', cUser), h('label', {}, 'Password', cPass), h('label', {}, 'From', cFrom), h('label', { class: 'wide' }, 'To', cTo));
  cType.addEventListener('change', () => {
    webhookFields.classList.toggle('hidden', cType.value !== 'webhook');
    slackFields.classList.toggle('hidden', cType.value !== 'slack');
    emailFields.classList.toggle('hidden', cType.value !== 'email');
  });

  const section = (title, sub) => h('div', { class: 'page-head mt' }, h('div', {}, h('h2', {}, title), h('div', { class: 'muted' }, sub)));
  append(root, pageHead('Monitoring', 'Health checks on devices, and who gets told when something goes wrong'),
    alertsBox,
    section('Health monitors', 'Program checks run by every matching device. Hardware checks (storage, temperature, power, battery) run automatically.'),
    h('div', { class: 'card mb' }, h('form', { class: 'stack', onsubmit: async e => {
      e.preventDefault();
      let labels;
      try { labels = parseLabels(mLabels.value); } catch (err) { return toast(err.message, true); }
      const check = { type: mType.value, interval_seconds: parseInt(mInterval.value || '60', 10), contains: mContains.value };
      if (mType.value === 'command') check.command = mCommand.value.match(/"[^"]*"|'[^']*'|\S+/g)?.map(a => a.replace(/^(["'])(.*)\1$/, '$2')) || [];
      else check.target = mTarget.value.trim();
      if (await guard(() => api('POST', '/api/v1/monitors', { name: mName.value.trim(), enabled: true, selector: { labels }, check }), 'Monitor added')) {
        mName.value = ''; refresh();
      }
    } },
    h('div', { class: 'form-grid' }, h('label', {}, 'Name', mName), h('label', {}, 'Type', mType), h('label', { class: 'wide' }, 'Target', mTarget),
      h('label', { class: 'wide hidden' }, 'Command (exit 0 ok · 1 warning · 2 critical, like Nagios plugins)', mCommand),
      h('label', {}, 'Must contain', mContains), h('label', {}, 'Every (seconds)', mInterval), h('label', {}, 'Devices (labels)', mLabels)),
    h('div', {}, h('button', { class: 'btn primary', type: 'submit' }, 'Add monitor')))),
    monitorsBox,
    section('Alert rules', 'When to notify, and which channels to use.'),
    h('div', { class: 'card mb' }, h('form', { class: 'stack', onsubmit: async e => {
      e.preventDefault();
      let labels;
      try { labels = parseLabels(rLabels.value); } catch (err) { return toast(err.message, true); }
      const chosen = [...rChannels.querySelectorAll('input:checked')].map(i => i.value);
      if (!chosen.length) return toast('Pick at least one channel (add one below first).', true);
      const body = { name: rName.value.trim(), enabled: true, condition: rCond.value, selector: { labels }, channels: chosen,
        min_severity: rSev.value, check_kinds: rKinds.value.split(/[\s,]+/).filter(Boolean), offline_minutes: parseInt(rOffline.value || '10', 10),
        repeat_minutes: parseInt(rRepeat.value || '0', 10), notify_resolved: rResolved.checked };
      if (await guard(() => api('POST', '/api/v1/alert-rules', body), 'Rule added')) { rName.value = ''; refresh(); }
    } },
    h('div', { class: 'form-grid' }, h('label', {}, 'Name', rName), h('label', {}, 'When', rCond), h('label', {}, 'At least', rSev),
      h('label', {}, 'Only check kinds', rKinds), h('label', { class: 'hidden' }, 'Offline for (minutes)', rOffline),
      h('label', {}, 'Devices (labels)', rLabels), h('label', {}, 'Repeat every (minutes, 0 = never)', rRepeat)),
    h('label', { class: 'check' }, rResolved, 'Also notify when it resolves'),
    h('div', {}, h('div', { class: 'muted mb' }, 'Notify:'), rChannels),
    h('div', {}, h('button', { class: 'btn primary', type: 'submit' }, 'Add rule')))),
    rulesBox,
    section('Notification channels', 'Webhooks reach any service (PagerDuty, Opsgenie, Teams, Discord, your own API) using an optional body template.'),
    h('div', { class: 'card mb' }, h('form', { class: 'stack', onsubmit: async e => {
      e.preventDefault();
      const body = { name: cName.value.trim(), type: cType.value };
      try {
        if (cType.value === 'webhook') body.webhook = { url: cURL.value.trim(), secret: cSecret.value, headers: kvLines(cHeaders.value), body_template: cTemplate.value };
        if (cType.value === 'slack') body.slack = { webhook_url: cSlack.value.trim() };
        if (cType.value === 'email') body.email = { host: cHost.value.trim(), port: parseInt(cPort.value || '587', 10), username: cUser.value,
          password: cPass.value, from: cFrom.value.trim(), to: cTo.value.split(/[\s,]+/).filter(Boolean) };
      } catch (err) { return toast(err.message, true); }
      if (await guard(() => api('POST', '/api/v1/channels', body), 'Channel added')) { cName.value = ''; refresh(); }
    } },
    h('div', { class: 'form-grid' }, h('label', {}, 'Name', cName), h('label', {}, 'Type', cType)),
    webhookFields, slackFields, emailFields,
    h('div', {}, h('button', { class: 'btn primary', type: 'submit' }, 'Add channel')))),
    channelsBox);
  syncType(); syncCond();

  async function refresh() {
    const [alerts, monitors, rules, chs] = await Promise.all([
      api('GET', '/api/v1/alerts'), api('GET', '/api/v1/monitors'), api('GET', '/api/v1/alert-rules'), api('GET', '/api/v1/channels')]);
    channels = chs;
    const chName = Object.fromEntries(chs.map(c => [c.id, c.name]));
    const firing = alerts.filter(a => a.status === 'firing');
    replace(alertsBox, h('h2', {}, `Alerts${firing.length ? ` — ${firing.length} firing` : ''}`), table(['Status', 'Severity', 'Rule', 'What', 'Since'],
      alerts.slice(0, 50).map(a => h('tr', {},
        h('td', {}, badge(a.status)), h('td', {}, badge(a.severity)), h('td', {}, a.rule_name),
        h('td', {}, a.summary, a.count > 1 ? h('span', { class: 'muted' }, ` (×${a.count})`) : null,
          a.device_id ? h('div', {}, h('a', { href: '#/devices/' + a.device_id }, a.device_name || a.device_id)) : null,
          a.deployment_id && !a.device_id ? h('div', {}, h('a', { href: '#/deployments/' + a.deployment_id }, 'rollout')) : null),
        h('td', { class: 'nowrap' }, ago(a.started_at), a.resolved_at ? h('div', { class: 'muted' }, 'resolved ' + ago(a.resolved_at)) : null))),
      'No alerts yet.'));
    const del = (path, msg) => async () => { if (confirm(msg) && await guard(() => api('DELETE', path)) !== undefined) refresh(); };
    replace(monitorsBox, table(['Monitor', 'Check', 'Devices', ''],
      monitors.map(m => h('tr', {},
        h('td', {}, h('strong', {}, m.name), ' ', badge(m.enabled ? 'enabled' : 'disabled')),
        h('td', { class: 'mono' }, m.check.type + ' ' + (m.check.target || (m.check.command || []).join(' '))),
        h('td', {}, labelChips(m.selector.labels)),
        h('td', {}, h('div', { class: 'btn-row' },
          h('button', { class: 'btn small', onclick: async () => {
            if (await guard(() => api('PUT', '/api/v1/monitors/' + m.id, { ...m, enabled: !m.enabled }))) refresh();
          } }, m.enabled ? 'Disable' : 'Enable'),
          h('button', { class: 'btn small danger', onclick: del('/api/v1/monitors/' + m.id, `Delete monitor ${m.name}?`) }, 'Delete'))))),
      'No program monitors yet.'));
    const condLabel = { health: 'health', device_offline: 'offline', update_failed: 'update failed', deployment_paused: 'rollout paused' };
    replace(rulesBox, table(['Rule', 'When', 'Devices', 'Notify', ''],
      rules.map(r => h('tr', {},
        h('td', {}, h('strong', {}, r.name), ' ', badge(r.enabled ? 'enabled' : 'disabled')),
        h('td', {}, condLabel[r.condition] || r.condition,
          r.condition === 'health' ? h('div', { class: 'muted' }, `${r.min_severity || 'critical'}+${(r.check_kinds || []).length ? ' · ' + r.check_kinds.join(', ') : ''}`) : null,
          r.condition === 'device_offline' ? h('div', { class: 'muted' }, `${r.offline_minutes || 10} min`) : null),
        h('td', {}, labelChips(r.selector.labels)),
        h('td', {}, r.channels.map(id => chName[id] || id).join(', ')),
        h('td', {}, h('div', { class: 'btn-row' },
          h('button', { class: 'btn small', onclick: async () => {
            if (await guard(() => api('PUT', '/api/v1/alert-rules/' + r.id, { ...r, enabled: !r.enabled }))) refresh();
          } }, r.enabled ? 'Disable' : 'Enable'),
          h('button', { class: 'btn small danger', onclick: del('/api/v1/alert-rules/' + r.id, `Delete rule ${r.name}?`) }, 'Delete'))))),
      'No alert rules yet.'));
    replace(rChannels, chs.length ? chs.map(c => h('label', { class: 'check chip' }, h('input', { type: 'checkbox', value: c.id }), c.name))
      : h('span', { class: 'muted' }, 'No channels yet — add one below.'));
    replace(channelsBox, table(['Channel', 'Type', 'Destination', ''],
      chs.map(c => h('tr', {},
        h('td', {}, h('strong', {}, c.name)), h('td', {}, c.type),
        h('td', { class: 'mono' }, c.webhook ? c.webhook.url : c.slack ? c.slack.webhook_url : c.email ? c.email.to.join(', ') : ''),
        h('td', {}, h('div', { class: 'btn-row' },
          h('button', { class: 'btn small', onclick: () => guard(() => api('POST', `/api/v1/channels/${c.id}/test`), 'Test notification sent') }, 'Send test'),
          h('button', { class: 'btn small danger', onclick: del('/api/v1/channels/' + c.id, `Delete channel ${c.name}?`) }, 'Delete'))))),
      'No channels yet.'));
  }
  await guard(refresh);
  return async () => { if (!root.contains(document.activeElement) || document.activeElement === document.body) await refresh(); };
}

// ---------- history charts ----------

const HISTORY_RANGES = [['24h', '24 hours'], ['7d', '7 days'], ['30d', '30 days'], ['90d', '90 days']];
const METRIC_SERIES = new Set(['load', 'memory used', 'disk used', 'signal']);
const KIND_ORDER = ['temperature', 'storage', 'disk_space', 'memory', 'cpu', 'battery', 'power', 'http', 'tcp', 'radio'];

function rangePicker(current, onChange) {
  const wrap = h('div', { class: 'seg', role: 'group', 'aria-label': 'Time range' });
  for (const [v, label] of HISTORY_RANGES) {
    const b = h('button', { type: 'button', 'aria-pressed': v === current ? 'true' : 'false', class: v === current ? 'on' : '', title: 'Last ' + label }, v);
    b.addEventListener('click', () => {
      wrap.querySelectorAll('button').forEach(x => { x.classList.toggle('on', x === b); x.setAttribute('aria-pressed', x === b ? 'true' : 'false'); });
      onChange(v);
    });
    wrap.append(b);
  }
  return wrap;
}

function statusLegend() {
  return h('div', { class: 'legend' }, ['ok', 'warning', 'critical', 'unknown'].map(s =>
    h('span', {}, h('span', { class: 'sw ' + s }), Charts.statusLabel(s))), h('span', {}, h('span', { class: 'sw unknown-gap' }), 'gap = no data'));
}

// Re-render charts when the window is resized, while their element is on the page.
function onResize(el, fn) {
  let timer;
  const handler = () => {
    if (!document.contains(el)) { window.removeEventListener('resize', handler); return; }
    clearTimeout(timer);
    timer = setTimeout(fn, 150);
  };
  window.addEventListener('resize', handler);
}

// A chart card with a chart/table toggle (the table keeps every value readable without hovering).
function chartCard(title, nowText, draw, rows, columns) {
  const body = h('div', { class: 'viz' });
  let showTable = false;
  const toggle = h('button', { class: 'btn ghost small', type: 'button' }, 'Table');
  const render = () => {
    if (showTable) {
      replace(body, h('div', { class: 'table-scroll' }, h('table', { class: 'data-table' },
        h('thead', {}, h('tr', {}, columns.map(c => h('th', {}, c)))),
        h('tbody', {}, rows().map(r => h('tr', {}, r.map(c => h('td', {}, c))))))));
    } else draw(body);
  };
  toggle.addEventListener('click', () => { showTable = !showTable; toggle.textContent = showTable ? 'Chart' : 'Table'; render(); });
  const card = h('div', { class: 'card chart-card' },
    h('div', { class: 'chart-head' }, h('h3', {}, title), h('div', { class: 'btn-row' }, nowText ? h('span', { class: 'now' }, nowText) : null, toggle)),
    body);
  return { card, render };
}

function historySection(deviceId) {
  let range = '24h', data = null, fetchedAt = 0;
  const charts = h('div', { class: 'grid cols-2' });
  const timeline = h('div', { class: 'card' });
  const status = h('span', { class: 'muted' });
  const el = h('section', { class: 'mt', 'aria-label': 'History' },
    h('div', { class: 'page-head' }, h('div', {}, h('h2', {}, 'History'), status),
      h('div', { class: 'filters' }, rangePicker(range, v => { range = v; load(true); }),
        h('button', { class: 'btn small', type: 'button', onclick: () => data && downloadCSV(`${deviceId}-history-${range}.csv`, data.series, deviceId) }, 'Download CSV'))),
    charts, h('div', { class: 'spacer' }), timeline);
  let renderers = [];

  function draw() {
    if (!data) return;
    const from = Math.floor(new Date(data.from).getTime() / 1000), to = Math.floor(new Date(data.to).getTime() / 1000);
    const res = data.resolution_seconds;
    replace(status, res >= 3600 ? 'hourly averages, shaded band = min–max' : `one sample every ${Math.round(res / 60)} min`);
    const numeric = data.series.filter(s => s.numeric && s.points.some(p => p.v !== null))
      .sort((a, b) => (KIND_ORDER.indexOf(a.kind) + 1 || 99) - (KIND_ORDER.indexOf(b.kind) + 1 || 99) || a.name.localeCompare(b.name));
    renderers = [];
    replace(charts, numeric.length ? numeric.map(s => {
      const last = [...s.points].reverse().find(p => p.v !== null);
      const unit = s.unit && s.unit !== 'load avg' ? s.unit : '';
      const c = chartCard(s.name, last ? ['now ', h('strong', {}, Charts.fmtNum(last.v)), unit ? ' ' + unit : ''] : null,
        body => Charts.line(body, { from, to, resolution: res, unit, title: s.name, series: [{ name: s.name, cls: 's1', points: s.points }] }),
        () => [...s.points].reverse().map(p => [Charts.timeFull(p.t), p.v === null ? '—' : Charts.fmtNum(p.v),
          p.min != null ? `${Charts.fmtNum(p.min)} – ${Charts.fmtNum(p.max)}` : '', p.s]),
        ['Time', 'Value' + (unit ? ` (${unit})` : ''), res >= 3600 ? 'Min – max' : '', 'Status']);
      renderers.push(c.render);
      return c.card;
    }) : h('div', { class: 'card empty' }, 'No measurements in this range yet.'));
    renderers.forEach(r => r());

    const statusSeries = data.series.filter(s => !METRIC_SERIES.has(s.name))
      .sort((a, b) => (a.name === 'online' ? -1 : b.name === 'online' ? 1 : a.name.localeCompare(b.name)));
    const strips = [];
    replace(timeline, h('div', { class: 'chart-head' }, h('h3', {}, 'Status over time'), statusLegend()),
      statusSeries.length ? statusSeries.map(s => {
        const cell = h('div', { class: 'strip-cell' });
        strips.push(() => Charts.strip(cell, { from, to, resolution: res, title: s.name, points: s.points }));
        const pct = s.availability == null ? '—' : (s.availability * 100).toFixed(s.availability >= 0.999 ? 0 : 1) + '%';
        return h('div', { class: 'timeline-row' },
          h('div', { class: 'mono small', title: s.name }, s.name),
          cell,
          h('div', { class: 'pct', title: s.name === 'online' ? 'share of the range the device was checking in' : 'share of samples that were ok' },
            pct + (s.name === 'online' ? ' up' : ' ok')));
      }) : h('div', { class: 'empty' }, 'No health history yet.'));
    strips.forEach(f => f());
    renderers.push(...strips);
  }

  async function load(force) {
    if (!force && Date.now() - fetchedAt < 60_000) return;
    fetchedAt = Date.now();
    charts.classList.add('loading');
    data = await api('GET', `/api/v1/devices/${deviceId}/history?range=${range}`);
    charts.classList.remove('loading');
    draw();
  }
  onResize(el, () => renderers.forEach(r => r()));
  return { el, load: () => guard(() => load(false)) };
}

function fleetHistorySection() {
  let range = '24h', data = null, fetchedAt = 0, cmp = null;
  const grid = h('div', { class: 'grid cols-2' });
  const checkSel = h('select', { 'aria-label': 'Compare check' }, h('option', { value: '' }, 'Compare a check across devices…'));
  const cmpLabels = h('input', { placeholder: 'labels, e.g. site=sg', 'aria-label': 'Compare labels', size: '16' });
  const cmpBox = h('div', {});
  const el = h('section', { class: 'mt', 'aria-label': 'Fleet history' },
    h('div', { class: 'page-head' }, h('h2', {}, 'Fleet history'),
      h('div', { class: 'filters' }, rangePicker(range, v => { range = v; load(true); }), checkSel, cmpLabels)),
    grid, h('div', { class: 'spacer' }), cmpBox);
  checkSel.addEventListener('change', () => loadCompare());
  cmpLabels.addEventListener('change', () => loadCompare());
  async function loadCompare() {
    if (!checkSel.value) { cmp = null; replace(cmpBox); return; }
    let labels;
    try { labels = parseLabels(cmpLabels.value); } catch (err) { return toast(err.message, true); }
    const q = new URLSearchParams({ check: checkSel.value, range });
    for (const [k, v] of Object.entries(labels)) q.append('label', k + '=' + v);
    cmp = await guard(() => api('GET', '/api/v1/history/compare?' + q));
    drawCompare();
  }
  function drawCompare() {
    if (!cmp) return;
    const from = Math.floor(new Date(cmp.from).getTime() / 1000), to = Math.floor(new Date(cmp.to).getTime() / 1000);
    const series = cmp.series.map((s, i) => ({ name: s.device_name, label: s.device_name, cls: 'c' + (i + 1), points: s.points, src: s }));
    const unit = cmp.series[0]?.unit || '';
    const numeric = cmp.series.some(s => s.numeric);
    const c = chartCard(`${cmp.check} — by device`, cmp.matching > series.length ? `${series.length} of ${cmp.matching} devices, worst first` : `${series.length} device${series.length === 1 ? '' : 's'}`,
      body => {
        if (!series.length) { replace(body, h('div', { class: 'viz-empty' }, 'No devices match.')); return; }
        if (numeric) {
          Charts.line(body, { from, to, resolution: cmp.resolution_seconds, unit, endLabels: series.length <= 4, title: cmp.check, series });
        } else {
          replace(body, ...series.map(s => {
            const cell = h('div', { class: 'strip-cell' });
            setTimeout(() => Charts.strip(cell, { from, to, resolution: cmp.resolution_seconds, title: s.name, points: s.points }));
            return h('div', { class: 'timeline-row' }, h('div', { class: 'small' }, s.name), cell,
              h('div', { class: 'pct' }, s.src.availability == null ? '—' : (s.src.availability * 100).toFixed(1) + '% ok'));
          }));
        }
        body.prepend(h('div', { class: 'legend mb' }, series.map(s => h('span', {}, h('span', { class: 'sw line ' + s.cls }),
          h('a', { href: '#/devices/' + s.src.device_id }, s.name))),
          h('button', { class: 'btn ghost small', type: 'button', onclick: () => downloadCSV(`compare-${cmp.check}-${range}.csv`, cmp.series) }, 'Download CSV')));
      },
      () => series.flatMap(s => [...s.points].reverse().map(p => [s.name, Charts.timeFull(p.t), p.v == null ? '—' : Charts.fmtNum(p.v), p.s])),
      ['Device', 'Time', 'Value' + (unit ? ` (${unit})` : ''), 'Status']);
    replace(cmpBox, c.card);
    c.render();
    renderers.push(c.render);
  }
  let renderers = [];
  function draw() {
    if (!data) return;
    const from = Math.floor(new Date(data.from).getTime() / 1000), to = Math.floor(new Date(data.to).getTime() / 1000);
    const res = data.resolution_seconds;
    const pts = data.points;
    // Buckets average device counts; one decimal is plenty (and "now" is a whole number).
    const pick = k => pts.map(p => ({ t: p.t, v: Math.round(p[k] * 10) / 10 }));
    const last = pts[pts.length - 1];
    const rows = cols => () => [...pts].reverse().map(p => [Charts.timeFull(p.t), ...cols.map(k => Charts.fmtNum(Math.round(p[k] * 10) / 10))]);
    const online = chartCard('Devices online', last ? ['now ', h('strong', {}, String(Math.round(last.online))), ` of ${Math.round(last.online + last.offline)}`] : null,
      body => Charts.line(body, { from, to, resolution: res, zeroBased: true, title: 'Devices online', series: [{ name: 'online', cls: 's1', points: pick('online') }] }),
      rows(['online', 'offline']), ['Time', 'Online', 'Offline']);
    const health = chartCard('Devices with health problems', null,
      body => {
        Charts.line(body, { from, to, resolution: res, zeroBased: true, endLabels: true, title: 'Devices with health problems', series: [
          { name: 'critical', label: 'critical', cls: 'critical', points: pick('critical') },
          { name: 'warning', label: 'warning', cls: 'warning', points: pick('warning') }] });
        body.prepend(h('div', { class: 'legend mb' }, h('span', {}, h('span', { class: 'sw line critical' }), '✕ critical'),
          h('span', {}, h('span', { class: 'sw line warning' }), '▲ warning')));
      },
      rows(['critical', 'warning', 'alerts']), ['Time', 'Critical', 'Warning', 'Firing alerts']);
    renderers = [online.render, health.render];
    replace(grid, online.card, health.card);
    renderers.forEach(r => r());
  }
  async function load(force) {
    if (!force && Date.now() - fetchedAt < 60_000) return;
    fetchedAt = Date.now();
    data = await api('GET', `/api/v1/history/fleet?range=${range}`);
    draw();
    if (checkSel.options.length === 1) {
      const names = await api('GET', '/api/v1/history/checks');
      append(checkSel, names.filter(n => n.name !== 'online').map(n => h('option', { value: n.name }, `${n.name} (${n.devices})`)));
    }
    if (force && checkSel.value) await loadCompare();
    else drawCompare();
  }
  onResize(el, () => renderers.forEach(r => r()));
  return { el, load: () => guard(() => load(false)) };
}

// CSV export of history series (client-side, from the data already loaded).
function downloadCSV(filename, series, fallbackDevice) {
  const esc = v => (v === null || v === undefined ? '' : /[",\n]/.test(String(v)) ? '"' + String(v).replace(/"/g, '""') + '"' : String(v));
  const lines = [['device', 'series', 'unit', 'time', 'value', 'min', 'max', 'status'].join(',')];
  for (const s of series) for (const p of s.points) {
    lines.push([s.device_name || fallbackDevice || '', s.name, s.unit || '', new Date(p.t * 1000).toISOString(), p.v, p.min, p.max, p.s].map(esc).join(','));
  }
  const url = URL.createObjectURL(new Blob([lines.join('\n') + '\n'], { type: 'text/csv' }));
  const a = h('a', { href: url, download: filename.replace(/[^\w.-]+/g, '-').replace(/-+/g, '-') });
  document.body.append(a); a.click(); a.remove();
  setTimeout(() => URL.revokeObjectURL(url), 10000);
}
