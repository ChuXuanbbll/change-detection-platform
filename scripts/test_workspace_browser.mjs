// Deterministic UI acceptance: no real detection jobs are created.
import assert from 'node:assert/strict';
import fs from 'node:fs/promises';
import path from 'node:path';
import { createRequire } from 'node:module';
const require = createRequire(import.meta.url);
const { chromium } = require(process.env.PLAYWRIGHT_MODULE || 'playwright');
const output = path.resolve(process.env.BROWSER_OUTPUT_DIR || 'data/validation/workspace-ui');
await fs.mkdir(output, { recursive: true });
const browser = await chromium.launch({ channel: 'chrome', headless: true });
const page = await browser.newPage({ viewport: { width: 1920, height: 1080 } });
page.setDefaultTimeout(15000);
const errors = [];
page.on('pageerror', e => errors.push(e.stack || e.message));
const bounds = [114.3208, 38.113, 114.3228, 38.115];
const dates = ['2023-11-01', '2024-09-01', '2025-11-01', '2026-05-01'];
const images = dates.map(date => ({ id: date.slice(0, 4), source: 'local', phase: '', capture_date: date,
  file_path: `${date.slice(0, 4)}.tif`, created_at: date, crs: 'EPSG:4326', width: 512, height: 512,
  bounds_west: bounds[0], bounds_south: bounds[1], bounds_east: bounds[2], bounds_north: bounds[3] }));
