// Dashboard smoke test: signs in, opens every page as an admin and as a
// client account, and fails on any JavaScript error or a page that doesn't
// render. Usage: node smoke.cjs http://localhost:8080 <admin-token>
'use strict';
const { chromium } = require('playwright');

const base = (process.argv[2] || 'http://localhost:8080').replace(/\/$/, '');
const adminToken = process.argv[3] || process.env.ARMADA_ADMIN_TOKEN;
const failures = [];
const fail = msg => { failures.push(msg); console.error('FAIL', msg); };

async function api(method, path, body, token = adminToken) {
  const res = await fetch(base + path, {
    method, headers: { Authorization: 'Bearer ' + token, 'Content-Type': 'application/json' },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const text = await res.text();
  if (!res.ok) throw new Error(`${method} ${path}: ${res.status} ${text}`);
  return text ? JSON.parse(text) : null;
}

async function seed() {
  // Two customers' devices, enrolled and checked in through the agent API.
  const ids = {};
  for (const customer of ['acme', 'globex']) {
    const tok = await api('POST', '/api/v1/tokens', { labels: { customer } });
    const res = await fetch(base + '/api/v1/agent/enroll', { method: 'POST', body: JSON.stringify({
      token: tok.token, name: customer + '-kiosk', info: { os: 'linux', arch: 'arm64', hostname: customer } }) });
    const er = await res.json();
    await fetch(base + '/api/v1/agent/heartbeat', { method: 'POST', headers: { Authorization: 'Bearer ' + er.device_key },
      body: JSON.stringify({ info: { os: 'linux', arch: 'arm64' }, packages: { myapp: '1.0.0' }, health: [], config: [] }) });
    ids[customer] = er.device_id;
  }
  await api('POST', '/api/v1/users', { email: 'it@acme.example', role: 'operator', scope: { labels: { customer: 'acme' } }, password: 'smoke-test-password' });
  return ids;
}

async function visit(page, who, hash, expectHeading) {
  await page.goto(base + '/' + hash);
  await page.waitForTimeout(700);
  const h1 = await page.textContent('h1').catch(() => '');
  if (expectHeading && !h1.includes(expectHeading)) fail(`${who} ${hash}: heading "${h1}", expected "${expectHeading}"`);
  return h1;
}

(async () => {
  if (!adminToken) throw new Error('admin token required');
  const ids = await seed();
  const browser = await chromium.launch();
  const watch = (page, who) => {
    page.on('pageerror', e => fail(`${who}: page error: ${e.message}`));
    page.on('console', m => { if (m.type() === 'error' && !/status of 40[134]/.test(m.text())) fail(`${who}: console error: ${m.text()}`); });
  };

  // Admin, signed in with the admin token.
  const admin = await browser.newPage({ viewport: { width: 1280, height: 900 } });
  watch(admin, 'admin');
  await admin.goto(base + '/');
  await admin.click('#login-switch');
  await admin.fill('#login-token', adminToken);
  await admin.click('#login-form button[type=submit]');
  await admin.waitForSelector('#app:not(.hidden)', { timeout: 10000 });
  const pages = [['#/', 'Fleet overview'], ['#/devices', 'Devices'], ['#/devices/' + ids.acme, 'acme-kiosk'],
    ['#/deployments', 'Deployments'], ['#/releases', 'Releases'], ['#/monitoring', 'Monitoring'], ['#/config', 'Configuration'],
    ['#/apps', 'Apps'], ['#/enroll', 'Enrollment'], ['#/activity', 'Activity'], ['#/users', 'Users'], ['#/account', 'My account']];
  for (const [hash, heading] of pages) {
    await visit(admin, 'admin', hash, heading);
    if (hash === '#/users') {
      const users = await admin.$$eval('#view table tbody tr', r => r.map(x => x.textContent)).catch(() => []);
      if (!users.some(u => u.includes('it@acme.example'))) fail('admin: users page does not list the client account');
    }
    if (hash === '#/devices') {
      const n = await admin.$$eval('#view table tbody tr', r => r.length).catch(() => 0);
      if (n !== 2) fail(`admin sees ${n} devices, expected 2`);
    }
  }

  // Client account: only its customer's devices, no fleet pages.
  const client = await browser.newPage({ viewport: { width: 1280, height: 900 } });
  watch(client, 'client');
  await client.goto(base + '/');
  await client.fill('#login-email', 'it@acme.example');
  await client.fill('#login-password', 'smoke-test-password');
  await client.click('#login-form button[type=submit]');
  await client.waitForSelector('#app:not(.hidden)', { timeout: 10000 });
  const nav = await client.$$eval('#nav a', as => as.filter(a => getComputedStyle(a).display !== 'none').map(a => a.textContent.trim()));
  for (const hidden of ['Deployments', 'Releases', 'Config', 'Users']) if (nav.includes(hidden)) fail(`client sees ${hidden} in the navigation`);
  await visit(client, 'client', '#/devices', 'Devices');
  const devs = await client.$$eval('#view table tbody tr', r => r.map(x => x.textContent));
  if (devs.length !== 1 || !devs[0].includes('acme-kiosk')) fail(`client sees devices: ${JSON.stringify(devs)}`);
  await visit(client, 'client', '#/deployments');
  if (!(await client.evaluate(() => location.hash)).match(/^#\/?$/)) fail('client reached a fleet page');

  await browser.close();
  if (failures.length) {
    console.error(`${failures.length} failure(s)`);
    process.exit(1);
  }
  console.log(`dashboard smoke test passed (${pages.length} admin pages, client account)`);
})().catch(e => { console.error(e); process.exit(1); });
