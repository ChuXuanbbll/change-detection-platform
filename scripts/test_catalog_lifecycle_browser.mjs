// Verify one refresh per document, shared initialization, navigation and error recovery.
import assert from 'node:assert/strict';
import { createRequire } from 'node:module';
const require = createRequire(import.meta.url);
const { chromium } = require(process.env.PLAYWRIGHT_MODULE || 'playwright');
const browser = await chromium.launch({ channel: 'chrome', headless: true });
const page = await browser.newPage({ viewport: { width: 1440, height: 900 } });
const errors = [], scans = [], scenes = [];
let release;
const initialGate = new Promise(resolve => { release = resolve; });
let fail = false;
page.on('pageerror', error => errors.push(error.message));
await page.route('**/py/local/catalog*', async route => {
  const url = new URL(route.request().url());
  assert.equal(url.searchParams.get('refresh'), 'true');
  scans.push(route.request().url());
  if (scans.length === 1) await initialGate;
  await route.fulfill({ status: fail ? 503 : 200, json: fail ? { detail: '测试：网络影像目录暂不可访问' } : { images: [] } });
});
await page.route('**/py/local/regions/*/scenes*', route => {
  const url = new URL(route.request().url());
  scenes.push(route.request().url());
  return route.fulfill({ json: { region_id: url.pathname.split('/')[4], scenes: [], phases: [] } });
});
const url = process.env.FRONTEND_URL || 'http://localhost:5173';
try {
  await page.goto(url, { waitUntil: 'domcontentloaded' });
  await page.getByText('正在扫描影像目录，完成后即可加载时相…').waitFor();
  const select = page.getByRole('combobox', { name: '选择影像来源' });
  await select.selectOption('130100');
  await select.selectOption('131000');
  assert.equal(scans.length, 1);
  assert.equal(scenes.length, 0, 'regional reads must wait for the page scan');
  release();
  await page.getByText('当前影像来源在所选范围没有影像时相。', { exact: true }).waitFor();
  assert.equal(scenes.length, 1, 'aborted region selection cannot issue a late request');
  assert.ok(scenes[0].includes('/131000/scenes'));
  for (const target of ['comparison', 'reports', 'overview']) {
    await page.locator(`.app-nav .nav-item[href="/${target}"]`).click();
    await page.waitForURL(`**/${target}`);
  }
  await select.selectOption('130100');
  await page.getByText('当前影像来源在所选范围没有影像时相。', { exact: true }).waitFor();
  assert.equal(scans.length, 1, 'SPA and region changes do not refresh the index');
  assert.ok(scenes.every(value => !value.includes('refresh=true')));
  await page.getByRole('button', { name: '刷新目录', exact: true }).click();
  await page.getByText('当前影像来源在所选范围没有影像时相。', { exact: true }).waitFor();
  assert.equal(scenes.filter(value => value.includes('refresh=true')).length, 1);
  await page.reload({ waitUntil: 'networkidle' });
  assert.equal(scans.length, 2, 'one additional scan on browser reload');
  fail = true;
  await page.reload({ waitUntil: 'networkidle' });
  await page.getByRole('alert').filter({ hasText: '测试：网络影像目录暂不可访问' }).waitFor();
  assert.equal(scans.length, 3);
  fail = false;
  await page.getByRole('button', { name: '重试读取目录' }).click();
  await page.getByRole('button', { name: '重试读取目录' }).waitFor({ state: 'hidden' });
  assert.equal(scans.length, 4, 'an explicit retry refreshes once');
  assert.deepEqual(errors, []);
  console.log(JSON.stringify({ passed: true, pageScans: scans.length, manualRefreshes: 1,
    checks: ['initialization barrier', 'rapid region change', 'SPA navigation', 'manual refresh', 'browser reload', 'failed scan retry'] }));
} finally {
  release();
  await browser.close();
}
