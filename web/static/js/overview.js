// Armada dashboard: Overview page and Needs attention.
'use strict';

// ---------- views ----------

// insightsSection: what needs attention among the devices in view —
// software not on the latest release, certificates to renew, and how often
// devices lose their connection (and why).
function insightsSection() {
  let range = '7d';
  const seg = h('div', { class: 'seg', role: 'group', 'aria-label': 'Disconnections over' });
  const body = h('div', { class: 'grid cols-3' });
  const el = h('section', { class: 'mt', 'aria-label': 'Needs attention' },
    h('div', { class: 'page-head' }, h('h2', {}, 'Needs attention'), seg), body);
  for (const [v, l] of [['1d', '24h'], ['7d', '7d'], ['30d', '30d']]) {
    const b = h('button', { type: 'button', class: v === range ? 'on' : '', onclick: () => {
      range = v; seg.querySelectorAll('button').forEach(x => x.classList.toggle('on', x === b)); load(true);
    } }, l);
    seg.append(b);
  }
  const plural = (n, w) => `${n} ${w}${n === 1 ? '' : 's'}`;
  const more = (n, href, what) => n > 0 ? h('li', { class: 'muted' }, href ? h('a', { href }, `and ${n} more ${what}`) : `and ${n} more ${what}`) : null;
  const devLink = (id, name) => h('a', { href: '#/devices/' + id }, name);
  // Update these: a staged rollout per target release, to just the devices
  // that are behind (and not already updating).
  const catchUpButton = s => {
    const todo = s.behind.filter(b => !b.updating && b.release_id);
    if (!todo.length || (ROLE_RANK[me.fleet_role] || 0) < ROLE_RANK.operator) return null;
    return h('div', { class: 'btn-row mt' }, h('button', { class: 'btn small primary', onclick: async () => {
      const byRel = {};
      for (const b of todo) (byRel[b.release_id] ||= []).push(b);
      const versions = [...new Set(todo.map(b => b.latest))].join(', ');
      if (!confirm(`Start a staged rollout of ${s.package} ${versions} to the ${todo.length} device${todo.length === 1 ? '' : 's'} not on it?` +
        '\n\nIt uses the default stages and safety checks; you can pause it on the Deployments page.')) return;
      const where = Object.keys(view).length ? ' (' + Object.values(view).join(' · ') + ')' : '';
      const made = [];
      for (const [rel, devs] of Object.entries(byRel)) {
        const d = await guard(() => api('POST', '/api/v1/deployments', { name: `Catch up ${s.package} ${devs[0].latest}${where}`,
          release_id: rel, selector: { device_ids: devs.map(b => b.device_id) } }));
        if (d) made.push(d);
      }
      if (made.length === 1) location.hash = '#/deployments/' + made[0].id;
      else if (made.length) { toast(`Started ${made.length} rollouts`); location.hash = '#/deployments'; }
    } }, `Update these ${todo.length}`));
  };
  let seq = 0, loadedAt = 0;
  // It reads the whole fleet, so the overview's 10-second refresh only
  // reloads it once a minute (or when the range changes).
  async function load(force) {
    if (!force && Date.now() - loadedAt < 60_000) return;
    loadedAt = Date.now();
    const my = ++seq;
    const ins = await api('GET', '/api/v1/insights?range=' + range).catch(() => null);
    if (!ins || my !== seq) return;

    // Software.
    const behindAll = ins.software.reduce((n, s) => n + s.behind.length, 0);
    const sw = h('div', { class: 'card insight', 'data-insight': 'software' },
      h('h3', {}, 'Software not on the latest version'),
      h('div', { class: 'insight-big ' + (behindAll ? 'warn' : 'ok') }, behindAll ? plural(behindAll, 'device') + ' behind' : 'All up to date'),
      !ins.software.length ? h('p', { class: 'muted' }, 'No releases uploaded for these devices yet.') : null,
      ins.software.map(s => h('details', { class: 'insight-pkg', open: s.behind.length > 0 && ins.software.length <= 2 },
        h('summary', {}, h('strong', {}, s.package), ` ${s.latest}: `, `${s.up_to_date} up to date`,
          s.behind.length ? h('span', { class: 'warn-text' }, ` · ${s.behind.length} behind`) : null),
        s.behind.length ? h('ul', { class: 'mini-list' }, s.behind.slice(0, 8).map(b => h('li', {}, devLink(b.device_id, b.name),
          h('span', { class: 'muted mono' }, ` ${b.installed} → ${b.latest}`), b.updating ? [' ', badge(b.updating)] : null,
          b.channel !== 'stable' ? h('span', { class: 'muted' }, ' · ' + b.channel) : null)), more(s.behind.length - 8, null, 'devices')) : null,
        catchUpButton(s))));

    // Certificates.
    const c = ins.certificates;
    const need = c.items.length;
    const certLine = cert => {
      const r = cert.renewal && cert.renewal.status;
      const status = r === 'csr_ready' ? 'waiting for the signed certificate' : r === 'failed' ? 'renewal failed'
        : cert.days_left < 0 ? `expired ${-cert.days_left}d ago` : `expires in ${cert.days_left}d`;
      return h('li', {}, devLink(cert.device_id, cert.device_name), h('span', { class: 'mono' }, ' ' + cert.target), h('span', { class: 'muted' }, ' · ' + status),
        r && r !== 'csr_ready' && r !== 'failed' ? h('span', { class: 'muted' }, ' · renewal ' + (RENEWAL_TEXT[r] || r)) : null);
    };
    const certs = h('div', { class: 'card insight', 'data-insight': 'certificates' },
      h('h3', {}, 'Certificates to renew'),
      h('div', { class: 'insight-big ' + (c.expired || c.failed ? 'bad' : need ? 'warn' : 'ok') }, need ? plural(need, 'certificate') + ' need attention' : c.total ? 'Nothing due' : 'No certificates tracked'),
      need ? h('div', { class: 'muted' }, [c.expired && `${c.expired} expired`, c.expiring && `${c.expiring} expiring within 30 days`,
        c.waiting && `${c.waiting} waiting for the client's signed certificate`, c.failed && `${c.failed} renewal failed`].filter(Boolean).join(' · ')) : null,
      need ? h('ul', { class: 'mini-list' }, c.items.slice(0, 8).map(certLine), more(need - 8, '#/certificates', 'on the Certificates page')) : null);

    // Connectivity.
    const cn = ins.connectivity;
    const off = ins.offline_now;
    const rangeText = { '1d': 'last 24 hours', '7d': 'last 7 days', '30d': 'last 30 days' }[range];
    const conn = h('div', { class: 'card insight', 'data-insight': 'connectivity' },
      h('h3', {}, 'Disconnections'),
      h('div', { class: 'insight-big ' + (cn.outages ? 'warn' : 'ok') }, cn.outages ? `${plural(cn.outages, 'disconnection')} on ${plural(cn.devices_hit, 'device')}` : 'No disconnections'),
      h('div', { class: 'muted' }, rangeText, cn.outages ? ` · ${cn.network} network · ${cn.restart} restart/power` + (cn.unknown ? ` · ${cn.unknown} unknown` : '') +
        ` · ${duration(cn.offline_seconds)} offline in all` : ''),
      cn.worst.length ? h('ul', { class: 'mini-list' }, cn.worst.map(w => h('li', {}, devLink(w.device_id, w.name),
        h('span', {}, ` ${w.outages}×`), h('span', { class: 'muted' }, ` · ${duration(w.offline_seconds)} offline` +
          (w.network && w.restart ? ` · ${w.network} network, ${w.restart} restart` : w.restart ? ' · restarts' : ' · network')),
        w.last_detail ? h('div', { class: 'muted small mono' }, w.last_detail) : null))) : null,
      off.length ? [h('div', { class: 'insight-sub' }, h('strong', {}, `Offline now: ${off.length}`)),
        h('ul', { class: 'mini-list' }, off.slice(0, 6).map(o => h('li', {}, devLink(o.id, o.name), h('span', { class: 'muted' }, ' · last seen ' + ago(o.since)))),
          more(off.length - 6, '#/devices?status=offline', 'offline devices'))] : null);
    replace(body, sw, certs, conn);
  }
  return { el, load };
}

