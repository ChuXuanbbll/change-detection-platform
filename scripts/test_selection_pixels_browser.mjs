// Deterministic UI checks; no imagery crops or detection jobs are submitted.
import assert from 'node:assert/strict';
import fs from 'node:fs/promises';
import { createRequire } from 'node:module';
const require = createRequire(import.meta.url);
const { chromium } = require(process.env.PLAYWRIGHT_MODULE || 'playwright');
const browser = await chromium.launch({ channel: 'chrome', headless: true });
try {
  const page = await browser.newPage({ viewport: { width: 1280, height: 900 } });
  const errors = [];
  page.on('pageerror', error => errors.push(error.message));
  await page.route('**/py/local/regions', route => route.fulfill({ json: { features: [], boundary: { type: 'Polygon', coordinates: [] }, bounds: [113, 36, 120, 43] } }));
  await page.route('**/py/local/catalog*', route => route.fulfill({ json: { images: [] } }));
  await page.route('**/py/local/selection-pixels', async route => {
    const body = route.request().postDataJSON();
    const width = body.scene_ids[0] === 'fine' ? 10000 : 5000;
    await route.fulfill({ json: { width, height: width, pixels: width * width } });
  });
  await page.goto(process.env.FRONTEND_URL || 'http://localhost:5173');
  await page.evaluate(async () => {
    const { createApp, h, ref } = await import('/node_modules/.vite/deps/vue.js');
    const { default: Map } = await import('/src/components/MapBasemap.vue');
    const bounds = ref({ west: 114.3, south: 38.1, east: 114.4, north: 38.2 });
    const phase = ref({ id: 'fine', month: '2025-01', region_id: '130100', version: '1', available: true,
      bounds: [114.3, 38.1, 114.4, 38.2], scenes: [{ id: 'fine', available: true }] });
    document.querySelector('#app').style.display = 'none';
    const host = document.createElement('div'); host.style.cssText = 'height:850px;width:1200px'; document.body.append(host);
    const app = createApp({ render: () => h(Map, { selectionBounds: bounds.value, activePhase: phase.value }) });
    app.mount(host);
    window.pixelTest = { bounds, phase, host };
  });
  const root = page.locator('[data-map-workspace]').last();
  await root.evaluate(node => {
    const s = node.__vueParentComponent.setupState;
    s.map.fitBounds([[38.1, 114.3], [38.2, 114.4]], { animate: false, padding: [80, 80] });
  });
  const label = page.getByRole('status', { name: '框选区域像元尺寸' });
  await label.filter({ hasText: '100,000,000' }).waitFor();
  const position = await root.evaluate(node => {
    const s = node.__vueParentComponent.setupState;
    const rect = s.map.getContainer().getBoundingClientRect();
    const corner = s.map.latLngToContainerPoint([38.2, 114.4]);
    const box = node.querySelector('.selection-pixel-content').getBoundingClientRect();
    return { rightInset: rect.left + corner.x - box.right, topInset: box.top - rect.top - corner.y,
      background: getComputedStyle(node.querySelector('.selection-pixel-content')).backgroundColor };
  });
  assert.ok(Math.abs(position.rightInset - 8) < 2, JSON.stringify(position));
  assert.ok(Math.abs(position.topInset - 8) < 2, JSON.stringify(position));
  assert.equal(position.background, 'rgb(21, 35, 48)');
  await fs.mkdir('data/validation/selection-pixels', { recursive: true });
  await page.screenshot({ path: 'data/validation/selection-pixels/label.png' });
  await page.evaluate(() => { window.pixelTest.phase.value = { ...window.pixelTest.phase.value, month: '2025-03', scenes: [{ id: 'coarse', available: true }] }; });
  await label.filter({ hasText: '25,000,000' }).waitFor();
  await page.evaluate(() => { window.pixelTest.bounds.value = null; });
  await label.waitFor({ state: 'detached' });
  assert.deepEqual(errors, []);
  console.log('PASS: dimensions, total, solid background, inside top-right anchor, phase switch, clear selection');
} finally { await browser.close(); }
