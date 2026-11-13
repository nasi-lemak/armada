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
  const ids = {}, keys = {};
  for (const customer of ['acme', 'globex', 'stock']) {
    // Stock devices enroll through a spare token.
    const tok = await api('POST', '/api/v1/tokens', customer === 'stock' ? { lifecycle: 'spare', description: 'warehouse' } : { labels: { customer } });
    const res = await fetch(base + '/api/v1/agent/enroll', { method: 'POST', body: JSON.stringify({
      token: tok.token, name: customer + '-kiosk', info: { os: 'linux', arch: 'arm64', hostname: customer } }) });
    const er = await res.json();
    await fetch(base + '/api/v1/agent/heartbeat', { method: 'POST', headers: { Authorization: 'Bearer ' + er.device_key },
      body: JSON.stringify({ info: { os: 'linux', arch: 'arm64' }, packages: { myapp: '1.0.0' }, health: [], config: [] }) });
    ids[customer] = er.device_id;
    keys[customer] = er.device_key;
  }
  await api('POST', '/api/v1/users', { email: 'it@acme.example', role: 'operator', scope: { labels: { customer: 'acme' } }, password: 'smoke-test-password' });

  // acme's broker reports a Mosquitto certificate expiring in 20 days.
  const day = 24 * 3600e3;
  await fetch(base + '/api/v1/agent/heartbeat', { method: 'POST', headers: { Authorization: 'Bearer ' + keys.acme },
    body: JSON.stringify({ info: { os: 'linux', arch: 'arm64' }, packages: { myapp: '1.0.0' }, certs: [{ target: 'mosquitto', info: {
      subject: 'CN=broker.acme.example,O=Acme', common_name: 'broker.acme.example', subject_fields: { common_name: 'broker.acme.example', organization: 'Acme' },
      dns_names: ['broker.acme.example'], issuer: 'CN=Acme Issuing CA', serial: '1F', key_type: 'ecdsa-p256', fingerprint: 'ab12', key_matches: true,
      not_before: new Date(Date.now() - 345 * day).toISOString(), not_after: new Date(Date.now() + 20 * day).toISOString() } }] }) });

  // globex is in maintenance; acme has a firing alert to acknowledge.
  await api('POST', '/api/v1/maintenance', { name: 'Smoke maintenance', selector: { device_ids: [ids.globex] },
    ends_at: new Date(Date.now() + 3600e3).toISOString(), note: 'replacing the screen' });
  const ch = await api('POST', '/api/v1/channels', { name: 'smoke', type: 'webhook', webhook: { url: 'https://hooks.example.invalid/x' } });
  await api('POST', '/api/v1/alert-rules', { name: 'Disk', enabled: true, condition: 'health', min_severity: 'warning', channels: [ch.id] });
  await fetch(base + '/api/v1/agent/heartbeat', { method: 'POST', headers: { Authorization: 'Bearer ' + keys.acme },
    body: JSON.stringify({ info: { os: 'linux', arch: 'arm64' }, packages: { myapp: '1.0.0' },
      health: [{ name: 'disk /', kind: 'storage', status: 'critical', message: '99% full' }] }) });
  for (let i = 0; i < 40; i++) { // the engine evaluates alerts every 10 seconds
    if ((await api('GET', '/api/v1/alerts?status=firing')).length) return ids;
    await new Promise(r => setTimeout(r, 500));
  }
  throw new Error('no alert fired');
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
    ['#/apps', 'Apps'], ['#/enroll', 'Enrollment'], ['#/activity', 'Activity'], ['#/certificates', 'Certificates'], ['#/reports', 'Reports'], ['#/users', 'Users'], ['#/account', 'My account']];
  for (const [hash, heading] of pages) {
    await visit(admin, 'admin', hash, heading);
    if (hash === '#/users') {
      const users = await admin.$$eval('#view table tbody tr', r => r.map(x => x.textContent)).catch(() => []);
      if (!users.some(u => u.includes('it@acme.example'))) fail('admin: users page does not list the client account');
    }
    if (hash === '#/devices') {
      const rows = await admin.$$eval('#view table tbody tr', r => r.map(x => x.textContent)).catch(() => []);
      if (rows.length !== 3) fail(`admin sees ${rows.length} devices, expected 3`);
      if (!rows.some(r => r.includes('stock-kiosk') && r.includes('spare'))) fail('device list does not mark the spare');
      if (!rows.some(r => r.includes('globex') && r.includes('maintenance'))) fail('device list does not mark the device in maintenance');
    }
    if (hash === '#/certificates') {
      const rows = await admin.$$eval('#view table tbody tr', r => r.map(x => x.textContent)).catch(() => []);
      if (rows.length !== 1 || !rows[0].includes('mosquitto') || !rows[0].includes('broker.acme.example') || !/1[89] days/.test(rows[0])) fail(`certificates page: ${JSON.stringify(rows)}`);
      admin.once('dialog', d => d.accept());
      await admin.click('#view tbody input[type=checkbox]');
      await admin.click('.bulk-bar button:has-text("Request CSRs")');
      await admin.waitForSelector('#view :text("making CSR")', { timeout: 5000 }).catch(() => fail('requesting a CSR did not start a renewal'));
      // Automatic renewal: set one up from the form and run it.
      await admin.click('#view button:has-text("Set up automatic renewal")');
      await admin.waitForSelector('#view td:has-text("Renew certificates")', { timeout: 5000 }).catch(() => fail('renewal policy not listed'));
      await admin.click('#view button:has-text("Check now")');
      await admin.waitForSelector('#toast:has-text("Checked:")', { timeout: 5000 }).catch(() => fail('running the renewal policy failed'));
    }
    if (hash === '#/enroll') {
      const toks = await admin.$$eval('#view table tbody tr', r => r.map(x => x.textContent)).catch(() => []);
      if (!toks.some(t => t.includes('warehouse') && t.includes('spare'))) fail('enrollment page does not show the spare token');
    }
    if (hash === '#/reports') {
      await admin.waitForTimeout(500);
      const text = await admin.textContent('#view');
      if (!text.includes('Whole fleet') || !text.includes('Availability') || !text.includes('globex-kiosk')) fail('admin report does not cover the fleet');
    }
    if (hash === '#/monitoring') {
      const text = await admin.textContent('#view');
      if (!text.includes('Smoke maintenance')) fail('monitoring page does not list the maintenance');
      admin.once('dialog', d => d.accept('looking into it'));
      await admin.click('#view button:has-text("Acknowledge")').catch(e => fail('no Acknowledge button: ' + e.message));
      await admin.waitForSelector('#view :text("acked by")', { timeout: 5000 }).catch(() => fail('acknowledgement not shown'));
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
  // Client puts its own device into maintenance from the device page.
  await visit(client, 'client', '#/devices/' + ids.acme, 'acme-kiosk');
  await client.click('#view button:has-text("Start maintenance")');
  await client.waitForSelector('#view button:has-text("End maintenance")', { timeout: 5000 }).catch(() => fail('client could not start maintenance'));
  await visit(client, 'client', '#/certificates', 'Certificates');
  const clientCerts = await client.$$eval('#view table tbody tr', r => r.map(x => x.textContent)).catch(() => []);
  if (clientCerts.length !== 1 || !clientCerts[0].includes('acme-kiosk')) fail(`client certificates: ${JSON.stringify(clientCerts)}`);
  await visit(client, 'client', '#/reports', 'Reports');
  await client.waitForTimeout(500);
  const clientReport = await client.textContent('#view');
  if (!clientReport.includes('customer=acme') || clientReport.includes('globex')) fail('client report is not limited to the client');
  await visit(client, 'client', '#/monitoring', 'Monitoring');
  if ((await client.textContent('#view')).includes('Smoke maintenance')) fail('client sees vendor maintenance');
  await visit(client, 'client', '#/deployments');
  if (!(await client.evaluate(() => location.hash)).match(/^#\/?$/)) fail('client reached a fleet page');

  // RMA: the admin swaps acme's kiosk for the spare from the device page.
  await visit(admin, 'admin', '#/devices/' + ids.acme, 'acme-kiosk');
  await admin.click('#view button:has-text("Replace…")');
  const replSel = '#view select[aria-label="Replacement device"]';
  await admin.waitForSelector(`${replSel} option[value="${ids.stock}"]`, { state: 'attached', timeout: 5000 });
  await admin.selectOption(replSel, ids.stock);
  await admin.click('#view form button.primary:has-text("Replace")');
  await admin.waitForFunction(id => location.hash === '#/devices/' + id, ids.stock, { timeout: 5000 }).catch(() => fail('replace did not open the replacement'));
  await admin.waitForTimeout(700);
  if (!(await admin.textContent('h1')).includes('acme-kiosk')) fail('replacement did not take over the name');
  await visit(client, 'client', '#/devices', 'Devices');
  const after = await client.$$eval('#view table tbody tr', r => r.map(x => x.textContent));
  if (after.length !== 1 || !after[0].includes(ids.stock)) fail(`client devices after the swap: ${JSON.stringify(after)}`);

  // Bulk: the admin selects every device, adds a label, then restarts the agents.
  await visit(admin, 'admin', '#/devices', 'Devices');
  admin.on('dialog', d => d.accept());
  await admin.click('input[aria-label="Select all shown"]');
  await admin.selectOption('select[aria-label="Bulk operation"]', 'set_labels');
  await admin.fill('input[aria-label="Bulk operation value"]', 'floor=2');
  await admin.click('.bulk-bar button:has-text("Apply")');
  await admin.waitForTimeout(1500);
  const fleet = await api('GET', '/api/v1/devices');
  const labelled = fleet.filter(d => d.labels.floor === '2').length;
  if (labelled !== fleet.length) fail(`bulk label reached ${labelled} of ${fleet.length} devices`);
  await admin.click('input[aria-label="Select all shown"]');
  await admin.selectOption('select[aria-label="Bulk operation"]', 'restart-agent');
  await admin.click('.bulk-bar button:has-text("Apply")');
  await admin.waitForFunction(() => location.hash.startsWith('#/bulk/'), null, { timeout: 5000 }).catch(() => fail('bulk run did not open its progress page'));
  await admin.waitForTimeout(800);
  if (!(await admin.textContent('h1')).includes('restart-agent')) fail('bulk progress page heading');
  const rows = (await admin.$$('#view table tbody tr')).length;
  if (rows !== fleet.length) fail(`bulk progress page lists ${rows} devices, expected ${fleet.length}`);

  await browser.close();
  if (failures.length) {
    console.error(`${failures.length} failure(s)`);
    process.exit(1);
  }
  console.log(`dashboard smoke test passed (${pages.length} admin pages, client account)`);
})().catch(e => { console.error(e); process.exit(1); });
