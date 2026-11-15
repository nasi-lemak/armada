// Armada dashboard: Event webhooks, sites and the API reference.
'use strict';

// ---------- event webhooks ----------

async function viewWebhooks(root) {
  const list = h('div', { class: 'card flush' });
  const shown = h('div', {});
  const nameIn = h('input', { required: true, placeholder: 'SIEM', 'aria-label': 'Webhook name' });
  const urlIn = h('input', { required: true, type: 'url', placeholder: 'https://hooks.example.com/armada', 'aria-label': 'Webhook URL' });
  const typesIn = h('input', { placeholder: 'all events, or e.g. device.* alert.* update.failed', 'aria-label': 'Event types' });
  const sample = { version: '1', id: 1234, time: new Date().toISOString(), type: 'device.enrolled', message: 'till-7 enrolled (linux/arm64, agent 1.4.2)',
    actor: '', device: { id: 'dev_…', name: 'till-7', channel: 'stable', labels: { customer: 'acme', site: 'sg' } }, hook: 'SIEM' };
  const showSecret = (h0, secret) => replace(shown, h('div', { class: 'callout stack' },
    h('div', {}, h('strong', {}, 'Copy the signing secret now'), ` — it won't be shown again (${h0.name}).`),
    h('div', { class: 'secret' }, secret),
    h('div', { class: 'muted' }, 'Each delivery is a POST with X-Armada-Signature: sha256=HMAC-SHA256(secret, X-Armada-Timestamp + "." + body). Check it, and reject old timestamps.')));
  append(root, pageHead('Event webhooks', 'Send activity-log events to other systems (a SIEM, a ticketing tool, your own service) as they happen.',
    h('a', { class: 'btn', href: '#/activity' }, 'Activity')),
    h('div', { class: 'card stack' }, h('h2', {}, 'Add a webhook'),
      h('form', { class: 'stack', onsubmit: async e => {
        e.preventDefault();
        const types = typesIn.value.trim() ? typesIn.value.trim().split(/[\s,]+/) : [];
        const hk = await guard(() => api('POST', '/api/v1/event-hooks', { name: nameIn.value.trim(), url: urlIn.value.trim(), types, enabled: true }), 'Webhook added');
        if (!hk) return;
        showSecret(hk, hk.secret);
        nameIn.value = urlIn.value = typesIn.value = '';
        refresh();
      } },
      h('div', { class: 'form-grid' }, h('label', {}, 'Name', nameIn), h('label', {}, 'URL', urlIn), h('label', {}, 'Event types', typesIn)),
      h('div', {}, h('button', { class: 'btn primary', type: 'submit' }, 'Add webhook'))),
      shown,
      h('details', {}, h('summary', {}, 'What a delivery looks like'),
        h('pre', { class: 'out' }, 'POST <your URL>\nContent-Type: application/json\nX-Armada-Event: device.enrolled\nX-Armada-Timestamp: 1767225600\nX-Armada-Signature: sha256=…\n\n' + JSON.stringify(sample, null, 2)),
        h('p', { class: 'muted' }, 'Deliveries are retried a few times; the event id is unique, so drop duplicates. A client\'s webhook gets only events about its own devices and must use https. After 20 failed deliveries in a row a webhook is turned off.'))),
    h('div', { class: 'spacer' }), list);

  async function refresh() {
    const hooks = await api('GET', '/api/v1/event-hooks');
    replace(list, table(['Webhook', 'Events', 'Last delivery', ''], hooks.map(hk => {
      const ld = hk.last_delivery;
      return h('tr', {},
        h('td', {}, h('strong', {}, hk.name), ' ', hk.enabled ? null : h('span', { class: 'badge' }, 'off'),
          h('div', { class: 'muted mono' }, hk.url),
          hk.disabled_reason ? h('div', { class: 'error' }, hk.disabled_reason) : null),
        h('td', {}, hk.types && hk.types.length ? h('span', { class: 'mono' }, hk.types.join(' ')) : 'all events',
          hk.client_scope && Object.keys(hk.client_scope.labels || {}).length ? h('div', { class: 'muted' }, 'only ' + scopeText(hk.client_scope)) : null),
        h('td', {}, ld ? [h('span', { class: 'badge ' + (ld.ok ? 'ok' : 'bad') }, ld.ok ? 'delivered' : 'failed'), ' ',
          h('span', { class: 'mono' }, ld.event_type), h('span', { class: 'muted' }, ' ' + ago(ld.at)),
          ld.error ? h('div', { class: 'error' }, ld.error) : null, hk.failures > 1 ? h('div', { class: 'muted' }, `${hk.failures} failures in a row`) : null]
          : h('span', { class: 'muted' }, 'nothing sent yet')),
        h('td', { class: 'nowrap' }, h('div', { class: 'btn-row' },
          h('button', { class: 'btn small', onclick: async () => {
            const r = await guard(() => api('POST', `/api/v1/event-hooks/${hk.id}/test`));
            if (r) toast(r.ok ? 'Test delivered' : 'Test failed: ' + r.error, !r.ok);
          } }, 'Send test'),
          h('button', { class: 'btn small ghost', onclick: async () => {
            if (await guard(() => api('PUT', '/api/v1/event-hooks/' + hk.id, { ...hk, enabled: !hk.enabled }), hk.enabled ? 'Turned off' : 'Turned on')) refresh();
          } }, hk.enabled ? 'Turn off' : 'Turn on'),
          h('button', { class: 'btn small ghost', onclick: async () => {
            if (!confirm(`Make a new signing secret for ${hk.name}? The old one stops working at once.`)) return;
            const r = await guard(() => api('PUT', '/api/v1/event-hooks/' + hk.id, { ...hk, rotate_secret: true }));
            if (r) { showSecret(r, r.secret); refresh(); }
          } }, 'New secret'),
          h('button', { class: 'btn small ghost danger', onclick: async () => {
            if (!confirm(`Delete the webhook ${hk.name}?`)) return;
            if (await guard(() => api('DELETE', '/api/v1/event-hooks/' + hk.id), 'Deleted') !== undefined) refresh();
          } }, 'Delete'))));
    }), 'No event webhooks yet.'));
  }
  await guard(refresh);
  return refresh;
}

