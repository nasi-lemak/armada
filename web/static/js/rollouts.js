// Armada dashboard: Deployments, releases and Android apps.
'use strict';

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
