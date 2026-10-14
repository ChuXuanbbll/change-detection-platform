// Administrative navigation acceptance; uses real boundaries and local raster tiles.
import assert from 'node:assert/strict';
import fs from 'node:fs/promises';
import path from 'node:path';
import { createRequire } from 'node:module';

const require = createRequire(import.meta.url);
const { chromium } = require(process.env.PLAYWRIGHT_MODULE || 'playwright');
const url = process.env.FRONTEND_URL || 'http://localhost:5173';
const output = path.resolve(process.env.BROWSER_OUTPUT_DIR || 'data/validation/regions');
await fs.mkdir(output, { recursive: true });
const browser = await chromium.launch({ channel: 'chrome', headless: true });
const report = { fixture: 'real bundled boundaries and local raster tiles; no inference submitted', layouts: [] };

async function regionPoints(page) {
  return page.locator('[data-region-id]').evaluateAll(paths => paths.map(node => {
    const rect = node.getBoundingClientRect();
    for (let row = 0; row < 20; row++) for (let col = 0; col < 20; col++) {
      const x = rect.left + rect.width * (col + 0.5) / 20;
      const y = rect.top + rect.height * (row + 0.5) / 20;
      if (document.elementFromPoint(x, y) === node) return { id: node.dataset.regionId, x, y };
    }
    return null;
  }).filter(Boolean));
}

async function assertRegionHover(page, expected) {
  const highlighted = await page.locator('[data-region-id]').evaluateAll(paths => paths
    .filter(node => getComputedStyle(node).stroke === 'rgb(103, 232, 249)')
    .map(node => node.dataset.regionId));
  assert.deepEqual(highlighted, expected ? [expected] : [], 'only the region under the pointer may be highlighted');
}

async function assertOverviewMinimum(page) {
  const zoomOut = page.getByRole('button', { name: 'Zoom out' });
  assert.equal(await zoomOut.getAttribute('aria-disabled'), 'true', 'overview must start at the minimum zoom');
  const boundaryPaths = () => page.locator('[data-region-id]').evaluateAll(paths => paths.map(node => node.getAttribute('d')));
  const initialPaths = await boundaryPaths();
  await zoomOut.click({ force: true });
  const container = await page.locator('.leaflet-container').boundingBox();
  await page.mouse.move(container.x + container.width / 2, container.y + container.height / 2);
  await page.mouse.wheel(0, 3000);
  await page.waitForTimeout(350);
  assert.deepEqual(await boundaryPaths(), initialPaths, 'button and wheel must not shrink the province below the initial view');
  await page.mouse.move(0, 0);
}

