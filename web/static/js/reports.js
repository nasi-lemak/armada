// Armada dashboard: Customer reports and the activity log.
'use strict';

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
      tile('Disconnections', rep.connectivity.outages, rep.connectivity.outages ? `on ${rep.connectivity.devices_hit} devices · ${rep.connectivity.network} network · ${rep.connectivity.restart} restart` : 'none'),
      tile('Replacements', sm.replacements, `${sm.maintenance_windows} maintenance windows`)),
    section('Devices', table(['Device', 'State', 'Availability', 'Downtime', 'Maintenance', 'Alerts', 'Drops', 'Updates', 'Health now'], rep.devices.map(d => h('tr', { class: 'clickable', onclick: () => { location.hash = '#/devices/' + d.id; } },
      h('td', {}, h('strong', {}, d.name), h('div', { class: 'muted mono' }, d.id)),
      h('td', {}, LIFECYCLE_TEXT[d.lifecycle] || d.lifecycle),
      h('td', { class: 'nowrap' + (low(d.availability) ? ' bad' : '') }, pct(d.availability)),
      h('td', { class: 'nowrap' }, d.downtime_minutes ? mins(d.downtime_minutes) : '—'),
      h('td', { class: 'nowrap' }, d.maintenance_minutes ? mins(d.maintenance_minutes) : '—'),
      h('td', {}, d.alerts),
      h('td', { title: d.disconnections ? `${d.network_drops} network, ${d.restarts} restart` : '' }, d.disconnections || '—'),
      h('td', {}, d.updates_succeeded, d.updates_failed ? h('span', { class: 'bad' }, ` (${d.updates_failed} failed)`) : null),
      h('td', {}, badge(d.health || 'ok')))), 'No devices.')),
    section('Alerts', table(['Started', 'Device', 'Alert', 'Severity', 'Lasted'], rep.alerts.map(a => h('tr', {},
      h('td', { class: 'nowrap' }, when(a.started_at)), h('td', {}, a.device_name),
      h('td', {}, a.rule, h('div', { class: 'muted' }, a.summary)), h('td', {}, badge(a.severity)),
      h('td', { class: 'nowrap' }, a.closed_out_of_service ? 'closed (out of service)' : a.resolved_at ? mins(a.duration_minutes) : 'still open'))), 'No alerts in this period.')),
    rep.updates.length ? section('Updates', table(['Release', 'Installed', 'Failed or rolled back'],
      rep.updates.map(u => h('tr', {}, h('td', { class: 'mono' }, u.name), h('td', {}, u.count), h('td', {}, u.failed || 0))))) : null,
    (rep.software || []).length ? section('Software', table(['Package', 'Newest', 'Up to date', 'Not yet updated'], rep.software.map(sw => h('tr', {},
      h('td', { class: 'mono' }, sw.package), h('td', { class: 'mono' }, sw.latest), h('td', {}, sw.up_to_date),
      h('td', {}, sw.behind.length ? sw.behind.map((b, i) => [i ? ', ' : '', h('a', { href: '#/devices/' + b.device_id }, b.name), h('span', { class: 'muted mono' }, ' ' + b.installed)]) : '—'))))) : null,
    rep.connectivity.worst.length ? section('Disconnections', table(['Device', 'Drops', 'Network', 'Restart', 'Offline', 'Last'], rep.connectivity.worst.map(w => h('tr', {},
      h('td', {}, h('a', { href: '#/devices/' + w.device_id }, w.name)), h('td', {}, w.outages), h('td', {}, w.network), h('td', {}, w.restart),
      h('td', { class: 'nowrap' }, duration(w.offline_seconds)), h('td', {}, when(w.last_at), w.last_detail ? h('div', { class: 'muted mono' }, w.last_detail) : null))))) : null,
    rep.changes.length ? section('Replacements and changes', table(['When', 'What'],
      rep.changes.map(e => h('tr', {}, h('td', { class: 'nowrap' }, when(e.time)), h('td', {}, e.message))))) : null,
    rep.maintenance.length ? section('Maintenance', table(['Maintenance', 'When', 'Devices'], rep.maintenance.map(m => h('tr', {},
      h('td', {}, m.name, m.note ? h('div', { class: 'muted' }, m.note) : null),
      h('td', { class: 'nowrap' }, when(m.starts_at), m.ends_at ? ' – ' + when(m.ends_at) : '', m.windows ? h('div', { class: 'muted' }, m.windows) : null),
      h('td', {}, m.devices))))) : null,
    h('div', { class: 'muted mt' }, rep.notes.map(n => h('p', {}, n))));
}