// ---------- sites ----------

// Sites: other Armada servers (on-prem installs, regions) that report here.
const SITE_STATE = { fresh: ['ok', 'reporting'], stale: ['bad', 'not reporting'], never: ['', 'waiting for first report'], file: ['info', 'imported by hand'] };
async function viewSites(root) {
  const list = h('div', { class: 'stack' });
  const totals = h('div', { class: 'grid cols-4' });
  const shown = h('div', {});
  const nameIn = h('input', { required: true, placeholder: 'Orchard store (on-prem)', 'aria-label': 'Site name' });
  const noteIn = h('input', { placeholder: 'optional', 'aria-label': 'Site note' });
  const showToken = (site, token) => replace(shown, h('div', { class: 'callout stack' },
    h('div', {}, h('strong', {}, `Token for ${site.name}`), ' — copy it now; it won\'t be shown again.'),
    h('div', { class: 'secret' }, token),
    h('div', {}, 'Start the site\'s server with:'),
    h('div', { class: 'secret' }, `ARMADA_CENTRAL_TOKEN=${token} armada-server … -central-url ${location.origin}`),
    h('div', { class: 'muted' }, 'It reports every 5 minutes, outbound only (a proxy works: HTTPS_PROXY). For a site with no route here, run ',
      h('code', {}, 'armadactl site export -o snapshot.json'), ' there and use Import snapshot below.')));
  append(root, pageHead('Sites', 'Other Armada servers — on-prem installs, regions — reporting here, so you can watch them all in one place.'),
    totals, h('div', { class: 'spacer' }),
    h('div', { class: 'card stack' }, h('h2', {}, 'Add a site'),
      h('form', { class: 'inline', onsubmit: async e => {
        e.preventDefault();
        const site = await guard(() => api('POST', '/api/v1/sites', { name: nameIn.value.trim(), note: noteIn.value.trim() }), 'Site added');
        if (site) { showToken(site, site.token); nameIn.value = noteIn.value = ''; refresh(); }
      } }, h('label', {}, 'Name', nameIn), h('label', {}, 'Note', noteIn), h('button', { class: 'btn primary', type: 'submit' }, 'Add site')),
      shown),
    h('div', { class: 'spacer' }), list);

  const stat = (label, value, tone) => h('div', { class: 'site-stat' }, h('div', { class: 'v ' + (tone || '') }, value), h('div', { class: 'l' }, label));
  async function refresh() {
    const sites = await api('GET', '/api/v1/sites');
    const snaps = sites.map(x => x.snapshot).filter(Boolean);
    const sum = f => snaps.reduce((n, sn) => n + f(sn), 0);
    const stale = sites.filter(x => x.state === 'stale' || x.state === 'never').length;
    replace(totals, sites.length ? [
      h('div', { class: 'card stat' }, h('div', { class: 'label' }, 'Sites'), h('div', { class: 'value' }, sites.length), h('div', { class: 'hint' + (stale ? ' bad' : '') }, stale ? `${stale} not reporting` : 'all reporting')),
      h('div', { class: 'card stat' }, h('div', { class: 'label' }, 'Devices'), h('div', { class: 'value' }, sum(sn => sn.summary.devices)), h('div', { class: 'hint' }, `${sum(sn => sn.summary.offline)} offline`)),
      h('div', { class: 'card stat' }, h('div', { class: 'label' }, 'Firing alerts'), h('div', { class: 'value' }, sum(sn => sn.summary.firing_alerts)), h('div', { class: 'hint' }, 'across all sites')),
      h('div', { class: 'card stat' }, h('div', { class: 'label' }, 'Needs attention'), h('div', { class: 'value' }, sum(sn => sn.insights.software.reduce((n, p) => n + p.behind.length, 0) + sn.insights.certificates.items.length)),
        h('div', { class: 'hint' }, 'software behind + certificates'))] : null);
    replace(list, sites.length ? sites.map(site => {
      const sn = site.snapshot;
      const [tone, text] = SITE_STATE[site.state] || ['', site.state];
      const fileIn = h('input', { type: 'file', accept: '.json,application/json', class: 'hidden' });
      fileIn.addEventListener('change', async () => {
        const f = fileIn.files[0];
        if (!f) return;
        let snap;
        try { snap = JSON.parse(await f.text()); } catch { return toast('That file isn\'t a snapshot (JSON)', true); }
        if (await guard(() => api('POST', `/api/v1/sites/${site.id}/import`, snap), 'Snapshot imported')) refresh();
      });
      const behind = sn ? sn.insights.software.reduce((n, p) => n + p.behind.length, 0) : 0;
      const certs = sn ? sn.insights.certificates.items.length : 0;
      const bk = sn && sn.summary.backup;
      return h('div', { class: 'card site-card' },
        h('div', { class: 'page-head' },
          h('div', {}, h('h2', {}, site.name, ' ', h('span', { class: 'badge ' + tone }, text)),
            h('div', { class: 'muted' }, site.last_report_at ? `report from ${ago(sn ? sn.generated_at : site.last_report_at)}` : 'no report yet',
              sn && sn.server_version ? ` · server ${sn.server_version}` : '', site.note ? ' · ' + site.note : '')),
          h('div', { class: 'btn-row' },
            sn && sn.public_url ? h('a', { class: 'btn small', href: sn.public_url, target: '_blank', rel: 'noopener' }, 'Open its dashboard') : null,
            h('button', { class: 'btn small ghost', onclick: () => fileIn.click() }, 'Import snapshot'), fileIn,
            h('button', { class: 'btn small ghost', onclick: async () => {
              if (!confirm(`Make a new token for ${site.name}? The old one stops working, and the site server needs the new one.`)) return;
              const r = await guard(() => api('POST', `/api/v1/sites/${site.id}/token`));
              if (r) { showToken(r, r.token); refresh(); }
            } }, 'New token'),
            h('button', { class: 'btn small ghost danger', onclick: async () => {
              if (confirm(`Remove ${site.name}? Its reports stop being accepted.`) && await guard(() => api('DELETE', '/api/v1/sites/' + site.id), 'Removed') !== undefined) refresh();
            } }, 'Remove'))),
        sn ? h('div', { class: 'site-stats' },
          stat('devices', `${sn.summary.online}/${sn.summary.online + sn.summary.offline}`, sn.summary.offline ? 'warn' : 'ok'),
          stat('health issues', sn.summary.health_critical + sn.summary.health_warning, sn.summary.health_critical ? 'bad' : sn.summary.health_warning ? 'warn' : 'ok'),
          stat('firing alerts', sn.summary.firing_alerts, sn.summary.firing_alerts ? 'bad' : 'ok'),
          stat('software behind', behind, behind ? 'warn' : 'ok'),
          stat('certificates due', certs, certs ? 'warn' : 'ok'),
          stat('disconnections (7d)', sn.insights.connectivity.outages, sn.insights.connectivity.outages ? 'warn' : 'ok'),
          stat('backups', bk ? (bk.warning ? (bk.enabled ? 'failing' : 'off') : 'ok') : '—', bk && bk.warning ? 'bad' : 'ok')) : null,
        sn ? h('details', {}, h('summary', {}, 'Details'),
          sn.firing_alerts && sn.firing_alerts.length ? [h('h3', {}, 'Firing alerts'), h('ul', { class: 'mini-list' }, sn.firing_alerts.map(a =>
            h('li', {}, badge(a.severity), ' ', a.rule, h('span', { class: 'muted' }, ` · ${a.summary} · since ${ago(a.started_at)}`))))] : null,
          sn.insights.offline_now.length ? [h('h3', {}, 'Offline now'), h('ul', { class: 'mini-list' }, sn.insights.offline_now.slice(0, 20).map(o =>
            h('li', {}, o.name, h('span', { class: 'muted' }, ' · last seen ' + ago(o.since)))))] : null,
          behind ? [h('h3', {}, 'Software behind'), h('ul', { class: 'mini-list' }, sn.insights.software.flatMap(p => p.behind.slice(0, 20).map(b =>
            h('li', {}, b.name, h('span', { class: 'muted mono' }, ` ${p.package} ${b.installed} → ${b.latest}`)))))] : null,
          certs ? [h('h3', {}, 'Certificates'), h('ul', { class: 'mini-list' }, sn.insights.certificates.items.slice(0, 20).map(c =>
            h('li', {}, c.device_name, h('span', { class: 'mono' }, ' ' + c.target), h('span', { class: 'muted' }, c.days_left < 0 ? ` · expired ${-c.days_left}d ago` : ` · ${c.days_left}d left`))))] : null,
          (sn.groupings || []).filter(g => g.values.length).map(g => [h('h3', {}, g.key), h('div', { class: 'chips' }, g.values.map(v => h('span', { class: 'chip' }, `${v.value} (${v.devices})`)))]),
          bk && bk.warning ? [h('h3', {}, 'Backups'), h('p', { class: 'muted' }, bk.warning)] : null) : null);
    }) : h('div', { class: 'card' }, h('div', { class: 'empty' }, 'No sites yet. Add one for each other Armada server you want to watch from here.')));
  }
  await guard(refresh);
  return refresh;
}