async function viewOverview(root) {
  const fleetHist = fleetHistorySection();
  fleetHist.el.classList.add('fleet-only');
  const stats = h('div', { class: 'grid cols-4' });
  const deps = h('div', {});
  const dist = h('div', {});
  const feed = h('div', {});
  const certWarning = h('div', {});
  const backupWarning = h('div', {});
  const attention = insightsSection();
  const viewName = sortedEntries(view).map(([, v]) => v).join(' · ');
  append(root,
    pageHead(viewName ? viewName : 'Fleet overview', viewName ? 'Only these devices: ' + viewText().replace(/;/g, ', ') : null),
    certWarning,
    backupWarning,
    stats,
    attention.el,
    fleetHist.el,
    h('div', { class: 'grid side mt' },
      h('div', { class: 'stack' },
        h('div', { class: 'card fleet-only' }, h('h2', {}, 'Active deployments'), deps),
        h('div', { class: 'card' }, h('h2', {}, 'Installed versions'), dist)),
      h('div', { class: 'card flush' }, h('h2', {}, 'Recent activity'), feed)));

  async function refresh() {
    if (fleetWide()) fleetHist.load();
    attention.load();
    const [sum, active, events] = await Promise.all([
      api('GET', '/api/v1/summary'),
      fleetWide() ? api('GET', '/api/v1/deployments?status=scheduled,running,paused') : [],
      api('GET', '/api/v1/events?limit=15'),
    ]);
    const tile = (label, value, hint, href) => h(href ? 'a' : 'div', { class: 'card stat', href },
      h('div', { class: 'label' }, label), h('div', { class: 'value' }, value), h('div', { class: 'hint' }, hint));
    // The server's own certificate: if it lapses, every device drops off.
    const sc = sum.server_cert;
    replace(certWarning, sc && (sc.warning || sc.error) ? h('div', { class: 'callout bad-callout mb' },
      h('strong', {}, sc.certificate && sc.days_left !== undefined && sc.days_left < 0 ? "The server's TLS certificate has expired. "
        : sc.warning ? `The server's TLS certificate expires in ${sc.days_left} days. ` : "The server's TLS certificate has a problem. "),
      sc.warning ? 'Renew it before then, or every device stops checking in. ' + (sc.source === 'tls-cert' ? 'Armada picks up the renewed files by itself.' : 'It is served by the proxy in front of Armada (-public-url).') : '',
      sc.error ? h('div', {}, sc.error) : null) : null);
    // Backups (admins): off, failing or overdue.
    const bk = sum.backup;
    replace(backupWarning, bk && bk.warning ? h('div', { class: 'callout ' + (bk.enabled ? 'bad-callout ' : '') + 'mb' },
      h('strong', {}, bk.enabled ? 'Backups need attention. ' : 'No automatic backups. '), bk.warning,
      bk.enabled ? h('div', { class: 'btn-row mt' }, h('button', { class: 'btn small', onclick: async () => {
        const st = await guard(() => api('POST', '/api/v1/backup/run'));
        if (st) { toast(st.last_error ? 'Backup failed: ' + st.last_error : 'Backed up', !!st.last_error); refresh(); }
      } }, 'Back up now')) : null) : null);
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

// eventFeed lists activity-log entries. names maps device IDs to names.
function eventFeed(events, names = {}, empty = 'No activity yet.') {
  events = events || [];
  if (!events.length) return h('div', { class: 'empty' }, empty);
  return h('ul', { class: 'feed' }, events.map(e => h('li', {},
    h('time', { datetime: e.time, title: new Date(e.time).toLocaleString() }, fmtTime(e.time)),
    h('div', {}, h('div', {}, e.message), h('div', { class: 'type' }, e.type,
      e.actor ? ' · ' + e.actor : null,
      e.device_id ? [' · ', h('a', { href: '#/devices/' + e.device_id }, names[e.device_id] || 'device')] : null,
      e.deployment_id ? [' · ', h('a', { href: '#/deployments/' + e.deployment_id }, 'rollout')] : null)))));
}
