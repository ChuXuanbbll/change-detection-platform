// Real checked-in administrative shapes; isolated imagery/tasks, no inference.
import assert from 'node:assert/strict';
import fs from 'node:fs/promises';
import path from 'node:path';
import { createHash } from 'node:crypto';
import { createRequire } from 'node:module';
const require = createRequire(import.meta.url);
const { chromium } = require(process.env.PLAYWRIGHT_MODULE || 'playwright');
const raw = await fs.readFile('data/boundaries/hebei-administrative.geojson');
const dataset = JSON.parse(raw);
const version = `${dataset.dataset_id}-${createHash('sha256').update(raw).digest('hex')}`;
const features = dataset.features.map(f => ({ ...f, properties: { ...f.properties, boundary_version: version, data_date:dataset.data_date } }));
const sources = JSON.parse(await fs.readFile('data/boundaries/hebei-source-groups.geojson', 'utf8'));
const province = features.find(f => f.properties.level === 'province');
const output = path.resolve(process.env.BROWSER_OUTPUT_DIR || 'data/validation/administrative-boundaries');
await fs.mkdir(output, { recursive: true });
const browser = await chromium.launch({ channel: 'chrome', headless: true });
const url = process.env.FRONTEND_URL || 'http://localhost:5173';
const checks = [];
try {
  for (const [name, width, height] of [['desktop', 1440, 1000], ['mobile', 390, 844]]) {
    const page = await browser.newPage({ viewport: { width, height } });
    page.setDefaultTimeout(15000);
    const errors = [], crops = [], mosaicRequests = [];
    let delayBoundary = false;
    page.on('pageerror', error => errors.push(error.message));
    const phasesFor = source => ['01', '03'].map(month => ({ id: `${source}:2025-${month}`, region_id: source,
      month: `2025-${month}`, version: `${source}-${month}-full`, available: true, bounds: null,
      scenes: [1, 2].map(i => ({ id: `${source}-${month}-${i}`, file: `${i}.tif`, filename: `${i}.tif`,
        region_id: source, date: `2025-${month}-01`, date_precision: 'month', available: true })) }));
    const json = (route, data, status = 200) => route.fulfill({ status, json: data });
    await page.route('**/api/**', route => {
      const req = route.request(), uri = new URL(req.url());
      if (uri.pathname === '/api/administrative-regions') {
        const source = uri.searchParams.get('source_id'), level = uri.searchParams.get('level') || 'county';
        return json(route, { type: 'FeatureCollection', features: features.filter(f => f.properties.level === level && (!source || f.properties.source_id === source)) });
      }
      if (uri.pathname.startsWith('/api/administrative-regions/')) {
        const feature = features.find(f => f.id === uri.pathname.split('/').at(-1));
        if (delayBoundary) return new Promise(resolve => setTimeout(resolve,250)).then(() => json(route,feature));
        return json(route, feature);
      }
      if (uri.pathname === '/api/settings') return json(route, { revision: 1, effective: true, settings: {
        default_model: 'fixture', detection: { threshold: .5, resolution: .8, kernel_size: 1, min_area_pixels: 1 },
        browser: { concurrency: 2, inactive_layers: 2, cache_mb: 64, idle_seconds: 300 } } });
      if (uri.pathname === '/api/models') return json(route, [{ id: 'fixture', name: 'Fixture', status: 'ready', enabled: true, sha256: 'fixture', mapping: {} }]);
      if (uri.pathname === '/api/ai/status') return json(route, { configured: false });
      if (uri.pathname === '/api/images/local') {
        crops.push(req.postDataJSON());
        return json(route, { items: [], count: 0, batch_id: 'fixture', message: '夹具不创建真实任务' });
      }
      return json(route, { error: 'No real API calls allowed in this fixture' }, 404);
    });
    await page.route('**/py/**', async route => {
      const uri = new URL(route.request().url());
      if (uri.pathname === '/py/local/catalog') return json(route, { images: [], boundary: province.geometry, bounds: province.properties.bounds });
      if (uri.pathname === '/py/local/regions') return json(route, { ...sources, boundary: province.geometry, bounds: province.properties.bounds });
      if (uri.pathname.endsWith('/scenes')) {
        const source = uri.pathname.split('/')[4], county = uri.searchParams.get('admin_code');
        // Delayed source response exercises stale-request cancellation.
        if (source === '130600') await new Promise(resolve => setTimeout(resolve, 250));
        let phases = phasesFor(source);
        if (county === '130632') phases = [];
        else if (county) phases = phases.map(phase => ({ ...phase, matching_scene_count: 1 }));
        return json(route, { region_id: source, name: source, phases, scenes: phases.flatMap(p => p.scenes) });
      }
      if (uri.pathname.includes('/mosaic/')) {
        mosaicRequests.push(uri);
        return route.fulfill({ contentType: 'image/png', body: Buffer.from('iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR4nGNgYGBgAAAABQABpfZFQAAAAABJRU5ErkJggg==', 'base64') });
      }
      if (uri.pathname === '/py/local/selection-pixels') return json(route, { width: 80, height: 80, pixels: 6400 });
      return json(route, { detail: 'No real imagery calls allowed in this fixture' }, 404);
    });
    await page.goto(url);
    const sourceSelect = page.getByRole('combobox', { name: '选择影像来源' });
    const countySelect = page.getByRole('combobox', { name: '选择县级行政区' });
    const settleNavigation = () => page.waitForFunction(() => !document.querySelector('[data-map-workspace]').__vueParentComponent.setupState.navigationAnimating);
    const checkTransition = async () => {
      const pane = page.locator('.leaflet-cdp-region-labels-pane');
      assert.equal(await pane.evaluate(node => getComputedStyle(node).opacity), '0', 'names hidden during navigation');
      await settleNavigation();
      assert.equal(await pane.evaluate(node => getComputedStyle(node).transitionDuration), '0.2s');
      await page.waitForTimeout(250);
      assert.equal(await pane.evaluate(node => getComputedStyle(node).opacity), '1', 'names fade in after navigation');
    };
    await page.locator('[data-region-id]').nth(13).waitFor();
    assert.equal(await page.locator('[data-region-id]').count(), 14, 'province shows all imagery source regions');
    assert.deepEqual(await page.locator('[data-region-id]').evaluateAll(nodes => nodes.map(node => node.dataset.regionId).sort()), sources.features.map(feature => feature.id).sort());
    assert.equal(await sourceSelect.locator('option').count(), 15, '14 business entries remain');
    assert.equal(await page.locator('.county-label').count(), 0);
    const points = await page.locator('[data-region-id]').evaluateAll(paths => paths.map(node => {
      const rect = node.getBoundingClientRect();
      for (let row = 0; row < 20; row++) for (let col = 0; col < 20; col++) {
        const x = rect.left + rect.width * (col + 0.5) / 20;
        const y = rect.top + rect.height * (row + 0.5) / 20;
        if (document.elementFromPoint(x, y) === node) return { id: node.dataset.regionId, x, y };
      }
      return null;
    }).filter(Boolean));
    assert.equal(points.length, 14, 'all source regions have a visible mouse target');
    for (const point of points) {
      await page.mouse.move(point.x, point.y);
      assert.deepEqual(await page.locator('[data-region-id]').evaluateAll(paths => paths.filter(node => getComputedStyle(node).stroke === 'rgb(103, 232, 249)').map(node => node.dataset.regionId)), [point.id], 'hover highlights only the source under the mouse');
    }
    await page.screenshot({ path: path.join(output, `${name}-province.png`), fullPage: true });
    for (const id of ['130682', '130181', 'xiongan']) {
      const point = points.find(point => point.id === id);
      await page.mouse.click(point.x, point.y);
      assert.equal(await sourceSelect.inputValue(), id, 'map click selects the independent imagery source');
      await checkTransition();
      await countySelect.locator(`option[value="${id === 'xiongan' ? '130629' : id}"]`).waitFor({ state: 'attached' });
      await page.waitForTimeout(900);
      await page.getByRole('button', { name: '河北全省' }).click();
      await checkTransition();
    }
    await page.locator('[data-region-id="xiongan"]').dispatchEvent('click');
    await countySelect.locator('option[value="130629"]').waitFor({ state: 'attached' });
    assert.equal(await countySelect.locator('option').count(), 4);
    const list = page.getByRole('region', { name: '影像来源列表' });
    await list.locator('[data-phase-id]').first().waitFor();
    await page.waitForTimeout(900);
    await list.locator('[data-phase-id="xiongan:2025-01"]').dragTo(page.locator('.leaflet-container'));
    await page.getByRole('tab', { name: /2025-01/ }).waitFor();
    await list.getByRole('button', { name: '加入 2025-03 时相' }).click();
    assert.equal(await page.getByRole('tab').count(), 2);
    const root = page.locator('[data-map-workspace]');
    // County focus respects manual zoom; blank masked areas cannot switch sources.
    const view = () => root.evaluate(node => {
      const m = node.__vueParentComponent.setupState.map;
      return { zoom: m.getZoom(), center: [m.getCenter().lat, m.getCenter().lng] };
    });
    await countySelect.selectOption('130629');
    await settleNavigation();
    await root.evaluate(node => node.__vueParentComponent.setupState.map.setZoom(13, { animate: false }));
    const closeView = await view();
    await countySelect.selectOption('130632');
    assert.deepEqual(await view(), closeView, 'changing county above threshold preserves viewport');
    await page.locator('[data-admin-code="130632"]').dispatchEvent('click');
    assert.deepEqual(await view(), closeView, 'repeated county click preserves viewport');
    await root.evaluate(node => {
      const s = node.__vueParentComponent.setupState;
      s.map.setZoom(s.cityFitZoom, { animate: false });
    });
    const wideView = await view();
    await countySelect.selectOption('130629');
    await checkTransition();
    assert.ok((await view()).zoom > wideView.zoom, 'wide city view zooms toward selected county');
    await root.evaluate(node => {
      const s = node.__vueParentComponent.setupState;
      s.map.fitBounds(s.cityExtent, { padding: [32, 32], animate: false });
    });
    const blank = await page.locator('.leaflet-container').evaluate(container => {
      const box = container.getBoundingClientRect();
      for (let y = box.top + 90; y < box.bottom - 100; y += 10) {
        for (let x = box.left + 60; x < box.right - 60; x += 10) {
          const target = document.elementFromPoint(x, y);
          if (target?.matches('[data-region-id]:not([data-region-id="xiongan"])')) return { x, y };
        }
      }
      return null;
    });
    assert.ok(blank, 'masked neighbouring city provides a blank-area target');
    const blankView = await view();
    await page.mouse.click(blank.x, blank.y);
    assert.equal(await sourceSelect.inputValue(), 'xiongan', 'blank click cannot enter another city');
    assert.deepEqual(await view(), blankView, 'blank click preserves viewport');
    await page.getByRole('button', { name: '多边形区域', exact: true }).click();
    await page.mouse.dblclick(blank.x, blank.y);
    assert.equal(await sourceSelect.inputValue(), 'xiongan', 'polygon double click cannot leave city');
    await page.getByRole('button', { name: '取消绘制', exact: true }).click();
    await page.mouse.dblclick(blank.x, blank.y);
    assert.equal(await sourceSelect.inputValue(), '', 'blank double click returns to province');
    await sourceSelect.selectOption('xiongan');
    await countySelect.locator('option[value="130629"]').waitFor({ state: 'attached' });
    await page.waitForTimeout(900);
    await list.getByRole('button', { name: '加入 2025-01 时相' }).click();
    await list.getByRole('button', { name: '加入 2025-03 时相' }).click();
    const center = features.find(f => f.id === '130629').properties.label;
    const manual = { type: 'Polygon', coordinates: [[[center[0], center[1]], [center[0]+.0001, center[1]], [center[0], center[1]+.0001], center]] };
    await root.evaluate((node, aoi) => node.__vueParentComponent.parent.setupState.setSelectedArea({ west:aoi.coordinates[0][0][0], south:aoi.coordinates[0][0][1], east:aoi.coordinates[0][1][0], north:aoi.coordinates[0][2][1] }, aoi), manual);
    const sourceListText = await list.innerText();
    await countySelect.selectOption('130629');
    await settleNavigation();
    assert.equal(await list.innerText(), sourceListText, 'county navigation preserves source list text and months');
    assert.equal(await page.getByRole('tab').count(), 2, 'browsing preserves loaded phases');
    assert.deepEqual(await root.evaluate(node => node.__vueParentComponent.parent.setupState.selectedGeometry), manual, 'browsing preserves manual AOI');
    await countySelect.selectOption('130632');
    await settleNavigation();
    assert.equal(await list.innerText(), sourceListText, 'county with no candidates still preserves source list');
    assert.equal(await page.getByRole('tab').count(), 2, 'empty candidate list preserves loaded phases');
    await countySelect.selectOption('130629');
    await page.getByRole('button', { name: '以容城县边界作为检测范围' }).click();
    await page.getByText(/检测范围：容城县完整县界/).waitFor();
    await page.locator('.submit-detection').click();
    await page.waitForFunction(() => document.querySelector('.submit-detection')?.disabled === false);
    assert.equal(crops.length, 1);
    assert.equal(crops[0].admin_code, '130629');
    assert.equal(crops[0].admin_version, version);
    assert.deepEqual(crops[0].aoi, features.find(f => f.id === '130629').geometry);
    assert.equal(crops[0].scene_ids.length, 4, 'whole-source phases are preserved');
    assert.ok(mosaicRequests.every(uri => uri.searchParams.get('v')?.endsWith('-full')));
    assert.equal(await page.locator('.region-label:not(.county-label)').evaluateAll(nodes => nodes.every(n => getComputedStyle(n).opacity === '0')), true);
    await page.screenshot({ path: path.join(output, `${name}-county.png`), fullPage: true });
    for (const source of ['130682', '130181']) {
      await sourceSelect.selectOption(source);
      await countySelect.locator(`option[value="${source}"]`).waitFor({ state: 'attached' });
      assert.equal(await countySelect.locator('option').count(), 2);
    }
    await sourceSelect.selectOption('130100');
    await countySelect.locator('option[value="130121"]').waitFor({ state: 'attached' });
    await page.waitForTimeout(900);
    const labelsClear = await page.locator('.county-label span').evaluateAll(nodes => nodes.every(node => {
      if (getComputedStyle(node.parentElement).opacity === '0') return true;
      const rect = node.getBoundingClientRect();
      const top = document.elementFromPoint(rect.left+rect.width/2, rect.top+rect.height/2);
      return !!top?.closest('.leaflet-container');
    }));
    assert.equal(labelsClear, true, 'county names avoid map controls on each viewport');
    await page.screenshot({ path: path.join(output, `${name}-all-counties.png`), fullPage: true });
    // Both a county with holes and a county with detached components must draw
    // all rings/parts, using real official snapshot geometries.
    for (const code of ['130121', '130107']) {
      await countySelect.selectOption(code);
      const feature = features.find(f => f.id === code);
      await page.getByRole('button', { name: `以${feature.properties.name}边界作为检测范围` }).click();
      await page.getByText(`检测范围：${feature.properties.name}完整县界`, { exact: false }).waitFor();
      const drawn = await root.evaluate(node => node.__vueParentComponent.setupState.selectionPolygon.toGeoJSON().geometry);
      const polygons = g => g.type === 'Polygon' ? [g.coordinates] : g.coordinates;
      assert.deepEqual(polygons(drawn).map(p => p.map(r => r.length)), polygons(feature.geometry).map(p => p.map(r => r.length)), 'render every component and hole');
    }
    await page.screenshot({ path: path.join(output, `${name}-city-counties.png`), fullPage: true });
    await sourceSelect.selectOption('xiongan');
    await countySelect.locator('option[value="130629"]').waitFor({ state: 'attached' });
    await countySelect.selectOption('130629');
    delayBoundary = true;
    await page.getByRole('button', {name:'以容城县边界作为检测范围'}).click();
    await sourceSelect.selectOption('130682');
    await page.waitForTimeout(350);
    assert.equal(await root.evaluate(node => node.__vueParentComponent.parent.setupState.administrativeBoundary), null, 'late boundary lookup cannot replace a changed workspace');
    delayBoundary = false;
    await sourceSelect.selectOption('130600');
    await sourceSelect.selectOption('xiongan');
    await countySelect.locator('option[value="130629"]').waitFor({ state: 'attached' });
    await page.waitForTimeout(400);
    assert.equal(await countySelect.locator('option').count(), 4, 'late source response cannot replace county list');
    await page.getByRole('button', { name: '河北全省' }).click();
    assert.equal(await page.locator('.county-label').count(), 0);
    assert.equal(await countySelect.count(), 0);
    assert.deepEqual(errors, []);
    checks.push({ viewport: name, province: 14, sources: 14, independentSourceClicks: ['130682', '130181', 'xiongan'], xionganCounties: 3, explicitCountyCrop: true, completeRings: true, pageErrors: errors });
    await page.close();
  }
  await fs.writeFile(path.join(output, 'report.json'), JSON.stringify({ boundaryVersion: version, checks }, null, 2));
  console.log(JSON.stringify({ ok: true, checks, output }));
} finally { await browser.close(); }
