// Armada dashboard: Configuration profiles.
'use strict';

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
