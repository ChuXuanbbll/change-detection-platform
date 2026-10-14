// Real Leaflet and Chromium; deterministic tiles, no model tasks or source mutations.
import assert from 'node:assert/strict';
import fs from 'node:fs/promises';
import path from 'node:path';
import { createRequire } from 'node:module';
const require = createRequire(import.meta.url);
const { chromium } = require(process.env.PLAYWRIGHT_MODULE || 'playwright');
const url = process.env.FRONTEND_URL || 'http://localhost:5173';
const output = path.resolve(process.env.BROWSER_OUTPUT_DIR || 'data/validation/phase-cache');
await fs.mkdir(output, { recursive: true });
const browser = await chromium.launch({ channel: 'chrome', headless: true });
const page = await browser.newPage({ viewport: { width: 1440, height: 1000 } });
const errors = [], requests = [];
page.on('pageerror', error => errors.push(error.message));
let fail = false, stale = false, delay = 0;
const sleep = ms => new Promise(resolve => setTimeout(resolve, ms));
const stats = () => page.evaluate(() => window.h.cache.stats());
const settled = () => page.waitForFunction(() => {
  if (!window.h.cache.scheduler.ready) return false;
  const images = [...document.querySelectorAll('.leaflet-tile')];
  return images.length && images.every(tile => tile.complete && tile.naturalWidth === 256);
});
const activate = index => page.evaluate(index => window.h.cache.activate(window.h.phases[index]), index);
try {
  await page.route('**/__phase-cache-test', route => route.fulfill({ contentType: 'text/html', body:
    '<link rel="stylesheet" href="/node_modules/leaflet/dist/leaflet.css"><div id="map" style="width:1000px;height:750px"></div>' }));
  await page.goto(`${url}/__phase-cache-test`);
  const png = await page.evaluate(() => {
    const canvas = document.createElement('canvas'); canvas.width = canvas.height = 256;
    const ctx = canvas.getContext('2d'); ctx.fillStyle = '#587745'; ctx.fillRect(0, 0, 256, 256);
    return canvas.toDataURL().split(',')[1];
  });
  await page.route('**/mosaic/**', async route => {
    requests.push(route.request().url());
    const status = stale ? 409 : fail ? 503 : 200;
    const wait = delay;
    if (wait) await sleep(wait);
    await route.fulfill({ status, contentType: 'image/png', body: Buffer.from(png, 'base64') }).catch(() => {});
  });
  const source = await (await page.request.get(`${url}/src/utils/phaseLayerCache.ts`)).text();
  const leafletUrl = source.match(/from\s+["']([^"']*leaflet[^"']*)["']/)?.[1];
  assert.ok(leafletUrl);
  await page.evaluate(async leafletUrl => {
    const { default: L } = await import(leafletUrl);
    const { PhaseLayerCache } = await import('/src/utils/phaseLayerCache.ts');
    const map = L.map('map', { zoomAnimation: false, fadeAnimation: false }).setView([38, 115], 8);
    const phases = Array.from({ length: 6 }, (_, i) => ({ id: `130100:2025-0${i + 1}`, region_id: '130100', month: `2025-0${i + 1}`,
      version: `version-${i}`, available: true, bounds: null,
      scenes: [{ id: `scene-${i}`, available: true }] }));
    const counters = { created: 0, revoked: 0 };
    const create = URL.createObjectURL.bind(URL), revoke = URL.revokeObjectURL.bind(URL);
    URL.createObjectURL = blob => { counters.created++; return create(blob); };
    URL.revokeObjectURL = value => { counters.revoked++; revoke(value); };
    const limits = { inactiveLayers: 3, inactiveBytes: 128 * 1024 * 1024, idleMs: 300000 };
    const h = window.h = { map, phases, limits, now: 0, events: [], counters };
    h.baseEvents = Object.values(map._events).reduce((sum, handlers) => sum + handlers.length, 0);
    h.cache = new PhaseLayerCache(map, state => h.events.push(state), limits, () => h.now);
  }, leafletUrl);
  const coldStart = Date.now();
  await activate(0); await settled();
  const fixtureColdMs = Date.now() - coldStart;
  await activate(1); await settled();
  const beforeRequests = requests.length, beforeStats = await stats();
  const switches = await page.evaluate(async () => {
    const h = window.h, nodes = [...document.querySelectorAll('.leaflet-tile')], timings = [];
    const created = h.counters.created;
    for (let i = 0; i < 100; i++) {
      const start = performance.now(); h.cache.activate(h.phases[i % 2]);
      await new Promise(resolve => requestAnimationFrame(resolve));
      timings.push(performance.now() - start);
    }
    return { maxMs: Math.max(...timings), p95Ms: [...timings].sort((a, b) => a - b)[94],
      sameNodes: nodes.every(node => node.isConnected), newDecodes: h.counters.created - created };
  });
  const warmExtraRequests = requests.length - beforeRequests;
  assert.equal(warmExtraRequests, 0, '100 warm switches make no requests');
  assert.equal(switches.sameNodes, true, 'decoded DOM images retained');
  assert.equal(switches.newDecodes, 0);
  assert.ok(switches.maxMs <= 100, `warm switch ${switches.maxMs} ms`);
  assert.deepEqual(await stats(), beforeStats);
  const beforePan = requests.length;
  await page.evaluate(() => window.h.map.panBy([400, 0], { animate: false })); await settled();
  assert.ok(requests.length > beforePan);
  assert.ok(requests.slice(beforePan).every(value => value.includes('version-1')), 'hidden layer never requests on pan');
  await page.evaluate(() => window.h.cache.suspend());
  const beforeSuspend = requests.length;
  await page.evaluate(() => window.h.map.setZoom(9)); await sleep(350);
  assert.equal(requests.length, beforeSuspend, 'suspended route makes no requests');
  await activate(2); await sleep(100);
  assert.equal(requests.length, beforeSuspend, 'new phase selected while route hidden stays unloaded');
  await page.evaluate(() => window.h.cache.resume()); await settled();
  await activate(1); await settled();
  await page.evaluate(() => { window.h.cache.activate(); });
  const closedRequests = requests.length;
  await activate(1); await settled();
  assert.equal(requests.length, closedRequests, 'close and reopen reuses cache');
  for (let i = 2; i < 6; i++) { await activate(i); await settled(); }
  assert.equal((await stats()).layers, 4);
  await page.evaluate(() => { window.h.limits.inactiveBytes = 1; window.h.cache.trim(); });
  assert.equal((await stats()).layers, 1, 'byte budget evicts inactive layers');
  await page.evaluate(() => { window.h.limits.inactiveBytes = 128 * 1024 * 1024; });
  await activate(0); await settled();
  await page.evaluate(() => { window.h.now += 300001; window.h.cache.trim(); });
  assert.equal((await stats()).layers, 1, 'idle expiry');
  await page.evaluate(() => { window.h.phases[0].version = 'new-version'; window.h.cache.reconcile(window.h.phases); });
  assert.equal((await stats()).layers, 0, 'version invalidates old decoded tiles');
  fail = true; await activate(1);
  await page.waitForFunction(() => window.h.events.some(state => state.error));
  fail = false; await page.evaluate(() => window.h.cache.activate()); await activate(1); await settled();
  stale = true; await activate(2);
  await page.waitForFunction(() => window.h.events.some(state => state.stale));
  await sleep(150);
  assert.equal(await page.evaluate(() => window.h.events.filter(state => state.stale).length), 1, 'one refresh per stale revision');
  stale = false;
  await page.evaluate(() => window.h.cache.clear());
  delay = 400; await activate(0); await activate(1); await settled(); delay = 0;
  assert.ok((await stats()).activeKey.includes('2025-02'), 'late old responses do not activate old phase');
  // A pending request must not retain the destroyed cache or create new blob URLs.
  delay = 400; await activate(4);
  await page.evaluate(() => window.h.cache.clear());
  await sleep(600); delay = 0;
  assert.equal((await stats()).tiles, 0);
  assert.equal(await page.evaluate(() => window.h.counters.created - window.h.counters.revoked), 0);
  const cdp = await page.context().newCDPSession(page);
  async function snapshot(name) {
    await cdp.send('HeapProfiler.collectGarbage');
    const chunks = [], listener = ({ chunk }) => chunks.push(chunk);
    cdp.on('HeapProfiler.addHeapSnapshotChunk', listener);
    await cdp.send('HeapProfiler.takeHeapSnapshot'); cdp.off('HeapProfiler.addHeapSnapshotChunk', listener);
    const body = chunks.join(''); await fs.writeFile(path.join(output, name), body);
    const data = JSON.parse(body), fields = data.snapshot.meta.node_fields;
    const nameIndex = fields.indexOf('name'), typeIndex = fields.indexOf('type');
    const objectType = data.snapshot.meta.node_types[typeIndex].indexOf('object');
    let layers = 0;
    for (let i = 0; i < data.nodes.length; i += fields.length) {
      if (data.nodes[i + typeIndex] === objectType && data.strings[data.nodes[i + nameIndex]] === 'RetainedTileLayer') layers++;
    }
    return layers;
  }
  await activate(0); await settled();
  const retainedBefore = await snapshot('before-dispose.heapsnapshot');
  assert.ok(retainedBefore > 0);
  const disposed = await page.evaluate(() => {
    const h = window.h; h.cache.dispose(); h.cache.dispose();
    return { stats: h.cache.stats(), liveUrls: h.counters.created - h.counters.revoked,
      extraEvents: Object.values(h.map._events).reduce((sum, handlers) => sum + handlers.length, 0) - h.baseEvents };
  });
  await sleep(500);
  const retainedAfter = await snapshot('after-dispose.heapsnapshot');
  assert.equal(disposed.stats.layers, 0); assert.equal(disposed.liveUrls, 0); assert.equal(disposed.extraEvents, 0);
  assert.equal(retainedAfter, 0, 'heap no longer retains destroyed tile layers');
  assert.deepEqual(errors, []);
  const report = { passed: true, fixtureColdMs, switches, warmExtraRequests,
    retainedBefore, retainedAfter, disposed, errors };
  await fs.writeFile(path.join(output, 'report.json'), JSON.stringify(report, null, 2));
  console.log(JSON.stringify(report, null, 2));
} finally { await browser.close(); }
