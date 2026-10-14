// Deterministic mouse/keyboard acceptance with intercepted imagery and detection APIs.
import assert from 'node:assert/strict';
import fs from 'node:fs/promises';
import { createRequire } from 'node:module';
const require = createRequire(import.meta.url);
const { chromium } = require(process.env.PLAYWRIGHT_MODULE || 'playwright');
const browser = await chromium.launch({ channel: 'chrome', headless: true });
const region = { type: 'Polygon', coordinates: [[[114.3, 38.1], [114.4, 38.1], [114.4, 38.2],
  [114.375, 38.2], [114.375, 38.125], [114.325, 38.125], [114.325, 38.2], [114.3, 38.2], [114.3, 38.1]]] };
const outer = { type: 'Polygon', coordinates: [[[114.29, 38.09], [114.41, 38.09], [114.41, 38.21], [114.29, 38.21], [114.29, 38.09]]] };
const feature = { type: 'Feature', id: 'fixture', geometry: region, properties: { region_id: 'fixture', name: '测试行政区', label: [114.31, 38.11], kind: 'city' } };
const phases = ['01', '03'].map(month => ({ id: `fixture:2025-${month}`, region_id: 'fixture', month: `2025-${month}`, version: '1',
  available: true, bounds: [114.3, 38.1, 114.4, 38.2], scenes: [{ id: `scene-${month}`, region_id: 'fixture', region: '测试行政区',
    date: `2025-${month}-01`, date_precision: 'month', file: 'fixture.tif', filename: 'fixture.tif', available: true, bounds: [114.3, 38.1, 114.4, 38.2] }] }));