try {
  for (const [name, width, height] of [['desktop', 1440, 900], ['mobile', 390, 844]]) {
    const page = await browser.newPage({ viewport: { width, height } });
    const errors = [], tileRequests = [], imageryRequests = [];
    page.on('pageerror', error => errors.push(error.message));
    page.on('request', request => {
      if (/\/(?:tiles|mosaic)\//.test(request.url())) tileRequests.push(request.url());
      if (/\/(?:imagery|scenes)/.test(request.url())) imageryRequests.push(request.url());
    });
    await page.goto(url);
    const nav = page.getByRole('region', { name: '行政区导航' });
    const paths = page.locator('[data-region-id]');
    await paths.nth(13).waitFor();
    assert.equal(await paths.count(), 14);
    assert.equal(await page.locator('.region-label').count(), 14);
    const labelsClear = await page.locator('.region-label span').evaluateAll(labels => labels.every(label => {
      const rect = label.getBoundingClientRect();
      const top = document.elementFromPoint(rect.left + rect.width / 2, rect.top + rect.height / 2);
      return top?.closest('.leaflet-container') != null;
    }));
    assert.equal(labelsClear, true, 'all labels must remain clear of navigation and map controls');
    assert.equal(tileRequests.length, 0, 'initial overview must not request raster tiles');
    assert.equal(imageryRequests.length, 0, 'initial overview must not request region imagery');
    const fit = await page.evaluate(() => {
      const container = document.querySelector('.leaflet-container').getBoundingClientRect();
      return [...document.querySelectorAll('[data-region-id]')].every(node => {
        const rect = node.getBoundingClientRect();
        return rect.left >= container.left && rect.right <= container.right && rect.top >= container.top && rect.bottom <= container.bottom;
      });
    });
    assert.equal(fit, true, 'province boundaries must fit inside the default viewport');
    await assertOverviewMinimum(page);
    const points = await regionPoints(page);
    assert.equal(points.length, 14, 'every region must have a visible hover target');
    const region = page.locator('[data-region-id="130100"]');
    const point = points.find(point => point.id === '130100');
    await page.mouse.move(point.x, point.y);
    await assertRegionHover(page, point.id);
    // Single-step mouse moves exercise rapid crossings without waiting for mouseout cleanup.
    for (const point of [...points, ...points.toReversed()]) {
      await page.mouse.move(point.x, point.y);
      await assertRegionHover(page, point.id);
    }
    const container = await page.locator('.leaflet-container').boundingBox();
    await page.mouse.move(container.x + 4, container.y + 4);
    await assertRegionHover(page, null);
    await page.mouse.move(point.x, point.y);
    await page.getByRole('button', { name: 'Zoom in' }).hover();
    await assertRegionHover(page, null);
    await page.mouse.move(0, 0);
    await assertRegionHover(page, null);
    await page.screenshot({ path: path.join(output, `${name}-overview.png`), fullPage: true });
    const overviewBox = await region.boundingBox();
    await region.dispatchEvent('click');
    const list = page.getByRole('region', { name: '行政区影像列表' });
    await list.locator('[data-phase-id]').first().waitFor();
    await page.waitForTimeout(1000);
    assert.equal(await page.locator('.leaflet-tile').count(), 0, 'region selection waits for an explicit drag or add');
    const detailBox = await region.boundingBox();
    assert.ok(detailBox.width > overviewBox.width, 'click must zoom into city');
    await list.getByRole('button', { name: /^加入 .* 时相$/ }).first().click();
    await page.waitForFunction(() => [...document.querySelectorAll('.leaflet-tile')].some(image => image.complete && image.naturalWidth === 256));
    assert.ok(tileRequests.every(request => request.includes('/regions/130100/mosaic/')), 'city must request only selected imagery');
    await page.screenshot({ path: path.join(output, `${name}-city.png`), fullPage: true });
    await nav.getByRole('button', { name: '河北全省' }).click();
    assert.equal(await page.locator('.leaflet-tile').count(), 0, 'return must remove imagery');
    await page.route('**/py/local/regions/130600/scenes*', route => route.fulfill({ json: { region_id: '130600', name: '保定市', scenes: [], phases: [] } }));
    await page.getByRole('combobox', { name: '选择行政区' }).selectOption('130600');
    await list.getByText('该行政区没有影像时相。', { exact: true }).waitFor();
    assert.equal(await page.locator('.leaflet-tile').count(), 0);
    await page.getByRole('combobox', { name: '选择行政区' }).selectOption('xiongan');
    await list.getByText('雄安新区影像', { exact: true }).waitFor();
    await list.locator('[data-phase-id]').first().waitFor();
    const xiongan = await (await page.request.get(`${url}/py/local/regions/xiongan/scenes`)).json();
    const scene = xiongan.phases.find(item => item.month === '2025-11');
    await list.locator(`[data-phase-id="${scene.id}"]`).getByRole('button', { name: /^加入 .* 时相$/ }).click();
    await page.waitForFunction(() => [...document.querySelectorAll('.leaflet-tile')].some(image => image.complete && image.naturalWidth === 256));
    await page.screenshot({ path: path.join(output, `${name}-xiongan.png`), fullPage: true });
    await nav.getByRole('button', { name: '河北全省' }).click();
    await page.route('**/py/local/regions/130100/scenes*', async route => {
      try {
        const response = await route.fetch();
        await new Promise(resolve => setTimeout(resolve, 250));
        await route.fulfill({ response });
      } catch { /* Aborted catalog requests are expected during rapid navigation. */ }
    });
    await page.getByRole('combobox', { name: '选择行政区' }).selectOption('130100');
    await page.getByRole('combobox', { name: '选择行政区' }).selectOption('131000');
    await list.getByText('廊坊市影像', { exact: true }).waitFor();
    await list.locator('[data-phase-id]').first().waitFor();
    assert.equal(await page.getByRole('combobox', { name: '选择行政区' }).inputValue(), '131000');
    const langfang = await (await page.request.get(`${url}/py/local/regions/131000/scenes`)).json();
    assert.deepEqual(await list.locator('[data-phase-id]').evaluateAll(nodes => nodes.map(node => node.dataset.phaseId)), langfang.phases.map(phase => phase.id));
    await nav.getByRole('button', { name: '河北全省' }).click();
    const tileCount = tileRequests.length;
    await page.getByRole('combobox', { name: '选择行政区' }).selectOption('130100');
    await page.waitForTimeout(1000);
    // Manual zooming back to the overview clears the region and all raster layers.
    for (let i = 0; i < 12 && await page.getByRole('combobox', { name: '选择行政区' }).inputValue(); i++) {
      const zoomOut = page.getByRole('button', { name: 'Zoom out' });
      if (await zoomOut.getAttribute('aria-disabled') === 'true') break;
      // The preceding zoom can reach the floor between reading aria-disabled and clicking.
      await zoomOut.click({ force: true });
      await page.waitForTimeout(400);
    }
    await page.waitForFunction(() => document.querySelector('select[aria-label="选择行政区"]')?.value === '');
    assert.equal(await page.getByRole('combobox', { name: '选择行政区' }).inputValue(), '');
    assert.equal(await page.locator('.leaflet-tile').count(), 0);
    await assertOverviewMinimum(page);
    // A smaller viewport must lower the limit enough to retain the complete province.
    const resized = name === 'desktop' ? { width: 390, height: 844 } : { width: 1440, height: 900 };
    await page.setViewportSize(resized);
    await page.waitForTimeout(350);
    await assertOverviewMinimum(page);
    assert.equal(await page.locator('[data-region-id]').evaluateAll(paths => {
      const map = document.querySelector('.leaflet-container').getBoundingClientRect();
      return paths.every(node => {
        const rect = node.getBoundingClientRect();
        return rect.left >= map.left && rect.right <= map.right && rect.top >= map.top && rect.bottom <= map.bottom;
      });
    }), true, 'resizing must preserve full-province fit at the new minimum');
    assert.deepEqual(errors, []);
    report.layouts.push({ name, width, height, regions: 14, initialTiles: 0, checks: ['labels', 'full province fit', 'initial minimum zoom', 'button and wheel zoom floor', 'exclusive rapid hover', 'background, controls and map exit clear hover', 'click zoom', 'real region tiles', 'explicit scene loading', 'empty region fixture', 'Xiongan 2025 real tiles', 'stale response cancellation', 'return button', 'zoom out', 'responsive minimum zoom'], totalTileRequests: tileCount });
    await page.close();
  }
  // Exercise native format adapters with deterministic tile services, separate from real rasters above.
  const page = await browser.newPage({ viewport: { width: 1440, height: 900 } });
  const requests = [];
  const png = Buffer.from('iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAusB9Wl6ZJcAAAAASUVORK5CYII=', 'base64');
  await page.route('**/format-fixture/**', route => {
    requests.push(new URL(route.request().url()));
    return route.fulfill({ contentType: 'image/png', body: png });
  });
  const bounds = [116.15, 39.35, 116.3, 39.5];
  await page.route('**/py/local/regions/131000/imagery', route => route.fulfill({ json: {
    region_id: '131000', name: '廊坊市', years: [2027], selected_year: 2027,
    sources: ['xyz', 'tms', 'wms'].map(type => ({ id: type, region_id: '131000', year: 2027, type, bounds,
      url: type === 'wms' ? '/format-fixture/wms' : `/format-fixture/${type}/{z}/{x}/{y}.png`,
      ...(type === 'wms' ? { layers: 'annual_2027', crs: 'EPSG:4326', version: '1.3.0' } : {}),
    })),
  } }));
  await page.goto(url);
  await page.evaluate(async () => {
    const [{ createApp }, { default: MapBasemap }] = await Promise.all([
      import('/node_modules/.vite/deps/vue.js'), import('/src/components/MapBasemap.vue')]);
    const host = document.createElement('div'); host.style.cssText = 'position:fixed;inset:0;z-index:9999;background:#152330';
    document.body.append(host); createApp(MapBasemap).mount(host);
  });
  await page.locator('[data-region-id="131000"]').last().waitFor();
  await page.getByRole('combobox', { name: '选择行政区' }).last().selectOption('131000');
  await page.waitForFunction(() => document.querySelectorAll('.leaflet-tile').length >= 3 &&
    [...document.querySelectorAll('.leaflet-tile')].every(image => image.complete && image.naturalWidth > 0));
  const xyz = requests.filter(request => request.pathname.includes('/xyz/'));
  const tms = requests.filter(request => request.pathname.includes('/tms/'));
  assert.ok(xyz.length && tms.length);
  for (const request of xyz) {
    const [z, x, y] = request.pathname.match(/(\d+)\/(\d+)\/(\d+)\.png$/).slice(1).map(Number);
    assert.ok(tms.some(tile => tile.pathname.endsWith(`/${z}/${x}/${2 ** z - 1 - y}.png`)), 'TMS must invert the XYZ Y coordinate');
  }
  const wms = requests.find(request => request.pathname.endsWith('/wms'));
  assert.ok(wms);
  assert.equal(wms.searchParams.get('layers'), 'annual_2027');
  assert.equal(wms.searchParams.get('crs'), 'EPSG:4326');
  assert.equal(wms.searchParams.get('version'), '1.3.0');
  const [south, west, north, east] = wms.searchParams.get('bbox').split(',').map(Number);
  assert.ok(south < north && west < east && west > 100 && south < 90, 'WMS 1.3 EPSG:4326 must use latitude/longitude axis order');
  report.formatAdapters = { fixture: 'deterministic display services', verified: ['XYZ', 'TMS Y inversion', 'WMS 1.3 EPSG:4326 axis order'] };
  await page.close();
  await fs.writeFile(path.join(output, 'report.json'), JSON.stringify(report, null, 2));
  console.log(JSON.stringify(report, null, 2));
} finally {
  await browser.close();
}