const tasks = [], downloads = [], maskRequests = [];
let releaseSecond = false, failSecond = false;
let releasePreparation;
const preparationGate = new Promise(resolve => { releasePreparation = resolve; });
let preparationID, firstTaskProgress = 25, firstTaskComplete = false;
const fixtures = await page.evaluate(() => {
  const canvas = document.createElement('canvas'); canvas.width = canvas.height = 512;
  const ctx = canvas.getContext('2d'), frames = {};
  for (const [i, year] of ['2023', '2024', '2025', '2026'].entries()) {
    ctx.fillStyle = ['#537546', '#677b48', '#4c6c50', '#67765e'][i]; ctx.fillRect(0, 0, 512, 512);
    for (let x = 0; x < 512; x += 64) { ctx.fillStyle = '#7c9158'; ctx.fillRect(x, 0, 30, 512); }
    ctx.fillStyle = '#567d9c'; ctx.fillRect(220, 0, 60, 512);
    ctx.fillStyle = '#bdb5a1'; ctx.fillRect(0, 245, 512, 15);
    for (let y = 35; y < 480; y += 95) for (let x = 25; x < 480; x += 85) { ctx.fillStyle = '#d0c7b7'; ctx.fillRect(x, y, 30 + i * 3, 22 + i * 3); }
    frames[year] = canvas.toDataURL('image/png').split(',')[1];
  }
  ctx.clearRect(0, 0, 512, 512);
  return { frames, blank: canvas.toDataURL('image/png').split(',')[1] };
});
function collection(index) {
  return { type: 'FeatureCollection', features: index === 2 ? [] : Array.from({ length: 12 }, (_, i) => {
    const w = bounds[0] + .0001 + (i % 4) * .0004, s = bounds[1] + .0001 + Math.floor(i / 4) * .0005;
    const e = w + .00025, n = s + .0003;
    return { type: 'Feature', id: `patch-${i + 1}`, bbox: [w, s, e, n],
      properties: { patch_id: `patch-${i + 1}`, area_m2: (i + 1) * 100, area_pixels: (i + 1) * 10, confidence_mean: .85, centroid: [(w + e) / 2, (s + n) / 2] },
      geometry: { type: 'Polygon', coordinates: [[[w, s], [e, s], [e, n], [w, n], [w, s]]] } };
  }) };
}
const json = (route, data) => route.fulfill({ json: data });
await page.route('**/api/**', async route => {
  const req = route.request(), pathname = new URL(req.url()).pathname;
  if (pathname === '/api/settings') return json(route, { revision:1, effective:true, settings: { default_model:'fixture-model', detection:{threshold:.5,resolution:.8,kernel_size:3,min_area_pixels:10}, browser:{concurrency:4,inactive_layers:3,cache_mb:128,idle_seconds:300} } });
  if (pathname === '/api/models') return json(route, [{id:'fixture-model',name:'Fixture ONNX',status:'ready',enabled:true,sha256:'test-hash',mapping:{}}]);
  if (pathname === '/api/algorithms') return json(route, { algorithms: [{ name: 'hebei_onnx', params_schema: { properties: { threshold: { type: 'number', default: .5 }, resolution: { type: 'number', default: .8 } } } }] });
  if (pathname.startsWith('/api/images/local/progress/')) return json(route, {id: preparationID, status:'running', stage:'mosaicking', progress:50});
  if (pathname === '/api/images/local') { preparationID = req.postDataJSON().progress_id; await preparationGate; assert.deepEqual(req.postDataJSON().scene_ids, ['2023', '2023-supplement', '2024', '2025', '2026']); return json(route, { items: images, count: images.length, batch_id:"fixture-batch" }); }
  if (pathname.endsWith('/preview')) return route.fulfill({ contentType: 'image/png', body: Buffer.from(fixtures.frames['2023'], 'base64') });
  if (pathname === '/api/tasks' && req.method() === 'POST') { tasks.push(req.postDataJSON()); return json(route, { id: `task-${tasks.length}`, status: 'pending' }); }
  const m = pathname.match(/^\/api\/tasks\/task-(\d+)(.*)$/);
  if (m) {
    const i = (Number(m[1]) - 1) % 3;
    if (!m[2] && m[1] === '1' && !firstTaskComplete) return json(route, {id:'task-1',status:'running',stage:firstTaskProgress < 90 ? 'inference':'vectorizing',progress:firstTaskProgress});
    if (!m[2]) return json(route, { id: `task-${m[1]}`, status: i === 1 && failSecond ? 'failed' : i === 1 && !releaseSecond ? 'running' : 'succeeded', progress: 50, error_msg: '测试：第二组检测失败' });
    if (m[2] === '/result') {
      const counts = Array.from({ length: 20 }, (_, j) => j >= 10 ? (j - 9) * 2 : 0), count = counts.reduce((a, b) => a + b, 0);
      return json(route, { mask_path: `mask-${m[1]}`, geojson: collection(i), stats_json: JSON.stringify({
        changed_pixels: i === 2 ? 0 : count, total_pixels: 1000, changed_area_m2: i === 2 ? 0 : 7800,
        feature_count: i === 2 ? 0 : 12, change_ratio: i === 2 ? 0 : count / 1000, mean_confidence: i === 2 ? 0 : .85, mean_confidence_all: .15,
        ...(i === 1 ? {} : { confidence_histograms: { bin_edges: Array.from({ length: 21 }, (_, j) => j / 20), changed_counts: i === 2 ? Array(20).fill(0) : counts,
          all_valid_counts: i === 2 ? [1000, ...Array(19).fill(0)] : counts.map((n, j) => n + (j === 0 ? 1000 - count : 0)), changed_sample_count: i === 2 ? 0 : count, all_valid_sample_count: 1000 } }) }) });
    }
    if (m[2].startsWith('/result/')) {
      downloads.push(pathname);
      if (m[2].endsWith('mask.tif')) return route.fulfill({ contentType: 'image/tiff', body: Buffer.from([73, 73, 42, 0, 8, 0, 0, 0]) });
      if (m[2].endsWith('change.geojson')) return json(route, collection(i));
      return route.fulfill({ contentType: 'text/csv', body: 'key,value\nchanged_pixels,110\n' });
    }
  }
  return route.fulfill({ status: 404, json: { error: `Unexpected fixture endpoint ${pathname}` } });
});
const scenes = images.map(image => ({ id: image.id, date: image.capture_date, file: image.file_path, filename: image.file_path, available: true, region_id: '130100', resolution_m: .8, date_precision: 'month' }));
let catalogVersion = 'fixture-1', staleVersion;
const catalogRequests = [], tileRequests = [];
await page.route('**/py/local/regions/130100/scenes*', route => {
  catalogRequests.push(route.request().url());
  return json(route, { region_id: '130100', name: '石家庄市', scenes,
    phases: scenes.map((scene, i) => ({ id: scene.id, region_id: '130100', month: scene.date.slice(0, 7),
      scenes: i ? [scene] : [scene, { ...scene, id: '2023-supplement' }, { ...scene, id: '2023-broken', available: false }],
      available: true, bounds: null, version: catalogVersion })) });
});
await page.route('**/py/local/regions/*/mosaic/**', route => {
  tileRequests.push(route.request().url());
  return route.fulfill({ status: new URL(route.request().url()).searchParams.get('v') === staleVersion ? 409 : 200,
    contentType: 'image/png', body: Buffer.from(fixtures.frames['2023'], 'base64') });
});
await page.route('**/py/local/catalog*', route => json(route, { images: [], warning: null, bounds: [113.45, 36, 119.9, 42.65], boundary: { type: 'Polygon', coordinates: [[[113.45, 36], [119.9, 36], [119.9, 42.65], [113.45, 42.65], [113.45, 36]]] } }));
await page.route('**/py/local/tiles/**', route => route.fulfill({ contentType: 'image/png', body: Buffer.from(fixtures.blank, 'base64') }));
await page.route('**/py/overlay/image', async route => {
  const request = route.request().postDataJSON();
  if (request.max_edge === 2048 && request.image_path === '2025.tif') await new Promise(resolve => setTimeout(resolve, 1000));
  return json(route, { image_b64: fixtures.frames[request.image_path.slice(0, 4)], bounds, crs: 'EPSG:4326', width: 512, height: 512 });
});
await page.route('**/py/overlay/mask', async route => {
  const data = route.request().postDataJSON(); maskRequests.push(data);
  const png = await page.evaluate(({ color, alpha, empty }) => {
    const canvas = document.createElement('canvas'); canvas.width = canvas.height = 512;
    const ctx = canvas.getContext('2d'); ctx.fillStyle = color; ctx.globalAlpha = alpha;
    if (!empty) ctx.fillRect(90, 90, 110, 100);
    return canvas.toDataURL('image/png').split(',')[1];
  }, { ...data, empty: data.mask_path === 'mask-3' });
  return json(route, { png_b64: png, bounds, crs: 'EPSG:4326', width: 512, height: 512 });
});
async function navigate(target) {
  const link = page.locator(`.app-nav .nav-item[href='/${target}']`);
  if (await page.locator('.mobile-menu').isVisible()) await page.locator('.mobile-menu').click();
  await link.click(); await page.waitForURL(`**/${target}`);
  if (page.viewportSize().width < 768) await page.waitForFunction(() => document.querySelector('.app-nav').getBoundingClientRect().right <= 1);
}
async function selectArea() {
  await page.getByRole('button', { name: '检测区域', exact: true }).click();
  const inputs = page.locator('.operations-panel input[type=number]');
  for (const [i, coordinate] of bounds.entries()) await inputs.nth(i).fill(String(coordinate));
  await page.getByRole('button', { name: '应用该区域框' }).click();
}
function assertAligned(positions) {
  for (const p of positions.slice(1)) for (const key of ['width', 'height', 'left', 'top']) assert.ok(Math.abs(p[key] - positions[0][key]) < 2, `linked ${key}: ${JSON.stringify(positions)}`);
}
async function positions() {
  return page.locator('.comparison-map').evaluateAll(nodes => nodes.map(node => {
    const map = node.getBoundingClientRect(), image = node.querySelector('img.leaflet-image-layer').getBoundingClientRect();
    return { width: image.width, height: image.height, left: image.left - map.left, top: image.top - map.top };
  }));
}
try {
  await page.goto(process.env.FRONTEND_URL || 'http://localhost:5173');
  await page.waitForURL('**/overview');
  await navigate('comparison'); await page.getByText('从一个区域开始', { exact: true }).waitFor();
  await navigate('reports'); await page.getByText('暂无检测报告', { exact: true }).waitFor();
  await navigate('overview');
  await page.getByRole('combobox', { name: '选择行政区' }).selectOption('130100');
  const list = page.getByRole('region', { name: '行政区影像列表' });
  await list.locator('[data-phase-id]').nth(3).waitFor();
  assert.ok(!catalogRequests[0].includes('refresh=true'), 'normal navigation reuses the catalog');
  assert.equal(await list.locator('[data-phase-id]').count(), 4, 'members are not separate list rows');
  for (const image of images) await list.locator(`[data-phase-id="${image.id}"]`).getByRole('button', { name: /^加入 .* 时相$/ }).click();
  catalogVersion = 'fixture-2';
  await list.getByRole('button', { name: '刷新目录' }).click();
  await page.waitForResponse(response => response.url().includes('v=fixture-2'));
  assert.equal(await page.getByRole('tab').count(), 4, 'refresh preserves loaded months');
  staleVersion = 'fixture-2'; catalogVersion = 'fixture-3';
  await page.locator('[data-map-workspace]').evaluate(node => node.__vueParentComponent.setupState.map.panBy([400, 0], { animate: false }));
  await page.waitForResponse(response => response.url().includes('v=fixture-3'));
  assert.equal(catalogRequests.filter(value => value.includes('refresh=true')).length, 1, 'only the manual refresh scans the source');
  assert.ok(catalogRequests.length >= 3 && !catalogRequests.at(-1).includes('refresh=true'), '409 reloads the current snapshot without a source scan');
  await selectArea();
  await page.getByRole('button', { name: '提交检测任务', exact: true }).click();
  await page.waitForFunction(() => document.querySelector('[role=progressbar]')?.getAttribute('aria-valuenow') === '10');
  assert.match(await page.locator('.task-progress').innerText(), /正在拼接影像/);
  await page.locator('.task-progress').scrollIntoViewIfNeeded();
  await page.locator('.task-progress').screenshot({path:path.join(output,'preparation-progress.png')});
  releasePreparation();
  await page.waitForFunction(() => document.querySelector('[role=progressbar]')?.getAttribute('aria-valuenow') === '26');
  firstTaskProgress = 70;
  await page.waitForFunction(() => document.querySelector('[role=progressbar]')?.getAttribute('aria-valuenow') === '38');
  assert.match(await page.locator('.task-progress').innerText(), /第 1\/3 组/);
  firstTaskProgress = 96;
  await page.waitForFunction(() => document.querySelector('[role=progressbar]')?.getAttribute('aria-valuenow') === '45');
  assert.match(await page.locator('.task-progress').innerText(), /正在生成变化图斑/);
  assert.match(await page.locator('.batch-indicator').innerText(), /45%/);
  await page.locator('.task-progress').scrollIntoViewIfNeeded();
  await page.locator('.task-progress').screenshot({path:path.join(output,'detection-progress.png')});
  firstTaskComplete = true;
  await navigate('reports');
  await page.waitForFunction(() => document.querySelector('option[value="2023:2024"]')?.textContent.includes('已完成'));
  assert.equal(tasks.length, 2, 'batch continues while Overview is unmounted from the visible route');
  assert.equal(await page.getByLabel('当前时相组').inputValue(), '2023:2024');
  await navigate('overview');
  assert.equal(await page.getByRole('button', { name: '框选区域', exact: true }).isDisabled(), true);
  assert.equal(await page.getByRole('button', { name: '清除选区', exact: true }).isDisabled(), true);
  await page.screenshot({ path: path.join(output, 'overview-1920.png') });
  await navigate('comparison');
  await page.locator('.opaque-change-mask').waitFor();
  await page.locator('.viewer-message').waitFor({ state: 'detached' });
  assert.equal(await page.locator('.hotspot-card').count(), 10);
  assert.match(await page.locator('.hotspot-card').first().innerText(), /1,200/);
  assertAligned(await positions());
  assert.ok(maskRequests.some(req => req.alpha === 1));
  const alpha = await page.locator('.opaque-change-mask').evaluate(async img => {
    await img.decode(); const canvas = document.createElement('canvas'); canvas.width = canvas.height = 512;
    const ctx = canvas.getContext('2d'); ctx.drawImage(img, 0, 0); return [ctx.getImageData(100, 100, 1, 1).data[3], ctx.getImageData(0, 0, 1, 1).data[3], getComputedStyle(img).opacity];
  });
  assert.deepEqual(alpha, [255, 0, '1']);
  await page.getByLabel('置信度统计范围').selectOption('all_valid');
  assert.match(await page.locator('.chart-sample').innerText(), /1,000/);
  await page.getByLabel('置信度统计范围').selectOption('changed');
  assert.match(await page.locator('.chart-sample').innerText(), /110/);
  await page.getByLabel('当前时相组').selectOption('2024:2025');
  await page.getByLabel('当前时相组').selectOption('2023:2024');
  await new Promise(resolve => setTimeout(resolve, 1300));
  assert.equal(await page.locator('.comparison-map img').first().getAttribute('src'), `data:image/png;base64,${fixtures.frames['2023']}`, 'stale preview must not replace selected imagery');
  await page.locator('.hotspot-card').first().click();
  assert.equal(await page.locator('.comparison-map path').count(), 3);
  assertAligned(await positions());
  await page.getByRole('button', { name: '重置视图' }).click();
  const initial = await positions();
  await page.locator('.comparison-map .leaflet-control-zoom-in').first().click();
  const zoomed = await positions(); assertAligned(zoomed);
  assert.ok(zoomed[0].width > initial[0].width, 'zoom affects all views');
  const mapRect = await page.locator('.comparison-map').first().boundingBox();
  await page.mouse.move(mapRect.x + mapRect.width / 2, mapRect.y + mapRect.height / 2);
  await page.mouse.down(); await page.mouse.move(mapRect.x + mapRect.width / 2 + 35, mapRect.y + mapRect.height / 2 + 25, { steps: 8 }); await page.mouse.up();
  await new Promise(resolve => setTimeout(resolve, 300)); assertAligned(await positions());
  await page.getByRole('button', { name: '重置视图' }).click();
  await page.screenshot({ path: path.join(output, 'comparison-1920.png') });
  await page.setViewportSize({ width: 1440, height: 900 });
  await page.screenshot({ path: path.join(output, 'comparison-1440.png') });
  assert.equal(tasks.length, 2); assert.equal(await page.getByLabel('当前时相组').inputValue(), '2023:2024');
  releaseSecond = true;
  await page.waitForFunction(() => document.querySelector('option[value="2025:2026"]')?.textContent.includes('已完成'));
  assert.equal(tasks.length, 3);
  assert.equal(await page.getByLabel('当前时相组').inputValue(), '2023:2024', 'later completion preserves selected pair');
  assert.deepEqual(tasks.map(t => [t.before_id, t.after_id]), [['2023', '2024'], ['2024', '2025'], ['2025', '2026']]);
  for (const task of tasks) { assert.equal(task.algorithm, 'hebei_onnx'); assert.equal(task.params.provider, 'cuda'); assert.equal(task.params.threshold, .5); }
  await page.getByLabel('当前时相组').selectOption('2024:2025');
  await page.getByText('置信度分布不可用', { exact: true }).waitFor();
  await page.getByLabel('当前时相组').selectOption('2025:2026');
  await page.getByText('此时相组没有检测到变化图斑', { exact: true }).waitFor();
  assert.equal(await page.locator('.metric-card').nth(1).locator('strong').innerText(), '—');
  assert.equal(await page.locator('.metric-card').nth(3).locator('strong').innerText(), '0.00%');
  await page.getByLabel('当前时相组').selectOption('2023:2024');
  await navigate('reports');
  assert.equal(await page.locator('.results-table').first().locator('tbody tr').count(), 3);
  assert.equal(await page.locator('.results-table').nth(1).locator('tbody tr').count(), 12);
  assert.match(await page.locator('.results-table').nth(1).locator('tbody tr').first().innerText(), /patch-12/);
  for (const [label, suffix] of [['GeoTIFF', 'tif'], ['GeoJSON', 'geojson'], ['CSV', 'csv']]) {
    const downloaded = page.waitForEvent('download'); await page.getByRole('button', { name: label, exact: true }).click();
    const file = await downloaded; assert.ok(file.suggestedFilename().endsWith(`.${suffix}`));
    const bytes = await fs.readFile(await file.path());
    if (suffix === 'tif') assert.equal(bytes.subarray(0, 4).toString('hex'), '49492a00');
    if (suffix === 'geojson') assert.equal(JSON.parse(bytes).type, 'FeatureCollection');
    if (suffix === 'csv') assert.match(bytes.toString(), /changed_pixels/);
  }
  assert.equal(await page.getByRole('button', { name: 'PNG', exact: true }).count(), 0);
  await page.screenshot({ path: path.join(output, 'reports-1440.png') });
  await page.setViewportSize({ width: 390, height: 844 });
  await navigate('comparison');
  await page.locator('.opaque-change-mask').waitFor();
  await page.waitForFunction(() => [...document.querySelectorAll('.comparison-map img')].every(img => img.complete && img.naturalWidth > 0));
  const mobileMaps = await positions();
  assert.ok(mobileMaps[0].left + mobileMaps[0].width > 0 && mobileMaps[0].top + mobileMaps[0].height > 0, 'mobile imagery remains in view after route activation');
  await page.screenshot({ path: path.join(output, 'comparison-mobile.png'), animations: 'disabled' });
  assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true);
  assert.equal(await page.getByLabel('当前时相组').inputValue(), '2023:2024');
  await navigate('overview');
  await page.getByRole('button', { name: '清除选区', exact: true }).click();
  await selectArea(); failSecond = true;
  await page.getByRole('button', { name: '提交检测任务', exact: true }).click();
  await navigate('reports');
  await page.getByText('测试：第二组检测失败', { exact: true }).waitFor();
  assert.equal(tasks.length, 5, 'failed pair must stop the remaining sequence');
  assert.match(await page.locator('.results-table').first().locator('tbody tr').first().innerText(), /已完成/);
  assert.equal(await page.getByRole('button', { name: 'GeoJSON', exact: true }).isEnabled(), true);
  // Complete a batch while Overview stays visible, then use its comparison link.
  // Navigating during detection hides the old map while selection is disabled
  // and misses the KeepAlive resize regression that clears a finished batch.
  await page.setViewportSize({ width: 1920, height: 1080 });
  await navigate('overview');
  await page.getByRole('button', { name: '清除选区', exact: true }).click();
  await selectArea(); failSecond = false;
  await page.getByRole('button', { name: '提交检测任务', exact: true }).click();
  await page.getByText('检测完成', { exact: true }).waitFor();
  assert.equal(tasks.length, 8);
  for (let visit = 0; visit < 2; visit++) {
    await page.getByRole('link', { name: '查看对比 →', exact: true }).click();
    await page.waitForURL('**/comparison');
    await page.locator('.opaque-change-mask').waitFor();
    await page.waitForFunction(() => document.querySelectorAll('.comparison-map img.leaflet-image-layer').length === 4 &&
      [...document.querySelectorAll('.comparison-map img')].every(img => img.complete && img.naturalWidth > 0));
    assert.equal(await page.locator('.batch-indicator').innerText(), '已完成 3 / 3 组');
    assert.equal(await page.getByLabel('当前时相组').inputValue(), '2023:2024');
    assert.equal(await page.locator('.empty-workspace').count(), 0);
    assertAligned(await positions());
    await navigate('overview');
    assert.equal(await page.getByRole('combobox', { name: '选择行政区' }).inputValue(), '130100');
    assert.equal(await page.getByRole('tab').count(), 4);
    assert.equal(await page.getByRole('button', { name: '提交检测任务', exact: true }).isEnabled(), true);
  }
  await navigate('reports');
  assert.equal(await page.locator('.results-table').first().locator('tbody tr').count(), 3);
  assert.deepEqual(errors, [], 'no uncaught browser errors');
  await fs.writeFile(path.join(output, 'report.json'), JSON.stringify({ passed: true, source: 'deterministic browser fixtures; no model inference', viewports: ['1920x1080', '1440x900', '390x844'], tasks: tasks.length, downloads, errors }, null, 2));
  console.log('Workspace browser acceptance passed:', output);
} catch (error) {
  await page.screenshot({ path: path.join(output, 'failure.png') }).catch(() => {});
  console.error('Browser errors:', errors); throw error;
} finally { await browser.close(); }
