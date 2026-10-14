// Browser acceptance with deterministic imagery/results; no detection jobs are submitted.
// Set PLAYWRIGHT_MODULE to an installed Playwright module when it is not on Node's module path.
import assert from 'node:assert/strict';
import fs from 'node:fs/promises';
import path from 'node:path';
import { createRequire } from 'node:module';

const require = createRequire(import.meta.url);
const { chromium } = require(process.env.PLAYWRIGHT_MODULE || 'playwright');
const url = process.env.FRONTEND_URL || 'http://localhost:5173';
const output = path.resolve(process.env.BROWSER_OUTPUT_DIR || 'data/validation/layer-controls');
await fs.mkdir(output, { recursive: true });
const browser = await chromium.launch({ channel: 'chrome', headless: true });
const bounds = [114.3208, 38.1130, 114.3228, 38.1150];
const dates = ['2023-11-01', '2024-09-01', '2025-11-01', '2026-05-01'];
const images = dates.map(date => ({ id: date.slice(0, 4), source: 'local', phase: '',
  capture_date: date, file_path: date.slice(0, 4) + '.tif', created_at: date,
  bounds_west: bounds[0], bounds_south: bounds[1], bounds_east: bounds[2], bounds_north: bounds[3],
  width: 512, height: 512 }));
const report = { fixture: 'deterministic browser fixtures; no GPU inference', layouts: [] };

function patchCollection(index) {
  const x = bounds[0] + 0.0003 + index * 0.00035, y = bounds[1] + 0.0004;
  return { type: 'FeatureCollection', schema_version: 2, features: [0, 1].map(offset => {
    const west = x + offset * 0.0004, east = west + 0.00025, north = y + 0.0007;
    return { type: 'Feature', id: `patch-${offset + 1}`, bbox: [west, y, east, north],
      properties: { patch_id: `patch-${offset + 1}`, area_pixels: [792, 600][offset],
        area_m2: [455.21, 800][offset], area_ha: [0.045521, 0.08][offset],
        perimeter_m: [100, 120][offset], centroid: [(west + east) / 2, (y + north) / 2],
        confidence_mean: offset === 0 ? 0.914764 : null },
      geometry: { type: 'Polygon', coordinates: [[[west, y], [east, y], [east, north], [west, north], [west, y]]] } };
  }) };
}