// ---------- API reference ----------

async function viewAPIDocs(root) {
  const search = h('input', { type: 'search', placeholder: 'Filter: path, summary or role…', 'aria-label': 'Filter endpoints' });
  const body = h('div', { class: 'stack' });
  let doc;
  const download = () => saveText('armada-openapi.json', JSON.stringify(doc, null, 2), 'application/json');
  append(root, pageHead('API reference', 'Every endpoint of the admin API, with the role it needs. Use a personal API token: Authorization: Bearer <token>.',
    h('button', { class: 'btn', onclick: () => doc && download() }, 'Download OpenAPI')),
  h('div', { class: 'toolbar mb' }, search), body);
  const render = () => {
    const q = search.value.trim().toLowerCase();
    const groups = {};
    for (const [path, ops] of Object.entries(doc.paths)) {
      for (const [method, op] of Object.entries(ops)) {
        const text = `${method} ${path} ${op.summary} ${op['x-armada-min-role']}`.toLowerCase();
        if (q && !text.includes(q)) continue;
        (groups[op.tags[0]] ||= []).push({ method: method.toUpperCase(), path, op });
      }
    }
    const order = { GET: 0, POST: 1, PUT: 2, PATCH: 3, DELETE: 4 };
    replace(body, Object.keys(groups).sort().map(tag => h('div', { class: 'card flush api-ref' }, h('h2', {}, tag),
      table(['', 'Path', 'What it does', 'Needs'], groups[tag].sort((a, b) => a.path.localeCompare(b.path) || order[a.method] - order[b.method]).map(e => h('tr', {},
        h('td', {}, h('span', { class: 'method m-' + e.method.toLowerCase() }, e.method)),
        h('td', { class: 'mono' }, e.path),
        h('td', {}, e.op.summary),
        h('td', { class: 'nowrap' }, e.op['x-armada-min-role'], e.op['x-armada-client-accounts'] ? null : h('div', { class: 'muted' }, 'fleet-wide only'))))))));
    if (!Object.keys(groups).length) replace(body, h('div', { class: 'empty' }, 'No endpoint matches.'));
  };
  search.addEventListener('input', render);
  await guard(async () => { doc = await api('GET', '/api/v1/openapi.json'); render(); });
}
