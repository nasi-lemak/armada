// Armada dashboard: Quick search (Ctrl+K or /).
'use strict';

// ---------- quick search (Ctrl+K or /) ----------

function openSearch() {
  if (!token || !me || document.querySelector('.palette-backdrop')) return;
  const input = h('input', { type: 'search', placeholder: 'Search devices by name, serial, asset tag, IP… or go to a page', 'aria-label': 'Search' });
  const list = h('ul', { class: 'palette-results', role: 'listbox' });
  const backdrop = h('div', { class: 'palette-backdrop', onclick: e => { if (e.target === backdrop) close(); } },
    h('div', { class: 'palette', role: 'dialog', 'aria-label': 'Search' }, input, list,
      h('div', { class: 'palette-hint muted' }, '↑↓ to choose · Enter to open · Esc to close')));
  let items = [], active = 0, seq = 0, timer;
  const close = () => { backdrop.remove(); document.removeEventListener('keydown', keys, true); };
  const go = it => { close(); if (it) location.hash = it.link; };
  const pages = () => [...document.querySelectorAll('#nav a, #nav-more a')].filter(a => getComputedStyle(a).display !== 'none')
    .map(a => ({ kind: 'page', title: a.textContent.trim(), link: a.getAttribute('href') }));
  const draw = () => {
    replace(list, items.length ? items.map((it, i) => h('li', { class: 'palette-item' + (i === active ? ' active' : ''), role: 'option',
      onmousemove: () => { if (active !== i) { active = i; draw(); } }, onclick: () => go(it) },
      h('span', { class: 'palette-kind' }, it.kind), h('span', { class: 'palette-title' }, it.title),
      it.subtitle ? h('span', { class: 'palette-sub muted' }, it.subtitle) : null))
      : h('li', { class: 'palette-empty muted' }, input.value.trim().length < 2 ? 'Type at least two letters' : 'Nothing found'));
    const el = list.children[active];
    if (el && el.scrollIntoView) el.scrollIntoView({ block: 'nearest' });
  };
  const search = async () => {
    const q = input.value.trim(), mine = ++seq;
    const pageHits = q ? pages().filter(p => p.title.toLowerCase().includes(q.toLowerCase())) : pages();
    let found = [];
    if (q.length >= 2) {
      try { found = await api('GET', '/api/v1/search?q=' + encodeURIComponent(q)); } catch { /* shown as nothing */ }
    }
    if (mine !== seq) return; // a newer query is on its way
    items = [...found, ...pageHits].slice(0, 30);
    active = 0;
    draw();
  };
  const keys = e => {
    if (e.key === 'Escape') { e.preventDefault(); close(); }
    else if (e.key === 'ArrowDown') { e.preventDefault(); active = Math.min(active + 1, items.length - 1); draw(); }
    else if (e.key === 'ArrowUp') { e.preventDefault(); active = Math.max(active - 1, 0); draw(); }
    else if (e.key === 'Enter') { e.preventDefault(); go(items[active]); }
  };
  input.addEventListener('input', () => { clearTimeout(timer); timer = setTimeout(search, 150); });
  document.addEventListener('keydown', keys, true);
  document.body.append(backdrop);
  input.focus();
  search();
}
document.addEventListener('keydown', e => {
  const typing = e.target.closest && e.target.closest('input, textarea, select, [contenteditable]');
  if (((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === 'k') || (e.key === '/' && !typing)) {
    e.preventDefault();
    openSearch();
  }
});
document.getElementById('search-open').addEventListener('click', openSearch);
