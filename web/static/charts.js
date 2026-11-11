// Small SVG charts for Armada: line charts (with hourly min–max bands) and
// status timelines. No dependencies; all text goes through textContent.
'use strict';

const SVGNS = 'http://www.w3.org/2000/svg';

function svgEl(tag, attrs) {
  const el = document.createElementNS(SVGNS, tag);
  for (const [k, v] of Object.entries(attrs || {})) if (v !== undefined && v !== null) el.setAttribute(k, v);
  return el;
}

const Charts = (() => {
  const fmtNum = v => {
    if (v === null || v === undefined || Number.isNaN(v)) return '—';
    const a = Math.abs(v);
    if (a >= 1e6) return (v / 1e6).toFixed(1).replace(/\.0$/, '') + 'M';
    if (a >= 1e4) return (v / 1e3).toFixed(1).replace(/\.0$/, '') + 'K';
    if (a >= 100) return Math.round(v).toLocaleString();
    if (a >= 10) return v.toFixed(1).replace(/\.0$/, '');
    return v.toFixed(2).replace(/\.?0+$/, '') || '0';
  };

  // "Nice" ticks: 3–5 round values covering [lo, hi].
  function ticks(lo, hi, count = 4) {
    if (hi === lo) { hi = lo + 1; lo = lo - (lo === 0 ? 0 : 1); }
    const span = hi - lo;
    const raw = span / count;
    const mag = Math.pow(10, Math.floor(Math.log10(raw)));
    const step = [1, 2, 2.5, 5, 10].map(m => m * mag).find(s => span / s <= count) || 10 * mag;
    const start = Math.floor(lo / step) * step;
    const out = [];
    for (let v = start; v < hi + step - 1e-9; v += step) out.push(+v.toFixed(10)); // last tick >= max
    out.step = step;
    return out;
  }

  // Tick labels carry exactly the precision the step needs (no "34, 34, 33.9").
  function tickFormatter(step) {
    const str = String(+step.toFixed(10));
    const decimals = str.includes('.') ? str.split('.')[1].length : 0;
    return v => (Math.abs(v) >= 1e4 ? fmtNum(v) : v.toLocaleString('en-US', { minimumFractionDigits: decimals, maximumFractionDigits: decimals }));
  }

  function timeLabel(t, spanSec) {
    const d = new Date(t * 1000);
    if (spanSec <= 2 * 86400) return d.toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit', hour12: false });
    return d.toLocaleDateString(undefined, { month: 'short', day: 'numeric' });
  }

  function timeFull(t) {
    return new Date(t * 1000).toLocaleString(undefined, { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit', hour12: false });
  }

  function timeTicks(from, to, width) {
    const span = to - from;
    const steps = [300, 900, 1800, 3600, 3 * 3600, 6 * 3600, 12 * 3600, 86400, 2 * 86400, 7 * 86400, 14 * 86400];
    const want = Math.max(2, Math.floor(width / 110));
    const step = steps.find(s => span / s <= want) || steps[steps.length - 1];
    const tz = new Date().getTimezoneOffset() * 60;
    const out = [];
    for (let t = Math.ceil((from - tz) / step) * step + tz; t <= to; t += step) out.push(t);
    return out;
  }

  // Split points into runs, breaking where samples are missing (device offline).
  function runs(points, maxGap) {
    const out = [];
    let cur = [];
    for (const p of points) {
      if (p.v === null || p.v === undefined) { if (cur.length) out.push(cur); cur = []; continue; }
      if (cur.length && p.t - cur[cur.length - 1].t > maxGap) { out.push(cur); cur = []; }
      cur.push(p);
    }
    if (cur.length) out.push(cur);
    return out;
  }

  const tooltip = (() => {
    let el;
    return {
      show(x, y, rows) {
        if (!el) { el = document.createElement('div'); el.className = 'viz-tooltip'; document.body.append(el); }
        el.replaceChildren(...rows);
        el.classList.remove('hidden');
        const r = el.getBoundingClientRect();
        let left = x + 14, top = y - r.height - 10;
        if (left + r.width > window.innerWidth - 8) left = x - r.width - 14;
        if (top < 8) top = y + 14;
        el.style.left = left + window.scrollX + 'px';
        el.style.top = top + window.scrollY + 'px';
      },
      hide() { if (el) el.classList.add('hidden'); },
    };
  })();

  function tipRow(cls, value, label) {
    const row = document.createElement('div');
    row.className = 'viz-tip-row';
    const key = document.createElement('span');
    key.className = 'viz-key ' + cls;
    const v = document.createElement('strong');
    v.textContent = value;
    const l = document.createElement('span');
    l.className = 'viz-tip-label';
    l.textContent = label;
    row.append(key, v, l);
    return row;
  }

  function tipHead(text) {
    const d = document.createElement('div');
    d.className = 'viz-tip-time';
    d.textContent = text;
    return d;
  }

  /**
   * Line chart.
   * opts: { from, to (unix s), resolution (s), unit, height,
   *         series: [{ name, cls ('s1' | 'warning' | 'critical'), points: [{t, v, min?, max?}] }],
   *         zeroBased (bool) }
   */
  function line(container, opts) {
    container.replaceChildren();
    container.classList.add('viz');
    const width = Math.max(240, container.clientWidth || 600);
    const height = opts.height || 150;
    const pad = { l: 44, r: opts.endLabels ? 70 : 12, t: 8, b: 22 };
    const plotW = width - pad.l - pad.r, plotH = height - pad.t - pad.b;
    const all = opts.series.flatMap(s => s.points.flatMap(p => [p.v, p.min, p.max])).filter(v => v !== null && v !== undefined);
    if (!all.length) {
      const e = document.createElement('div');
      e.className = 'viz-empty';
      e.textContent = 'No data in this range.';
      container.append(e);
      return;
    }
    let lo = Math.min(...all), hi = Math.max(...all);
    if (opts.zeroBased || lo >= 0 && lo < hi * 0.25) lo = Math.min(0, lo);
    if (opts.unit === '%') { lo = Math.max(0, Math.min(lo, 0)); hi = Math.max(hi, 10); }
    const yt = ticks(lo, hi);
    const fmtTick = tickFormatter(yt.step);
    lo = Math.min(lo, yt[0]); hi = Math.max(hi, yt[yt.length - 1]);
    const x = t => pad.l + (t - opts.from) / (opts.to - opts.from) * plotW;
    const y = v => pad.t + (1 - (v - lo) / (hi - lo || 1)) * plotH;

    const svg = svgEl('svg', { width, height, viewBox: `0 0 ${width} ${height}`, role: 'img', class: 'viz-svg' });
    const title = svgEl('title');
    title.textContent = opts.title || opts.series.map(s => s.name).join(', ');
    svg.append(title);
    for (const v of yt) {
      svg.append(svgEl('line', { x1: pad.l, x2: width - pad.r, y1: y(v), y2: y(v), class: v === yt[0] ? 'viz-axis' : 'viz-grid' }));
      const tx = svgEl('text', { x: pad.l - 6, y: y(v) + 4, 'text-anchor': 'end', class: 'viz-tick' });
      tx.textContent = fmtTick(v);
      svg.append(tx);
    }
    for (const t of timeTicks(opts.from, opts.to, plotW)) {
      const tx = svgEl('text', { x: x(t), y: height - 6, 'text-anchor': 'middle', class: 'viz-tick' });
      tx.textContent = timeLabel(t, opts.to - opts.from);
      svg.append(tx);
    }
    const maxGap = (opts.resolution || 300) * 2.5;
    for (const s of opts.series) {
      for (const run of runs(s.points, maxGap)) {
        // Min–max band for hourly roll-ups, or a light wash under a single series.
        if (run.some(p => p.min !== undefined && p.min !== null) && run.length > 1) {
          const top = run.map(p => `${x(p.t)},${y(p.max ?? p.v)}`);
          const bot = run.slice().reverse().map(p => `${x(p.t)},${y(p.min ?? p.v)}`);
          svg.append(svgEl('polygon', { points: top.concat(bot).join(' '), class: 'viz-band ' + s.cls }));
        } else if (opts.series.length === 1 && run.length > 1) {
          const pts = run.map(p => `${x(p.t)},${y(p.v)}`);
          pts.push(`${x(run[run.length - 1].t)},${y(lo)}`, `${x(run[0].t)},${y(lo)}`);
          svg.append(svgEl('polygon', { points: pts.join(' '), class: 'viz-area ' + s.cls }));
        }
        if (run.length === 1) {
          // A lone sample (e.g. right after a gap) gets a full-size marker.
          svg.append(svgEl('circle', { cx: x(run[0].t), cy: y(run[0].v), r: 4, class: 'viz-end ' + s.cls }));
        } else {
          svg.append(svgEl('polyline', { points: run.map(p => `${x(p.t)},${y(p.v)}`).join(' '), class: 'viz-line ' + s.cls }));
        }
      }
      // Direct end label (multi-series charts): name + latest value, in text ink.
      const last = [...s.points].reverse().find(p => p.v !== null && p.v !== undefined);
      if (last && opts.endLabels) {
        svg.append(svgEl('circle', { cx: x(last.t), cy: y(last.v), r: 4, class: 'viz-end ' + s.cls }));
        const tx = svgEl('text', { x: x(last.t) + 8, y: y(last.v) + 4, class: 'viz-end-label' });
        tx.textContent = `${s.label || s.name} ${fmtNum(last.v)}`;
        svg.append(tx);
      }
    }

    // Crosshair + tooltip, snapping to the nearest sample time.
    const times = [...new Set(opts.series.flatMap(s => s.points.map(p => p.t)))].sort((a, b) => a - b);
    const cross = svgEl('line', { y1: pad.t, y2: pad.t + plotH, class: 'viz-cross hidden' });
    const hit = svgEl('rect', { x: pad.l, y: pad.t, width: plotW, height: plotH, class: 'viz-hit' });
    svg.append(cross, hit);
    let idx = -1;
    const showAt = (i, cx, cy) => {
      idx = Math.max(0, Math.min(times.length - 1, i));
      const t = times[idx];
      cross.setAttribute('x1', x(t)); cross.setAttribute('x2', x(t));
      cross.classList.remove('hidden');
      const rows = [tipHead(timeFull(t) + (opts.resolution >= 3600 ? ' (hour)' : ''))];
      for (const s of opts.series) {
        const p = s.points.find(q => q.t === t);
        if (!p || p.v === null || p.v === undefined) { rows.push(tipRow(s.cls, 'no data', s.label || s.name)); continue; }
        let val = fmtNum(p.v) + (opts.unit ? ' ' + opts.unit : '');
        if (p.min !== undefined && p.min !== null && p.min !== p.max) val += `  (${fmtNum(p.min)}–${fmtNum(p.max)})`;
        rows.push(tipRow(s.cls, val, opts.series.length > 1 || opts.resolution >= 3600 ? (s.label || s.name) + (opts.resolution >= 3600 ? ' avg' : '') : ''));
      }
      const r = svg.getBoundingClientRect();
      tooltip.show(cx ?? r.left + x(t), cy ?? r.top + pad.t, rows);
    };
    const nearest = px => {
      const t = opts.from + (px - pad.l) / plotW * (opts.to - opts.from);
      let best = 0;
      for (let i = 1; i < times.length; i++) if (Math.abs(times[i] - t) < Math.abs(times[best] - t)) best = i;
      return best;
    };
    hit.addEventListener('pointermove', e => {
      const r = svg.getBoundingClientRect();
      showAt(nearest(e.clientX - r.left), e.clientX, e.clientY);
    });
    const hide = () => { cross.classList.add('hidden'); tooltip.hide(); };
    hit.addEventListener('pointerleave', hide);
    svg.setAttribute('tabindex', '0');
    svg.addEventListener('focus', () => showAt(idx < 0 ? times.length - 1 : idx));
    svg.addEventListener('blur', hide);
    svg.addEventListener('keydown', e => {
      if (e.key === 'ArrowLeft' || e.key === 'ArrowRight') {
        e.preventDefault();
        showAt(idx + (e.key === 'ArrowLeft' ? -1 : 1));
      }
    });
    container.append(svg);
    const samples = opts.series.reduce((n, s) => n + s.points.filter(p => p.v !== null && p.v !== undefined).length, 0);
    if (samples < 3) {
      const note = document.createElement('div');
      note.className = 'viz-note';
      note.textContent = 'Collecting data — the first samples arrived recently.';
      container.append(note);
    }
  }

  /**
   * Status timeline: one thin strip of ok/warning/critical/unknown segments.
   * Missing samples (device offline) leave the strip empty.
   */
  function strip(container, opts) {
    container.replaceChildren();
    const width = Math.max(200, container.clientWidth || 400);
    const height = 14;
    const svg = svgEl('svg', { width, height, viewBox: `0 0 ${width} ${height}`, role: 'img', class: 'viz-strip', tabindex: '0' });
    const title = svgEl('title');
    title.textContent = opts.title || 'status';
    svg.append(title);
    svg.append(svgEl('rect', { x: 0, y: 3, width, height: 8, rx: 4, class: 'viz-track' }));
    const x = t => (t - opts.from) / (opts.to - opts.from) * width;
    const res = opts.resolution || 300;
    const segs = [];
    for (const p of opts.points) {
      const last = segs[segs.length - 1];
      if (last && last.s === p.s && p.t - last.end <= res * 1.5) last.end = p.t + res;
      else segs.push({ s: p.s, start: p.t, end: p.t + res });
    }
    for (const sg of segs) {
      const x0 = Math.max(0, x(sg.start)), x1 = Math.min(width, x(sg.end));
      if (x1 - x0 <= 0) continue;
      const r = svgEl('rect', { x: x0, y: 3, width: Math.max(1.5, x1 - x0 - 1), height: 8, rx: 2, class: 'viz-status ' + sg.s });
      r.addEventListener('pointermove', e => tooltip.show(e.clientX, e.clientY,
        [tipHead(`${timeFull(sg.start)} – ${timeFull(sg.end)}`), tipRow('st-' + sg.s, statusLabel(sg.s), opts.title || '')]));
      r.addEventListener('pointerleave', () => tooltip.hide());
      svg.append(r);
    }
    container.append(svg);
  }

  const statusLabel = s => ({ ok: '✓ ok', warning: '▲ warning', critical: '✕ critical', unknown: '? unknown' }[s] || s);

  return { line, strip, fmtNum, timeFull, statusLabel, ticks, tickFormatter };
})();
