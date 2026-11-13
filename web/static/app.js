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
// Who is signed in (GET /api/v1/me). Role and scope only hide UI; the server
// enforces both on every request.
let me = null;
const ROLE_RANK = { viewer: 1, operator: 2, admin: 3 };
const atLeast = role => !!me && ROLE_RANK[me.role] >= ROLE_RANK[role];
const fleetWide = () => !!me && !me.scoped;

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
  if (res.status === 401) { me = null; showLogin('Your session expired or the token is invalid.'); throw new APIError(401, 'unauthorized'); }
  if (!res.ok) {
    let msg = res.statusText, body = null;
    try { body = await res.json(); msg = body.error || msg; } catch { /* not JSON */ }
    if (body && body.two_factor_setup_required && location.hash !== '#/account') location.hash = '#/account';
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

// downloadReport saves a report in a file format (csv or html).
async function downloadReport(query, format) {
  const res = await fetch(`/api/v1/reports?${query}&format=${format}`, { headers: { Authorization: 'Bearer ' + token } });
  if (!res.ok) {
    let msg = 'download failed (HTTP ' + res.status + ')';
    try { msg = (await res.json()).error || msg; } catch { /* not JSON */ }
    throw new APIError(res.status, msg);
  }
  const name = (/filename="([^"]+)"/.exec(res.headers.get('Content-Disposition') || '') || [])[1] || 'report.' + format;
  const url = URL.createObjectURL(await res.blob());
  const a = h('a', { href: url, download: format === 'html' ? name.replace(/\.csv$/, '.html') : name });
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
  queued: '', sent: 'info', rejected: 'warn', 'no access': 'bad', awaiting_approval: 'warn', live: 'ok', scheduled: 'info', applied: 'ok', removing: 'info', revoked: 'bad', active: 'ok', attested: 'ok', 'attested (unlocked)': 'warn',
  maintenance: 'info', acked: 'info', waiting: '', ended: '',
  spare: '', repair: 'warn', retired: '', skipped: '', applied: 'ok',
  requested: 'info', csr_ready: 'warn', installing: 'info', installed: 'ok', cancelled: '',
};
const LIFECYCLE_TEXT = { active: 'in service', spare: 'spare', repair: 'in repair', retired: 'retired' };
function lifecycleBadge(d) {
  if (!d.lifecycle || d.lifecycle === 'active') return null;
  const b = badge(d.lifecycle);
  b.lastChild.textContent = LIFECYCLE_TEXT[d.lifecycle];
  const li = d.lifecycle_info;
  if (li) b.title = `${LIFECYCLE_TEXT[d.lifecycle]} since ${new Date(li.changed_at).toLocaleString()}${li.changed_by ? ' by ' + li.changed_by : ''}${li.note ? ': ' + li.note : ''}`;
  return b;
}
// untilText describes when a maintenance ends ("until 14:00", "until Nov 20").
function untilText(ts) { return ts ? 'until ' + fmtTime(ts).replace(/:\d\d$/, '') : 'open-ended'; }
function maintBadge(mi) {
  if (!mi) return null;
  const what = [mi.alerts_held && 'alerts held', mi.updates_paused && 'updates paused'].filter(Boolean).join(', ');
  const b = badge('maintenance');
  b.title = `${mi.name} ${untilText(mi.until)} (${what})${mi.note ? ' — ' + mi.note : ''}`;
  return b;
}
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
function scheduleLine(d) {
  const st = d.strategy;
  const parts = [];
  if (d.status === 'scheduled' && st.start_at) parts.push('Starts ' + new Date(st.start_at).toLocaleString());
  if (st.windows || (st.days || []).length) {
    parts.push('Offers updates and advances only ' + (st.windows || 'all day') + ((st.days || []).length ? ' on ' + st.days.join(', ') : '') + ' (' + (st.time_zone || 'UTC') + ')');
  }
  return parts.length ? h('div', { class: 'muted mt' }, parts.join(' · ')) : null;
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
  [/^#\/bulk\/([\w-]+)$/, 'devices', viewBulk, 'operator'],
  [/^#\/deployments$/, 'deployments', viewDeployments, 'fleet'],
  [/^#\/deployments\/([\w-]+)$/, 'deployments', viewDeployment, 'fleet'],
  [/^#\/releases$/, 'releases', viewReleases, 'fleet'],
  [/^#\/enroll$/, 'enroll', viewEnroll, 'operator'],
  [/^#\/apps$/, 'apps', viewApps, 'fleet'],
  [/^#\/monitoring$/, 'monitoring', viewMonitoring],
  [/^#\/config$/, 'config', viewConfig, 'fleet'],
  [/^#\/activity$/, 'activity', viewActivity],
  [/^#\/reports$/, 'reports', viewReports],
  [/^#\/certificates$/, 'certificates', viewCertificates],
  [/^#\/users$/, 'users', viewUsers, 'admin'],
  [/^#\/account$/, 'account', viewAccount],
];
const allowed = need => !need || (need === 'fleet' ? fleetWide() : atLeast(need));

async function route() {
  if (!token) return showLogin();
  if (!me && await guard(loadMe) === undefined) return;
  clearInterval(refreshTimer);
  currentRefresh = null;
  const hash = location.hash || '#/';
  for (const [re, nav, view, need] of routes) {
    const m = hash.match(re);
    if (!m) continue;
    if (!allowed(need)) break;
    if (me.two_factor_setup_required && nav !== 'account') { location.hash = '#/account'; return; }
    document.querySelectorAll('#nav a').forEach(a => a.classList.toggle('active', a.dataset.route === nav));
    const root = $('#view');
    root.replaceChildren();
    root.classList.toggle('fleet-page', need === 'fleet');
    root.classList.remove('device-readonly');
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
  fleetHist.el.classList.add('fleet-only');
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
        h('div', { class: 'card fleet-only' }, h('h2', {}, 'Active deployments'), deps),
        h('div', { class: 'card' }, h('h2', {}, 'Installed versions'), dist)),
      h('div', { class: 'card flush' }, h('h2', {}, 'Recent activity'), feed)));

  async function refresh() {
    if (fleetWide()) fleetHist.load();
    const [sum, active, events] = await Promise.all([
      api('GET', '/api/v1/summary'),
      fleetWide() ? api('GET', '/api/v1/deployments?status=scheduled,running,paused') : [],
      api('GET', '/api/v1/events?limit=15'),
    ]);
    const tile = (label, value, hint, href) => h(href ? 'a' : 'div', { class: 'card stat', href },
      h('div', { class: 'label' }, label), h('div', { class: 'value' }, value), h('div', { class: 'hint' }, hint));
    const inService = sum.online + sum.offline;
    const pct = inService ? Math.round(100 * sum.online / inService) : 0;
    const outOfService = [sum.spare && `${sum.spare} spare`, sum.in_repair && `${sum.in_repair} in repair`].filter(Boolean).join(' · ');
    replace(stats,
      tile('Devices', sum.devices, [Object.entries(sum.channels).map(([c, n]) => `${n} ${c}`).join(' · ') || 'none enrolled',
        sum.peripherals ? ` · ${sum.peripherals} peripherals` : '', outOfService ? ' · ' + outOfService : ''].join(''), '#/devices'),
      tile('Online', sum.online, inService ? `${pct}% of active · ${sum.offline} offline` : '—', '#/devices'),
      tile('Health issues', sum.health_critical + sum.health_warning,
        sum.health_critical + sum.health_warning ? `${sum.health_critical} critical · ${sum.health_warning} warning` : 'all healthy', '#/devices'),
      tile('Firing alerts', sum.firing_alerts, sum.firing_alerts ? 'see Monitoring' : 'nothing firing', '#/monitoring'),
      fleetWide() ? tile('Active rollouts', sum.active_deployments, `${sum.releases} releases available`, '#/deployments') : null);

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
  const lifeSel = h('select', { 'aria-label': 'Lifecycle filter' }, [['', 'In the fleet'], ['active', 'In service'], ['spare', 'Spares'],
    ['repair', 'In repair'], ['retired', 'Retired'], ['all', 'All, with retired']].map(([v, l]) => h('option', { value: v }, l)));
  const list = h('div', { class: 'card flush' });
  const count = h('span', {});
  const selected = new Set();
  const bulk = bulkBar(selected, () => { render(); guard(refresh); });
  const recent = h('div', { class: 'need-operator mt' });
  append(root, pageHead('Devices', count, h('a', { class: 'btn primary need-operator', href: '#/enroll' }, 'Enroll devices')),
    h('div', { class: 'toolbar' }, search, status, healthSel, kindSel, lifeSel), bulk.el, h('div', { class: 'spacer' }), list, recent);
  let devices = [];
  function render() {
    const q = search.value.trim().toLowerCase();
    const names = Object.fromEntries(devices.map(d => [d.id, d.name]));
    const shown = devices.filter(d => (!status.value || d.status === status.value) && (!healthSel.value || d.health === healthSel.value) &&
      (!kindSel.value || (kindSel.value === 'peripheral') === !!d.parent_id) && (!q || [
      d.id, d.name, d.channel, d.info.hostname, d.info.model, d.info.machine_id, ...(d.info.ips || []),
      ...Object.entries(d.labels).map(([k, v]) => k + '=' + v),
    ].some(s => (s || '').toLowerCase().includes(q))));
    replace(count, `${devices.length} ${lifeSel.value === 'retired' ? 'retired' : 'enrolled'} · ${devices.filter(d => d.status === 'online' && d.lifecycle !== 'retired').length} online`);
    // Selection survives refreshes and filtering, but not devices that left the list.
    const present = new Set(devices.map(d => d.id));
    for (const id of [...selected]) if (!present.has(id)) selected.delete(id);
    const allBox = h('input', { type: 'checkbox', 'aria-label': 'Select all shown', checked: shown.length > 0 && shown.every(d => selected.has(d.id)),
      onclick: e => { e.stopPropagation(); for (const d of shown) e.target.checked ? selected.add(d.id) : selected.delete(d.id); render(); } });
    const pick = d => h('td', { class: 'need-operator pick', onclick: e => e.stopPropagation() },
      h('input', { type: 'checkbox', 'aria-label': 'Select ' + d.name, checked: selected.has(d.id),
        onchange: e => { e.target.checked ? selected.add(d.id) : selected.delete(d.id); render(); } }));
    bulk.update(devices);
    replace(list, table([h('span', { class: 'need-operator' }, allBox), 'Device', 'Status', 'Health', 'Channel', 'Platform', 'Packages', 'Labels', 'Last seen'],
      shown.map(d => h('tr', { class: 'clickable' + (selected.has(d.id) ? ' selected' : ''), onclick: () => { location.hash = '#/devices/' + d.id; } },
        pick(d),
        h('td', {}, h('div', {}, h('strong', {}, d.name)), h('div', { class: 'muted mono' }, d.id),
          d.parent_id ? h('div', { class: 'muted' }, 'via ', names[d.parent_id] || d.parent_id) : null,
          d.agent_status ? h('div', { class: 'muted' }, d.agent_status) : null),
        h('td', {}, d.lifecycle === 'retired' ? lifecycleBadge(d) : badge(d.status),
          d.lifecycle !== 'retired' && lifecycleBadge(d) ? h('div', {}, lifecycleBadge(d)) : null,
          d.license_revoked && d.lifecycle !== 'retired' ? h('div', {}, badge('revoked')) : null,
          d.maintenance ? h('div', {}, maintBadge(d.maintenance)) : null),
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
  lifeSel.addEventListener('change', () => guard(refresh));
  async function refresh() {
    const [devs, ops] = await Promise.all([
      api('GET', '/api/v1/devices' + (lifeSel.value ? '?lifecycle=' + lifeSel.value : '')),
      atLeast('operator') ? api('GET', '/api/v1/bulk') : []]);
    devices = devs;
    render();
    replace(recent, ops.length ? [h('h2', {}, 'Recent bulk operations'), h('div', { class: 'card flush' }, bulkTable(ops.slice(0, 5)))] : null);
  }
  await guard(refresh);
  search.focus();
  return refresh;
}

// ---------- bulk operations ----------

function bulkTable(ops) {
  return table(['Operation', 'Devices', 'Progress', 'By', 'When'], ops.map(b => h('tr', { class: 'clickable', onclick: () => { location.hash = '#/bulk/' + b.id; } },
    h('td', {}, h('strong', {}, b.summary)),
    h('td', {}, `${b.applied.length}`, b.skipped.length ? h('span', { class: 'muted' }, ` (${b.skipped.length} skipped)`) : null),
    h('td', {}, progressText(b.progress) || h('span', { class: 'muted' }, 'done')),
    h('td', {}, b.created_by || '—'),
    h('td', { class: 'nowrap' }, ago(b.created_at)))));
}
function progressText(p) {
  if (!p) return '';
  return Object.entries(p).sort(([a], [b]) => a.localeCompare(b)).map(([k, n]) => `${n} ${k}`).join(' · ');
}

// bulkBar acts on the selected devices: preview with a dry run, confirm,
// then apply with the number of devices the preview matched.
function bulkBar(selected, done) {
  const fleetOp = !!me && (ROLE_RANK[me.fleet_role] || 0) >= ROLE_RANK.operator;
  const ops = [
    ['reboot', 'Reboot'], ['restart-agent', 'Restart agent'], ['wake', 'Check in now'], ['lock', 'Lock screen (Android)'],
    fleetOp && ['exec', 'Run command…'], ['set_labels', 'Add labels…'], ['remove_labels', 'Remove labels…'], ['set_channel', 'Set channel…'],
    ['maintenance', 'Start maintenance…'],
    fleetOp && ['lifecycle:spare', 'Mark as spare'], fleetOp && ['lifecycle:repair', 'Send for repair'],
    fleetOp && ['lifecycle:active', 'Put in service'], fleetOp && ['lifecycle:retired', 'Retire…'],
  ].filter(Boolean);
  const opSel = h('select', { 'aria-label': 'Bulk operation' }, ops.map(([v, l]) => h('option', { value: v }, l)));
  const arg = h('input', { 'aria-label': 'Bulk operation value' });
  const dur = h('select', { 'aria-label': 'Maintenance duration' },
    [['1', '1 hour'], ['2', '2 hours'], ['4', '4 hours'], ['8', '8 hours'], ['24', '1 day'], ['72', '3 days']].map(([v, l]) => h('option', { value: v }, l)));
  const label = h('strong', {});
  const placeholders = { exec: 'systemctl restart myapp', set_labels: 'floor=2 zone=east', remove_labels: 'floor zone', set_channel: 'beta', maintenance: 'note (optional)' };
  const sync = () => {
    const op = opSel.value;
    arg.classList.toggle('hidden', !(op in placeholders));
    arg.placeholder = placeholders[op] || '';
    dur.classList.toggle('hidden', op !== 'maintenance');
  };
  opSel.addEventListener('change', sync);
  const run = async () => {
    const ids = [...selected], op = opSel.value, v = arg.value.trim();
    if (op === 'maintenance') {
      const ends = new Date(Date.now() + parseInt(dur.value, 10) * 3600e3).toISOString();
      if (!confirm(`Start maintenance on ${ids.length} devices? Their alerts are held and updates paused.`)) return;
      if (await guard(() => api('POST', '/api/v1/maintenance', { selector: { device_ids: ids }, ends_at: ends, note: v }), 'Maintenance started')) { arg.value = ''; done(); }
      return;
    }
    const req = { selector: { device_ids: ids } };
    try {
      if (op.startsWith('lifecycle:')) Object.assign(req, { operation: 'lifecycle', state: op.slice(10) });
      else if (op === 'exec') {
        const argv = v.match(/"[^"]*"|'[^']*'|\S+/g);
        if (!argv) throw new Error('Type the command to run');
        Object.assign(req, { operation: 'exec', args: argv.map(a => a.replace(/^(["'])(.*)\1$/, '$2')) });
      } else if (op === 'set_labels') Object.assign(req, { operation: op, labels: parseLabels(v) });
      else if (op === 'remove_labels') Object.assign(req, { operation: op, label_keys: v.split(/[\s,]+/).filter(Boolean) });
      else if (op === 'set_channel') Object.assign(req, { operation: op, channel: v });
      else req.operation = op;
    } catch (err) { return toast(err.message, true); }
    const prev = await guard(() => api('POST', '/api/v1/bulk', { ...req, dry_run: true }));
    if (!prev) return;
    const skipped = prev.skipped.slice(0, 6).map(t => `• ${t.name}: ${t.reason}`).join('\n') + (prev.skipped.length > 6 ? `\n• …and ${prev.skipped.length - 6} more` : '');
    if (!prev.applied.length) return toast(`${prev.summary} applies to none of the ${ids.length} selected devices${prev.skipped.length ? ': ' + prev.skipped[0].reason : ''}`, true);
    const msg = `${prev.summary} on ${prev.applied.length} of ${ids.length} selected devices.` +
      (prev.skipped.length ? `\n\nSkipped ${prev.skipped.length}:\n${skipped}` : '');
    if (req.state === 'retired') {
      const typed = prompt(msg + `\n\nRetiring is final: their keys are destroyed and licenses revoked. Type ${prev.applied.length} to confirm:`);
      if (typed === null) return;
      if (typed.trim() !== String(prev.applied.length)) return toast('The number did not match; nothing changed.', true);
    } else if (!confirm(msg + '\n\nGo ahead?')) return;
    const b = await guard(() => api('POST', '/api/v1/bulk', { ...req, expect: prev.matched }));
    if (!b) return;
    toast(`${b.summary}: ${b.applied.length} devices${b.skipped.length ? `, ${b.skipped.length} skipped` : ''}`);
    arg.value = '';
    selected.clear();
    if (b.progress) location.hash = '#/bulk/' + b.id;
    else done();
  };
  const el = h('div', { class: 'bulk-bar need-operator hidden' }, label, opSel, arg, dur,
    h('button', { class: 'btn primary', onclick: run }, 'Apply…'),
    h('button', { class: 'btn', onclick: () => { selected.clear(); done(); } }, 'Clear selection'));
  sync();
  return { el, update: () => {
    el.classList.toggle('hidden', selected.size === 0);
    replace(label, `${selected.size} selected`);
  } };
}

async function viewBulk(root, id) {
  const body = h('div', {});
  append(root, h('div', { class: 'crumb' }, h('a', { href: '#/devices' }, 'Devices'), ' / bulk operation'), body);
  async function refresh() {
    const b = await api('GET', '/api/v1/bulk/' + id);
    const rows = [
      ...b.applied.map(t => [t, t.status ? badge(t.status) : badge('applied')]),
      ...b.skipped.map(t => [t, h('span', {}, badge('skipped'), h('div', { class: 'muted' }, t.reason))]),
    ];
    replace(body, pageHead(b.summary, `${b.applied.length} devices${b.skipped.length ? ` · ${b.skipped.length} skipped` : ''} · by ${b.created_by || '—'} · ${ago(b.created_at)}` +
      (b.progress ? ' · ' + progressText(b.progress) : '')),
      h('div', { class: 'card flush' }, table(['Device', 'Result'], rows.map(([t, res]) => h('tr', {},
        h('td', {}, h('a', { href: '#/devices/' + t.id }, t.name), h('div', { class: 'muted mono' }, t.id)), h('td', {}, res))))));
  }
  await guard(refresh);
  return refresh;
}

async function viewDevice(root, id) {
  const head = h('div', {});
  const info = h('div', { class: 'card' });
  const meta = h('div', { class: 'card need-operator' });
  const actions = h('div', { class: 'card need-operator' });
  const lifeCard = h('div', { class: 'card fleet-only need-operator' });
  const updates = h('div', { class: 'card flush' });
  const actionLog = h('div', { class: 'card flush' });
  const events = h('div', { class: 'card flush' });
  const healthCard = h('div', { class: 'card flush' });
  const periphCard = h('div', { class: 'card flush hidden' });
  const logs = logsCard(id, () => dev);
  const configCard = h('div', { class: 'card flush hidden fleet-only' });
  const certCard = h('div', { class: 'card flush hidden' });
  const hist = historySection(id);
  append(root, head,
    h('div', { class: 'grid side' },
      h('div', { class: 'stack' }, info, healthCard, periphCard, configCard, certCard, logs.el, updates, actionLog),
      h('div', { class: 'stack' }, meta, actions, lifeCard, events)),
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
  const execBox = h('div', {},
    h('form', { class: 'inline mt', onsubmit: e => {
      e.preventDefault();
      const argv = execIn.value.match(/"[^"]*"|'[^']*'|\S+/g);
      if (!argv) return;
      queue('exec', argv.map(a => a.replace(/^(["'])(.*)\1$/, '$2'))).then(() => { execIn.value = ''; });
    } }, execIn, h('button', { class: 'btn', type: 'submit' }, 'Run')),
    h('p', { class: 'muted' }, 'Commands run without a shell, only on devices with allow_exec enabled.'));
  const androidBox = h('div', { class: 'hidden' },
    h('div', { class: 'btn-row mt' },
      h('button', { class: 'btn', onclick: () => queue('lock') }, 'Lock screen'),
      h('button', { class: 'btn danger', onclick: () => {
        const name = dev ? dev.name : id;
        const typed = prompt(`Factory-reset ${name}? Everything on it is erased and it leaves the fleet until it is set up again with the QR code.\n\nType the device name to confirm:`);
        if (typed === null) return;
        if (typed.trim() !== name) return toast('The name did not match; nothing was wiped.', true);
        queue('wipe', ['confirm=' + id]);
      } }, 'Wipe device')),
    h('p', { class: 'muted' }, 'Wipe is for lost or stolen devices: it erases everything.'));
  // Lifecycle: spare, repair, replace (RMA), retire. Fleet operators only.
  const lifeNow = h('div', {});
  const lifeButtons = h('div', { class: 'btn-row' });
  const replSel = h('select', { 'aria-label': 'Replacement device' });
  const replOld = h('select', { 'aria-label': 'Old device becomes' },
    [['repair', 'In repair'], ['spare', 'Spare'], ['retired', 'Retired']].map(([v, l]) => h('option', { value: v }, l)));
  const replNote = h('input', { placeholder: 'e.g. RMA 4411 (optional)', 'aria-label': 'Replacement note' });
  const replForm = h('form', { class: 'stack hidden mt', onsubmit: async e => {
    e.preventDefault();
    if (!replSel.value) return toast('Pick the replacement device', true);
    const name = replSel.selectedOptions[0].textContent;
    if (replOld.value === 'retired' && !confirm(`Retire ${dev.name} as part of the swap? Its key is destroyed and it can't come back.`)) return;
    const r = await guard(() => api('POST', `/api/v1/devices/${id}/replace`, { replacement_id: replSel.value, old_state: replOld.value, note: replNote.value.trim() }),
      `${name} now replaces ${dev.name}`);
    if (r) location.hash = '#/devices/' + r.id;
  } },
  h('div', { class: 'form-grid' }, h('label', { class: 'wide' }, 'Replacement (takes over name, channel and labels)', replSel),
    h('label', {}, 'This device becomes', replOld), h('label', {}, 'Note', replNote)),
  h('div', { class: 'btn-row' }, h('button', { class: 'btn primary', type: 'submit' }, 'Replace'),
    h('button', { class: 'btn', type: 'button', onclick: () => replForm.classList.add('hidden') }, 'Cancel')));
  append(lifeCard, h('h2', {}, 'Lifecycle'), lifeNow, lifeButtons, replForm,
    h('p', { class: 'muted' }, 'Spares get updates but no alerts. Devices in repair get neither. Retiring is final: the key is destroyed, licenses are revoked, and the record and history are kept.'));
  const setLife = async (state, ask) => {
    const note = prompt(ask + '\n\nNote (optional):', '');
    if (note === null) return;
    if (await guard(() => api('POST', `/api/v1/devices/${id}/lifecycle`, { state, note: note.trim() }), `${dev.name} is now ${LIFECYCLE_TEXT[state]}`)) refresh();
  };
  const openReplace = async () => {
    const all = await guard(() => api('GET', '/api/v1/devices'));
    if (!all) return;
    const cands = all.filter(x => x.id !== id && !!x.parent_id === !!dev.parent_id && (x.lifecycle === 'spare' || x.lifecycle === 'active'))
      .sort((a, b) => (a.lifecycle === 'spare' ? 0 : 1) - (b.lifecycle === 'spare' ? 0 : 1) || a.name.localeCompare(b.name));
    replace(replSel, h('option', { value: '' }, cands.length ? 'Choose…' : 'No spare or active devices to use'),
      cands.map(x => h('option', { value: x.id }, `${x.name}${x.lifecycle === 'spare' ? ' (spare)' : ''} · ${x.id}`)));
    replForm.classList.remove('hidden');
  };
  function renderLifecycle(d) {
    const li = d.lifecycle_info;
    const link = (label, other) => other ? h('div', {}, label, ' ', h('a', { href: '#/devices/' + other }, other)) : null;
    replace(lifeNow, h('div', { class: 'mb' },
      h('div', {}, lifecycleBadge(d) || badge('active'), li ? h('span', { class: 'muted' }, ` since ${ago(li.changed_at)}${li.changed_by ? ' by ' + li.changed_by : ''}`) : null),
      li && li.note ? h('div', { class: 'muted' }, li.note) : null,
      link('Replaces', li && li.replaces), link('Replaced by', li && li.replaced_by),
      d.lifecycle === 'retired' ? h('p', { class: 'muted' }, 'To use this hardware again, enroll it again; it joins as a new device.') : null));
    const b = (label, fn, cls) => h('button', { class: 'btn' + (cls ? ' ' + cls : ''), onclick: fn }, label);
    const retire = b('Retire…', () => {
      const typed = prompt(`Retire ${d.name}? Its key is destroyed (it can never check in again) and its app licenses are revoked. The record and history are kept.\n\nType the device name to confirm:`);
      if (typed === null) return;
      if (typed.trim() !== d.name) return toast('The name did not match; nothing changed.', true);
      guard(() => api('POST', `/api/v1/devices/${id}/lifecycle`, { state: 'retired' }), `${d.name} retired`).then(r => r && refresh());
    }, 'danger');
    const byState = {
      active: [b('Replace…', openReplace), b('Mark as spare', () => setLife('spare', `Mark ${d.name} as a spare? It keeps getting updates but no alerts.`)),
        b('Send for repair', () => setLife('repair', `Send ${d.name} for repair? It gets no updates or alerts until it returns.`)), retire],
      spare: [b('Put in service', () => setLife('active', `Put ${d.name} in service? Alerts apply to it again.`)),
        b('Send for repair', () => setLife('repair', `Send ${d.name} for repair?`)), retire],
      repair: [b('Return to service', () => setLife('active', `Return ${d.name} to service?`)),
        b('Back to spares', () => setLife('spare', `Put ${d.name} back with the spares?`)), retire],
      retired: [],
    };
    replace(lifeButtons, byState[d.lifecycle || 'active']);
    if (d.lifecycle !== 'active') replForm.classList.add('hidden');
  }

  // Maintenance on this device: alerts held and updates paused for a while.
  const maintNow = h('div', {});
  const maintFor = h('select', { 'aria-label': 'Maintenance duration' },
    [['1', '1 hour'], ['2', '2 hours'], ['4', '4 hours'], ['8', '8 hours'], ['24', '1 day'], ['72', '3 days']].map(([v, l]) => h('option', { value: v }, l)));
  const maintNote = h('input', { placeholder: 'what\'s being done (optional)', 'aria-label': 'Maintenance note' });
  const maintForm = h('form', { class: 'stack', onsubmit: async e => {
    e.preventDefault();
    const ends = new Date(Date.now() + parseInt(maintFor.value, 10) * 3600e3).toISOString();
    if (await guard(() => api('POST', '/api/v1/maintenance', { selector: { device_ids: [id] }, ends_at: ends, note: maintNote.value.trim() }),
      'Maintenance started: alerts held and updates paused')) { maintNote.value = ''; refresh(); }
  } }, h('div', { class: 'inline' }, maintFor, maintNote, h('button', { class: 'btn', type: 'submit' }, 'Start maintenance')));
  const maintBox = h('div', {}, h('h2', {}, 'Maintenance'), maintNow, maintForm,
    h('p', { class: 'muted' }, 'While in maintenance, alerts for this device are held and rollouts skip it. Alerts that are still true afterwards fire then.'));
  const queue = (type, args) => guard(() => api('POST', `/api/v1/devices/${id}/actions`, { type, args }), `${type} queued; runs at next check-in`).then(r => r && refresh());
  append(actions, h('h2', {}, 'Actions'),
    h('div', { class: 'btn-row' },
      h('button', { class: 'btn', title: 'Ask the device to report now instead of at its next heartbeat', onclick: async () => {
        const r = await guard(() => api('POST', `/api/v1/devices/${id}/wake`));
        if (!r) return;
        toast(r.connected ? 'Device is checking in now' : 'Device has no live connection; it checks in at its next heartbeat');
        if (r.connected) setTimeout(() => guard(refresh), 1500);
      } }, 'Check in now'),
      h('button', { class: 'btn', onclick: () => confirm('Reboot this device?') && queue('reboot') }, 'Reboot'),
      h('button', { class: 'btn', onclick: () => queue('restart-agent') }, 'Restart agent')),
    execBox, androidBox,
    h('hr', { class: 'sep' }),
    maintBox,
    h('hr', { class: 'sep' }),
    licenseBox,
    h('button', { class: 'btn danger fleet-only', onclick: async () => {
      if (!confirm(`Remove ${dev ? dev.name : id} from the fleet? Its key stops working immediately.`)) return;
      if (await guard(() => api('DELETE', '/api/v1/devices/' + id), 'Device removed') !== undefined) location.hash = '#/devices';
    } }, 'Remove device'));

  async function refresh() {
    const det = await api('GET', '/api/v1/devices/' + id);
    dev = det.device;
    certCard.classList.toggle('hidden', !(det.certificates || []).length);
    replace(certCard, h('h2', {}, 'Certificates ', h('a', { href: '#/certificates', class: 'muted' }, '(manage)')),
      table(['Service', 'Certificate', 'Expires', 'Renewal'], (det.certificates || []).map(c => h('tr', {},
        h('td', { class: 'mono' }, c.target),
        h('td', {}, c.installed ? [c.installed.common_name, h('div', { class: 'muted' }, c.installed.issuer)] : h('span', { class: 'bad' }, c.error || 'no certificate')),
        h('td', { class: 'nowrap' }, expiryBadge(c)),
        h('td', {}, c.renewal ? renewalBadge(c.renewal) : '—')))));
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
    replace(licenseBox, android || d.attestation ? h('div', { class: 'mb fleet-only' },
      h('h2', {}, 'App licenses'),
      h('p', { class: 'muted' }, d.license_revoked
        ? 'Revoked: this device gets no new licenses, so your apps stop working on it.'
        : d.attestation ? 'Active: apps on this device can get licenses.' : 'This device has no attested hardware key, so it cannot get licenses.'),
      d.license_revoked
        ? h('button', { class: 'btn', onclick: () => setRevoked(false) }, 'Reinstate licenses')
        : d.attestation ? h('button', { class: 'btn danger', onclick: () => setRevoked(true) }, 'Revoke licenses') : null,
      h('hr', { class: 'sep' })) : null);
    maintForm.classList.toggle('hidden', !!d.maintenance);
    replace(maintNow, d.maintenance ? h('div', { class: 'mb' },
      h('p', {}, maintBadge(d.maintenance), ' ', h('strong', {}, d.maintenance.name), ' ', untilText(d.maintenance.until),
        d.maintenance.note ? h('div', { class: 'muted' }, d.maintenance.note) : null),
      h('button', { class: 'btn', onclick: async () => {
        if (await guard(() => api('POST', `/api/v1/maintenance/${d.maintenance.id}/end`), 'Maintenance ended')) refresh();
      } }, 'End maintenance')) : null);
    replace(labelBox, labelChips(d.labels, async k => {
      if (await guard(() => api('PATCH', '/api/v1/devices/' + id, { remove_labels: [k] }))) refresh();
    }));
    root.classList.toggle('device-readonly', (ROLE_RANK[roleForDevice(d)] || 0) < ROLE_RANK.operator);
    renderLifecycle(d);
    actions.classList.toggle('hidden', d.lifecycle === 'retired');
    replace(head, h('div', { class: 'crumb' }, h('a', { href: '#/devices' }, 'Devices'), ' / ', d.id),
      pageHead(d.name, h('span', {}, d.lifecycle === 'retired' ? null : badge(d.status),
        lifecycleBadge(d) ? [' ', lifecycleBadge(d)] : null,
        d.connected ? [' ', h('span', { title: 'Holding a live connection: actions reach it within seconds' }, badge('live'))] : null,
        d.license_revoked ? [' ', badge('revoked')] : null,
        d.maintenance ? [' ', maintBadge(d.maintenance), ' ', untilText(d.maintenance.until), ' ·'] : null, ' last seen ', ago(d.last_seen_at),
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
    logs.el.classList.toggle('hidden', !!d.parent_id || android);
    execBox.classList.toggle('hidden', android);
    androidBox.classList.toggle('hidden', !android);
    configCard.classList.toggle('hidden', !(det.config || []).length);
    replace(configCard, h('h2', {}, 'Configuration'), table(['Setting', 'Profile', 'State'],
      (det.config || []).map(c => h('tr', {},
        h('td', { class: 'mono' }, c.key),
        h('td', {}, c.profile_name ? h('a', { href: '#/config' }, c.profile_name) : h('span', { class: 'muted' }, '—')),
        h('td', {}, badge(c.status), c.message ? h('div', { class: 'muted' }, c.message) : null))), ''));
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

// Remote logs: queue a "logs" action and show the result as soon as the
// device answers (within seconds when it holds a live connection).
function logsCard(id, getDevice) {
  const source = h('select', { 'aria-label': 'Log source' }, h('option', { value: 'journal' }, 'System journal'), h('option', { value: 'file' }, 'Log file'));
  const target = h('input', { placeholder: 'all units, or e.g. myapp.service', 'aria-label': 'Unit or file name' });
  const targetLabel = h('label', {}, 'Unit', target);
  const lines = h('select', { 'aria-label': 'Lines' }, ['100', '200', '1000', '5000'].map(n => h('option', { value: n }, n + ' lines')));
  lines.value = '200';
  const since = h('select', { 'aria-label': 'Since' }, [['', 'Any time'], ['15m', 'Last 15 min'], ['1h', 'Last hour'], ['6h', 'Last 6 hours'], ['24h', 'Last day'], ['168h', 'Last week']].map(([v, l]) => h('option', { value: v }, l)));
  const grep = h('input', { placeholder: 'optional', 'aria-label': 'Only lines containing' });
  const status = h('div', { class: 'muted', role: 'status' });
  const out = h('pre', { class: 'logs hidden', tabindex: '0', 'aria-label': 'Log output' });
  const save = h('button', { class: 'btn small hidden', type: 'button' }, 'Download');
  const submit = h('button', { class: 'btn primary', type: 'submit' }, 'Fetch logs');
  let text = '';
  source.addEventListener('change', () => {
    const file = source.value === 'file';
    targetLabel.firstChild.textContent = file ? 'File name' : 'Unit';
    target.placeholder = file ? 'a name from the agent\'s log_files, e.g. app' : 'all units, or e.g. myapp.service';
    since.disabled = file;
  });
  save.addEventListener('click', () => {
    const url = URL.createObjectURL(new Blob([text], { type: 'text/plain' }));
    const a = h('a', { href: url, download: `${(getDevice() || {}).name || id}-${target.value || source.value}.log`.replace(/[^\w.-]+/g, '-') });
    document.body.append(a); a.click(); a.remove();
    setTimeout(() => URL.revokeObjectURL(url), 10000);
  });
  const form = h('form', { class: 'stack', onsubmit: async e => {
    e.preventDefault();
    const args = [source.value];
    const t = target.value.trim();
    if (t) args.push((source.value === 'file' ? 'name=' : 'unit=') + t);
    else if (source.value === 'file') return toast('Enter the log file name', true);
    args.push('lines=' + lines.value);
    if (since.value && source.value === 'journal') args.push('since=' + since.value);
    if (grep.value.trim()) args.push('grep=' + grep.value.trim());
    const act = await guard(() => api('POST', `/api/v1/devices/${id}/actions`, { type: 'logs', args }));
    if (!act) return;
    submit.disabled = true;
    const d = getDevice();
    const started = Date.now();
    status.textContent = d && d.connected ? 'Asking the device…' : 'Queued. The device has no live connection, so it answers at its next heartbeat.';
    try {
      for (;;) {
        await new Promise(r => setTimeout(r, 700));
        const acts = await api('GET', `/api/v1/devices/${id}/actions`);
        const a = acts.find(x => x.id === act.id);
        if (a && a.status !== 'queued' && a.status !== 'sent') {
          text = a.output || '';
          replace(out, text);
          out.classList.remove('hidden');
          save.classList.toggle('hidden', a.status !== 'succeeded');
          status.textContent = a.status === 'succeeded'
            ? `Received in ${((Date.now() - started) / 1000).toFixed(1)}s · ${Math.max(0, text.split('\n').length - 1)} lines`
            : `The device could not read these logs (${a.status}).`;
          out.scrollTop = out.scrollHeight;
          return;
        }
        if (Date.now() - started > 120_000) {
          status.textContent = 'Still waiting. The result will appear under Actions when the device answers.';
          return;
        }
      }
    } catch (err) {
      if (!(err instanceof APIError && err.status === 401)) status.textContent = err.message;
    } finally {
      submit.disabled = false;
    }
  } },
  h('div', { class: 'form-grid' }, h('label', {}, 'Source', source), targetLabel, h('label', {}, 'Show', lines),
    h('label', {}, 'Since', since), h('label', {}, 'Only lines containing', grep)),
  h('div', { class: 'btn-row' }, submit, save), status);
  const el = h('div', { class: 'card need-operator' }, h('h2', {}, 'Logs'), form, out);
  return { el };
}

function strategyFields() {
  const f = {
    stages: h('input', { value: '5,25,50,100', 'aria-label': 'Stages' }),
    soak: h('input', { value: '15', type: 'number', min: '0', 'aria-label': 'Minutes per stage' }),
    maxFail: h('input', { value: '20', type: 'number', min: '0', max: '100', 'aria-label': 'Max failure %' }),
    minSamples: h('input', { value: '3', type: 'number', min: '1', 'aria-label': 'Min samples' }),
    manual: h('input', { type: 'checkbox' }),
    downgrade: h('input', { type: 'checkbox' }),
    startAt: h('input', { type: 'datetime-local', 'aria-label': 'Start at' }),
    windows: h('input', { placeholder: 'any time, or 22:00-05:00', 'aria-label': 'Rollout windows' }),
    days: h('div', { class: 'btn-row', role: 'group', 'aria-label': 'Days' },
      ['mon', 'tue', 'wed', 'thu', 'fri', 'sat', 'sun'].map(d => h('label', { class: 'check chip' }, h('input', { type: 'checkbox', value: d }), d))),
    tz: h('input', { 'aria-label': 'Time zone' }),
  };
  try { f.tz.value = Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC'; } catch { f.tz.value = 'UTC'; }
  f.nodes = [
    h('label', {}, 'Stages (cumulative %)', f.stages),
    h('label', {}, 'Minutes per stage', f.soak),
    h('label', {}, 'Pause above failure %', f.maxFail),
    h('label', {}, 'Min results before judging', f.minSamples),
    h('label', { class: 'check' }, f.manual, 'Advance stages manually'),
    h('label', { class: 'check' }, f.downgrade, 'Allow downgrade (rollback)'),
    h('label', {}, 'Start at (empty = now, your local time)', f.startAt),
    h('label', {}, 'Only during (HH:MM-HH:MM, comma separated)', f.windows),
    h('label', {}, 'Window time zone', f.tz),
    h('div', { class: 'wide' }, h('div', { class: 'muted mb' }, 'Only on these days (none = every day)'), f.days),
  ];
  f.value = () => ({
    stages: f.stages.value.split(',').map(s => parseInt(s.trim(), 10)).filter(n => !isNaN(n)),
    stage_duration_seconds: Math.round(parseFloat(f.soak.value || '0') * 60),
    auto_advance: !f.manual.checked,
    max_failure_rate: parseFloat(f.maxFail.value || '0') / 100,
    min_samples: parseInt(f.minSamples.value || '1', 10),
    allow_downgrade: f.downgrade.checked,
    start_at: f.startAt.value ? new Date(f.startAt.value).toISOString() : undefined,
    windows: f.windows.value.trim() || undefined,
    days: [...f.days.querySelectorAll('input:checked')].map(i => i.value),
    time_zone: f.windows.value.trim() || f.days.querySelector('input:checked') ? f.tz.value.trim() : undefined,
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
      const d = await guard(() => api('POST', '/api/v1/deployments', body));
      if (d) toast(d.status === 'scheduled' ? 'Deployment scheduled' : 'Deployment started');
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
    h('button', { class: 'btn primary need-operator', onclick: async () => {
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
    if (d.needs_approval) {
      const mine = me && d.created_by === me.name;
      buttons.push(h('button', { class: 'btn primary', disabled: mine || undefined,
        title: mine ? 'Someone other than you must approve a rollout you started' : 'Let this rollout go past its first stage',
        onclick: () => confirm(`Approve ${d.name} (${d.package} ${d.version}), started by ${d.created_by}? It will continue to every targeted device.`) && op('approve', 'Approved') }, 'Approve'));
    }
    if (d.status === 'running') buttons.push(h('button', { class: 'btn', onclick: () => op('pause', 'Paused') }, 'Pause'));
    if (d.status === 'paused') buttons.push(h('button', { class: 'btn primary', onclick: () => op('resume', 'Resumed') }, 'Resume'));
    if (d.status === 'running' && !last && !d.needs_approval) buttons.push(h('button', { class: 'btn', onclick: () => op('advance', 'Advanced') }, 'Advance to ' + d.strategy.stages[d.stage_index + 1] + '%'));
    if ((d.status === 'running' || d.status === 'paused') && (s.failed || s.rolled_back)) buttons.push(h('button', { class: 'btn', onclick: () => op('retry', 'Failed devices will retry') }, 'Retry failed'));
    const open = d.status === 'running' || d.status === 'paused';
    if (open && last) buttons.push(h('button', { class: 'btn', title: 'Stop offering this release, including to devices enrolled later', onclick: () => confirm('Mark this rollout complete? It stops offering the release to devices that have not updated yet.') && op('complete', 'Marked complete') }, 'Complete'));
    if (open) buttons.push(h('button', { class: 'btn danger', onclick: () => confirm('Abort this rollout? Devices that already updated keep the new version.') && op('abort', 'Aborted') }, 'Abort'));
    replace(head, h('div', { class: 'crumb' }, h('a', { href: '#/deployments' }, 'Deployments'), ' / ', d.id),
      pageHead(d.name, h('span', {}, badge(d.status), ' ', d.status_reason || '',
        d.created_by ? h('div', { class: 'muted' }, 'started by ' + d.created_by + (d.approved_by ? ' · approved by ' + d.approved_by : d.needs_approval ? ' · needs approval by someone else to go past the first stage' : '')) : null), ...buttons.map(b => (b.classList.add('need-operator'), b))));

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
          h('dt', {}, 'Stage'), h('dd', {}, d.status === 'scheduled' ? `${currentPercent(d)}% · not started yet` : `${currentPercent(d)}% · started ${ago(d.stage_started_at)}`),
          h('dt', {}, 'Advance'), h('dd', {}, d.strategy.auto_advance ? `automatically every ${duration(d.strategy.stage_duration_seconds)}` : 'manually'),
          h('dt', {}, 'Safety'), h('dd', {}, `pause above ${Math.round(d.strategy.max_failure_rate * 100)}% failures (after ${d.strategy.min_samples} results)`))),
      h('div', { class: 'mt' }, stageStrip(d), scheduleLine(d), h('div', { class: 'spacer' }), progressBar(s, det.targeted)));

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
    h('div', { class: 'card need-operator' }, h('h2', {}, 'Upload a release'),
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
        h('td', {}, h('div', { class: 'btn-row need-operator' },
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
    h('div', { class: 'card need-admin' }, h('h2', {}, 'Register or update an app'),
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
        h('td', {}, h('div', { class: 'btn-row need-admin' },
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
      fleetOperator ? h('label', { class: 'check' }, spareIn, 'Enroll as spares (stock for replacements: updates, no alerts)') : null,
      h('div', {}, h('button', { class: 'btn primary', type: 'submit' }, 'Create token'))),
      h('div', { class: 'mt' }, created)),
    h('div', { class: 'spacer' }), list);

  if ((ROLE_RANK[me.fleet_role] || 0) < ROLE_RANK.operator) {
    list.classList.add('hidden');
    created.before(h('p', { class: 'muted' }, 'Devices enrolled with these tokens are added to your account automatically.'));
    return;
  }
  async function refresh() {
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
      const res = await fetch('/api/v1/certificates/csrs' + (q ? '?' + q : ''), { headers: { Authorization: 'Bearer ' + token } });
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
    bar, h('div', { class: 'spacer' }), list);

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
          h('td', {}, r ? [renewalBadge(r), h('div', { class: 'muted' }, `${r.requested_by || ''} · ${ago(r.updated_at)}`),
            r.message ? h('div', { class: 'bad', title: r.message }, r.message.length > 120 ? r.message.slice(0, 120) + '…' : r.message) : null] : h('span', { class: 'muted' }, '—')),
          h('td', {}, h('div', { class: 'btn-row' },
            r && r.status === 'csr_ready' ? h('button', { class: 'btn small', onclick: () => saveText(`${c.device_name}_${c.target}.csr`, r.csr) }, 'CSR') : null,
            r && ['requested', 'csr_ready'].includes(r.status) ? h('button', { class: 'btn small need-operator', onclick: async () => {
              if (confirm(`Cancel the ${c.target} renewal on ${c.device_name}?`) && await guard(() => api('POST', `/api/v1/certificates/${c.device_id}/${c.target}/cancel`), 'Cancelled')) refresh();
            } }, 'Cancel') : null)));
      }), 'No managed certificates yet. List them in the agent\'s managed_certs on each device (see the README).'));
  }
  search.addEventListener('input', render);
  filter.addEventListener('change', render);
  async function refresh() { certsList = await api('GET', '/api/v1/certificates'); render(); }
  await guard(refresh);
  return async () => { if (!root.contains(document.activeElement) || document.activeElement === document.body) await refresh(); };
}

// ---------- customer reports ----------

async function viewReports(root) {
  const isAdmin = !!me && me.fleet_role === 'admin';
  const fleetStaff = fleetWide();
  const myScopes = ((me && me.grants) || []).filter(g => scopeText(g.scope)).map(g => g.scope);
  const opScopes = ((me && me.grants) || []).filter(g => (ROLE_RANK[g.role] || 0) >= ROLE_RANK.operator && scopeText(g.scope)).map(g => g.scope);
  const canSchedule = isAdmin || opScopes.length > 0;

  // What to report on.
  const who = fleetStaff
    ? h('input', { placeholder: 'empty = whole fleet, or customer=acme', 'aria-label': 'Report for' })
    : h('select', { 'aria-label': 'Report for' }, [myScopes.length > 1 ? h('option', { value: '' }, 'All my devices') : null,
      ...myScopes.map((sc, i) => h('option', { value: String(i) }, scopeText(sc)))]);
  const period = h('select', { 'aria-label': 'Period' }, [['this-month', 'This month so far'], ['last-month', 'Last month'],
    ['last-week', 'Last week'], ['last-7d', 'Last 7 days'], ['last-30d', 'Last 30 days'], ['custom', 'Custom dates']].map(([v, l]) => h('option', { value: v }, l)));
  const fromIn = h('input', { type: 'date', 'aria-label': 'From' });
  const toIn = h('input', { type: 'date', 'aria-label': 'To' });
  const tzIn = h('input', { value: Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC', 'aria-label': 'Time zone', size: 16 });
  const custom = h('span', { class: 'inline hidden' }, fromIn, toIn);
  period.addEventListener('change', () => custom.classList.toggle('hidden', period.value !== 'custom'));
  const out = h('div', {});
  const query = () => {
    const q = new URLSearchParams();
    const labels = fleetStaff ? parseLabels(who.value) : (who.value === '' ? {} : (myScopes[parseInt(who.value, 10)].labels || {}));
    for (const [k, v] of Object.entries(labels)) q.append('label', k + '=' + v);
    if (period.value === 'custom') {
      if (!fromIn.value || !toIn.value) throw new Error('Choose both dates');
      q.set('from', fromIn.value); q.set('to', toIn.value);
    } else q.set('period', period.value);
    if (tzIn.value.trim()) q.set('tz', tzIn.value.trim());
    return q.toString();
  };
  const withQuery = fn => async () => { let q; try { q = query(); } catch (e) { return toast(e.message, true); } await guard(() => fn(q)); };
  const show = withQuery(async q => renderReport(out, await api('GET', '/api/v1/reports?' + q)));
  append(root, pageHead('Reports', 'Availability, alerts, updates and replacements for a client, ready to send'),
    h('div', { class: 'card mb' }, h('div', { class: 'toolbar' },
      h('label', {}, 'For', who), h('label', {}, 'Period', period), custom, h('label', {}, 'Time zone', tzIn),
      h('button', { class: 'btn primary', onclick: show }, 'Show report'),
      h('button', { class: 'btn', onclick: withQuery(q => downloadReport(q, 'csv')) }, 'Download CSV'),
      h('button', { class: 'btn', onclick: withQuery(q => downloadReport(q, 'html')), title: 'A standalone page: open it and print to PDF' }, 'Download printable'))),
    out);

  // Scheduled reports.
  const schedBox = h('div', { class: 'card flush' });
  const sName = h('input', { placeholder: 'optional', 'aria-label': 'Schedule name' });
  const sFor = isAdmin
    ? h('input', { placeholder: 'customer=acme (empty = whole fleet)', 'aria-label': 'Schedule for' })
    : h('select', { 'aria-label': 'Schedule for' }, opScopes.map((sc, i) => h('option', { value: String(i) }, scopeText(sc))));
  const sPeriod = h('select', { 'aria-label': 'Schedule period' }, [['monthly', 'Monthly (on the 1st)'], ['weekly', 'Weekly (on Mondays)']].map(([v, l]) => h('option', { value: v }, l)));
  const sTZ = h('input', { value: tzIn.value, 'aria-label': 'Schedule time zone' });
  const sTo = h('input', { placeholder: 'it@customer.com, manager@customer.com', 'aria-label': 'Recipients' });
  append(root, h('div', { class: 'page-head mt' }, h('div', {}, h('h2', {}, 'Scheduled reports'),
      h('div', { class: 'muted' }, 'Emailed with the device table attached as CSV, after each week or month ends.'))),
    h('div', { class: 'card mb' + (canSchedule ? '' : ' hidden') }, h('form', { class: 'stack', onsubmit: async e => {
      e.preventDefault();
      let scope;
      try { scope = isAdmin ? { labels: parseLabels(sFor.value) } : opScopes[parseInt(sFor.value || '0', 10)]; } catch (err) { return toast(err.message, true); }
      const body = { name: sName.value.trim(), client_scope: scope, period: sPeriod.value, time_zone: sTZ.value.trim(),
        recipients: sTo.value.split(/[\s,;]+/).filter(Boolean) };
      if (await guard(() => api('POST', '/api/v1/report-schedules', body), 'Report scheduled')) { sName.value = ''; sTo.value = ''; refresh(); }
    } },
    h('div', { class: 'form-grid' }, h('label', { class: isAdmin || opScopes.length > 1 ? '' : 'hidden' }, 'For', sFor), h('label', {}, 'Name', sName),
      h('label', {}, 'Every', sPeriod), h('label', {}, 'Time zone', sTZ), h('label', { class: 'wide' }, 'Send to', sTo)),
    h('div', {}, h('button', { class: 'btn primary', type: 'submit' }, 'Schedule report')))),
    schedBox);

  const canEdit = sc => isAdmin || (scopeText(sc) !== '' && opScopes.some(c => scopeText(c) === scopeText(sc)));
  async function refresh() {
    const list = await api('GET', '/api/v1/report-schedules');
    replace(schedBox, table(['Report', 'For', 'Every', 'Send to', 'Last sent', ''], list.map(r => h('tr', {},
      h('td', {}, h('strong', {}, r.name), r.created_by ? h('div', { class: 'muted' }, 'by ' + r.created_by) : null),
      h('td', {}, scopeText(r.client_scope) || h('span', { class: 'muted' }, 'whole fleet')),
      h('td', {}, r.period, r.time_zone ? h('div', { class: 'muted' }, r.time_zone) : null),
      h('td', {}, r.recipients.join(', ')),
      h('td', { class: 'nowrap' }, r.last_sent_at ? ago(r.last_sent_at) : h('span', { class: 'muted' }, 'not yet'),
        r.last_error ? h('div', { class: 'bad', title: r.last_error }, 'last attempt failed') : null),
      h('td', {}, !canEdit(r.client_scope) ? null : h('div', { class: 'btn-row' },
        h('button', { class: 'btn small', onclick: () => guard(() => api('POST', `/api/v1/report-schedules/${r.id}/send`), 'Sent the last ' + (r.period === 'weekly' ? 'week' : 'month') + "'s report").then(refresh) }, 'Send now'),
        h('button', { class: 'btn small danger', onclick: async () => {
          if (confirm(`Stop sending ${r.name}?`) && await guard(() => api('DELETE', '/api/v1/report-schedules/' + r.id)) !== undefined) refresh();
        } }, 'Delete'))))),
    'No scheduled reports.'));
  }
  await guard(refresh);
  await show();
}

function renderReport(out, rep) {
  const sm = rep.summary;
  const tz = rep.time_zone;
  const day = (ts, back) => new Date(new Date(ts).getTime() - (back ? 1000 : 0)).toLocaleDateString(undefined, { day: 'numeric', month: 'short', year: 'numeric', timeZone: tz });
  const when = ts => new Date(ts).toLocaleString(undefined, { day: 'numeric', month: 'short', hour: '2-digit', minute: '2-digit', hour12: false, timeZone: tz });
  const pct = v => v === undefined || v === null ? '—' : v.toFixed(2) + '%';
  const mins = m => m === undefined || m === null ? '—' : m < 60 ? `${Math.round(m)} min` : m < 2880 ? `${Math.floor(m / 60)}h ${String(Math.round(m % 60)).padStart(2, '0')}m` : `${(m / 1440).toFixed(1)} days`;
  const tile = (label, value, hint) => h('div', { class: 'card stat' }, h('div', { class: 'label' }, label), h('div', { class: 'value' }, value), h('div', { class: 'hint' }, hint));
  const low = v => v !== undefined && v !== null && v < sm.availability_target;
  const section = (title, body) => [h('h2', { class: 'mt' }, title), h('div', { class: 'card flush' }, body)];
  replace(out,
    h('div', { class: 'page-head mt' }, h('div', {}, h('h2', {}, rep.title), h('div', { class: 'muted' }, `${day(rep.from)} – ${day(rep.to, true)} (${tz})`))),
    h('div', { class: 'grid cols-4' },
      tile('Devices', sm.devices, [`${sm.active} in service`, sm.spare && `${sm.spare} spare`, sm.in_repair && `${sm.in_repair} in repair`].filter(Boolean).join(' · ')),
      tile('Availability', pct(sm.availability), sm.below_target ? `${sm.below_target} below ${sm.availability_target}%` : `target ${sm.availability_target}%`),
      tile('Alerts', `${sm.alerts_critical} / ${sm.alerts_warning}`, `critical / warning · resolved in ${mins(sm.mean_time_to_resolve_minutes)} on average`),
      tile('Updates installed', sm.updates_succeeded, `${sm.updates_failed} failed or rolled back`),
      tile('Replacements', sm.replacements, `${sm.maintenance_windows} maintenance windows`)),
    section('Devices', table(['Device', 'State', 'Availability', 'Downtime', 'Maintenance', 'Alerts', 'Updates', 'Health now'], rep.devices.map(d => h('tr', { class: 'clickable', onclick: () => { location.hash = '#/devices/' + d.id; } },
      h('td', {}, h('strong', {}, d.name), h('div', { class: 'muted mono' }, d.id)),
      h('td', {}, LIFECYCLE_TEXT[d.lifecycle] || d.lifecycle),
      h('td', { class: 'nowrap' + (low(d.availability) ? ' bad' : '') }, pct(d.availability)),
      h('td', { class: 'nowrap' }, d.downtime_minutes ? mins(d.downtime_minutes) : '—'),
      h('td', { class: 'nowrap' }, d.maintenance_minutes ? mins(d.maintenance_minutes) : '—'),
      h('td', {}, d.alerts),
      h('td', {}, d.updates_succeeded, d.updates_failed ? h('span', { class: 'bad' }, ` (${d.updates_failed} failed)`) : null),
      h('td', {}, badge(d.health || 'ok')))), 'No devices.')),
    section('Alerts', table(['Started', 'Device', 'Alert', 'Severity', 'Lasted'], rep.alerts.map(a => h('tr', {},
      h('td', { class: 'nowrap' }, when(a.started_at)), h('td', {}, a.device_name),
      h('td', {}, a.rule, h('div', { class: 'muted' }, a.summary)), h('td', {}, badge(a.severity)),
      h('td', { class: 'nowrap' }, a.closed_out_of_service ? 'closed (out of service)' : a.resolved_at ? mins(a.duration_minutes) : 'still open'))), 'No alerts in this period.')),
    rep.updates.length ? section('Updates', table(['Release', 'Installed', 'Failed or rolled back'],
      rep.updates.map(u => h('tr', {}, h('td', { class: 'mono' }, u.name), h('td', {}, u.count), h('td', {}, u.failed || 0))))) : null,
    rep.changes.length ? section('Replacements and changes', table(['When', 'What'],
      rep.changes.map(e => h('tr', {}, h('td', { class: 'nowrap' }, when(e.time)), h('td', {}, e.message))))) : null,
    rep.maintenance.length ? section('Maintenance', table(['Maintenance', 'When', 'Devices'], rep.maintenance.map(m => h('tr', {},
      h('td', {}, m.name, m.note ? h('div', { class: 'muted' }, m.note) : null),
      h('td', { class: 'nowrap' }, when(m.starts_at), m.ends_at ? ' – ' + when(m.ends_at) : '', m.windows ? h('div', { class: 'muted' }, m.windows) : null),
      h('td', {}, m.devices))))) : null,
    h('div', { class: 'muted mt' }, rep.notes.map(n => h('p', {}, n))));
}

async function viewActivity(root) {
  const feed = h('div', { class: 'card flush' });
  append(root, pageHead('Activity', 'Everything that happened across the fleet'), feed);
  async function refresh() { replace(feed, eventFeed(await api('GET', '/api/v1/events?limit=200'))); }
  await guard(refresh);
  return refresh;
}

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
  const res = await fetch(path, { method: 'POST', headers: { 'Content-Type': 'application/json', Authorization: 'Bearer ' + token }, body: JSON.stringify(body || {}) });
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
  store.del('armada.token');
  showLogin();
});

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
        h('p', { class: 'muted' }, 'For scripts and armadactl (--token). A token acts as you, with your role and scope.'),
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

window.addEventListener('hashchange', route);
document.addEventListener('visibilitychange', () => { if (!document.hidden && currentRefresh) guard(currentRefresh); });

if (location.hash.startsWith('#/sso')) {
  finishSSO();
} else if (token) {
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
  // Who manages which alerts: admins everything; client operators their client's.
  const isAdmin = !!me && me.fleet_role === 'admin';
  const clientScopes = ((me && me.grants) || []).filter(g => (ROLE_RANK[g.role] || 0) >= ROLE_RANK.operator && scopeText(g.scope)).map(g => g.scope);
  const canManageAlerts = isAdmin || clientScopes.length > 0;
  const canEditScope = sc => isAdmin || (scopeText(sc) !== '' && clientScopes.some(c => scopeText(c) === scopeText(sc)));
  const forClient = () => {
    const input = isAdmin
      ? h('input', { placeholder: 'empty = vendor-wide, or customer=acme', 'aria-label': 'For client' })
      : h('select', { 'aria-label': 'For client' }, clientScopes.map((sc, i) => h('option', { value: String(i) }, scopeText(sc))));
    return { input, label: h('label', { class: isAdmin || clientScopes.length > 1 ? '' : 'hidden' }, 'For client', input),
      read: () => isAdmin ? { labels: parseLabels(input.value) } : clientScopes[parseInt(input.value || '0', 10)] };
  };
  const rFor = forClient(), cFor = forClient();
  const rName = h('input', { required: true, placeholder: 'Storage failing', 'aria-label': 'Rule name' });
  const rCond = h('select', { 'aria-label': 'Condition' }, [['health', 'Health check failing'], ['device_offline', 'Device offline'], ['cert_expiry', 'Certificate expiring'],
    ...(isAdmin ? [['update_failed', 'Update failed'], ['deployment_paused', 'Rollout paused']] : [])].map(([v, l]) => h('option', { value: v }, l)));
  const rSev = h('select', { 'aria-label': 'Minimum severity' }, ['critical', 'warning'].map(v => h('option', { value: v }, v)));
  const rKinds = h('input', { placeholder: 'any, or storage power temperature', 'aria-label': 'Check kinds' });
  const rOffline = h('input', { type: 'number', min: '1', value: '10', 'aria-label': 'Offline minutes' });
  const rExpiry = h('input', { type: 'number', min: '1', max: '365', value: '30', 'aria-label': 'Days before expiry' });
  const rLabels = h('input', { placeholder: 'all devices, or site=sg', 'aria-label': 'Rule labels' });
  const rRepeat = h('input', { type: 'number', min: '0', value: '0', 'aria-label': 'Repeat minutes' });
  const rResolved = h('input', { type: 'checkbox', checked: true });
  const rChannels = h('div', { class: 'chips' });
  const syncCond = () => {
    rSev.closest('label').classList.toggle('hidden', rCond.value !== 'health');
    rKinds.closest('label').classList.toggle('hidden', rCond.value !== 'health');
    rOffline.closest('label').classList.toggle('hidden', rCond.value !== 'device_offline');
    rExpiry.closest('label').classList.toggle('hidden', rCond.value !== 'cert_expiry');
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
  const serverOnly = isAdmin ? '' : ' hidden'; // clients' email goes through the vendor's relay
  const emailFields = h('div', { class: 'form-grid hidden' }, h('label', { class: serverOnly }, 'SMTP host (empty = server relay)', cHost),
    h('label', { class: serverOnly }, 'Port', cPort), h('label', { class: serverOnly }, 'Username', cUser), h('label', { class: serverOnly }, 'Password', cPass),
    h('label', { class: serverOnly }, 'From', cFrom), h('label', { class: 'wide' }, 'To', cTo));
  if (!isAdmin) cTemplate.closest('label').classList.add('hidden');
  cType.addEventListener('change', () => {
    webhookFields.classList.toggle('hidden', cType.value !== 'webhook');
    slackFields.classList.toggle('hidden', cType.value !== 'slack');
    emailFields.classList.toggle('hidden', cType.value !== 'email');
  });

  const section = (title, sub, cls) => h('div', { class: 'page-head mt ' + (cls || '') }, h('div', {}, h('h2', {}, title), h('div', { class: 'muted' }, sub)));
  monitorsBox.classList.add('fleet-only');

  // --- maintenance form ---
  // Fleet operators plan any maintenance; client operators their client's.
  const fleetOp = !!me && (ROLE_RANK[me.fleet_role] || 0) >= ROLE_RANK.operator;
  const canPlan = fleetOp || clientScopes.length > 0;
  const canEditMaint = sc => fleetOp || (scopeText(sc) !== '' && clientScopes.some(c => scopeText(c) === scopeText(sc)));
  const maintBox = h('div', { class: 'card flush' });
  const xFor = fleetOp
    ? h('input', { placeholder: 'empty = vendor, or customer=acme', 'aria-label': 'Maintenance for client' })
    : h('select', { 'aria-label': 'Maintenance for client' }, clientScopes.map((sc, i) => h('option', { value: String(i) }, scopeText(sc))));
  const xForLabel = h('label', { class: fleetOp || clientScopes.length > 1 ? '' : 'hidden' }, 'For client', xFor);
  const xName = h('input', { placeholder: 'Firmware upgrade at the Orchard store', 'aria-label': 'Maintenance name' });
  const xLabels = h('input', { required: true, placeholder: 'site=orchard (use customer=* for all of a client\'s devices)', 'aria-label': 'Maintenance devices' });
  const xKind = h('select', { 'aria-label': 'Maintenance kind' }, [['once', 'One-off'], ['recurring', 'Recurring']].map(([v, l]) => h('option', { value: v }, l)));
  const xStart = h('input', { type: 'datetime-local', 'aria-label': 'Starts' });
  const xEnd = h('input', { type: 'datetime-local', 'aria-label': 'Ends' });
  const xWindows = h('input', { placeholder: '02:00-04:00', 'aria-label': 'Daily windows' });
  const xDays = h('input', { placeholder: 'sun (empty = every day)', 'aria-label': 'Days' });
  const xTZ = h('input', { value: Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC', 'aria-label': 'Time zone' });
  const xAlerts = h('input', { type: 'checkbox', checked: true });
  const xUpdates = h('input', { type: 'checkbox', checked: true });
  const xNote = h('input', { placeholder: 'optional', 'aria-label': 'Maintenance note' });
  const syncKind = () => {
    for (const el of [xWindows, xDays, xTZ]) el.closest('label').classList.toggle('hidden', xKind.value !== 'recurring');
    xEnd.closest('label').firstChild.textContent = xKind.value === 'recurring' ? 'Repeat until (optional)' : 'Ends';
  };
  xKind.addEventListener('change', syncKind);
  const maintForm = h('div', { class: 'card mb' + (canPlan ? '' : ' hidden') }, h('form', { class: 'stack', onsubmit: async e => {
    e.preventDefault();
    let labels, clientScope;
    try {
      labels = parseLabels(xLabels.value);
      clientScope = fleetOp ? { labels: parseLabels(xFor.value) } : clientScopes[parseInt(xFor.value || '0', 10)];
    } catch (err) { return toast(err.message, true); }
    const body = { name: xName.value.trim(), note: xNote.value.trim(), selector: { labels }, client_scope: clientScope,
      suppress_alerts: xAlerts.checked, pause_updates: xUpdates.checked };
    if (xStart.value) body.starts_at = new Date(xStart.value).toISOString();
    if (xEnd.value) body.ends_at = new Date(xEnd.value).toISOString();
    if (xKind.value === 'recurring') {
      body.windows = xWindows.value.trim(); body.time_zone = xTZ.value.trim();
      body.days = xDays.value.split(/[\s,]+/).filter(Boolean);
    }
    if (await guard(() => api('POST', '/api/v1/maintenance', body), 'Maintenance planned')) { xName.value = ''; xNote.value = ''; refresh(); }
  } },
  h('div', { class: 'form-grid' }, xForLabel, h('label', {}, 'Name', xName), h('label', {}, 'Devices (labels)', xLabels), h('label', {}, 'Kind', xKind),
    h('label', {}, 'Starts (empty = now)', xStart), h('label', {}, 'Ends', xEnd),
    h('label', {}, 'Daily windows', xWindows), h('label', {}, 'Days', xDays), h('label', {}, 'Time zone', xTZ),
    h('label', { class: 'wide' }, 'Note', xNote)),
  h('label', { class: 'check' }, xAlerts, 'Hold alerts'), h('label', { class: 'check' }, xUpdates, 'Pause updates'),
  h('div', {}, h('button', { class: 'btn primary', type: 'submit' }, 'Plan maintenance'))));

  append(root, pageHead('Monitoring', 'Health checks on devices, and who gets told when something goes wrong'),
    alertsBox,
    section('Maintenance', 'Planned work: alerts for the covered devices are held and rollouts skip them. For one device, use its page.'),
    maintForm, maintBox,
    section('Health monitors', 'Program checks run by every matching device. Hardware checks (storage, temperature, power, battery) run automatically.', 'fleet-only'),
    h('div', { class: 'card mb need-operator fleet-only' }, h('form', { class: 'stack', onsubmit: async e => {
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
    section('Alert rules', isAdmin ? 'When to notify, and which channels to use. Rules for a client only see that client\'s devices and can only use its channels.'
      : 'When to notify you about your devices, and where.'),
    h('div', { class: 'card mb' + (canManageAlerts ? '' : ' hidden') }, h('form', { class: 'stack', onsubmit: async e => {
      e.preventDefault();
      let labels, clientScope;
      try { labels = parseLabels(rLabels.value); clientScope = rFor.read(); } catch (err) { return toast(err.message, true); }
      const chosen = [...rChannels.querySelectorAll('input:checked')].map(i => i.value);
      if (!chosen.length) return toast('Pick at least one channel (add one below first).', true);
      const body = { name: rName.value.trim(), enabled: true, condition: rCond.value, selector: { labels }, channels: chosen,
        min_severity: rSev.value, check_kinds: rKinds.value.split(/[\s,]+/).filter(Boolean), offline_minutes: parseInt(rOffline.value || '10', 10),
        expiry_days: rCond.value === 'cert_expiry' ? parseInt(rExpiry.value || '30', 10) : 0,
        repeat_minutes: parseInt(rRepeat.value || '0', 10), notify_resolved: rResolved.checked, client_scope: clientScope };
      if (await guard(() => api('POST', '/api/v1/alert-rules', body), 'Rule added')) { rName.value = ''; refresh(); }
    } },
    h('div', { class: 'form-grid' }, rFor.label, h('label', {}, 'Name', rName), h('label', {}, 'When', rCond), h('label', {}, 'At least', rSev),
      h('label', {}, 'Only check kinds', rKinds), h('label', { class: 'hidden' }, 'Offline for (minutes)', rOffline),
      h('label', { class: 'hidden' }, 'Days before expiry (critical within 7)', rExpiry),
      h('label', {}, 'Devices (labels)', rLabels), h('label', {}, 'Repeat every (minutes, 0 = never)', rRepeat)),
    h('label', { class: 'check' }, rResolved, 'Also notify when it resolves'),
    h('div', {}, h('div', { class: 'muted mb' }, 'Notify:'), rChannels),
    h('div', {}, h('button', { class: 'btn primary', type: 'submit' }, 'Add rule')))),
    rulesBox,
    section('Notification channels', isAdmin ? 'Webhooks reach any service (PagerDuty, Opsgenie, Teams, Discord, your own API) using an optional body template.'
      : 'Where your alerts go: a webhook (public HTTPS address; the standard JSON notification), Slack, or email.'),
    h('div', { class: 'card mb' + (canManageAlerts ? '' : ' hidden') }, h('form', { class: 'stack', onsubmit: async e => {
      e.preventDefault();
      const body = { name: cName.value.trim(), type: cType.value };
      try {
        body.client_scope = cFor.read();
        if (cType.value === 'webhook') body.webhook = { url: cURL.value.trim(), secret: cSecret.value, headers: kvLines(cHeaders.value), body_template: cTemplate.value };
        if (cType.value === 'slack') body.slack = { webhook_url: cSlack.value.trim() };
        if (cType.value === 'email') body.email = { host: cHost.value.trim(), port: parseInt(cPort.value || '587', 10), username: cUser.value,
          password: cPass.value, from: cFrom.value.trim(), to: cTo.value.split(/[\s,]+/).filter(Boolean) };
      } catch (err) { return toast(err.message, true); }
      if (await guard(() => api('POST', '/api/v1/channels', body), 'Channel added')) { cName.value = ''; refresh(); }
    } },
    h('div', { class: 'form-grid' }, cFor.label, h('label', {}, 'Name', cName), h('label', {}, 'Type', cType)),
    webhookFields, slackFields, emailFields,
    h('div', {}, h('button', { class: 'btn primary', type: 'submit' }, 'Add channel')))),
    channelsBox);
  syncType(); syncCond(); syncKind();
  // A rule can only notify channels of the same client (or vendor channels
  // for vendor rules): offer just those.
  let allChannels = [];
  const syncRuleChannels = () => {
    let want = '';
    try { want = scopeText(rFor.read()); } catch { /* typing */ }
    const usable = allChannels.filter(c => scopeText(c.client_scope) === want);
    replace(rChannels, usable.length ? usable.map(c => h('label', { class: 'check chip' }, h('input', { type: 'checkbox', value: c.id }), c.name))
      : h('span', { class: 'muted' }, 'No channels for this client yet — add one below.'));
  };
  rFor.input.addEventListener(isAdmin ? 'input' : 'change', syncRuleChannels);

  async function refresh() {
    const [alerts, monitors, rules, chs, maint] = await Promise.all([
      api('GET', '/api/v1/alerts'),
      fleetWide() ? api('GET', '/api/v1/monitors') : [],
      api('GET', '/api/v1/alert-rules'),
      api('GET', '/api/v1/channels'),
      api('GET', '/api/v1/maintenance')]);
    channels = chs;
    const chName = Object.fromEntries(chs.map(c => [c.id, c.name]));
    const firing = alerts.filter(a => a.status === 'firing');
    const del = (path, msg) => async () => { if (confirm(msg) && await guard(() => api('DELETE', path)) !== undefined) refresh(); };
    // Operators can acknowledge: fleet operators any alert, client operators
    // their devices' (the server checks).
    const canAck = a => a.status === 'firing' && (fleetOp || (!!a.device_id && !a.deployment_id && clientScopes.length > 0));
    const ack = a => async () => {
      const note = prompt(`Acknowledge "${a.rule_name}"? Repeat notifications stop until it resolves or gets worse.\n\nNote (optional):`, '');
      if (note === null) return;
      if (await guard(() => api('POST', `/api/v1/alerts/${a.id}/ack`, { note: note.trim() }), 'Acknowledged')) refresh();
    };
    const unack = a => async () => { if (await guard(() => api('POST', `/api/v1/alerts/${a.id}/unack`))) refresh(); };
    const unacked = firing.filter(a => !a.acked_at).length;
    replace(alertsBox, h('h2', {}, `Alerts${firing.length ? ` — ${firing.length} firing` : ''}${unacked && unacked < firing.length ? `, ${unacked} not acknowledged` : ''}`),
      table(['Status', 'Severity', 'Rule', 'What', 'Since', ''],
      alerts.slice(0, 50).map(a => h('tr', {},
        h('td', {}, badge(a.status),
          a.status === 'firing' && a.acked_at ? h('div', { title: 'Acknowledged ' + ago(a.acked_at) }, badge('acked')) : null,
          a.status === 'firing' && a.in_maintenance ? h('div', { title: 'Held: the device is in maintenance' }, badge('maintenance')) : null),
        h('td', {}, badge(a.severity)), h('td', {}, a.rule_name),
        h('td', {}, a.summary, a.count > 1 ? h('span', { class: 'muted' }, ` (×${a.count})`) : null,
          a.device_id ? h('div', {}, h('a', { href: '#/devices/' + a.device_id }, a.device_name || a.device_id)) : null,
          a.deployment_id && !a.device_id ? h('div', {}, h('a', { href: '#/deployments/' + a.deployment_id }, 'rollout')) : null),
        h('td', { class: 'nowrap' }, ago(a.started_at), a.resolved_at ? h('div', { class: 'muted' }, 'resolved ' + ago(a.resolved_at)) : null),
        h('td', {}, a.acked_at ? h('div', { class: 'muted' }, 'acked by ' + a.acked_by, a.ack_note ? ': ' + a.ack_note : '') : null,
          !canAck(a) ? null : a.acked_at
            ? h('button', { class: 'btn small', onclick: unack(a) }, 'Un-acknowledge')
            : h('button', { class: 'btn small', onclick: ack(a) }, 'Acknowledge')))),
      'No alerts yet.'));
    const whenText = m => {
      const fmt = ts => new Date(ts).toLocaleString(undefined, { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit', hour12: false });
      const span = m.ends_at ? `${fmt(m.starts_at)} – ${fmt(m.ends_at)}` : 'from ' + fmt(m.starts_at);
      if (!m.windows && !(m.days || []).length) return span;
      return h('div', {}, `${m.windows || 'all day'}${(m.days || []).length ? ' ' + m.days.join(', ') : ''} ${m.time_zone || 'UTC'}`, h('div', { class: 'muted' }, span));
    };
    const targets = m => [
      (m.selector.device_ids || []).length ? h('span', { class: 'chip' }, m.selector.device_ids.length === 1 ? m.selector.device_ids[0] : m.selector.device_ids.length + ' devices') : null,
      m.selector.channel ? h('span', { class: 'chip' }, 'channel ' + m.selector.channel) : null,
      ...sortedEntries(m.selector.labels).map(([k, v]) => h('span', { class: 'chip' }, k + '=' + v))];
    replace(maintBox, table(['Maintenance', 'State', 'Devices', 'When', 'Effect', ''],
      maint.map(m => h('tr', {},
        h('td', {}, h('strong', {}, m.name), m.note ? h('div', { class: 'muted' }, m.note) : null,
          h('div', { class: 'muted' }, [scopeText(m.client_scope) ? 'for ' + scopeText(m.client_scope) : null, m.created_by ? 'by ' + m.created_by : null].filter(Boolean).join(' · '))),
        h('td', {}, badge(m.state)),
        h('td', {}, h('div', { class: 'chips' }, targets(m))),
        h('td', { class: 'nowrap' }, whenText(m)),
        h('td', {}, [m.suppress_alerts !== false && 'alerts held', m.pause_updates !== false && 'updates paused'].filter(Boolean).join(', ')),
        h('td', {}, !canEditMaint(m.client_scope) ? null : h('div', { class: 'btn-row' },
          m.state !== 'ended' ? h('button', { class: 'btn small', onclick: async () => {
            if (await guard(() => api('POST', `/api/v1/maintenance/${m.id}/end`), m.state === 'scheduled' ? 'Cancelled' : 'Ended')) refresh();
          } }, m.state === 'scheduled' ? 'Cancel' : 'End now') : null,
          h('button', { class: 'btn small danger', onclick: del('/api/v1/maintenance/' + m.id, `Delete maintenance ${m.name}?`) }, 'Delete'))))),
      'No maintenance planned.'));
    replace(monitorsBox, table(['Monitor', 'Check', 'Devices', ''],
      monitors.map(m => h('tr', {},
        h('td', {}, h('strong', {}, m.name), ' ', badge(m.enabled ? 'enabled' : 'disabled')),
        h('td', { class: 'mono' }, m.check.type + ' ' + (m.check.target || (m.check.command || []).join(' '))),
        h('td', {}, labelChips(m.selector.labels)),
        h('td', {}, h('div', { class: 'btn-row need-operator' },
          h('button', { class: 'btn small', onclick: async () => {
            if (await guard(() => api('PUT', '/api/v1/monitors/' + m.id, { ...m, enabled: !m.enabled }))) refresh();
          } }, m.enabled ? 'Disable' : 'Enable'),
          h('button', { class: 'btn small danger', onclick: del('/api/v1/monitors/' + m.id, `Delete monitor ${m.name}?`) }, 'Delete'))))),
      'No program monitors yet.'));
    const condLabel = { health: 'health', device_offline: 'offline', update_failed: 'update failed', deployment_paused: 'rollout paused', cert_expiry: 'certificate expiring' };
    const forCell = sc => scopeText(sc) ? labelChips(sc.labels) : h('span', { class: 'muted' }, 'vendor-wide');
    replace(rulesBox, table(['Rule', 'For', 'When', 'Devices', 'Notify', ''],
      rules.map(r => h('tr', {},
        h('td', {}, h('strong', {}, r.name), ' ', badge(r.enabled ? 'enabled' : 'disabled'), r.created_by ? h('div', { class: 'muted' }, 'by ' + r.created_by) : null),
        h('td', {}, forCell(r.client_scope)),
        h('td', {}, condLabel[r.condition] || r.condition,
          r.condition === 'health' ? h('div', { class: 'muted' }, `${r.min_severity || 'critical'}+${(r.check_kinds || []).length ? ' · ' + r.check_kinds.join(', ') : ''}`) : null,
          r.condition === 'device_offline' ? h('div', { class: 'muted' }, `${r.offline_minutes || 10} min`) : null,
          r.condition === 'cert_expiry' ? h('div', { class: 'muted' }, `${r.expiry_days || 30} days before`) : null),
        h('td', {}, labelChips(r.selector.labels)),
        h('td', {}, r.channels.map(id => chName[id] || id).join(', ')),
        h('td', {}, !canEditScope(r.client_scope) ? null : h('div', { class: 'btn-row' },
          h('button', { class: 'btn small', onclick: async () => {
            if (await guard(() => api('PUT', '/api/v1/alert-rules/' + r.id, { ...r, enabled: !r.enabled }))) refresh();
          } }, r.enabled ? 'Disable' : 'Enable'),
          h('button', { class: 'btn small danger', onclick: del('/api/v1/alert-rules/' + r.id, `Delete rule ${r.name}?`) }, 'Delete'))))),
      'No alert rules yet.'));
    allChannels = chs;
    syncRuleChannels();
    replace(channelsBox, table(['Channel', 'For', 'Type', 'Destination', ''],
      chs.map(c => h('tr', {},
        h('td', {}, h('strong', {}, c.name)), h('td', {}, forCell(c.client_scope)), h('td', {}, c.type),
        h('td', { class: 'mono' }, c.webhook ? c.webhook.url : c.slack ? c.slack.webhook_url : c.email ? c.email.to.join(', ') : ''),
        h('td', {}, !canEditScope(c.client_scope) ? null : h('div', { class: 'btn-row' },
          h('button', { class: 'btn small', onclick: () => guard(() => api('POST', `/api/v1/channels/${c.id}/test`), 'Test notification sent') }, 'Send test'),
          h('button', { class: 'btn small danger', onclick: del('/api/v1/channels/' + c.id, `Delete channel ${c.name}?`) }, 'Delete'))))),
      'No channels yet.'));
  }
  await guard(refresh);
  return async () => { if (!root.contains(document.activeElement) || document.activeElement === document.body) await refresh(); };
}

// ---------- configuration profiles ----------

const CONFIG_KINDS = {
  files: { label: 'Files (Linux agents)', target: 'Managed config name', hint: 'a name from the agent\'s managed_config, e.g. myapp' },
  android_app: { label: 'Android app settings', target: 'App package', hint: 'com.example.kiosk' },
  android_system_update: { label: 'Android OS updates', target: null },
  agent: { label: 'Agent settings', target: null },
};

async function viewConfig(root) {
  const list = h('div', { class: 'card flush' });
  let editing = null;
  const nameIn = h('input', { required: true, 'aria-label': 'Profile name' });
  const kindIn = h('select', { 'aria-label': 'Kind' }, Object.entries(CONFIG_KINDS).map(([k, v]) => h('option', { value: k }, v.label)));
  const targetIn = h('input', { 'aria-label': 'Target' });
  const targetLabel = h('label', {}, 'Target', targetIn);
  const labelsIn = h('input', { placeholder: 'all devices, or site=sg', 'aria-label': 'Device labels' });
  const chanIn = h('input', { placeholder: 'any', 'aria-label': 'Channel' });
  const prioIn = h('input', { type: 'number', value: '0', min: '-1000', max: '1000', 'aria-label': 'Priority' });
  const enabledIn = h('input', { type: 'checkbox', checked: true });

  // files
  const filesBox = h('div', { class: 'stack' });
  const addFile = (f = {}) => {
    const name = h('input', { placeholder: 'app.conf', value: f.name || '', 'aria-label': 'File name' });
    const mode = h('input', { placeholder: '0644', value: f.mode || '', 'aria-label': 'Mode' });
    const tmpl = h('input', { type: 'checkbox', checked: !!f.template });
    const content = h('textarea', { rows: '8', class: 'mono', 'aria-label': 'Content' });
    content.value = f.content || '';
    const row = h('div', { class: 'card inset stack' },
      h('div', { class: 'form-grid' }, h('label', {}, 'File name', name), h('label', {}, 'Mode', mode),
        h('label', { class: 'check', title: 'Use {{.Device.Name}}, {{.Device.ID}}, {{index .Device.Labels "site"}} in the content' }, tmpl, 'Fill in device values')),
      h('label', {}, 'Content', content),
      h('div', {}, h('button', { class: 'btn small ghost', type: 'button', onclick: () => row.remove() }, 'Remove file')));
    row.read = () => ({ name: name.value.trim(), mode: mode.value.trim(), template: tmpl.checked, content: content.value });
    filesBox.append(row);
  };
  const filesFields = h('div', { class: 'stack' }, filesBox,
    h('div', {}, h('button', { class: 'btn small', type: 'button', onclick: () => addFile() }, 'Add file')));
  // android app
  const valuesIn = h('textarea', { rows: '6', class: 'mono', placeholder: '{"server_url": "https://api.example.com", "kiosk_mode": true, "max_items": 20}', 'aria-label': 'Values (JSON)' });
  const appFields = h('label', {}, 'Managed configuration (JSON: strings, numbers, booleans, string lists)', valuesIn);
  // system update
  const suType = h('select', { 'aria-label': 'Update policy' }, [['automatic', 'Install automatically'], ['windowed', 'Install during a daily window'], ['postpone', 'Postpone (up to 30 days)']].map(([v, l]) => h('option', { value: v }, l)));
  const suStart = h('input', { placeholder: '02:00', 'aria-label': 'Window start' });
  const suEnd = h('input', { placeholder: '04:00', 'aria-label': 'Window end' });
  const suFields = h('div', { class: 'form-grid' }, h('label', {}, 'Policy', suType), h('label', {}, 'Window start (device time)', suStart), h('label', {}, 'Window end', suEnd));
  // agent
  const agWin = h('input', { placeholder: 'keep local setting, or 02:00-05:00', 'aria-label': 'Maintenance window' });
  const agAuto = h('select', { 'aria-label': 'Auto update' }, [['', 'Keep local setting'], ['true', 'On'], ['false', 'Off']].map(([v, l]) => h('option', { value: v }, l)));
  const agentFields = h('div', { class: 'form-grid' }, h('label', {}, 'Maintenance window', agWin), h('label', {}, 'Install updates automatically', agAuto));

  const syncKind = () => {
    const k = CONFIG_KINDS[kindIn.value];
    targetLabel.classList.toggle('hidden', !k.target);
    if (k.target) { targetLabel.firstChild.textContent = k.target; targetIn.placeholder = k.hint; }
    filesFields.classList.toggle('hidden', kindIn.value !== 'files');
    appFields.classList.toggle('hidden', kindIn.value !== 'android_app');
    suFields.classList.toggle('hidden', kindIn.value !== 'android_system_update');
    agentFields.classList.toggle('hidden', kindIn.value !== 'agent');
    if (kindIn.value === 'files' && !filesBox.children.length) addFile();
  };
  kindIn.addEventListener('change', syncKind);
  const title = h('h2', {}, 'New profile');
  const submit = h('button', { class: 'btn primary', type: 'submit' }, 'Save profile');
  const reset = () => {
    editing = null;
    title.textContent = 'New profile';
    nameIn.value = targetIn.value = labelsIn.value = chanIn.value = valuesIn.value = suStart.value = suEnd.value = agWin.value = '';
    prioIn.value = '0'; enabledIn.checked = true; agAuto.value = ''; kindIn.disabled = false;
    filesBox.replaceChildren();
    syncKind();
  };
  const fill = p => {
    reset();
    editing = p;
    title.textContent = 'Edit ' + p.name;
    nameIn.value = p.name; kindIn.value = p.kind; kindIn.disabled = true; targetIn.value = p.target || '';
    labelsIn.value = sortedEntries(p.selector.labels || {}).map(([k, v]) => k + '=' + v).join(' ');
    chanIn.value = p.selector.channel || ''; prioIn.value = p.priority; enabledIn.checked = p.enabled;
    filesBox.replaceChildren();
    (p.files || []).forEach(addFile);
    valuesIn.value = p.values ? JSON.stringify(p.values, null, 2) : '';
    if (p.system_update) { suType.value = p.system_update.type; suStart.value = p.system_update.window_start || ''; suEnd.value = p.system_update.window_end || ''; }
    if (p.agent) { agWin.value = p.agent.maintenance_window || ''; agAuto.value = p.agent.auto_update === undefined ? '' : String(p.agent.auto_update); }
    syncKind();
    nameIn.scrollIntoView({ block: 'center' });
  };

  append(root, pageHead('Configuration', 'Settings and files kept applied on matching devices'),
    h('div', { class: 'card need-operator' }, title,
      h('form', { class: 'stack', onsubmit: async e => {
        e.preventDefault();
        let labels;
        try { labels = parseLabels(labelsIn.value); } catch (err) { return toast(err.message, true); }
        const body = { name: nameIn.value.trim(), kind: kindIn.value, target: targetIn.value.trim(), priority: parseInt(prioIn.value || '0', 10),
          enabled: enabledIn.checked, selector: { labels, channel: chanIn.value.trim() } };
        if (body.kind === 'files') body.files = [...filesBox.children].map(r => r.read()).filter(f => f.name);
        if (body.kind === 'android_app') {
          try { body.values = JSON.parse(valuesIn.value || '{}'); } catch (err) { return toast('Values must be valid JSON: ' + err.message, true); }
        }
        if (body.kind === 'android_system_update') body.system_update = { type: suType.value, window_start: suStart.value.trim(), window_end: suEnd.value.trim() };
        if (body.kind === 'agent') {
          body.agent = {};
          if (agWin.value.trim()) body.agent.maintenance_window = agWin.value.trim();
          if (agAuto.value) body.agent.auto_update = agAuto.value === 'true';
        }
        const saved = await guard(() => editing ? api('PUT', '/api/v1/config-profiles/' + editing.id, body) : api('POST', '/api/v1/config-profiles', body),
          'Saved — connected devices apply it within seconds');
        if (saved) { reset(); refresh(); }
      } },
      h('div', { class: 'form-grid' }, h('label', {}, 'Name', nameIn), h('label', {}, 'Kind', kindIn), targetLabel,
        h('label', {}, 'Devices (labels)', labelsIn), h('label', {}, 'Channel', chanIn),
        h('label', {}, 'Priority (higher wins)', prioIn), h('label', { class: 'check' }, enabledIn, 'Enabled')),
      filesFields, appFields, suFields, agentFields,
      h('p', { class: 'muted' }, 'When several profiles of the same kind and target match a device, the highest priority wins; they are not merged. Files only go into directories the device lists in managed_config, and a failed reload restores the previous files.'),
      h('div', { class: 'btn-row' }, submit, h('button', { class: 'btn ghost', type: 'button', onclick: reset }, 'Clear')))),
    h('div', { class: 'spacer' }), list);
  reset();

  async function refresh() {
    const ps = await api('GET', '/api/v1/config-profiles');
    const del = p => async () => { if (confirm(`Delete ${p.name}? Devices remove its configuration.`) && await guard(() => api('DELETE', '/api/v1/config-profiles/' + p.id), 'Profile deleted') !== undefined) refresh(); };
    replace(list, table(['Profile', 'Devices', 'Priority', 'State', ''],
      ps.map(p => {
        const c = p.compliance || {};
        return h('tr', {},
          h('td', {}, h('strong', {}, p.name), ' ', p.enabled ? null : badge('disabled'),
            h('div', { class: 'muted mono' }, p.kind + (p.target ? ':' + p.target : ''))),
          h('td', {}, p.selector.channel ? h('span', { class: 'muted' }, 'channel ' + p.selector.channel + ' ') : null,
            Object.keys(p.selector.labels || {}).length ? labelChips(p.selector.labels) : h('span', { class: 'muted' }, 'all')),
          h('td', {}, String(p.priority)),
          h('td', { class: 'nowrap' }, `${c.applied || 0} applied · ${c.pending || 0} pending`,
            c.failed ? [' · ', h('span', { class: 'bad-text' }, `${c.failed} failed`)] : null),
          h('td', {}, h('div', { class: 'btn-row need-operator' },
            h('button', { class: 'btn small', onclick: () => fill(p) }, 'Edit'),
            h('button', { class: 'btn small', onclick: async () => {
              if (await guard(() => api('PUT', '/api/v1/config-profiles/' + p.id, { ...p, enabled: !p.enabled }))) refresh();
            } }, p.enabled ? 'Disable' : 'Enable'),
            h('button', { class: 'btn small danger', onclick: del(p) }, 'Delete'))));
      }), 'No configuration profiles yet.'));
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