try {
  const page = await browser.newPage({ viewport: { width: 1440, height: 1000 } });
  page.setDefaultTimeout(15000);
  const errors = [], submissions = [], previews = [];
  let pendingSubmission;
  page.on('pageerror', error => errors.push(error.message));
  await page.route('**/api/settings', route => route.fulfill({ json: { revision: 1, effective: true, settings: {
    sources: [], default_model: 'fixture', detection: { threshold: .5, resolution: .8, kernel_size: 1, min_area_pixels: 1 },
    browser: { concurrency: 2, inactive_layers: 2, cache_mb: 64, idle_seconds: 300 } } } }));
  await page.route('**/api/models', route => route.fulfill({ json: [{ id: 'fixture', name: 'Fixture', status: 'ready', enabled: true, sha256: 'fixture', mapping: {} }] }));
  await page.route('**/py/local/catalog*', route => route.fulfill({ json: { images: [], boundary: outer, bounds: [114.29, 38.09, 114.41, 38.21] } }));
  await page.route('**/py/local/regions', route => route.fulfill({ json: { type: 'FeatureCollection', features: [feature], boundary: outer, bounds: [114.29, 38.09, 114.41, 38.21] } }));
  await page.route('**/py/local/regions/fixture/scenes*', route => route.fulfill({ json: { region_id: 'fixture', name: '测试行政区', scenes: phases.flatMap(p => p.scenes), phases } }));
  await page.route('**/mosaic/**', route => route.fulfill({ contentType: 'image/png', body: Buffer.from('iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR4nGNgYGBgAAAABQABpfZFQAAAAABJRU5ErkJggg==', 'base64') }));
  await page.route('**/py/local/selection-pixels', route => {
    previews.push(route.request().postDataJSON());
    return route.fulfill({ json: { width: 100, height: 100, pixels: 10000 } });
  });
  await page.route('**/api/images/local', async route => {
    submissions.push(route.request().postDataJSON());
    if (submissions.length === 2) { pendingSubmission = route; return; }
    await route.fulfill({ json: { items: [], count: 0, batch_id: 'fixture' } });
  });
  await page.goto(process.env.FRONTEND_URL || 'http://localhost:5173');
  const root = page.locator('[data-map-workspace]');
  const toolbar = page.getByRole('toolbar', { name: '地图工具' });
  const submit = page.locator('.submit-detection');
  await page.getByRole('combobox', { name: '选择行政区' }).selectOption('fixture');
  await page.getByRole('button', { name: '加入 2025-01 时相' }).click();
  await page.getByRole('button', { name: '加入 2025-03 时相' }).click();
  await page.waitForTimeout(900); // Wait for the existing city navigation animation.
  await root.evaluate(node => {
    const s = node.__vueParentComponent.setupState;
    s.map.fitBounds([[38.09, 114.29], [38.21, 114.41]], { animate: false, padding: [120, 120] });
  });
  const screen = position => root.evaluate((node, [lng, lat]) => {
    const map = node.__vueParentComponent.setupState.map;
    const p = map.latLngToContainerPoint([lat, lng]), rect = map.getContainer().getBoundingClientRect();
    return { x: rect.left + p.x, y: rect.top + p.y };
  }, position);
  const click = async position => { const p = await screen(position); await page.mouse.click(p.x, p.y); };
  const move = async position => { const p = await screen(position); await page.mouse.move(p.x, p.y); };
  const geometry = () => root.evaluate(node => {
    const polygon = node.__vueParentComponent.props.selectionGeometry;
    return polygon ? JSON.parse(JSON.stringify(polygon)) : null;
  });
  const start = () => toolbar.getByRole('button', { name: '多边形区域', exact: true }).click();

  await start();
  assert.ok(await submit.isDisabled());
  await click([114.35, 38.16]); // First vertex is in the concavity, so reject it.
  await page.getByRole('status').filter({ hasText: '第一个顶点' }).waitFor();
  assert.equal(await root.locator('.polygon-vertex').count(), 0);
  await click([114.31, 38.11]);
  await click([114.39, 38.11]);
  await page.keyboard.press('Backspace');
  assert.equal(await root.locator('.polygon-vertex').count(), 1);
  await click([114.39, 38.11]);
  await click([114.39, 38.18]);
  await click([114.31, 38.18]); // Crosses outside before returning to the left arm.
  await root.evaluate(node => {
    const state = node.__vueParentComponent.setupState;
    const points = state.polygonPoints;
    const last = points[points.length - 1];
    if (Math.abs(last[0] - 114.375) > 1e-9) throw new Error(`first-exit snapping failed: ${last}`);
  });
  await toolbar.getByRole('button', { name: '完成绘制', exact: true }).click();
  assert.equal(await geometry(), null);
  await page.getByRole('status').filter({ hasText: '越出当前行政区' }).waitFor();
  await page.keyboard.press('Escape');
  assert.equal(await root.locator('.polygon-vertex').count(), 0);

  await start();
  await click([114.305, 38.105]);
  await click([114.395, 38.105]);
  await click([114.31, 38.12]);
  await page.keyboard.press('Enter');
  const original = await geometry();
  assert.equal(original.type, 'Polygon');
  assert.equal(original.coordinates[0].length, 4);
  assert.deepEqual(original.coordinates[0][0], original.coordinates[0].at(-1));
  const draft = () => root.evaluate(node => JSON.parse(JSON.stringify(node.__vueParentComponent.setupState.polygonPoints)));
  const dragVertex = async target => {
    const marker = await root.locator('.polygon-vertex').first().boundingBox();
    await page.mouse.move(marker.x + marker.width / 2, marker.y + marker.height / 2);
    await page.mouse.down(); await move(target);
  };
  assert.equal(await toolbar.getByRole('button', { name: '编辑顶点', exact: true }).count(), 0);
  assert.equal(await toolbar.getByRole('button', { name: '编辑边', exact: true }).count(), 0);
  assert.equal(await toolbar.getByRole('button', { name: '完成编辑', exact: true }).count(), 0);
  assert.equal(await root.locator('.polygon-vertex').count(), 3);
  assert.equal(await root.locator('.polygon-edge-hit').count(), 3);
  assert.equal(await root.locator('.polygon-midpoint').count(), 3);
  assert.ok(!(await submit.isDisabled()), 'idle selection must remain submittable');
  // Drag the actual edge away from its midpoint; its endpoints follow neighbours.
  const triangleStart = await screen([114.345, 38.105]);
  const centerBeforeEdge = await root.evaluate(node => node.__vueParentComponent.setupState.map.getCenter());
  await page.mouse.move(triangleStart.x, triangleStart.y); await page.mouse.down();
  assert.ok(await submit.isDisabled(), 'dragging must lock submission');
  await move([114.35, 38.108]);
  assert.deepEqual(await geometry(), original, 'in-progress drag must not submit intermediate geometry');
  await page.mouse.up();
  const triangleMoved = await draft();
  assert.deepEqual(await geometry(), { type: 'Polygon', coordinates: [[...triangleMoved, triangleMoved[0]]] });
  assert.ok(Math.abs(triangleMoved[0][1] - 38.108) < .0003);
  assert.ok(triangleMoved[0][0] > original.coordinates[0][0][0]);
  assert.ok(triangleMoved[1][0] < original.coordinates[0][1][0]);
  assert.deepEqual(triangleMoved[2], original.coordinates[0][2]);
  assert.deepEqual(await root.evaluate(node => node.__vueParentComponent.setupState.map.getCenter()), centerBeforeEdge);
  await root.evaluate((node, original) => {
    const s = node.__vueParentComponent.setupState;
    const p = s.polygonPoints.map(([lng, lat]) => s.map.project([lat, lng]));
    const q = original.coordinates[0].slice(0, -1).map(([lng, lat]) => s.map.project([lat, lng]));
    const cross = (a, b, c) => (b.x-a.x)*(c.y-a.y)-(b.y-a.y)*(c.x-a.x);
    if (Math.abs(cross(q[2], q[0], p[0])) > .001 || Math.abs(cross(q[2], q[1], p[1])) > .001) throw new Error('edge endpoints left neighbouring supporting lines');
  }, original);
  const afterEdge = await geometry();
  assert.ok(!(await submit.isDisabled()));
  // Rebuilt edge targets must release listeners even if another owner retains the node.
  await root.evaluate(node => {
    const oldEdge = node.querySelector('.polygon-edge-hit');
    const state = node.__vueParentComponent.setupState;
    state.renderPolygonDraft();
    oldEdge.dispatchEvent(new PointerEvent('pointerdown', { button: 0, pointerId: 7, bubbles: true, cancelable: true }));
    if (state.edgeDrag) throw new Error('detached edge target still starts a drag');
  });
  // KeepAlive pages must not consume keyboard shortcuts while their map is detached.
  const cachedMap = await root.elementHandle();
  await page.locator('.app-nav .nav-item[href="/reports"]').click();
  await page.locator('.reports-page').waitFor();
  assert.equal(await page.evaluate(() => {
    const event = new KeyboardEvent('keydown', { key: 'Escape', bubbles: true, cancelable: true });
    window.dispatchEvent(event);
    return event.defaultPrevented;
  }), false, 'cached overview must not consume Escape on the reports page');
  assert.deepEqual(await cachedMap.evaluate(node => JSON.parse(JSON.stringify(node.__vueParentComponent.props.selectionGeometry))), afterEdge);
  await page.locator('.app-nav .nav-item[href="/overview"]').click();
  await root.waitFor();
  await root.evaluate(node => { node.__vueParentComponent.setupState.selectionMessage = 'keyboard reactivation fixture'; });
  await page.keyboard.press('Escape');
  assert.equal(await root.evaluate(node => node.__vueParentComponent.setupState.selectionMessage), '', 'map shortcuts resume after activation');
  // Invalid edge movement preserves the last saved position.
  const triangleEdge = await screen([114.34, triangleMoved[0][1]]);
  await page.mouse.move(triangleEdge.x, triangleEdge.y); await page.mouse.down();
  await move([114.34, 38.09]); await page.mouse.up();
  assert.deepEqual(await geometry(), afterEdge);
  // Esc cancels only the current gesture; both kinds of handle remain present.
  await page.mouse.move(triangleEdge.x, triangleEdge.y); await page.mouse.down();
  await move([114.34, 38.109]); await page.keyboard.press('Escape'); await page.mouse.up();
  assert.deepEqual(await geometry(), afterEdge);
  assert.deepEqual(await draft(), afterEdge.coordinates[0].slice(0, -1));
  await root.locator('.polygon-midpoint').first().click();
  assert.equal(await root.locator('.polygon-vertex').count(), 4);
  assert.equal((await geometry()).coordinates[0].length, 5);
  await page.keyboard.press('Delete');
  assert.equal(await root.locator('.polygon-vertex').count(), 3);
  assert.deepEqual(await geometry(), afterEdge);
  // Vertex handles remain usable with edge hit targets present.
  await dragVertex([114.301, 38.101]);
  assert.ok(await submit.isDisabled());
  await page.keyboard.press('Escape'); await page.mouse.up();
  assert.deepEqual(await geometry(), afterEdge);
  assert.deepEqual(await draft(), afterEdge.coordinates[0].slice(0, -1));
  await dragVertex([114.301, 38.101]); await page.mouse.up();
  const edited = await geometry();
  assert.notDeepEqual(edited, original);
  assert.ok(!(await submit.isDisabled()));
  await page.getByRole('status', { name: '框选区域像元尺寸' }).filter({ hasText: '矩形裁切网格' }).waitFor();
  assert.deepEqual(previews.at(-1).aoi, edited);
  await submit.click();
  await page.waitForFunction(() => document.body.textContent.includes('该区域没有本地影像覆盖')).catch(async error => {
    console.log(JSON.stringify({ submissions, errors, text: await page.locator('body').innerText() }, null, 2));
    throw error;
  });
  assert.equal(submissions.length, 1);
  assert.deepEqual(submissions[0].aoi, edited);
  assert.equal(submissions[0].region_id, 'fixture');
  assert.deepEqual(submissions[0].scene_ids, ['scene-01', 'scene-03']);

  // Cancelling a new drawing restores the previously committed polygon.
  await start(); await click([114.31, 38.11]); await page.keyboard.press('Escape');
  assert.deepEqual(await geometry(), edited);
  // Rectangle drawing replaces polygon geometry and still works with pointer capture.
  await toolbar.getByRole('button', { name: '框选区域', exact: true }).click();
  const first = await screen([114.305, 38.102]), last = await screen([114.315, 38.12]);
  await page.mouse.move(first.x, first.y); await page.mouse.down(); await page.mouse.move(last.x, last.y, { steps: 5 }); await page.mouse.up();
  assert.equal(await geometry(), null);
  // Rectangles expose vertices, edges and midpoint insertion at the same time.
  assert.equal(await root.locator('.polygon-vertex').count(), 4);
  assert.equal(await root.locator('.polygon-midpoint').count(), 4);
  assert.equal(await root.locator('.polygon-edge-hit').count(), 4);
  await root.evaluate(node => node.__vueParentComponent.setupState.map.setView([38.111, 114.31], 15, { animate: false }));
  const rectangleDraft = await draft();
  const edgeStart = await screen([114.3075, rectangleDraft[0][1]]);
  await page.mouse.move(edgeStart.x, edgeStart.y); await page.mouse.down();
  await move([114.31, 38.104]); await page.mouse.up();
  const movedRectangle = await draft();
  assert.ok(Math.abs(movedRectangle[0][1] - 38.104) < .0003);
  assert.ok(Math.abs(movedRectangle[0][0] - rectangleDraft[0][0]) < 1e-9);
  assert.ok(Math.abs(movedRectangle[1][0] - rectangleDraft[1][0]) < 1e-9);
  assert.deepEqual(movedRectangle.slice(2), rectangleDraft.slice(2));
  assert.equal((await geometry()).coordinates[0].length, 5);
  await dragVertex([114.303, 38.103]); await page.mouse.up();
  assert.notDeepEqual(await draft(), movedRectangle, 'rectangular selection allows immediate vertex editing too');
  // Panning and zooming preserve the saved geometry without any editor mode.
  const beforeNavigation = await geometry();
  await root.evaluate(node => {
    const s = node.__vueParentComponent.setupState;
    if (!s.map.dragging.enabled() || !s.map.scrollWheelZoom.enabled() || !s.map.doubleClickZoom.enabled()) throw new Error('selection locks navigation');
    s.map.setZoom(15, { animate: false });
  });
  const center = await root.evaluate(node => node.__vueParentComponent.setupState.map.getCenter());
  const background = await screen([center.lng, center.lat]);
  await page.mouse.move(background.x, background.y); await page.mouse.down();
  await page.mouse.move(background.x + 35, background.y + 25, { steps: 8 }); await page.mouse.up();
  await page.waitForTimeout(600);
  const afterPan = await root.evaluate(node => node.__vueParentComponent.setupState.map.getCenter());
  assert.notDeepEqual(afterPan, center, 'blank-map drag must pan');
  const oldZoom = await root.evaluate(node => node.__vueParentComponent.setupState.map.getZoom());
  await page.mouse.wheel(0, -120); await page.waitForTimeout(500);
  assert.ok(await root.evaluate((node, zoom) => node.__vueParentComponent.setupState.map.getZoom() > zoom, oldZoom));
  assert.deepEqual(await geometry(), beforeNavigation);
  await root.evaluate(node => {
    const s = node.__vueParentComponent.setupState;
    s.map.setZoom(s.cityExitZoom, { animate: false });
    if (!s.activeRegion) throw new Error('zoom-out cleared the selection');
  });
  assert.deepEqual(await geometry(), beforeNavigation);
  assert.ok(!(await submit.isDisabled()));
  await root.evaluate(node => node.__vueParentComponent.setupState.map.fitBounds([[38.09, 114.29], [38.21, 114.41]], { animate: false, padding: [120, 120] }));
  // A legacy rectangle extending outside the region edits its actual crop footprint.
  await toolbar.getByRole('button', { name: '框选区域', exact: true }).click();
  const outsideBoxStart = await screen([114.295, 38.102]), outsideBoxEnd = await screen([114.315, 38.12]);
  await page.mouse.move(outsideBoxStart.x, outsideBoxStart.y); await page.mouse.down();
  await page.mouse.move(outsideBoxEnd.x, outsideBoxEnd.y, { steps: 5 }); await page.mouse.up();
  assert.ok((await draft()).every(p => p[0] >= 114.3));
  await page.getByRole('status').filter({ hasText: '按行政区边界裁切后的矩形' }).waitFor();
  assert.equal(await geometry(), null, 'cancelling restores the original box');
  await start(); await click([114.305, 38.105]); await click([114.315, 38.105]); await click([114.31, 38.115]);
  // Closing by clicking the first vertex.
  await root.locator('.polygon-vertex').first().click();
  assert.ok(await geometry());
  await toolbar.getByRole('button', { name: '清除选区', exact: true }).click();
  assert.equal(await geometry(), null);
  assert.ok(await submit.isDisabled());
  await start(); await click([114.305, 38.105]); await click([114.315, 38.105]);
  const end = await screen([114.31, 38.115]); await page.mouse.dblclick(end.x, end.y);
  if (!(await geometry())) console.log(await root.evaluate(node => {
    const s = node.__vueParentComponent.setupState;
    return { points: s.polygonPoints, mode: s.polygonMode, message: s.selectionMessage };
  }));
  assert.equal((await geometry()).coordinates[0].length, 4);
  // Manual bounds explicitly switch back to a rectangle.
  await page.getByRole('button', { name: '应用该区域框' }).click();
  assert.equal(await geometry(), null);
  await start(); await click([114.305, 38.105]); await click([114.315, 38.105]); await click([114.31, 38.115]);
  await page.keyboard.press('Enter');
  await root.locator('.polygon-vertex').first().click();
  await page.keyboard.press('Delete');
  assert.equal(await root.locator('.polygon-vertex').count(), 3, 'deleting below three vertices must fail');
  const handle = await root.locator('.polygon-vertex').first().boundingBox();
  await page.mouse.move(handle.x + handle.width / 2, handle.y + handle.height / 2);
  await page.mouse.down(); await move([114.28, 38.105]); await page.mouse.up();
  const snapped = await geometry();
  assert.ok(Math.abs(snapped.coordinates[0][0][0] - 114.3) < 1e-9, 'edited vertex must snap to boundary');
  await submit.click();
  await toolbar.getByRole('button', { name: '多边形区域', exact: true }).waitFor({ state: 'visible' });
  assert.ok(await toolbar.getByRole('button', { name: '多边形区域', exact: true }).isDisabled());
  assert.equal(await root.locator('.polygon-vertex').count(), 0);
  assert.equal(await root.locator('.polygon-edge-hit').count(), 0);
  assert.equal(await root.locator('.polygon-midpoint').count(), 0);
  assert.ok(await toolbar.getByRole('button', { name: '清除选区', exact: true }).isDisabled());
  assert.ok(await submit.isDisabled());
  assert.deepEqual(await geometry(), snapped);
  assert.deepEqual(submissions[1].aoi, snapped);
  await pendingSubmission.fulfill({ json: { items: [], count: 0, batch_id: 'fixture' } });
  await page.waitForFunction(() => document.body.textContent.includes('该区域没有本地影像覆盖'));
  await page.getByRole('status', { name: '框选区域像元尺寸' }).filter({ hasText: '矩形裁切网格' }).waitFor();
  // Leaving the city clears both geometry and bounds.
  await page.getByRole('button', { name: '河北全省', exact: true }).click();
  assert.equal(await geometry(), null);
  assert.ok(await submit.isDisabled());
  await page.getByRole('combobox', { name: '选择行政区' }).selectOption('fixture');
  await page.waitForTimeout(900);
  await start(); await click([114.305, 38.105]); await click([114.315, 38.105]); await click([114.31, 38.115]);
  await page.keyboard.press('Enter');
  await root.evaluate(node => {
    const s = node.__vueParentComponent.setupState;
    const aoi = JSON.parse(JSON.stringify(node.__vueParentComponent.props.selectionGeometry));
    s.addResultOverlays({ aoi, resultBounds: [38.105, 114.305, 38.115, 114.315] });
    const ring = s.resultRect.getLatLngs()[0][0].map(p => [p.lng, p.lat]);
    if (ring.length !== 3) throw new Error('polygon results show a rectangular outline');
    const expected = aoi.coordinates[0].slice(0, -1);
    if (JSON.stringify(ring) !== JSON.stringify(expected)) throw new Error('result outline differs from frozen AOI');
  });
  await fs.mkdir('data/validation/polygon/browser', { recursive: true });
  await page.screenshot({ path: 'data/validation/polygon/browser/polygon.png' });
  assert.deepEqual(errors, []);
  console.log('PASS: vertex/edge editing, detached listener disposal, KeepAlive shortcuts, gesture cancellation, rectangle editing, navigation, frozen AOI submission and busy locks');
} finally { await browser.close(); }
