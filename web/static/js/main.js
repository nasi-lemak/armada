// Armada dashboard: Routing and startup. Loaded last: the route table refers to every page, and startup runs as soon as this file loads.
'use strict';

// ---------- routing ----------

let refreshTimer = null;
let currentRefresh = null;

const routes = [
  [/^#?\/?$/, 'overview', viewOverview],
  [/^#\/devices(?:\?(.*))?$/, 'devices', viewDevices],
  [/^#\/devices\/([\w-]+)$/, 'devices', viewDevice],
  [/^#\/bulk\/([\w-]+)$/, 'devices', viewBulk, 'operator'],
  [/^#\/schedules$/, 'devices', viewSchedules, 'operator'],
  [/^#\/labels\?(.*)$/, 'devices', viewLabels],
  [/^#\/webhooks$/, 'activity', viewWebhooks, 'operator'],
  [/^#\/api$/, 'account', viewAPIDocs],
  [/^#\/deployments$/, 'deployments', viewDeployments, 'fleet'],
  [/^#\/deployments\/([\w-]+)$/, 'deployments', viewDeployment, 'fleet'],
  [/^#\/releases$/, 'releases', viewReleases, 'fleet'],
  [/^#\/enroll$/, 'enroll', viewEnroll, 'operator'],
  [/^#\/apps$/, 'apps', viewApps, 'fleet'],
  [/^#\/monitoring$/, 'monitoring', viewMonitoring],
  [/^#\/config$/, 'config', viewConfig, 'fleet'],
  [/^#\/activity(?:\?(.*))?$/, 'activity', viewActivity],
  [/^#\/reports$/, 'reports', viewReports],
  [/^#\/certificates$/, 'certificates', viewCertificates],
  [/^#\/users$/, 'users', viewUsers, 'admin'],
  [/^#\/sites$/, 'sites', viewSites, 'admin'],
  [/^#\/account$/, 'account', viewAccount],
];
const allowed = need => !need || (need === 'fleet' ? fleetWide() : atLeast(need));

async function route() {
  if (!token) return showLogin();
  if (!me && await guard(loadMe) === undefined) return;
  if (!groupings) await loadGroupings();
  clearInterval(refreshTimer);
  currentRefresh = null;
  const hash = location.hash || '#/';
  for (const [re, nav, view, need] of routes) {
    const m = hash.match(re);
    if (!m) continue;
    if (!allowed(need)) break;
    if (me.two_factor_setup_required && nav !== 'account') { location.hash = '#/account'; return; }
    document.querySelectorAll('#nav a, #nav-more a').forEach(a => a.classList.toggle('active', a.dataset.route === nav));
    layoutNav();
    const root = $('#view');
    root.replaceChildren();
    root.classList.toggle('fleet-page', need === 'fleet');
    root.classList.remove('device-readonly');
    const refresh = await view(root, ...m.slice(1));
    if (typeof refresh === 'function') {
      currentRefresh = refresh;
      refreshTimer = setInterval(() => { if (!document.hidden) guard(refresh); }, 10000);
    }
    return;
  }
  location.hash = '#/';
}

window.addEventListener('hashchange', route);
document.addEventListener('visibilitychange', () => { if (!document.hidden && currentRefresh) guard(currentRefresh); });

if (location.hash.startsWith('#/sso')) {
  finishSSO();
} else if (token) {
  $('#app').classList.remove('hidden');
  route();
} else {
  showLogin();
}