try {
  for (const [name, width, height] of [['desktop', 1440, 900], ['bigscreen', 1920, 1080], ['mobile', 390, 844]]) {
    if (process.env.BROWSER_LAYOUT && process.env.BROWSER_LAYOUT !== name) continue;
    const page = await browser.newPage({ viewport: { width, height } });
    const errors = [];
    page.on('pageerror', error => errors.push(error.message));
    page.on('console', message => { if (message.type() === 'error') errors.push(message.text()); });
    const png = await page.evaluate(() => {
      const canvas = document.createElement('canvas');
      canvas.width = canvas.height = 512;
      const ctx = canvas.getContext('2d');
      const frames = {};
      for (const [index, year] of ['2023', '2024', '2025', '2026'].entries()) {
        ctx.fillStyle = ['#667c54', '#6b815a', '#69775f', '#758064'][index];
        ctx.fillRect(0, 0, 512, 512);
        ctx.fillStyle = '#536d43';
        for (let x = 0; x < 512; x += 64) ctx.fillRect(x, 0, 32, 512);
        ctx.fillStyle = '#98958c';
        ctx.fillRect(20, 220, 472, 28);
        ctx.fillStyle = '#b9b5ab';
        for (let y = 40; y < 512; y += 95) for (let x = 40; x < 512; x += 100) ctx.fillRect(x, y, 45, 35 + index * 4);
        frames[year] = canvas.toDataURL('image/png').split(',')[1];
      }
      ctx.clearRect(0, 0, 512, 512);
      const blank = canvas.toDataURL('image/png').split(',')[1];
      return { frames, blank };
    });
    const tasks = [], geojsonDownloads = [];
    const json = (route, data) => route.fulfill({ json: data });
    await page.route('**/api/**', route => {
      const request = route.request(), pathname = new URL(request.url()).pathname;
      if (pathname === '/api/algorithms') return json(route, { algorithms: [{ name: 'hebei_onnx', params_schema: { properties: {} } }] });
      if (pathname === '/api/images/local') return json(route, { items: images, count: images.length });
      if (/\/preview$/.test(pathname)) return route.fulfill({ contentType: 'image/png', body: Buffer.from(png.frames['2023'], 'base64') });
      if (pathname === '/api/tasks' && request.method() === 'POST') {
        const data = request.postDataJSON();
        tasks.push(data);
        return json(route, { id: 'task-' + tasks.length, status: 'pending' });
      }
      const task = pathname.match(/^\/api\/tasks\/task-(\d+)(.*)$/);
      if (task) {
        const index = Number(task[1]) - 1;
        if (!task[2]) return json(route, { id: 'task-' + task[1], status: 'succeeded', progress: 100 });
        if (task[2] === '/result') return json(route, { mask_path: 'mask-' + task[1],
          ...(index > 0 ? { geojson: patchCollection(index) } : {}),
          stats_json: JSON.stringify({ feature_count: [6, 3, 8][index], change_ratio: [0.0017, 0.005, 0.01][index],
            mean_confidence: [0.9321, 0.8754, 0.9543][index] }) });
        if (task[2] === '/result/change.geojson') {
          geojsonDownloads.push(task[1]);
          const x = bounds[0] + 0.0003 + index * 0.00035, y = bounds[1] + 0.0004;
          return json(route, { type: 'FeatureCollection', features: [{ type: 'Feature', properties: {},
            geometry: { type: 'Polygon', coordinates: [[[x, y], [x + 0.0005, y], [x + 0.0005, y + 0.0007],
              [x, y + 0.0007], [x, y]]] } }] });
        }
      }
      return route.fulfill({ status: 404, json: { error: 'Unexpected acceptance-test endpoint ' + pathname } });
    });
    await page.route('**/py/local/catalog', route => json(route, {
      images: [], warning: null, bounds: [113.45, 36, 119.9, 42.65],
      boundary: { type: 'Polygon', coordinates: [[[113.45, 36], [119.9, 36], [119.9, 42.65], [113.45, 42.65], [113.45, 36]]] },
    }));
    await page.route(/\/py\/local\/(?:regions\/[^/]+\/)?tiles\//, route => route.fulfill({ contentType: 'image/png', body: Buffer.from(png.blank, 'base64') }));
    await page.route('**/py/overlay/image', route => json(route, {
      image_b64: png.frames[route.request().postDataJSON().image_path.slice(0, 4)], bounds, width: 512, height: 512,
    }));
    await page.route('**/py/overlay/mask', async route => {
      const color = route.request().postDataJSON().color;
      const mask = await page.evaluate(color => {
        const canvas = document.createElement('canvas');
        canvas.width = canvas.height = 512;
        const ctx = canvas.getContext('2d');
        ctx.fillStyle = color + '73';
        ctx.fillRect(200, 200, 112, 112);
        return canvas.toDataURL('image/png').split(',')[1];
      }, color);
      return json(route, { png_b64: mask, bounds, width: 512, height: 512 });
    });

    const scenes = images.map(image => ({ id: image.id, date: image.capture_date, file: image.file_path, filename: image.file_path, available: true, region_id: '130100', resolution_m: .8, date_precision: 'month' }));
    await page.route('**/py/local/regions/130100/scenes*', route => json(route, { region_id: '130100', name: '石家庄市', scenes,
      phases: scenes.map(scene => ({ id: scene.id, region_id: '130100', month: scene.date.slice(0, 7), scenes: [scene], available: true, bounds: null, version: 'fixture' })) }));
    await page.route('**/py/local/regions/*/mosaic/**', route => route.fulfill({ contentType: 'image/png', body: Buffer.from(png.frames['2023'], 'base64') }));
    await page.goto(url);
    await page.getByRole('combobox', { name: '选择行政区' }).selectOption('130100');
    const list = page.getByRole('region', { name: '行政区影像列表' });
    await list.locator('[data-phase-id]').nth(3).waitFor();
    for (const image of images) await list.locator(`[data-phase-id="${image.id}"]`).getByRole('button', { name: /^加入 .* 时相$/ }).click();
    assert.equal(await page.locator('input[type="range"]').count(), 0);
    const inputs = page.locator('input[type="number"]');
    for (const [index, coordinate] of bounds.entries()) await inputs.nth(index).fill(String(coordinate));
    await page.getByRole('button', { name: '应用该区域框' }).click();
    await page.getByRole('button', { name: '提交检测任务', exact: true }).click();
    const summary = page.getByRole('status', { name: '变化检测统计' });
    await page.waitForFunction(() => document.querySelector('[aria-label="变化检测统计"]')?.textContent.includes('8 变化图斑'));
    assert.equal(tasks.length, 3);
    assert.deepEqual(tasks.map(task => [task.before_id, task.after_id]), [['2023', '2024'], ['2024', '2025'], ['2025', '2026']]);
    for (const text of ['6 变化图斑', '3 变化图斑', '8 变化图斑', '占比 0.17%', '置信度 0.9321'])
      assert.ok((await summary.innerText()).includes(text), text);
    assert.deepEqual(geojsonDownloads, ['1'], 'inline patch packages must avoid separate GeoJSON downloads');
    const latestPatches = page.locator('.leaflet-cdp-result-2-pane path.leaflet-interactive');
    // ResizeObserver and Leaflet SVG rendering settle after the summary is populated.
    await page.waitForFunction(() => {
      const patch = document.querySelector('.leaflet-cdp-result-2-pane path.leaflet-interactive');
      const rect = patch?.getBoundingClientRect();
      const map = document.querySelector('.leaflet-container').getBoundingClientRect();
      return rect && rect.top >= map.top && rect.bottom <= map.bottom;
    });
    await latestPatches.nth(0).click({ force: true });
    const detail = page.locator('[aria-label="变化图斑详情"]');
    await detail.waitFor();
    for (const text of ['455.21 m²', '792', '0.9148'])
      assert.ok((await detail.innerText()).includes(text), text);
    assert.deepEqual(await detail.locator('dt').allTextContents(), ['时相', '面积', '像元数', '平均变化置信度', '几何']);
    const patchGeometry = await detail.evaluate(node => {
      const popup = node.closest('[data-patch-details]'), rect = popup.getBoundingClientRect();
      const map = document.querySelector('.leaflet-container').getBoundingClientRect();
      const centerElement = document.elementFromPoint(rect.left + rect.width / 2, rect.top + rect.height / 2);
      return { inside: rect.left >= map.left && rect.right <= map.right && rect.top >= map.top && rect.bottom <= map.bottom,
        unobscured: popup.contains(centerElement) };
    });
    assert.equal(patchGeometry.inside, true, 'patch popup must fit within the map');
    assert.equal(patchGeometry.unobscured, true, 'statistics must not cover patch details');
    await page.screenshot({ path: path.join(output, name + '-patch-details.png'), fullPage: true });
    await page.getByRole('button', { name: '关闭图斑详情' }).click();
    await detail.waitFor({ state: 'detached' });
    await latestPatches.nth(1).click({ force: true });
    await detail.waitFor();
    assert.ok((await detail.innerText()).includes('800 m²'));
    assert.match(await detail.innerText(), /平均变化置信度\s+—/);
    await page.getByRole('button', { name: '关闭图斑详情' }).click();
    await detail.waitFor({ state: 'detached' });
    const layers = page.getByRole('region', { name: '图层控制' });
    await layers.getByRole('button', { name: /^图层/ }).click();
    const keys = () => layers.locator('[data-layer-key]').evaluateAll(nodes => nodes.map(node => node.dataset.layerKey));
    assert.deepEqual(await keys(), ['2025:2026', '2024:2025', '2023:2024']);
    assert.equal(await page.locator('.leaflet-cdp-images-pane img').count(), 0, 'detection crops must not cover the selected phase');
    assert.equal(await page.locator('.leaflet-cdp-results-pane img').count(), 3);
    await page.getByRole('tab', { name: /2023-11/ }).click();
    assert.equal(await page.getByRole('tab', { name: /2023-11/ }).getAttribute('aria-selected'), 'true');
    assert.equal(await page.locator('.leaflet-cdp-results-pane img').count(), 3);
    const earliestResult = layers.locator('[data-layer-key="2023:2024"]');
    await earliestResult.getByRole('checkbox').uncheck();
    assert.equal(await page.locator('.leaflet-cdp-results-pane img').count(), 2);
    assert.ok((await summary.innerText()).includes('已隐藏'));
    assert.ok((await summary.innerText()).includes('6 变化图斑'));
    await earliestResult.getByRole('checkbox').check();
    await layers.locator('[data-layer-key="2025:2026"]').getByRole('button', { name: '下移检测图层' }).click();
    assert.deepEqual((await keys()).slice(0, 2), ['2024:2025', '2025:2026']);
    assert.ok(await page.evaluate(() =>
      Number(getComputedStyle(document.querySelector('.leaflet-cdp-result-1-pane')).zIndex) >
      Number(getComputedStyle(document.querySelector('.leaflet-cdp-result-2-pane')).zIndex)));
    await layers.getByRole('button', { name: '恢复时间顺序' }).click();
    assert.deepEqual(await keys(), ['2025:2026', '2024:2025', '2023:2024']);
    await summary.scrollIntoViewIfNeeded();
    await page.screenshot({ path: path.join(output, name + '.png'), fullPage: true });
    await layers.getByRole('button', { name: '隐藏底图' }).click();
    assert.equal(await page.locator('.leaflet-cdp-images-pane img').count(), 0);
    assert.equal(await page.locator('.leaflet-tile-pane img:visible').count(), 0, 'hidden phase tiles may remain cached');
    assert.equal(await page.locator('.leaflet-cdp-results-pane img').count(), 3);
    assert.equal(await layers.getByRole('checkbox').count(), 3, 'only detection results have visibility checkboxes');
    const showBaseMaps = layers.getByRole('button', { name: '显示底图', exact: true });
    await showBaseMaps.waitFor();
    await page.getByRole('tab', { name: /2024-09/ }).click();
    assert.equal(await page.locator('.leaflet-tile-pane img:visible').count(), 0, 'switching phases must preserve the hidden state');
    await showBaseMaps.click();
    await layers.getByRole('button', { name: '隐藏底图', exact: true }).waitFor();
    await page.waitForFunction(() => Array.from(document.querySelectorAll('.leaflet-tile-pane img')).some(image => {
      const style = getComputedStyle(image);
      return image.src && style.visibility !== 'hidden' && style.display !== 'none';
    }));
    assert.equal(await page.locator('.leaflet-cdp-results-pane img').count(), 3);
    await layers.getByRole('button', { name: '隐藏底图', exact: true }).click();
    await layers.getByRole('button', { name: /^图层/ }).click();
    await summary.scrollIntoViewIfNeeded();
    await page.screenshot({ path: path.join(output, name + '-mask-only.png'), fullPage: true });
    const geometry = await summary.evaluate(node => {
      const stats = node.getBoundingClientRect(), map = document.querySelector('.leaflet-container').getBoundingClientRect();
      return { inside: stats.left >= map.left && stats.right <= map.right && stats.top >= map.top && stats.bottom <= map.bottom,
        stats: { width: stats.width, height: stats.height } };
    });
    assert.equal(geometry.inside, true, 'summary must stay inside the map');
    await page.getByRole('button', { name: '清除选区', exact: true }).click();
    await page.waitForFunction(() => !document.querySelector('[aria-label="变化检测统计"]'));
    assert.equal(await page.locator('.leaflet-cdp-results-pane img').count(), 0);
    assert.equal(await page.locator('.leaflet-cdp-images-pane img').count(), 0);
    assert.deepEqual(errors, []);
    report.layouts.push({ name, width, height, status: 'passed', geometry, patchGeometry });
    console.log(name + ': all layer interactions and summaries passed');
    await page.close();
  }
  await fs.writeFile(path.join(output, 'report.json'), JSON.stringify(report, null, 2));
} catch (error) {
  for (const context of browser.contexts()) for (const page of context.pages()) {
    await page.screenshot({ path: path.join(output, 'failure.png'), fullPage: true }).catch(() => {});
  }
  throw error;
} finally {
  await browser.close();
}
