// Real imagery browsing plus an intercepted detection request; no model job is submitted.
import assert from 'node:assert/strict';
import fs from 'node:fs/promises';
import path from 'node:path';
import { createRequire } from 'node:module';
const require = createRequire(import.meta.url);
const { chromium } = require(process.env.PLAYWRIGHT_MODULE || 'playwright');
const url = process.env.FRONTEND_URL || 'http://localhost:5173';
const output = path.resolve(process.env.BROWSER_OUTPUT_DIR || 'data/validation/imagery-workspace/browser');
await fs.mkdir(output, { recursive: true });
const browser = await chromium.launch({ channel: 'chrome', headless: true });
const page = await browser.newPage({ viewport: { width: 1440, height: 1000 } });
page.setDefaultTimeout(30000);
const errors = [], crops = [], tileRequests = [];
const tileFlights = new Set();
let lastTileActivity = 0;
page.on('pageerror', error => errors.push(error.message));
page.on('request', request => {
  if (request.url().includes('/mosaic/')) {
    tileRequests.push(request.url()); tileFlights.add(request); lastTileActivity = Date.now();
  }
});
for (const event of ['requestfinished', 'requestfailed']) page.on(event, request => {
  if (tileFlights.delete(request)) lastTileActivity = Date.now();
});
// A visible detail grid may finish before coarse/background preview tiles.
// Warm-cache assertions must wait for both, otherwise switches cancel cold prefetch.
async function warmTilesReady() {
  await visibleTilesReady();
  const deadline = Date.now() + 30000;
  while (tileFlights.size || Date.now() - lastTileActivity < 600) {
    assert.ok(Date.now() < deadline, 'tile prefetch did not settle');
    await page.waitForTimeout(100);
  }
}
const visibleTilesReady = () => page.waitForFunction(() => {
  const images = [...document.querySelectorAll('.leaflet-tile')].filter(image => getComputedStyle(image).visibility !== 'hidden');
  return images.length > 0 && images.every(image => image.complete && image.naturalWidth === 256);
});
const list = page.getByRole('region', { name: '影像来源列表' });
const view = () => page.locator('[data-map-workspace]').evaluate(node => {
  let component = node.__vueParentComponent;
  while (component && !component.exposed?.getView) component = component.parent;
  return component.exposed.getView();
});
const checkView = async expected => {
  const actual = await view();
  assert.equal(actual.zoom, expected.zoom);
  assert.ok(Math.abs(actual.center.lat - expected.center.lat) < 1e-9);
  assert.ok(Math.abs(actual.center.lng - expected.center.lng) < 1e-9);
};
try {
  // This script intercepts detection; model configuration is a fixture too.
  await page.route('**/api/settings', route => route.fulfill({ json: { revision: 1, effective: true, settings: { default_model: 'fixture', detection: { threshold: .5, resolution: .8, kernel_size: 3, min_area_pixels: 10 }, browser: { concurrency: 4, inactive_layers: 3, cache_mb: 128, idle_seconds: 300 } } } }));
  await page.route('**/api/models', route => route.fulfill({ json: [{ id: 'fixture', name: 'Fixture', status: 'ready', enabled: true, sha256: 'fixture', mapping: {} }] }));
  await page.goto(url);
  await page.getByRole('combobox', { name: '选择影像来源' }).selectOption('xiongan');
  await list.locator('[data-phase-id]').first().waitFor();
  await page.waitForTimeout(1000);
  const data = await (await page.request.get(`${url}/py/local/regions/xiongan/scenes`)).json();
  const january = data.phases.find(phase => phase.month === '2025-01');
  const march = data.phases.find(phase => phase.month === '2025-03');
  const may = data.phases.find(phase => phase.month === '2025-05');
  assert.ok(may.scenes.length >= 2);
  const row = id => list.locator(`[data-phase-id="${id}"]`);
  const firstLoadStart = Date.now();
  await row(january.id).dragTo(page.locator('.leaflet-container'));
  await page.getByRole('tab', { name: /2025-01/ }).waitFor();
  await visibleTilesReady();
  const firstPhaseLoadMs = Date.now() - firstLoadStart;
  await row(march.id).getByRole('button', { name: /^加入 .* 时相$/ }).click();
  await row(may.id).getByRole('button', { name: /^加入 .* 时相$/ }).click();
  assert.equal(await page.getByRole('tab').count(), 3);
  assert.ok((await page.getByRole('tab', { name: /2025-05/ }).innerText()).includes(`${may.scenes.length} 张`));
  await row(january.id).dragTo(page.locator('.leaflet-container'));
  assert.equal(await page.getByRole('tab').count(), 3, 'duplicate drag cannot create a duplicate phase');
  await page.getByRole('button', { name: 'Zoom in', exact: true }).click();
  await page.waitForTimeout(350);
  const map = await page.locator('.leaflet-container').boundingBox();
  await page.mouse.move(map.x + map.width * .45, map.y + map.height * .55);
  await page.mouse.down();
  await page.mouse.move(map.x + map.width * .45 + 50, map.y + map.height * .55 + 30, { steps: 8 });
  await page.mouse.up();
  await page.waitForTimeout(500);
  const sharedView = await view();
  for (const month of ['2025-03', '2025-05', '2025-01', '2025-05']) {
    await page.getByRole('tab', { name: new RegExp(month) }).click();
    await checkView(sharedView);
    await warmTilesReady();
  }
  const beforeWarm = tileRequests.length;
  const warmSwitches = await page.evaluate(async () => {
    const timings = [];
    for (let i = 0; i < 100; i++) {
      const month = i % 2 ? '2025-05' : '2025-01';
      const button = [...document.querySelectorAll('button[role=tab]')].find(node => node.textContent.includes(month));
      const start = performance.now(); button.click();
      await new Promise(resolve => requestAnimationFrame(resolve));
      timings.push(performance.now() - start);
    }
    return { maxMs: Math.max(...timings), p95Ms: [...timings].sort((a, b) => a - b)[94] };
  });
  assert.equal(tileRequests.length, beforeWarm, '100 real imagery switches issue no requests');
  assert.ok(warmSwitches.maxMs <= 100, `real imagery switch took ${warmSwitches.maxMs} ms`);
  await page.waitForFunction(() => {
    const tiles = [...document.querySelectorAll('.leaflet-tile')];
    return tiles.length > 0 && tiles.every(image => image.complete && image.naturalWidth === 256);
  });
  await page.waitForTimeout(350);
  await page.screenshot({ path: path.join(output, 'multi-phase.png'), fullPage: true });
  await page.getByRole('button', { name: '关闭时相 2025-05' }).click();
  await checkView(sharedView);
  assert.equal(await page.getByRole('tab').count(), 2);
  await row(march.id).getByRole('button', { name: /^移除 .* 时相$/ }).click();
  await checkView(sharedView);
  assert.equal(await page.getByRole('tab').count(), 1);
  await row(march.id).getByRole('button', { name: /^加入 .* 时相$/ }).click();
  await checkView(sharedView);
  const coordinates = [115.97205398, 38.95545219, 115.97255398, 38.95595219];
  const inputs = page.locator('.operations-panel input[type=number]');
  for (const [index, coordinate] of coordinates.entries()) await inputs.nth(index).fill(String(coordinate));
  await page.getByRole('button', { name: '应用该区域框' }).click();
  await page.waitForTimeout(350);
  const framedView = await view();
  await page.getByRole('tab', { name: /2025-01/ }).click();
  await checkView(framedView);
  assert.equal(await page.locator('.leaflet-cdp-area-pane path').count(), 1, 'phase switch preserves the detection frame');
  let release;
  const pending = new Promise(resolve => { release = resolve; });
  await page.route('**/api/images/local', async route => {
    crops.push(route.request().postDataJSON());
    await pending;
    await route.fulfill({ json: { items: [], count: 0 } });
  });
  await page.getByRole('button', { name: '提交检测任务', exact: true }).click();
  await page.waitForFunction(() => document.querySelector('select[aria-label="选择影像来源"]')?.disabled);
  assert.deepEqual([...crops[0].scene_ids].sort(), [...january.scenes, ...march.scenes].filter(scene => scene.available).map(scene => scene.id).sort());
  assert.equal(crops[0].region_id, 'xiongan');
  assert.equal(await page.getByRole('button', { name: '关闭时相 2025-01' }).isDisabled(), true);
  assert.equal(await row(march.id).getByRole('button', { name: /^移除 .* 时相$/ }).isDisabled(), true);
  await page.getByRole('tab', { name: /2025-03/ }).click();
  await checkView(framedView);
  await page.locator('[data-map-workspace]').evaluate(node => {
    const map = node.__vueParentComponent.setupState.map;
    map.setZoom(map.getMinZoom(), { animate: false });
  });
  assert.equal(await page.getByRole('combobox', { name: '选择影像来源' }).inputValue(), 'xiongan', 'zooming to the floor cannot change regions during detection');
  assert.equal(await page.getByRole('tab').count(), 2);
  const lockedView = await view();
  await page.getByRole('tab', { name: /2025-01/ }).click();
  await checkView(lockedView);
  release();
  await page.getByText('该区域没有本地影像覆盖，请重新选择区域', { exact: true }).waitFor();
  await page.getByRole('combobox', { name: '选择影像来源' }).selectOption('130600');
  await list.getByText('保定市影像', { exact: true }).waitFor();
  assert.equal(await page.getByRole('tab').count(), 0);
  assert.equal(await page.locator('.leaflet-cdp-area-pane path').count(), 0);
  await page.route('**/py/local/regions/130800/scenes*', route => route.fulfill({ status: 503, json: { detail: '影像目录不可访问: Y:测试断开' } }));
  await page.getByRole('combobox', { name: '选择影像来源' }).selectOption('130800');
  await list.getByRole('alert').waitFor();
  assert.match(await list.getByRole('alert').innerText(), /目录不可访问/);
  assert.deepEqual(errors, []);
  const report = { passed: true, realImagery: true, modelInference: false, sharedView,
    firstPhaseLoadMs, firstLoadCacheState: 'new browser context; backend and OS caches not flushed', warmSwitches, warmExtraRequests: 0,
    checks: ['native drag', 'monthly grouping', 'duplicate drag', 'pan and zoom preserved across phases', 'close and remove without moving', 'detection frame preserved', 'selected scene request', 'input locking while browsing', 'region reset', 'unavailable directory'], errors };
  await fs.writeFile(path.join(output, 'report.json'), JSON.stringify(report, null, 2));
  console.log(JSON.stringify(report, null, 2));
} catch (error) {
  await page.screenshot({ path: path.join(output, 'failure.png'), fullPage: true }).catch(() => {});
  throw error;
} finally { await browser.close(); }
