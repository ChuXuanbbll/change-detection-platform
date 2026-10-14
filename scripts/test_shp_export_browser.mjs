// Focused report-page acceptance. Seed completed results, without running detection.
// Python service at SHP_PY_URL must be running the current code.
import assert from 'node:assert/strict';
import fs from 'node:fs/promises';
import path from 'node:path';
import { createRequire } from 'node:module';
const require = createRequire(import.meta.url);
const { chromium } = require(process.env.PLAYWRIGHT_MODULE || 'playwright');
const pyUrl = process.env.SHP_PY_URL || 'http://127.0.0.1:8090';
const output = path.resolve(process.env.BROWSER_OUTPUT_DIR || 'data/validation/shp-export/browser');
await fs.mkdir(output, { recursive: true });
const collection = { type: 'FeatureCollection', features: [{ type: 'Feature', id: 'patch-1',
  geometry: { type: 'Polygon', coordinates: [[[114, 38], [115, 38], [115, 39], [114, 39], [114, 38]]] },
  properties: { patch_id: '图斑一', area_m2: 100, area_pixels: 12, confidence_mean: .9, centroid: [114.5, 38.5] } }] };
const archives = {};
for (const [task, data] of [['task-1', collection], ['task-2', { type: 'FeatureCollection', features: [] }]]) {
  const response = await fetch(`${pyUrl}/py/export/shp`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(data) });
  assert.equal(response.status, 200, await response.clone().text());
  archives[task] = Buffer.from(await response.arrayBuffer());
  assert.equal(archives[task].subarray(0, 4).toString('hex'), '504b0304');
}
const browser = await chromium.launch({ channel: 'chrome', headless: true });
const page = await browser.newPage({ viewport: { width: 1440, height: 900 } });
page.setDefaultTimeout(15000);
const errors = [], downloads = [];
page.on('pageerror', error => errors.push(error.message));
let failShp = false;
await page.route('**/py/local/catalog*', route => route.fulfill({ json: { images: [] } }));
await page.route('**/api/**', async route => {
  const pathname = new URL(route.request().url()).pathname;
  const match = pathname.match(/^\/api\/tasks\/(task-[12])\/result\/(.*)$/);
  if (!match) return route.fulfill({ json: { algorithms: [], items: [] } });
  const [, task, file] = match;
  downloads.push(pathname);
  if (file === 'change.zip') {
    if (failShp) return route.fulfill({ status: 400, json: { error: '图斑几何为空或无效', code: 'GEOJSON_INVALID' } });
    return route.fulfill({ contentType: 'application/zip', body: archives[task] });
  }
  if (file === 'change.geojson') return route.fulfill({ json: collection });
  if (file === 'mask.tif') return route.fulfill({ contentType: 'image/tiff', body: Buffer.from([73, 73, 42, 0]) });
  return route.fulfill({ contentType: 'text/csv', body: 'metric,value\nfeature_count,1\n' });
});
try {
  await page.goto(`${process.env.FRONTEND_URL || 'http://localhost:5173'}/reports`);
  await page.getByText('暂无检测报告', { exact: true }).waitFor();
  await page.evaluate(collection => {
    const app = document.querySelector('.workspace-shell').__vueParentComponent;
    const workspace = app.provides[Object.getOwnPropertySymbols(app.provides).find(key => key.description === 'detection-workspace')];
    const image = (year) => ({ id: String(year), capture_date: `${year}-01-01`, file_path: `${year}.tif`, source: 'local', created_at: `${year}-01-01` });
    workspace.pairs.value = [
      { key: '2023:2024', taskId: 'task-1', status: 'succeeded', before: image(2023), after: image(2024), color: '#f43f5e', geojson: collection },
      { key: '2024:2025', taskId: 'task-2', status: 'succeeded', before: image(2024), after: image(2025), color: '#06b6d4', geojson: { type: 'FeatureCollection', features: [] } },
      { key: '2025:2026', taskId: 'task-3', status: 'running', before: image(2025), after: image(2026), color: '#f59e0b' }
    ];
    workspace.selectedPairKey.value = '2023:2024';
  }, collection);
  for (const [pair, task] of [['2023:2024', 'task-1'], ['2024:2025', 'task-2']]) {
    await page.getByLabel('当前时相组').selectOption(pair);
    const downloaded = page.waitForEvent('download');
    await page.getByRole('button', { name: 'SHP（ZIP）', exact: true }).click();
    const file = await downloaded;
    assert.equal(file.suggestedFilename(), `result_${task}_shp.zip`);
    assert.deepEqual(await fs.readFile(await file.path()), archives[task]);
    assert.equal(downloads.at(-1), `/api/tasks/${task}/result/change.zip`);
    await file.saveAs(path.join(output, file.suggestedFilename()));
  }
  failShp = true;
  await page.getByRole('button', { name: 'SHP（ZIP）', exact: true }).click();
  await page.getByText('图斑几何为空或无效', { exact: true }).waitFor();
  assert.ok(await page.getByRole('button', { name: 'SHP（ZIP）', exact: true }).isEnabled());
  failShp = false;
  await page.getByLabel('当前时相组').selectOption('2023:2024');
  for (const [label, extension] of [['GeoJSON', 'geojson'], ['GeoTIFF', 'tif'], ['CSV', 'csv']]) {
    const downloaded = page.waitForEvent('download');
    await page.getByRole('button', { name: label, exact: true }).click();
    assert.equal((await downloaded).suggestedFilename(), `result_task-1.${extension}`);
  }
  await page.screenshot({ path: path.join(output, 'reports-shp.png') });
  await page.getByLabel('当前时相组').selectOption('2025:2026');
  assert.ok(await page.getByRole('button', { name: 'SHP（ZIP）', exact: true }).isDisabled());
  assert.deepEqual(errors, []);
  await fs.writeFile(path.join(output, 'report.json'), JSON.stringify({ passed: true, downloads, errors, source: 'seeded report state with real Python ZIP conversion; no inference' }, null, 2));
  console.log('SHP report browser acceptance passed:', output);
} catch (error) {
  await page.screenshot({ path: path.join(output, 'failure.png') }).catch(() => {});
  throw error;
} finally { await browser.close(); }
