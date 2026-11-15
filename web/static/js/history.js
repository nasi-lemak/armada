// Armada dashboard: Health history charts.
'use strict';

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
    h('div', { class: 'page-head' }, h('h2', {}, 'Fleet history', Object.keys(view).length ? h('span', { class: 'muted small' }, ' · whole fleet') : null),
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