async function viewActivity(root, query) {
  const PAGE = 100;
  const search = h('input', { type: 'search', placeholder: 'Search messages…', 'aria-label': 'Search activity' });
  const typeSel = h('select', { 'aria-label': 'Event type' }, h('option', { value: '' }, 'All activity'));
  const actor = h('input', { type: 'search', placeholder: 'Who did it', title: 'A user\'s email, a token name, or system', 'aria-label': 'Who did it', class: 'w-actor' });
  const since = h('input', { type: 'date', 'aria-label': 'From', title: 'From' });
  const until = h('input', { type: 'date', 'aria-label': 'To', title: 'To (inclusive)' });
  const feed = h('div', { class: 'card flush' });
  const more = h('button', { class: 'btn hidden', onclick: () => guard(loadMore) }, 'Load more');
  const count = h('span', {});
  const fields = [[search, 'q'], [typeSel, 'type'], [actor, 'actor'], [since, 'since'], [until, 'until']];
  const params = new URLSearchParams(query || '');
  for (const [el, key] of fields) el.value = params.get(key) || '';
  const pendingType = params.get('type') || '';
  const filterQuery = () => {
    const p = new URLSearchParams();
    for (const [el, key] of fields) if (el.value.trim()) p.set(key, el.value.trim());
    return p;
  };
  const syncURL = () => {
    const p = filterQuery().toString();
    const hash = '#/activity' + (p ? '?' + p : '');
    if (location.hash !== hash) history.replaceState(null, '', hash);
  };
  const apiQuery = extra => {
    const p = filterQuery();
    p.set('tz', Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC');
    for (const [k, v] of Object.entries(extra)) p.set(k, v);
    return p.toString();
  };
  const exportCSV = () => guard(() => downloadFile('/api/v1/events?' + apiQuery({ format: 'csv' }), 'activity.csv'));
  append(root, pageHead('Activity', count, h('a', { class: 'btn need-operator', href: '#/webhooks', title: 'Send events to other systems as they happen' }, 'Webhooks'),
    h('button', { class: 'btn', onclick: exportCSV, title: 'Everything matching these filters, as a spreadsheet (up to 50,000 entries)' }, 'Export CSV')),
    h('div', { class: 'toolbar mb' }, search, typeSel, actor, h('label', { class: 'date-range muted' }, 'From', since), h('label', { class: 'date-range muted' }, 'to', until),
      h('button', { class: 'btn ghost', onclick: () => { for (const [el] of fields) el.value = ''; changed(); } }, 'Clear')),
    feed, h('div', { class: 'btn-row mt' }, more));

  let events = [], names = {}, exhausted = false, seq = 0;
  const render = () => {
    const filtered = [...filterQuery().keys()].length > 0;
    replace(feed, eventFeed(events, names, filtered ? 'Nothing matches these filters.' : 'No activity yet.'));
    replace(count, filtered ? `${events.length}${exhausted ? '' : '+'} matching` : 'Everything that happened across the fleet');
    more.classList.toggle('hidden', exhausted);
  };
  async function load(limit) {
    const my = ++seq;
    const evs = await api('GET', '/api/v1/events?' + apiQuery({ limit: String(limit) }));
    if (my !== seq) return; // a newer query won
    events = evs;
    exhausted = evs.length < limit;
    render();
  }
  async function loadMore() {
    if (!events.length) return;
    const my = seq;
    const evs = await api('GET', '/api/v1/events?' + apiQuery({ limit: String(PAGE), before_id: String(events[events.length - 1].id) }));
    if (my !== seq) return;
    events = events.concat(evs);
    exhausted = evs.length < PAGE;
    render();
  }
  let timer;
  const changed = () => { syncURL(); clearTimeout(timer); timer = setTimeout(() => guard(() => load(PAGE)), 250); };
  for (const [el] of fields) el.addEventListener(el.tagName === 'SELECT' || el.type === 'date' ? 'change' : 'input', changed);

  const [types, devs] = await Promise.all([api('GET', '/api/v1/events/types').catch(() => []), api('GET', '/api/v1/devices?lifecycle=all').catch(() => [])]);
  names = Object.fromEntries(devs.map(d => [d.id, d.name]));
  // Group types by prefix: "Devices (all)" then each type.
  const groups = {};
  for (const t of types) (groups[t.split('.')[0]] ||= []).push(t);
  for (const [g, ts] of Object.entries(groups).sort()) {
    typeSel.append(h('optgroup', { label: g }, ts.length > 1 ? h('option', { value: g + '.*' }, `${g}: all`) : null, ts.map(t => h('option', { value: t }, t))));
  }
  if (pendingType && ![...typeSel.options].some(o => o.value === pendingType)) typeSel.append(h('option', { value: pendingType }, pendingType));
  typeSel.value = pendingType;
  // Auto-refresh keeps however many entries are loaded.
  const refresh = () => load(Math.min(1000, Math.max(PAGE, events.length)));
  await guard(refresh);
  return refresh;
}
