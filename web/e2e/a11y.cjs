// Accessibility checks the smoke test runs on every page (no third-party
// scanner: these are the WCAG checks that matter most for this dashboard).
//   - names: form fields, buttons and links say what they are to screen readers
//   - headings: one h1, no skipped levels
//   - contrast: text against its background, 4.5:1 (3:1 for large text)
//   - focus: Tab moves through the page and the focused element is visible
//   - the page's language is declared
'use strict';

// Runs in the page; returns a list of problems.
function pageChecks() {
  const problems = [];
  const visible = el => {
    const r = el.getBoundingClientRect();
    const cs = getComputedStyle(el);
    return r.width > 0 && r.height > 0 && cs.visibility !== 'hidden' && cs.display !== 'none' && !el.closest('[hidden], .hidden');
  };
  const describe = el => {
    const id = el.id ? '#' + el.id : '';
    const cls = el.className && typeof el.className === 'string' ? '.' + el.className.trim().split(/\s+/).slice(0, 2).join('.') : '';
    const text = (el.textContent || el.getAttribute('placeholder') || '').trim().replace(/\s+/g, ' ').slice(0, 40);
    return `<${el.tagName.toLowerCase()}${id}${cls}>${text ? ' "' + text + '"' : ''}`;
  };
  const name = el => {
    const label = el.getAttribute('aria-label') || '';
    if (label.trim()) return label;
    const by = el.getAttribute('aria-labelledby');
    if (by) return by.split(/\s+/).map(i => (document.getElementById(i) || {}).textContent || '').join(' ').trim();
    if (el.labels && [...el.labels].some(l => l.textContent.trim())) return [...el.labels].map(l => l.textContent).join(' ').trim();
    if (el.getAttribute('title')) return el.getAttribute('title');
    if (/^(BUTTON|A|SUMMARY|OPTION)$/.test(el.tagName)) {
      const t = (el.textContent || '').trim();
      if (t) return t;
      const img = el.querySelector('img[alt], svg[aria-label]');
      if (img) return img.getAttribute('alt') || img.getAttribute('aria-label');
    }
    if (el.tagName === 'INPUT' && /^(submit|button|reset)$/.test(el.type) && el.value) return el.value;
    return '';
  };

  if (!document.documentElement.lang) problems.push('the page has no lang attribute');

  // Names.
  for (const el of document.querySelectorAll('input, select, textarea, button, a[href], [role=button], summary')) {
    if (!visible(el) || el.type === 'hidden') continue;
    if (!name(el)) problems.push(`no accessible name: ${describe(el)}`);
  }
  for (const img of document.querySelectorAll('img')) {
    if (visible(img) && !img.hasAttribute('alt')) problems.push(`image without alt: ${describe(img)}`);
  }

  // Headings.
  const hs = [...document.querySelectorAll('h1, h2, h3, h4, h5, h6')].filter(visible);
  const h1s = hs.filter(h => h.tagName === 'H1');
  if (h1s.length !== 1) problems.push(`${h1s.length} h1 headings (want 1)`);
  let prev = 0;
  for (const h of hs) {
    const level = +h.tagName[1];
    if (prev && level > prev + 1) problems.push(`heading level skipped: h${prev} then ${describe(h).replace('<', '<')}`);
    prev = level;
  }

  // Contrast.
  const rgba = s => {
    const m = s.match(/rgba?\(([\d.]+),\s*([\d.]+),\s*([\d.]+)(?:,\s*([\d.]+))?\)/);
    if (m) return [+m[1], +m[2], +m[3], m[4] === undefined ? 1 : +m[4]];
    const c = s.match(/color\(srgb ([\d.]+) ([\d.]+) ([\d.]+)(?: \/ ([\d.]+))?\)/);
    if (c) return [c[1] * 255, c[2] * 255, c[3] * 255, c[4] === undefined ? 1 : +c[4]];
    return null;
  };
  const blend = (top, bottom) => {
    const a = top[3];
    return [top[0] * a + bottom[0] * (1 - a), top[1] * a + bottom[1] * (1 - a), top[2] * a + bottom[2] * (1 - a), 1];
  };
  const background = el => {
    const layers = [];
    for (let e = el; e; e = e.parentElement) {
      const bg = rgba(getComputedStyle(e).backgroundColor);
      if (bg && bg[3] > 0) {
        layers.push(bg);
        if (bg[3] >= 1) break;
      }
      if (getComputedStyle(e).backgroundImage !== 'none') return null; // gradients and images: can't tell
    }
    let out = [255, 255, 255, 1];
    for (let i = layers.length - 1; i >= 0; i--) out = blend(layers[i], out);
    return out;
  };
  const lum = c => {
    const ch = c.slice(0, 3).map(v => {
      v /= 255;
      return v <= 0.03928 ? v / 12.92 : Math.pow((v + 0.055) / 1.055, 2.4);
    });
    return 0.2126 * ch[0] + 0.7152 * ch[1] + 0.0722 * ch[2];
  };
  const seen = new Set();
  const walker = document.createTreeWalker(document.body, NodeFilter.SHOW_TEXT);
  for (let n = walker.nextNode(); n; n = walker.nextNode()) {
    const el = n.parentElement;
    if (!n.textContent.trim() || !el || seen.has(el) || !visible(el)) continue;
    seen.add(el);
    if (el.closest('svg, input[disabled], button[disabled], select[disabled], [aria-disabled=true], option')) continue; // charts are checked by their own palette; disabled controls are exempt
    const cs = getComputedStyle(el);
    if (+cs.opacity === 0) continue;
    const bg = background(el);
    let fg = rgba(cs.color);
    if (!bg || !fg) continue;
    fg = blend(fg, bg);
    const [l1, l2] = [lum(fg), lum(bg)].sort((a, b) => b - a);
    const ratio = (l1 + 0.05) / (l2 + 0.05);
    const size = parseFloat(cs.fontSize);
    const large = size >= 24 || (size >= 18.66 && +cs.fontWeight >= 700);
    const need = large ? 3 : 4.5;
    if (ratio < need - 0.05) problems.push(`contrast ${ratio.toFixed(2)}:1 (need ${need}): ${describe(el)} color ${cs.color} on ${bg.slice(0, 3).map(Math.round).join(',')}`);
  }
  return problems;
}

