import assert from 'node:assert/strict';

// Isolated HTTP fixtures exercise the actual Vue workspace and button handler.
export async function runAiDetectionChecks({ browser, url, fixtureForPage, report, region, bounds, polygon }) {
  const page = await browser.newPage({ viewport: { width: 1440, height: 900 } });
  page.setDefaultTimeout(15000);
  const phases = ['2025-03', '2025-05', '2025-07', '2025-09'].map((month, i) => ({
    id: `detect-phase-${i}`, region_id: region.id, month, available: true, version: 'v1', bounds,
    scenes: [{ id: `detect-scene-${i}`, region_id: region.id, region: region.properties.name,
      date: month, date_precision: 'month', available: true, bounds, version: 'v1' }],
  }));
  const acks = [], preparations = [], submissions = [], bodies = [];
  let finishTask = false, nextCommand;
  await fixtureForPage(page);
  await page.route('**/py/local/regions/*/scenes*', route => route.fulfill({ json: {
    region_id: region.id, name: region.properties.name, phases, scenes: phases.flatMap(phase => phase.scenes),
  } }));
  await page.route('**/api/ai/sessions/*/actions/*', route => {
    acks.push({ id: route.request().url().split('/').pop(), ...route.request().postDataJSON() });
    return route.fulfill({ json: {} });
  });
  await page.route('**/api/ai/sessions/*/messages', route => {
    const body = route.request().postDataJSON(); bodies.push(body);
    const command = nextCommand(body);
    const events = [ ['ui_command', command], ['final', { status: 'succeeded', message: '夹具命令已发送' }] ];
    return route.fulfill({ contentType: 'text/event-stream', body: events.map(([type, data]) => `event: ${type}\ndata: ${JSON.stringify(data)}\n\n`).join('') });
  });
  await page.route('**/api/images/local', route => {
    if (route.request().method() !== 'POST') return route.fulfill({ json: { items: [], count: 0 } });
    const body = route.request().postDataJSON(); preparations.push(body);
    const items = phases.filter(phase => phase.scenes.some(scene => body.scene_ids.includes(scene.id))).map(phase => ({
      id: `image-${phase.month}`, source: 'local', phase: phase.month, capture_date: `${phase.month}-01`, file_path: "fixture.tif",
      bounds_west: bounds[0], bounds_south: bounds[1], bounds_east: bounds[2], bounds_north: bounds[3],
      meta_json: JSON.stringify({ aoi: body.aoi, region_id: body.region_id }),
    }));
    return route.fulfill({ json: { items, count: items.length, batch_id: 'detect-batch' } });
  });
  await page.route('**/api/tasks', route => {
    submissions.push(route.request().postDataJSON());
    return route.fulfill({ json: { id: `detect-task-${submissions.length}`, status: 'pending' } });
  });
  await page.route('**/api/tasks/*', route => route.fulfill({ json: {
    id: route.request().url().split('/').pop(), status: finishTask ? 'succeeded' : 'running', progress: finishTask ? 100 : 10,
  } }));
  await page.route('**/api/tasks/*/result', route => route.fulfill({ json: { stats_json: '{"changed_pixels":0,"change_ratio":0}' } }));
  const send = async (action, args = {}, options = {}) => {
    const index = acks.length;
    const previous = acks.at(-1);
    nextCommand = body => ({ action_id: `detection-action-${index}`, action, args,
      expected_revision: options.fresh ? body.workspace.revision : previous?.context_revision || body.workspace.revision });
    const input = page.getByRole('textbox', { name: '输入给 AI 助手的消息' });
    await input.fill(`夹具${index}`); await input.press('Enter');
    await page.waitForFunction(count => document.querySelectorAll('.ai-message.is-final').length >= count, index + 1);
    await assertEventually(() => acks.length > index, page);
    return acks[index];
  };
  try {
    await page.goto(url);
    await page.getByRole('button', { name: '打开 AI 助手' }).click();
    assert.equal((await send('select_region', { region_id: region.id })).status, 'applied');
    assert.equal((await send('load_phases', { phase_ids: phases.map(phase => phase.id) })).status, 'applied');
    assert.equal((await send('set_existing_aoi', { aoi: polygon, bounds, source: 'workspace' })).status, 'applied');
    const lastRevision = acks.at(-1).context_revision;
    assert.equal((await send('select_region', { region_id: region.id })).status, 'applied');
    assert.equal(acks.at(-1).context_revision, lastRevision, 'no-op action must preserve revision');
    // Panel edits participate in the revision and must not be overwritten by AI.
    await page.getByRole('button', { name: '关闭 AI 助手', exact: true }).click();
    await page.locator('label').filter({ hasText: '变化阈值' }).locator('input').fill('0.73');
    await page.getByRole('button', { name: '打开 AI 助手' }).click();
    const stale = await send('submit_detection', { phase_dates: ['2025-03','2025-09'] });
    assert.equal(stale.status, 'rejected'); assert.equal(stale.error.code, 'UI_CONTEXT_STALE');
    assert.equal(preparations.length, 0);
    const submitted = await send('submit_detection', { phase_dates: ['2025-03','2025-09'] });
    assert.equal(submitted.status, 'applied', JSON.stringify(submitted));
    assert.equal(submitted.result.task_id, 'detect-task-1'); assert.equal(submitted.result.pair_count, 1);
    assert.deepEqual(submitted.workspace.phases.map(phase => phase.month), ['2025-03','2025-09']);
    assert.deepEqual(preparations[0].scene_ids, ['detect-scene-0','detect-scene-3']);
    assert.deepEqual(preparations[0].aoi, polygon);
    assert.equal(preparations[0].params.threshold, 0.73);
    assert.equal(preparations[0].model_id, 'fixture-model'); assert.equal(preparations[0].config_revision, 1);
    assert.equal(submissions.length, 1);
    assert.equal((await send('submit_detection')).error.code, 'WORKSPACE_BUSY');
    assert.equal(submissions.length, 1, 'repeated AI command created another task');
    finishTask = true;
    await page.getByRole('button', { name: '关闭 AI 助手', exact: true }).click();
    const button = page.getByRole('button', { name: '提交检测任务', exact: true });
    await button.waitFor(); await assertEventually(() => button.isEnabled(), page);
    await button.click(); await assertEventually(() => submissions.length === 2, page);
    assert.deepEqual(preparations[1].scene_ids, preparations[0].scene_ids);
    assert.deepEqual(preparations[1].params, preparations[0].params, 'button and AI must retain the same edited settings');
    assert.deepEqual(submissions[1].params, submissions[0].params);
    assert.equal(bodies.at(-1).workspace.detection.settings.threshold, 0.73);
    report.checks.push('acknowledged revisions chain without phantom bumps; no-op preserves revision; panel edits reject stale submit');
    report.checks.push('four catalog phases resolve to two explicit dates; shared AI/button submission retains AOI and edited parameters; duplicate task blocked');
  } finally { await page.close(); }
}

async function assertEventually(predicate, page) {
  const deadline = Date.now() + 10000;
  while (Date.now() < deadline) { if (await predicate()) return; await page.waitForTimeout(25); }
  assert.fail('expected fixture condition was not reached');
}
