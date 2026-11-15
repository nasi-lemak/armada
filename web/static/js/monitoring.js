// Armada dashboard: Monitoring: alerts, monitors, alert rules and channels.
'use strict';

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
    ['flaky_connectivity', 'Device keeps losing its connection'], ['site_outage', 'Many devices at a site offline at once'],
    ...(isAdmin ? [['update_failed', 'Update failed'], ['deployment_paused', 'Rollout paused'], ['server_cert_expiry', "Server's own certificate expiring"], ['site_silent', 'A site server stopped reporting']] : [])].map(([v, l]) => h('option', { value: v }, l)));
  const rSev = h('select', { 'aria-label': 'Minimum severity' }, ['critical', 'warning'].map(v => h('option', { value: v }, v)));
  const rKinds = h('input', { placeholder: 'any, or storage power temperature', 'aria-label': 'Check kinds' });
  const rOffline = h('input', { type: 'number', min: '1', value: '10', 'aria-label': 'Offline minutes' });
  const rExpiry = h('input', { type: 'number', min: '1', max: '365', value: '30', 'aria-label': 'Days before expiry' });
  const rDrops = h('input', { type: 'number', min: '1', value: '5', 'aria-label': 'Disconnections' });
  const rWindow = h('input', { type: 'number', min: '1', max: '720', value: '24', 'aria-label': 'Within hours' });
  const rGroup = h('input', { value: 'site', 'aria-label': 'Group devices by label' });
  const rMinDev = h('input', { type: 'number', min: '1', value: '3', 'aria-label': 'At least devices' });
  const rMinPct = h('input', { type: 'number', min: '1', max: '100', value: '50', 'aria-label': 'At least percent' });
  const rLabels = h('input', { placeholder: 'all devices, or site=sg', 'aria-label': 'Rule labels' });
  const rRepeat = h('input', { type: 'number', min: '0', value: '0', 'aria-label': 'Repeat minutes' });
  const rResolved = h('input', { type: 'checkbox', checked: true });
  const rChannels = h('div', { class: 'chips' });
  const syncCond = () => {
    rSev.closest('label').classList.toggle('hidden', rCond.value !== 'health');
    rKinds.closest('label').classList.toggle('hidden', rCond.value !== 'health');
    rOffline.closest('label').classList.toggle('hidden', !['device_offline', 'site_outage', 'site_silent'].includes(rCond.value));
    if (rCond.value === 'site_silent' && rOffline.value === '10') rOffline.value = '30';
    if (rCond.value === 'site_outage' && rOffline.value === '10') rOffline.value = '5';
    for (const el of [rDrops, rWindow]) el.closest('label').classList.toggle('hidden', rCond.value !== 'flaky_connectivity');
    for (const el of [rGroup, rMinDev, rMinPct]) el.closest('label').classList.toggle('hidden', rCond.value !== 'site_outage');
    rExpiry.closest('label').classList.toggle('hidden', rCond.value !== 'cert_expiry' && rCond.value !== 'server_cert_expiry');
    if (rCond.value === 'server_cert_expiry' && rExpiry.value === '30') rExpiry.value = '21';
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
        expiry_days: ['cert_expiry', 'server_cert_expiry'].includes(rCond.value) ? parseInt(rExpiry.value || '30', 10) : 0,
        disconnections: parseInt(rDrops.value || '5', 10), window_hours: parseInt(rWindow.value || '24', 10),
        group_by: rCond.value === 'site_outage' ? rGroup.value.trim() : '', min_devices: parseInt(rMinDev.value || '3', 10), min_percent: parseInt(rMinPct.value || '50', 10),
        repeat_minutes: parseInt(rRepeat.value || '0', 10), notify_resolved: rResolved.checked, client_scope: clientScope };
      if (await guard(() => api('POST', '/api/v1/alert-rules', body), 'Rule added')) { rName.value = ''; refresh(); }
    } },
    h('div', { class: 'form-grid' }, rFor.label, h('label', {}, 'Name', rName), h('label', {}, 'When', rCond), h('label', {}, 'At least', rSev),
      h('label', {}, 'Only check kinds', rKinds), h('label', { class: 'hidden' }, 'Offline for (minutes)', rOffline),
      h('label', { class: 'hidden' }, 'Disconnections', rDrops), h('label', { class: 'hidden' }, 'Within (hours)', rWindow),
      h('label', { class: 'hidden', title: 'Devices with the same value of this label count as one site, e.g. site, project or customer' }, 'Group by label', rGroup),
      h('label', { class: 'hidden' }, 'At least (devices)', rMinDev), h('label', { class: 'hidden' }, 'And at least (%)', rMinPct),
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
    const condLabel = { health: 'health', device_offline: 'offline', update_failed: 'update failed', deployment_paused: 'rollout paused', cert_expiry: 'certificate expiring', server_cert_expiry: "server's certificate expiring" };
    const forCell = sc => scopeText(sc) ? labelChips(sc.labels) : h('span', { class: 'muted' }, 'vendor-wide');
    replace(rulesBox, table(['Rule', 'For', 'When', 'Devices', 'Notify', ''],
      rules.map(r => h('tr', {},
        h('td', {}, h('strong', {}, r.name), ' ', badge(r.enabled ? 'enabled' : 'disabled'), r.created_by ? h('div', { class: 'muted' }, 'by ' + r.created_by) : null),
        h('td', {}, forCell(r.client_scope)),
        h('td', {}, condLabel[r.condition] || r.condition,
          r.condition === 'health' ? h('div', { class: 'muted' }, `${r.min_severity || 'critical'}+${(r.check_kinds || []).length ? ' · ' + r.check_kinds.join(', ') : ''}`) : null,
          r.condition === 'device_offline' ? h('div', { class: 'muted' }, `${r.offline_minutes || 10} min`) : null,
          r.condition === 'cert_expiry' ? h('div', { class: 'muted' }, `${r.expiry_days || 30} days before`) : null,
          r.condition === 'site_silent' ? h('div', { class: 'muted' }, `silent ${r.offline_minutes || 30} min`) : null,
          r.condition === 'flaky_connectivity' ? h('div', { class: 'muted' }, `${r.disconnections || 5}+ drops in ${r.window_hours || 24} h`) : null,
          r.condition === 'site_outage' ? h('div', { class: 'muted' }, `${r.min_devices || 3}+ devices and ${r.min_percent || 50}%+ per ${r.group_by || 'site'}, offline ${r.offline_minutes || 5} min`) : null,
          r.condition === 'server_cert_expiry' ? h('div', { class: 'muted' }, `${r.expiry_days || 21} days before`) : null),
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
