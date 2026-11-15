// Armada dashboard: Devices: the list, bulk operations, scheduled actions, asset labels, a device's page, its timeline and logs.
'use strict';

async function viewDevices(root, query) {
  const search = h('input', { type: 'search', placeholder: 'Search name, serial, asset tag, IP, label…', 'aria-label': 'Search devices' });
  const status = h('select', { 'aria-label': 'Status filter' }, h('option', { value: '' }, 'All statuses'), h('option', { value: 'online' }, 'Online'), h('option', { value: 'offline' }, 'Offline'));
  const healthSel = h('select', { 'aria-label': 'Health filter' }, ['', 'critical', 'warning', 'unknown', 'ok'].map(v => h('option', { value: v }, v ? 'Health: ' + v : 'Any health')));
  const kindSel = h('select', { 'aria-label': 'Kind filter' }, [['', 'All kinds'], ['agent', 'Agents'], ['peripheral', 'Peripherals']].map(([v, l]) => h('option', { value: v }, l)));
  const lifeSel = h('select', { 'aria-label': 'Lifecycle filter' }, [['', 'In the fleet'], ['active', 'In service'], ['spare', 'Spares'],
    ['repair', 'In repair'], ['retired', 'Retired'], ['all', 'All, with retired']].map(([v, l]) => h('option', { value: v }, l)));
  const list = h('div', { class: 'card flush' });
  const count = h('span', {});
  const selected = new Set();
  const bulk = bulkBar(selected, () => { render(); guard(refresh); });
  // Filters live in the URL, so a view can be bookmarked, shared and kept on refresh.
  const params = new URLSearchParams(query || '');
  search.value = params.get('q') || '';
  for (const [el, key] of [[status, 'status'], [healthSel, 'health'], [kindSel, 'kind'], [lifeSel, 'lifecycle']]) {
    const v = params.get(key);
    if (v && [...el.options].some(o => o.value === v)) el.value = v;
  }
  const syncURL = () => {
    const p = new URLSearchParams();
    if (search.value.trim()) p.set('q', search.value.trim());
    for (const [el, key] of [[status, 'status'], [healthSel, 'health'], [kindSel, 'kind'], [lifeSel, 'lifecycle']]) if (el.value) p.set(key, el.value);
    const hash = '#/devices' + (p.toString() ? '?' + p : '');
    if (location.hash !== hash) history.replaceState(null, '', hash);
  };
  let shownDevices = [];
  const exportCSV = () => {
    const cell = v => {
      v = String(v ?? '');
      if (/^[=+\-@\t\r]/.test(v)) v = "'" + v; // not a formula in spreadsheets
      return /[",\n]/.test(v) ? '"' + v.replace(/"/g, '""') + '"' : v;
    };
    const head = ['id', 'name', 'serial', 'asset_tag', 'location', 'contact', 'status', 'lifecycle', 'health', 'channel', 'platform', 'labels', 'packages', 'ips', 'last_seen', 'enrolled'];
    const rows = shownDevices.map(d => { const inv = d.inventory || {}; return [d.id, d.name, d.info.serial, inv.asset_tag, inv.location, inv.contact, d.status, d.lifecycle, d.health || 'ok',
      d.channel, d.parent_id ? 'ble/' + d.info.model : `${d.info.os}/${d.info.arch}`, sortedEntries(d.labels).map(([k, v]) => k + '=' + v).join(' '),
      sortedEntries(d.packages).map(([k, v]) => k + '=' + v).join(' '), (d.info.ips || []).join(' '), d.last_seen_at, d.enrolled_at]; });
    saveText(`devices-${new Date().toISOString().slice(0, 10)}.csv`, [head, ...rows].map(r => r.map(cell).join(',')).join('\n') + '\n', 'text/csv');
  };
  const recent = h('div', { class: 'need-operator mt' });
  append(root, pageHead('Devices', count, h('a', { class: 'btn need-operator', href: '#/schedules' }, 'Scheduled actions'),
    h('a', { class: 'btn primary need-operator', href: '#/enroll' }, 'Enroll devices')),
    h('div', { class: 'toolbar' }, search, status, healthSel, kindSel, lifeSel,
      h('button', { class: 'btn', onclick: exportCSV, title: 'The devices shown, with their inventory, as a spreadsheet' }, 'Export CSV')),
    bulk.el, h('div', { class: 'spacer' }), list, recent);
  let devices = [];
  function render() {
    const q = search.value.trim().toLowerCase();
    const names = Object.fromEntries(devices.map(d => [d.id, d.name]));
    const shown = devices.filter(d => (!status.value || d.status === status.value) && (!healthSel.value || d.health === healthSel.value) &&
      (!kindSel.value || (kindSel.value === 'peripheral') === !!d.parent_id) && (!q || [
      d.id, d.name, d.channel, d.info.hostname, d.info.model, d.info.machine_id, d.info.serial, ...(d.info.ips || []),
      ...Object.values(d.inventory || {}),
      ...Object.entries(d.labels).map(([k, v]) => k + '=' + v),
    ].some(s => (s || '').toLowerCase().includes(q))));
    shownDevices = shown;
    syncURL();
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
          d.info.serial || (d.inventory && d.inventory.asset_tag) ? h('div', { class: 'muted' },
            [d.inventory && d.inventory.asset_tag ? 'asset ' + d.inventory.asset_tag : null, d.info.serial ? 'S/N ' + d.info.serial : null].filter(Boolean).join(' · ')) : null,
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

function inventoryRows(inv) {
  if (!inv) return [];
  return [['Asset tag', inv.asset_tag], ['Location', inv.location], ['On-site contact', inv.contact], ['Notes', inv.notes]]
    .filter(([, v]) => v).flatMap(([k, v]) => [h('dt', {}, k), h('dd', { class: k === 'Notes' ? 'pre-wrap' : '' }, v)]);
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

// ---------- scheduled actions ----------

const SCHEDULE_OPS = [['reboot', 'Reboot'], ['restart-agent', 'Restart the agent'], ['lock', 'Lock (Android)'], ['wake', 'Check in now'], ['netcheck', 'Network check'], ['exec', 'Run a command']];
const WEEKDAYS = ['mon', 'tue', 'wed', 'thu', 'fri', 'sat', 'sun'];
function scheduleWhen(sa) {
  const days = !sa.days || !sa.days.length || sa.days.length === 7 ? 'every day' : sa.days.map(d => d[0].toUpperCase() + d.slice(1)).join(', ');
  return `${days} at ${sa.at} ${sa.time_zone || 'UTC'}`;
}
function scheduleTargets(sa) {
  const sel = sa.selector || {};
  const parts = [sel.channel && 'channel ' + sel.channel, ...sortedEntries(sel.labels).map(([k, v]) => k + '=' + v),
    sel.device_ids && sel.device_ids.length && `${sel.device_ids.length} chosen devices`].filter(Boolean);
  return parts.join(' · ') || (sa.client_scope && Object.keys(sa.client_scope.labels || {}).length ? 'all of this client\'s devices' : 'all devices');
}
function lastRunCell(r) {
  if (!r) return h('span', { class: 'muted' }, 'not yet');
  const when = h('span', { class: 'muted', title: new Date(r.at).toLocaleString() }, ' ' + ago(r.at));
  if (r.missed) return [h('span', { class: 'badge warn' }, 'missed'), when];
  if (r.error) return [h('span', { class: 'badge bad', title: r.error }, 'failed'), when, h('div', { class: 'muted' }, r.error)];
  return [h('a', { href: '#/bulk/' + r.bulk_id }, `${r.applied} device${r.applied === 1 ? '' : 's'}`),
    r.skipped ? h('span', { class: 'muted' }, ` (${r.skipped} skipped)`) : null, r.manual ? h('span', { class: 'muted' }, ' · by hand') : null, when];
}

async function viewSchedules(root) {
  const fleetOperator = (ROLE_RANK[me.fleet_role] || 0) >= ROLE_RANK.operator;
  const list = h('div', { class: 'card flush' });
  const formCard = h('div', { class: 'card hidden' });
  const nameIn = h('input', { required: true, placeholder: 'Nightly kiosk reboot', 'aria-label': 'Name' });
  const opSel = h('select', { 'aria-label': 'Action' }, SCHEDULE_OPS.filter(([v]) => v !== 'exec' || fleetOperator).map(([v, l]) => h('option', { value: v }, l)));
  const cmdIn = h('input', { placeholder: 'systemctl restart kiosk', 'aria-label': 'Command', class: 'mono' });
  const cmdRow = h('label', { class: 'hidden' }, 'Command (runs as the agent\'s user)', cmdIn);
  const labelsIn = h('input', { placeholder: 'site=sg role=kiosk', 'aria-label': 'Device labels' });
  const chanIn = h('input', { placeholder: 'any', 'aria-label': 'Device channel' });
  const allIn = h('input', { type: 'checkbox' });
  const atIn = h('input', { type: 'time', value: '03:00', required: true, 'aria-label': 'Time' });
  const tzIn = h('input', { value: Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC', 'aria-label': 'Time zone' });
  const dayBoxes = WEEKDAYS.map(d => h('input', { type: 'checkbox', value: d, 'aria-label': d }));
  const holdIn = h('input', { type: 'number', min: '0', max: '1440', value: '30', 'aria-label': 'Hold alerts (minutes)' });
  const enabledIn = h('input', { type: 'checkbox', checked: true });
  const formTitle = h('h2', {}, 'New scheduled action');
  let editing = null;
  opSel.addEventListener('change', () => cmdRow.classList.toggle('hidden', opSel.value !== 'exec'));
  const openForm = sa => {
    editing = sa || null;
    formTitle.textContent = sa ? 'Edit ' + sa.name : 'New scheduled action';
    nameIn.value = sa ? sa.name : '';
    opSel.value = sa ? sa.operation : 'reboot';
    cmdIn.value = sa && sa.args ? sa.args.join(' ') : '';
    labelsIn.value = sa ? sortedEntries(sa.selector.labels).map(([k, v]) => k + '=' + v).join(' ') : '';
    chanIn.value = sa ? sa.selector.channel || '' : '';
    allIn.checked = sa ? !!sa.all : false;
    atIn.value = sa ? sa.at : '03:00';
    if (sa) tzIn.value = sa.time_zone || 'UTC';
    for (const b of dayBoxes) b.checked = !!(sa && sa.days && sa.days.includes(b.value));
    holdIn.value = sa && sa.hold_alerts_minutes !== undefined ? sa.hold_alerts_minutes : 30;
    enabledIn.checked = sa ? sa.enabled : true;
    cmdRow.classList.toggle('hidden', opSel.value !== 'exec');
    formCard.classList.remove('hidden');
    nameIn.focus();
  };
  const form = h('form', { class: 'stack', onsubmit: async e => {
    e.preventDefault();
    let labels;
    try { labels = parseLabels(labelsIn.value); } catch (err) { return toast(err.message, true); }
    const body = {
      name: nameIn.value.trim(), operation: opSel.value, args: opSel.value === 'exec' ? cmdIn.value.trim().split(/\s+/).filter(Boolean) : [],
      selector: { labels, channel: chanIn.value.trim() || undefined, device_ids: editing ? editing.selector.device_ids : undefined },
      all: allIn.checked, at: atIn.value, time_zone: tzIn.value.trim(), days: dayBoxes.filter(b => b.checked).map(b => b.value),
      hold_alerts_minutes: parseInt(holdIn.value || '0', 10), enabled: enabledIn.checked,
    };
    const saved = await guard(() => editing ? api('PUT', '/api/v1/scheduled-actions/' + editing.id, body) : api('POST', '/api/v1/scheduled-actions', body),
      editing ? 'Saved' : 'Scheduled');
    if (!saved) return;
    toast(`${saved.name}: ${saved.matches} device${saved.matches === 1 ? '' : 's'} match now` + (saved.next_run_at ? ', next run ' + new Date(saved.next_run_at).toLocaleString() : ''));
    formCard.classList.add('hidden');
    refresh();
  } },
  h('div', { class: 'form-grid' },
    h('label', {}, 'Name', nameIn), h('label', {}, 'Action', opSel), cmdRow,
    h('label', {}, 'Devices with labels', labelsIn), h('label', {}, 'Channel', chanIn),
    h('label', {}, 'Time', atIn), h('label', {}, 'Time zone', tzIn),
    h('label', { title: 'A maintenance window on the devices from each run, so a reboot doesn\'t page anyone. 0: none.' }, 'Hold alerts (minutes)', holdIn)),
  h('label', { class: 'check' }, allIn, 'Every device I can see (when no labels or channel are given)'),
  h('div', {}, h('div', { class: 'muted small' }, 'Days (none = every day)'),
    h('div', { class: 'chips' }, dayBoxes.map(b => h('label', { class: 'check chip' }, b, b.value[0].toUpperCase() + b.value.slice(1))))),
  h('label', { class: 'check' }, enabledIn, 'Enabled'),
  h('div', { class: 'btn-row' }, h('button', { class: 'btn primary', type: 'submit' }, 'Save'),
    h('button', { class: 'btn ghost', type: 'button', onclick: () => formCard.classList.add('hidden') }, 'Cancel')));
  append(formCard, formTitle, form);
  append(root, pageHead('Scheduled actions', 'Reboots, agent restarts and other actions that run on a schedule. Each run picks its devices afresh and holds their alerts while it runs.',
    h('button', { class: 'btn primary', onclick: () => openForm() }, 'New scheduled action')),
  formCard, h('div', { class: 'spacer' }), list);

  async function refresh() {
    const all = await api('GET', '/api/v1/scheduled-actions');
    replace(list, table(['Scheduled action', 'When', 'Devices', 'Next run', 'Last run', ''], all.map(sa => h('tr', {},
      h('td', {}, h('strong', {}, sa.name), h('div', { class: 'muted' }, (SCHEDULE_OPS.find(([v]) => v === sa.operation) || [, sa.operation])[1],
        sa.args && sa.args.length ? h('span', { class: 'mono' }, ': ' + sa.args.join(' ')) : null,
        sa.hold_alerts_minutes ? ` · alerts held ${sa.hold_alerts_minutes} min` : '')),
      h('td', {}, scheduleWhen(sa)),
      h('td', {}, scheduleTargets(sa), h('div', { class: 'muted' }, `${sa.matches} device${sa.matches === 1 ? '' : 's'} now`)),
      h('td', { class: 'nowrap' }, sa.enabled && sa.next_run_at ? [h('span', { title: new Date(sa.next_run_at).toLocaleString() },
        new Date(sa.next_run_at).toLocaleString(undefined, { weekday: 'short', hour: '2-digit', minute: '2-digit', hour12: false })),
        h('div', { class: 'muted' }, 'in ' + duration(Math.max(60, (new Date(sa.next_run_at) - Date.now()) / 1000)))] : h('span', { class: 'badge' }, 'paused')),
      h('td', {}, lastRunCell(sa.last_run)),
      h('td', { class: 'nowrap' }, h('div', { class: 'btn-row' },
        h('button', { class: 'btn small', onclick: async () => {
          if (!confirm(`Run "${sa.name}" now on ${sa.matches} device${sa.matches === 1 ? '' : 's'}?`)) return;
          const r = await guard(() => api('POST', `/api/v1/scheduled-actions/${sa.id}/run`));
          if (r) { toast(r.last_run.error ? 'Failed: ' + r.last_run.error : `Queued on ${r.last_run.applied} devices`, !!r.last_run.error); refresh(); }
        } }, 'Run now'),
        h('button', { class: 'btn small ghost', onclick: async () => {
          if (await guard(() => api('PUT', '/api/v1/scheduled-actions/' + sa.id, { ...sa, enabled: !sa.enabled }), sa.enabled ? 'Paused' : 'Resumed')) refresh();
        } }, sa.enabled ? 'Pause' : 'Resume'),
        h('button', { class: 'btn small ghost', onclick: () => openForm(sa) }, 'Edit'),
        h('button', { class: 'btn small ghost danger', onclick: async () => {
          if (!confirm(`Delete "${sa.name}"?`)) return;
          if (await guard(() => api('DELETE', '/api/v1/scheduled-actions/' + sa.id), 'Deleted') !== undefined) refresh();
        } }, 'Delete'))))), 'Nothing is scheduled yet.'));
  }
  await guard(refresh);
  return refresh;
}

// ---------- asset labels ----------

// Printable labels: a QR code that opens the device's page, with its name,
// serial and asset tag. Sheet layout for office printers; one label per
// page for label printers (the page size comes from style.css).
const LABEL_LAYOUTS = [['sheet', 'Sheet (A4 or Letter), 63.5 × 38.1 mm'], ['roll-62x29', 'Label printer, 62 × 29 mm'],
  ['roll-50x25', 'Label printer, 50 × 25 mm'], ['roll-100x50', 'Label printer, 100 × 50 mm']];
function qrSVG(l) {
  const svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
  svg.setAttribute('viewBox', `-2 -2 ${l.qr_size + 4} ${l.qr_size + 4}`); // with a quiet zone
  svg.setAttribute('shape-rendering', 'crispEdges');
  svg.setAttribute('role', 'img');
  svg.setAttribute('aria-label', 'QR code for ' + l.name);
  const bg = document.createElementNS(svg.namespaceURI, 'rect');
  for (const [k, v] of Object.entries({ x: -2, y: -2, width: l.qr_size + 4, height: l.qr_size + 4, fill: '#fff' })) bg.setAttribute(k, v);
  const path = document.createElementNS(svg.namespaceURI, 'path');
  path.setAttribute('d', l.qr);
  path.setAttribute('fill', '#000');
  svg.append(bg, path);
  return svg;
}
async function viewLabels(root, query) {
  const params = new URLSearchParams(query || '');
  const ids = (params.get('ids') || '').split(',').filter(Boolean);
  const layout = h('select', { 'aria-label': 'Layout' }, LABEL_LAYOUTS.map(([v, l]) => h('option', { value: v }, l)));
  try { layout.value = localStorage.getItem('armada.labelLayout') || 'sheet'; } catch { /* storage off */ }
  if (!layout.value) layout.value = 'sheet';
  const show = Object.fromEntries([['serial', 'Serial'], ['asset_tag', 'Asset tag'], ['location', 'Location']].map(([k, l]) => [k, h('input', { type: 'checkbox', checked: true, 'aria-label': l })]));
  const sheet = h('div', { class: 'labels' });
  const count = h('span', {});
  append(root, h('div', { class: 'no-print' },
    pageHead('Print labels', count, h('button', { class: 'btn primary', onclick: () => window.print() }, 'Print')),
    h('div', { class: 'toolbar mb' }, layout, ...Object.entries(show).map(([k, box]) => h('label', { class: 'check' }, box, box.getAttribute('aria-label')))),
    h('p', { class: 'muted' }, 'Scanning a label with a phone camera opens the device\'s page (after signing in). For a label printer, choose its label size here and "actual size" (100%) in the print dialog.')),
  sheet);
  let labels = [];
  const render = () => {
    sheet.className = 'labels ' + (layout.value === 'sheet' ? 'sheet' : 'roll s' + layout.value.slice(5));
    replace(sheet, labels.map(l => h('div', { class: 'asset-label' }, qrSVG(l),
      h('div', { class: 'al-text' },
        h('div', { class: 'al-name' }, l.name),
        show.asset_tag.checked && l.asset_tag ? h('div', { class: 'al-tag' }, l.asset_tag) : null,
        show.serial.checked && l.serial ? h('div', { class: 'al-mono' }, 'S/N ' + l.serial) : null,
        show.location.checked && l.location ? h('div', { class: 'al-small' }, l.location) : null,
        h('div', { class: 'al-small al-mono' }, l.device_id)))));
  };
  layout.addEventListener('change', () => { try { localStorage.setItem('armada.labelLayout', layout.value); } catch { /* storage off */ } render(); });
  for (const box of Object.values(show)) box.addEventListener('change', render);
  if (!ids.length) { replace(sheet, h('div', { class: 'empty' }, 'Choose devices on the Devices page, then Print QR labels.')); return; }
  await guard(async () => {
    labels = [];
    for (let i = 0; i < ids.length; i += 100) labels = labels.concat(await api('GET', '/api/v1/labels?ids=' + ids.slice(i, i + 100).join(',')));
    replace(count, `${labels.length} label${labels.length === 1 ? '' : 's'}`);
    render();
  });
}

// bulkBar acts on the selected devices: preview with a dry run, confirm,
// then apply with the number of devices the preview matched.
function bulkBar(selected, done) {
  const fleetOp = !!me && (ROLE_RANK[me.fleet_role] || 0) >= ROLE_RANK.operator;
  const ops = [
    ['reboot', 'Reboot'], ['restart-agent', 'Restart agent'], ['wake', 'Check in now'], ['netcheck', 'Network check'], ['lock', 'Lock screen (Android)'],
    fleetOp && ['exec', 'Run command…'], ['set_labels', 'Add labels…'], ['remove_labels', 'Remove labels…'], ['set_channel', 'Set channel…'],
    ['maintenance', 'Start maintenance…'], ['labels', 'Print QR labels'],
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
    if (op === 'labels') { location.hash = '#/labels?ids=' + ids.join(','); return; }
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

// timelineCard is a device's history: activity, alerts, updates and
// maintenance in one feed, newest first, grouped by day.
const TIMELINE_KINDS = [['', 'Everything'], ['alert', 'Alerts'], ['update', 'Updates'], ['action', 'Actions'], ['lifecycle', 'Lifecycle'], ['connectivity', 'Connectivity'],
  ['cert', 'Certificates'], ['health', 'Health'], ['config', 'Config'], ['maintenance', 'Maintenance'], ['other', 'Other']];
function timelineCard(id) {
  const PAGE = 30;
  const kindSel = h('select', { 'aria-label': 'Show', class: 'small' }, TIMELINE_KINDS.map(([v, l]) => h('option', { value: v }, l)));
  const list = h('div', {});
  const more = h('button', { class: 'btn small hidden', onclick: () => guard(loadMore) }, 'Load more');
  const el = h('div', { class: 'card flush timeline' },
    h('div', { class: 'card-head' }, h('h2', {}, 'Timeline'), kindSel), list, h('div', { class: 'tl-more' }, more));
  let entries = [], next = '', seq = 0;
  const q = extra => {
    const p = new URLSearchParams(extra);
    if (kindSel.value) p.set('kinds', kindSel.value);
    return `/api/v1/devices/${id}/timeline?` + p;
  };
  const dayLabel = d => {
    const today = new Date(); today.setHours(0, 0, 0, 0);
    const day = new Date(d); day.setHours(0, 0, 0, 0);
    const diff = Math.round((today - day) / 86400000);
    if (diff === 0) return 'Today';
    if (diff === 1) return 'Yesterday';
    return d.toLocaleDateString(undefined, { weekday: 'short', month: 'short', day: 'numeric', year: day.getFullYear() === today.getFullYear() ? undefined : 'numeric' });
  };
  const span = e => {
    if (e.ongoing) return e.kind === 'update' ? 'in progress' : 'ongoing';
    if (!e.end) return null;
    const sec = (new Date(e.end) - new Date(e.time)) / 1000;
    return sec > 0 ? (e.kind === 'maintenance' ? 'for ' : 'lasted ') + duration(sec) : null;
  };
  const render = () => {
    if (!entries.length) {
      replace(list, h('div', { class: 'empty' }, kindSel.value ? 'Nothing of this kind yet.' : 'Nothing has happened yet.'));
    } else {
      const out = [];
      let lastDay = '';
      for (const e of entries) {
        const d = new Date(e.time);
        const label = dayLabel(d);
        if (label !== lastDay) { out.push(h('li', { class: 'tl-day' }, label)); lastDay = label; }
        const meta = [TIMELINE_KINDS.find(([v]) => v === e.kind)?.[1] || e.kind, e.actor, span(e)].filter(Boolean);
        out.push(h('li', { class: 'tl-item' },
          h('time', { datetime: e.time, title: d.toLocaleString() }, d.toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit', hour12: false })),
          h('span', { class: 'dot ' + ({ good: 'ok', warn: 'warn', bad: 'bad' }[e.tone] || '') + (e.ongoing ? ' halo' : '') }),
          h('div', {}, h('div', {}, e.link ? h('a', { href: e.link }, e.title) : e.title),
            e.detail ? h('div', { class: 'muted pre-wrap' }, e.detail) : null,
            h('div', { class: 'type' }, meta.join(' · ')))));
      }
      replace(list, h('ul', { class: 'tl' }, out));
    }
    more.classList.toggle('hidden', !next);
  };
  async function load(limit) {
    const my = ++seq;
    const tl = await api('GET', q({ limit: String(limit) }));
    if (my !== seq) return;
    entries = tl.entries; next = tl.next || '';
    render();
  }
  async function loadMore() {
    if (!next) return;
    const my = seq;
    const tl = await api('GET', q({ limit: String(PAGE), cursor: next }));
    if (my !== seq) return;
    entries = entries.concat(tl.entries); next = tl.next || '';
    render();
  }
  kindSel.addEventListener('change', () => guard(() => load(PAGE)));
  // A refresh keeps however many entries are shown.
  return { el, refresh: () => load(Math.min(200, Math.max(PAGE, entries.length))) };
}

async function viewDevice(root, id) {
  const head = h('div', {});
  // Screenshots (when the server allows them): loaded images are kept here
  // so the 10-second refresh doesn't drop them.
  const shots = {};
  const shotBtn = h('button', { class: 'btn hidden', title: 'Capture the screen (the device must allow it: allow_screenshots). Recorded in the activity log.',
    onclick: () => queue('screenshot') }, 'Screenshot');
  const info = h('div', { class: 'card' });
  const meta = h('div', { class: 'card need-operator' });
  const actions = h('div', { class: 'card need-operator' });
  const lifeCard = h('div', { class: 'card fleet-only need-operator' });
  const updates = h('div', { class: 'card flush' });
  const actionLog = h('div', { class: 'card flush' });
  const timeline = timelineCard(id);
  const healthCard = h('div', { class: 'card flush' });
  const periphCard = h('div', { class: 'card flush hidden' });
  const logs = logsCard(id, () => dev);
  const configCard = h('div', { class: 'card flush hidden fleet-only' });
  const certCard = h('div', { class: 'card flush hidden' });
  const hist = historySection(id);
  append(root, head,
    h('div', { class: 'grid side' },
      h('div', { class: 'stack' }, info, healthCard, periphCard, configCard, certCard, logs.el, updates, actionLog),
      h('div', { class: 'stack' }, meta, actions, lifeCard, timeline.el)),
    hist.el);
  let dev;

  // Settings form (rendered once so typing isn't clobbered by refresh).
  const nameIn = h('input', { 'aria-label': 'Name' });
  const chanIn = h('input', { 'aria-label': 'Channel' });
  const labelIn = h('input', { placeholder: 'key=value', 'aria-label': 'New label' });
  const labelBox = h('div', {});
  const invTag = h('input', { placeholder: 'e.g. ACME-00417', 'aria-label': 'Asset tag' });
  const invLoc = h('input', { placeholder: 'e.g. Orchard store, till 3', 'aria-label': 'Location' });
  const invContact = h('input', { placeholder: 'name, phone', 'aria-label': 'On-site contact' });
  const invNotes = h('textarea', { rows: 2, placeholder: 'anything worth knowing on site', 'aria-label': 'Notes' });
  append(meta, h('h2', {}, 'Settings'),
    h('form', { class: 'stack', onsubmit: async e => {
      e.preventDefault();
      if (await guard(() => api('PATCH', '/api/v1/devices/' + id, { name: nameIn.value, channel: chanIn.value }), 'Saved')) refresh();
    } },
    h('div', { class: 'form-grid' }, h('label', {}, 'Name', nameIn), h('label', {}, 'Channel', chanIn)),
    h('button', { class: 'btn', type: 'submit' }, 'Save')),
    h('h2', { class: 'mt' }, 'Inventory'),
    h('form', { class: 'stack', onsubmit: async e => {
      e.preventDefault();
      const inventory = { asset_tag: invTag.value.trim(), location: invLoc.value.trim(), contact: invContact.value.trim(), notes: invNotes.value.trim() };
      if (await guard(() => api('PATCH', '/api/v1/devices/' + id, { inventory }), 'Saved')) {
        for (const el of [invTag, invLoc, invContact, invNotes]) el.dataset.loaded = el.value.trim();
        refresh();
      }
    } },
    h('div', { class: 'form-grid' }, h('label', {}, 'Asset tag', invTag), h('label', {}, 'Location', invLoc), h('label', {}, 'On-site contact', invContact)),
    h('label', {}, 'Notes', invNotes),
    h('button', { class: 'btn', type: 'submit' }, 'Save inventory')),
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
      h('button', { class: 'btn', onclick: () => queue('restart-agent') }, 'Restart agent'),
      h('button', { class: 'btn', title: 'Test the route from the device to the server: DNS, connection, TLS, HTTPS, gateway. The report shows under Actions.', onclick: () => queue('netcheck') }, 'Network check'),
      shotBtn),
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
    const inv = d.inventory || {};
    // Refresh a field only if nobody has typed in it since it was loaded.
    for (const [el, v] of [[invTag, inv.asset_tag], [invLoc, inv.location], [invContact, inv.contact], [invNotes, inv.notes]]) {
      if (document.activeElement !== el && (el.dataset.loaded === undefined || el.value === el.dataset.loaded)) {
        el.value = el.dataset.loaded = v || '';
      }
    }
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
        d.agent_status ? ' · ' + d.agent_status : ''),
        h('a', { class: 'btn small', href: '#/labels?ids=' + d.id, title: 'A QR code that opens this page, with the serial and asset tag' }, 'Print label')));
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
          h('dt', {}, 'Serial number'), h('dd', { class: 'mono' }, d.info.serial || h('span', { class: 'muted' }, 'not reported')),
          ...inventoryRows(d.inventory),
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
        h('td', {}, a.output ? h('pre', { class: 'out' }, a.output) : h('span', { class: 'muted' }, a.status === 'queued' || a.status === 'sent' ? 'waiting for device' : '—'),
          a.type === 'screenshot' && a.status === 'succeeded' ? screenshotCell(a) : null))),
      'No actions yet.'));
    await timeline.refresh();
    shotBtn.classList.toggle('hidden', !(me.features && me.features.screenshots) || (dev.info.os || '').toLowerCase() === 'android');
  }
  // A screenshot: fetched with the token (an <img> can't send it), shown as a data: URL.
  function screenshotCell(a) {
    if (shots[a.id]) return h('a', { href: shots[a.id], download: `screenshot-${a.id}.png`, title: 'Download' }, h('img', { class: 'shot', src: shots[a.id], alt: 'Screenshot' }));
    return h('button', { class: 'btn small', onclick: async e => {
      const res = await fetch(`/api/v1/actions/${a.id}/attachment`, { headers: authHeaders() });
      if (!res.ok) { let m = 'HTTP ' + res.status; try { m = (await res.json()).error || m; } catch { /* not JSON */ } return toast(m, true); }
      const blob = await res.blob();
      shots[a.id] = await new Promise(ok => { const fr = new FileReader(); fr.onload = () => ok(fr.result); fr.readAsDataURL(blob); });
      e.target.replaceWith(screenshotCell(a));
    } }, 'Show screenshot');
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