// Tab through the page: focus must move and be visible.
async function focusChecks(page, stops = 25) {
  const problems = [];
  await page.evaluate(() => { document.activeElement && document.activeElement.blur(); window.scrollTo(0, 0); });
  let last = null, moved = 0;
  for (let i = 0; i < stops; i++) {
    await page.keyboard.press('Tab');
    const info = await page.evaluate(() => {
      const el = document.activeElement;
      if (!el || el === document.body) return null;
      const cs = getComputedStyle(el);
      const ring = (cs.outlineStyle !== 'none' && parseFloat(cs.outlineWidth) > 0) || (cs.boxShadow && cs.boxShadow !== 'none');
      const d = `<${el.tagName.toLowerCase()}${el.id ? '#' + el.id : ''}> "${(el.textContent || el.getAttribute('aria-label') || '').trim().slice(0, 30)}"`;
      return { d, ring, key: d + el.getBoundingClientRect().top };
    });
    if (!info) continue;
    if (info.key !== last) moved++;
    last = info.key;
    if (!info.ring) problems.push(`no visible focus: ${info.d}`);
  }
  if (moved < 3) problems.push(`Tab moved focus only ${moved} times`);
  return [...new Set(problems)];
}

// check runs every check on the current page in light and dark mode and
// returns the problems found, each prefixed with where.
async function check(page, where) {
  const out = [];
  for (const scheme of ['light', 'dark']) {
    await page.emulateMedia({ colorScheme: scheme });
    await page.waitForTimeout(100);
    for (const p of await page.evaluate(pageChecks)) out.push(`${where} (${scheme}): ${p}`);
  }
  await page.emulateMedia({ colorScheme: 'light' });
  for (const p of await focusChecks(page)) out.push(`${where}: ${p}`);
  return [...new Set(out)];
}

module.exports = { check };
