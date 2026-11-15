// Armada dashboard. Plain JS, no build step. All data is rendered through
// DOM APIs (never innerHTML) so device-reported strings can't inject markup.
//
// The code is split into classic scripts loaded in order by index.html;
// they share one global scope. Shared by every page: API calls, DOM helpers, formatting, the top menu and the customer/project picker. Loaded first.
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
// view is the customer/project picker's choice ({customer: 'acme'}); every
// API call sends it, and the server narrows lists to those devices.
let view = {};
let groupings = null;
const viewText = () => sortedEntries(view).map(([k, v]) => k + '=' + v).join(';');
function authHeaders(extra) {
  const hd = { Authorization: 'Bearer ' + token, ...(extra || {}) };
  if (Object.keys(view).length) hd['X-Armada-View'] = viewText();
  return hd;
}
const ROLE_RANK = { viewer: 1, operator: 2, admin: 3 };
const atLeast = role => !!me && ROLE_RANK[me.role] >= ROLE_RANK[role];
const fleetWide = () => !!me && !me.scoped;

class APIError extends Error {
  constructor(status, msg) { super(msg); this.status = status; }
}

async function api(method, path, body) {
  const opts = { method, headers: authHeaders() };
  if (body instanceof FormData) {
    opts.body = body;
  } else if (body instanceof Blob) {
    opts.headers['Content-Type'] = body.type || 'application/octet-stream';
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
    for (const [k, v] of Object.entries(authHeaders())) xhr.setRequestHeader(k, v);
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
  const res = await fetch(`/api/v1/releases/${r.id}/artifact`, { headers: authHeaders() });
  if (!res.ok) throw new APIError(res.status, 'download failed (HTTP ' + res.status + ')');
  const url = URL.createObjectURL(await res.blob());
  const a = h('a', { href: url, download: `${r.package}-${r.version}` });
  document.body.append(a);
  a.click();
  a.remove();
  setTimeout(() => URL.revokeObjectURL(url), 10000);
}

// downloadReport saves a report in a file format (csv or html).
function downloadReport(query, format) {
  return downloadFile(`/api/v1/reports?${query}&format=${format}`, 'report.' + format,
    name => format === 'html' ? name.replace(/\.csv$/, '.html') : name);
}

// downloadFile saves an API response as a file, named as the server suggests.
async function downloadFile(path, fallbackName, rename = n => n) {
  const res = await fetch(path, { headers: authHeaders() });
  if (!res.ok) {
    let msg = 'download failed (HTTP ' + res.status + ')';
    try { msg = (await res.json()).error || msg; } catch { /* not JSON */ }
    throw new APIError(res.status, msg);
  }
  const name = (/filename="([^"]+)"/.exec(res.headers.get('Content-Disposition') || '') || [])[1] || fallbackName;
  const url = URL.createObjectURL(await res.blob());
  const a = h('a', { href: url, download: rename(name) });
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

// ---------- top menu: links that don't fit go under More ----------

const NAV_LINKS = [...document.querySelectorAll('#nav a')];
function layoutNav() {
  const nav = $('#nav'), more = $('#nav-more');
  if (!nav || !more) return;
  const menu = more.querySelector('.nav-more-menu'), btn = more.querySelector('.nav-more-btn');
  nav.append(...NAV_LINKS); // back in their order, then fold what doesn't fit
  more.classList.add('hidden');
  if (nav.scrollWidth > nav.clientWidth + 1) {
    more.classList.remove('hidden');
    const shown = NAV_LINKS.filter(a => getComputedStyle(a).display !== 'none');
    for (let i = shown.length - 1; i >= 0 && nav.scrollWidth > nav.clientWidth + 1; i--) menu.prepend(shown[i]);
  }
  // On a phone everything folds: then it's the menu.
  const anyLeft = [...nav.querySelectorAll('a')].some(a => getComputedStyle(a).display !== 'none');
  btn.textContent = anyLeft ? 'More ▾' : 'Menu ▾';
  btn.classList.toggle('active', !!menu.querySelector('a.active'));
}
(() => {
  const more = $('#nav-more');
  const btn = more.querySelector('.nav-more-btn'), menu = more.querySelector('.nav-more-menu');
  const close = () => { more.classList.remove('open'); btn.setAttribute('aria-expanded', 'false'); };
  btn.addEventListener('click', e => {
    e.stopPropagation();
    const open = !more.classList.contains('open');
    more.classList.toggle('open', open);
    btn.setAttribute('aria-expanded', String(open));
  });
  menu.addEventListener('click', close);
  document.addEventListener('click', e => { if (!more.contains(e.target)) close(); });
  document.addEventListener('keydown', e => { if (e.key === 'Escape') close(); });
  let pending = 0;
  window.addEventListener('resize', () => { cancelAnimationFrame(pending); pending = requestAnimationFrame(layoutNav); });
  if (document.fonts) document.fonts.ready.then(layoutNav);
})();

// ---------- customer / project picker ----------

async function loadGroupings() {
  try { groupings = await api('GET', '/api/v1/groupings'); } catch { groupings = []; }
  const params = new URLSearchParams(location.search);
  view = {};
  for (const g of groupings) {
    const v = params.get(g.key);
    if (v) view[g.key] = v;
  }
  renderScopeBar();
}
function setView(key, value) {
  if (value) view[key] = value; else delete view[key];
  const params = new URLSearchParams(location.search);
  for (const g of groupings) params.delete(g.key);
  for (const [k, v] of sortedEntries(view)) params.set(k, v);
  const qs = params.toString();
  history.replaceState(null, '', location.pathname + (qs ? '?' + qs : '') + location.hash);
  renderScopeBar();
  route();
}
function renderScopeBar() {
  const bar = $('#scopebar');
  const groups = (groupings || []).filter(g => g.values.length || view[g.key]);
  bar.classList.toggle('hidden', !groups.length);
  bar.classList.toggle('active', Object.keys(view).length > 0);
  if (!groups.length) return replace(bar);
  const label = k => k.charAt(0).toUpperCase() + k.slice(1).replace(/[_-]/g, ' ');
  replace(bar, groups.map(g => {
    const sel = h('select', { 'aria-label': label(g.key) },
      h('option', { value: '' }, `All ${g.key === 'customer' ? 'customers' : g.key === 'project' ? 'projects' : g.key + ' values'}`),
      g.values.map(v => h('option', { value: v.value }, `${v.value} (${v.devices})`)));
    if (view[g.key] && !g.values.some(v => v.value === view[g.key])) sel.append(h('option', { value: view[g.key] }, view[g.key] + ' (0)'));
    sel.value = view[g.key] || '';
    sel.addEventListener('change', () => setView(g.key, sel.value));
    return h('label', { class: 'scope-pick' }, label(g.key), sel);
  }),
  Object.keys(view).length ? [h('span', { class: 'scope-note' }, 'Every page shows only these devices.'),
    h('button', { class: 'btn ghost small', onclick: () => { view = {}; setView('', ''); } }, 'Show all')] : null);
}

function pageHead(title, sub, ...actions) {
  return h('div', { class: 'page-head' },
    h('div', {}, h('h1', {}, title), sub && h('div', { class: 'sub' }, sub)),
    actions.length ? h('div', { class: 'btn-row' }, actions) : null);
}
