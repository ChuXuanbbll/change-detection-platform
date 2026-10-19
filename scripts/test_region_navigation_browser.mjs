// Real administrative geometry, deterministic empty imagery catalog; no detection jobs.
import assert from 'node:assert/strict';
import fs from 'node:fs/promises';
import { createRequire } from 'node:module';
const require = createRequire(import.meta.url);
const { chromium } = require(process.env.PLAYWRIGHT_MODULE || 'playwright');
const browser = await chromium.launch({ channel: 'chrome', headless: true });
const url = process.env.FRONTEND_URL || 'http://localhost:5173';
const output = process.env.BROWSER_OUTPUT_DIR || 'data/validation/region-navigation';
await fs.mkdir(output, { recursive: true });
const regions = JSON.parse(await fs.readFile('data/boundaries/hebei-regions.geojson', 'utf8'));
const boundary = JSON.parse(await fs.readFile('data/boundaries/hebei.geojson', 'utf8')).features[0].geometry;
const report = [];
try {
  for (const [width, height] of [[1440, 900], [1920, 1080], [390, 844]]) {
    const page = await browser.newPage({ viewport: { width, height } });
    const errors = [];
    await page.route('**/api/settings', route => route.fulfill({json:{revision:1,effective:true,settings:{default_model:'test',detection:{threshold:.5,resolution:.8,kernel_size:3,min_area_pixels:10},browser:{concurrency:4,inactive_layers:3,cache_mb:128,idle_seconds:300}}}}));
    await page.route('**/api/models', route => route.fulfill({json:[{id:'test',name:'Test',status:'ready',enabled:true,sha256:'test-hash',mapping:{}}]}));
    page.on('pageerror', error => errors.push(error.message));
    await page.route('**/py/local/catalog*', route => route.fulfill({ json: { images: [] } }));
    await page.route('**/py/local/regions', route => route.fulfill({ json: { ...regions, boundary, bounds: [113.45, 36, 119.9, 42.65] } }));
    await page.route('**/py/local/regions/*/scenes*', route => route.fulfill({ json: { scenes: [], phases: [] } }));
    await page.goto(url);
    const root = page.locator('[data-map-workspace]');
    const select = page.getByRole('combobox', { name: '选择影像来源' });
    await page.locator('[data-region-id="130700"]').waitFor();
    await page.evaluate(async () => { window.viewMath = await import('/src/utils/regionView.ts'); });
    await root.evaluate(node => {
      const s = node.__vueParentComponent.setupState;
      window.invalidNavigationFrames = [];
      s.map.on('move', () => {
        if (!s.activeRegion || s.navigationAnimating) return;
        const actual = s.map.project(s.map.getCenter());
        const allowed = s.map.project(s.regionNavigation.constrain(s.map.getCenter()));
        const error = actual.distanceTo(allowed);
        if (error > 1) window.invalidNavigationFrames.push({ error, id: s.activeRegion.id, zoom: s.map.getZoom(), stack: new Error().stack });
      });
    });
    // Inspect the existing Vue setup state, without adding a production test API.
    const state = () => root.evaluate(node => {
      const s = node.__vueParentComponent.setupState, m = s.map, feature = s.activeRegion;
      if (!feature) return { overview: s.isOverview, zoom: m.getZoom(), fit: s.cityFitZoom, exit: s.cityExitZoom, animating: s.navigationAnimating };
      const rings = feature.geometry.type === 'Polygon' ? [feature.geometry.coordinates] : feature.geometry.coordinates;
      const polygons = rings.map(p => p.map(r => r.map(([lng, lat]) => m.project([lat, lng]))));
      const points = polygons.flat(2);
      const extent = { min: { x: Math.min(...points.map(p => p.x)), y: Math.min(...points.map(p => p.y)) },
        max: { x: Math.max(...points.map(p => p.x)), y: Math.max(...points.map(p => p.y)) } };
      const center = m.project(m.getCenter()), size = m.getSize();
      const limits = window.viewMath.centerLimits(extent, size);
      return { id: feature.id, zoom: m.getZoom(), fit: s.cityFitZoom, exit: s.cityExitZoom, animating: s.navigationAnimating,
        error: Math.hypot(center.x - (extent.min.x + extent.max.x) / 2, center.y - (extent.min.y + extent.max.y) / 2),
        valid: center.x >= limits.min.x - 1 && center.x <= limits.max.x + 1 && center.y >= limits.min.y - 1 && center.y <= limits.max.y + 1 &&
          window.viewMath.regionVisible(polygons, center, size, 31), size };
    });
    const settle = () => page.waitForTimeout(1100);
    const drag = async (dx, dy) => {
      const box = await page.locator('.leaflet-container').boundingBox();
      const x = box.x + box.width / 2, y = box.y + box.height * 0.55;
      await page.mouse.move(x, y); await page.mouse.down();
      for (let i = 1; i <= 8; i++) {
        await page.mouse.move(x + dx * i / 8, y + dy * i / 8);
        assert.ok((await state()).valid, 'drag must remain constrained before mouseup');
      }
      await page.mouse.up(); await page.waitForTimeout(450);
      assert.ok((await state()).valid, 'inertia must remain constrained');
    };
    for (const id of ['130700', '130300', '130400']) {
      await select.selectOption(id); await settle();
      const initial = await state();
      assert.equal(initial.id, id, JSON.stringify(initial)); assert.equal(initial.animating, false);
      assert.ok(initial.error <= 2, `city ${id} must remain centered: ${JSON.stringify(initial)}`);
      assert.equal(initial.exit, initial.fit - 1);
      for (const [dx, dy] of [[600, 0], [-600, 0], [0, 600], [0, -600], [600, 600], [-600, -600]]) await drag(dx, dy);
      await root.evaluate(node => { const s = node.__vueParentComponent.setupState; s.map.setZoom(s.cityFitZoom + 3, { animate: false }); });
      for (const [dx, dy] of [[600, 0], [-600, 0], [0, 600], [0, -600], [600, -600]]) await drag(dx, dy);
      await root.evaluate(node => { const s = node.__vueParentComponent.setupState; s.map.setZoom(s.cityExitZoom + 0.25, { animate: false }); });
      assert.equal(await select.inputValue(), id);
      await page.getByRole('button', { name: 'Zoom out' }).click({ force: true }); await page.waitForTimeout(500);
      assert.equal(await select.inputValue(), '', 'one-step-below-entry threshold returns to province');
      assert.equal((await state()).overview, true);
      report.push({ width, height, id, centerError: initial.error });
    }
    // Rapid changes and interruption must release the transition guard.
    await select.selectOption('130700'); await page.waitForTimeout(100);
    await select.selectOption('130300'); await page.waitForTimeout(100);
    const box = await page.locator('.leaflet-container').boundingBox();
    await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2);
    await page.mouse.wheel(0, -100); await settle();
    assert.equal((await state()).id, '130300'); assert.equal((await state()).animating, false);
    assert.ok((await state()).valid);
    await page.setViewportSize({ width: width === 390 ? 1440 : 390, height }); await page.waitForTimeout(500);
    assert.equal((await state()).id, '130300', 'resize must not exit city'); assert.ok((await state()).valid);
    for (const target of ['comparison', 'overview']) {
      // The desktop nav is outside the mobile viewport; dispatch still exercises
      // router KeepAlive without making this a responsive-navigation test.
      await page.locator(`.app-nav .nav-item[href="/${target}"]`).dispatchEvent('click');
      await page.waitForURL(`**/${target}`);
    }
    await page.waitForTimeout(500);
    assert.equal((await state()).id, '130300'); assert.ok((await state()).valid);
    if (width === 1440) {
      // Deterministic imagery checks share the actual workspace, tile cache and
      // component watchers, without starting an inference job.
      await page.setViewportSize({ width, height }); await page.waitForTimeout(400);
      const phases = ['2025-01', '2025-03'].map(month => ({ id: `130700:${month}`, region_id: '130700', month,
        version: month, available: true, bounds: [113.8, 39.5, 116.5, 42.2],
        scenes: [{ id: month, region_id: '130700', region: '张家口市', file: `${month}.tif`, filename: `${month}.tif`,
          date: `${month}-01`, date_precision: 'day', date_source: 'fixture', available: true }] }));
      await page.route('**/py/local/regions/130700/scenes*', route => route.fulfill({ json: { scenes: phases.flatMap(p => p.scenes), phases } }));
      const png = await page.evaluate(() => {
        const c = document.createElement('canvas'); c.width = c.height = 256;
        const ctx = c.getContext('2d'); ctx.fillStyle = '#547945'; ctx.fillRect(0, 0, 256, 256);
        return c.toDataURL().split(',')[1];
      });
      await page.route('**/mosaic/**', route => route.fulfill({ contentType: 'image/png', body: Buffer.from(png, 'base64') }));
      await select.selectOption('130700'); await settle();
      await page.locator('[data-phase-id]').first().getByRole('button', { name: /^加入 .* 时相$/ }).click();
      await page.waitForFunction(() => [...document.querySelectorAll('.leaflet-tile')].some(t => t.complete && t.naturalWidth === 256));
      await drag(600, 600);
      const beforePhase = await root.evaluate(node => node.__vueParentComponent.exposed.getView());
      await page.locator('[data-phase-id]').nth(1).getByRole('button', { name: /^加入 .* 时相$/ }).click();
      assert.deepEqual(await root.evaluate(node => node.__vueParentComponent.exposed.getView()), beforePhase);
      // Hold the crop request: exercise actual detection locking without submitting a model job.
      let releaseCrop;
      const cropGate = new Promise(resolve => { releaseCrop = resolve; });
      await page.route('**/api/images/local', async route => {
        await cropGate; await route.fulfill({ json: { items: [], count: 0 } });
      });
      await root.evaluate(node => {
        node.__vueParentComponent.parent.setupState.selectedArea = { south: 40.7, west: 114.7, north: 40.75, east: 114.75 };
      });
      await page.getByRole('button', { name: '提交检测任务', exact: true }).click();
      await page.waitForFunction(() => document.querySelector('select[aria-label="选择影像来源"]').disabled);
      await root.evaluate(node => { const s = node.__vueParentComponent.setupState; s.map.setZoom(0, { animate: false }); });
      const locked = await state();
      assert.equal(locked.id, '130700'); assert.equal(locked.zoom, locked.exit + 0.25);
      assert.equal(await page.getByRole('tab').count(), 2);
      releaseCrop();
      await page.waitForFunction(() => !document.querySelector('select[aria-label="选择影像来源"]').disabled);
      // Result navigation at high zoom and another city's programmatic selection.
      await root.evaluate(node => node.__vueParentComponent.exposed.flyToBounds([40.7, 114.7, 40.75, 114.75], 32));
      await settle();
      assert.equal((await state()).id, '130700'); assert.ok((await state()).valid);
      await root.evaluate(node => node.__vueParentComponent.exposed.flyToBounds([36.55, 114.45, 36.6, 114.5], 32));
      await settle();
      assert.equal((await state()).id, '130400'); assert.ok((await state()).valid);
      await root.evaluate(node => { const s = node.__vueParentComponent.setupState; s.map.setZoom(s.cityExitZoom, { animate: false }); });
      assert.equal(await select.inputValue(), ''); assert.equal(await page.locator('.leaflet-tile').count(), 0);
      assert.equal(await page.getByRole('tab').count(), 0);
      // Late catalog response cannot reactivate the city after an automatic exit.
      await page.route('**/py/local/regions/130700/scenes*', async route => {
        await new Promise(resolve => setTimeout(resolve, 1500));
        await route.fulfill({ json: { scenes: phases.flatMap(p => p.scenes), phases } }).catch(() => {});
      });
      await select.selectOption('130700'); await settle();
      await root.evaluate(node => { const s = node.__vueParentComponent.setupState; s.map.setZoom(s.cityExitZoom, { animate: false }); });
      await page.waitForTimeout(700);
      assert.equal(await select.inputValue(), ''); assert.equal(await page.locator('[data-phase-id]').count(), 0);
      // Catalog failure must not disable navigation.
      await page.route('**/py/local/regions/130300/scenes*', route => route.fulfill({ status: 503, json: { detail: 'fixture catalog failure' } }));
      await select.selectOption('130300'); await settle(); await drag(-600, -600);
      assert.equal((await state()).id, '130300'); assert.ok((await state()).valid);
    }
    await page.screenshot({ path: `${output}/${width}-city.png`, fullPage: true });
    assert.deepEqual(await page.evaluate(() => window.invalidNavigationFrames), [], 'drag and inertia frames stay within constraints');
    assert.deepEqual(errors, []);
    await page.close();
  }
  await fs.writeFile(`${output}/report.json`, JSON.stringify(report, null, 2));
  console.log(JSON.stringify(report));
} finally { await browser.close(); }
