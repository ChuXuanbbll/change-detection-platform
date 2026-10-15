// Deterministic browser acceptance for the AI assistant UI. No provider or
// detection job is contacted; all API and Python responses are fixture routes.
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import path from "node:path";
import { runAiReliabilityChecks } from "./test_ai_browser_reliability.mjs";
import { runAiDetectionChecks } from "./test_ai_detection_browser.mjs";
import { createRequire } from "node:module";

const require = createRequire(import.meta.url);
const { chromium } = require(process.env.PLAYWRIGHT_MODULE || "playwright");
const url = process.env.FRONTEND_URL || "http://127.0.0.1:5173/overview";
const output = path.resolve(
  process.env.BROWSER_OUTPUT_DIR || "data/validation/ai-browser",
);
await fs.mkdir(output, { recursive: true });

const browser = await chromium.launch({ channel: "chrome", headless: true });
const bounds = [115.97, 38.95, 115.98, 38.96];
const polygon = {
  type: "Polygon",
  coordinates: [
    [
      [bounds[0], bounds[1]],
      [bounds[2], bounds[1]],
      [bounds[2], bounds[3]],
      [bounds[0], bounds[3]],
      [bounds[0], bounds[1]],
    ],
  ],
};
const region = {
  type: "Feature",
  id: "fixture-region",
  geometry: polygon,
  properties: {
    region_id: "fixture-region",
    name: "测试县",
    label: [115.975, 38.955],
    kind: "county",
  },
};
const newRegionBounds = [116.01, 38.95, 116.02, 38.96];
const newRegionPolygon = {
  type: "Polygon",
  coordinates: [
    [
      [newRegionBounds[0], newRegionBounds[1]],
      [newRegionBounds[2], newRegionBounds[1]],
      [newRegionBounds[2], newRegionBounds[3]],
      [newRegionBounds[0], newRegionBounds[3]],
      [newRegionBounds[0], newRegionBounds[1]],
    ],
  ],
};
const newRegion = {
  type: "Feature",
  id: "fixture-new-region",
  geometry: newRegionPolygon,
  properties: {
    region_id: "fixture-new-region",
    name: "新县",
    label: [116.015, 38.955],
    kind: "county",
  },
};
const phases = ["2025-01", "2025-07"].map((month, index) => ({
  id: `phase-${index + 1}`,
  region_id: region.id,
  month,
  scenes: [
    {
      id: `scene-${index + 1}`,
      region_id: region.id,
      region: region.properties.name,
      file: `scene-${index + 1}.tif`,
      filename: `scene-${index + 1}.tif`,
      date: `${month}-01`,
      date_precision: "month",
      bounds,
      resolution_m: 0.8,
      available: true,
      version: "fixture-v1",
    },
  ],
  version: "fixture-v1",
  available: true,
  bounds,
}));
const regions = {
  type: "FeatureCollection",
  features: [region, newRegion],
  bounds,
  boundary: polygon,
};
const scenes = {
  region_id: region.id,
  name: region.properties.name,
  scenes: phases.flatMap((p) => p.scenes),
  phases,
};
const aoi = polygon;
const restoredWorkspace = {
  revision: "workspace-restored",
  page: "/overview",
  region_id: region.id,
  phase_ids: phases.map((phase) => phase.id),
  phases: phases.map((phase) => ({
    id: phase.id,
    month: phase.month,
    scene_ids: phase.scenes.map((scene) => scene.id),
  })),
  active_phase: "2025-07",
  bounds,
  aoi,
};
const actionCommands = [
  { action_id: "action-fly", action: "fly_to_bounds", args: { bounds } },
  {
    action_id: "action-region",
    action: "select_region",
    args: { region_id: region.id },
  },
  {
    action_id: "action-phase",
    action: "load_phase",
    args: { phase_id: phases[0].id },
  },
  {
    action_id: "action-aoi",
    action: "set_existing_aoi",
    args: { bounds, aoi, source: "workspace" },
  },
  {
    action_id: "action-task",
    action: "show_task",
    args: { task_id: "fixture-task" },
  },
  {
    action_id: "action-result",
    action: "show_result",
    args: { result_id: "fixture-task" },
  },
  {
    action_id: "action-stale",
    action: "fly_to_bounds",
    expected_revision: "expired-revision",
    args: { bounds },
  },
  { action_id: "action-unsupported", action: "unsupported_action", args: {} },
];
const acknowledgements = [];
let messageCount = 0;
let configured = false;
let statusCalls = 0;
let statusFailRemaining = 0;
let workflowEvents = [];
let resumeWorkflows = false;
let pendingActionRecovery = false;
let terminalActionRecovery = false;
let restoreWorkspaceSnapshot = false;
let snapshotWorkspace = restoredWorkspace;
let delayedSessionSnapshot = false;
let sessionSnapshotPending = false;
let releaseSessionSnapshot = null;
let ackProbe = false;
let ackProbeSideEffects = 0;
const ackAttempts = new Map();
let activePhaseProbe = false;
let activePhaseMessageCount = 0;
const messageWorkspaces = [];
const pendingRestoreCommand = {
  action_id: "action-pending-restore",
  action: "set_existing_aoi",
  args: { bounds, aoi, source: "workspace" },
};
const terminalCommands = [
  ...["failed", "timeout", "cancelled"].map((status) => ({
    action_id: `action-${status}-terminal`,
    action: "fly_to_bounds",
    args: { bounds },
    status,
  })),
];

function json(route, data, status = 200) {
  return route.fulfill({
    status,
    contentType: "application/json",
    body: JSON.stringify(data),
  });
}

function sse(route, events) {
  const body = events
    .map(
      ({ type, data, id }) =>
        `${id == null ? "" : `id: ${id}\n`}event: ${type}\ndata: ${JSON.stringify(data)}\n\n`,
    )
    .join("");
  return route.fulfill({ status: 200, contentType: "text/event-stream", body });
}

function fixtureForPage(page) {
  return page.route("**/*", async (route) => {
    const request = route.request();
    const parsed = new URL(request.url());
    const pathname = parsed.pathname;
    if (!pathname.startsWith("/api/") && !pathname.startsWith("/py/"))
      return route.continue();

    if (pathname === "/api/ai/status") {
      statusCalls += 1;
      if (statusFailRemaining > 0) {
        statusFailRemaining -= 1;
        return json(route, { error: "fixture status unavailable" }, 503);
      }
      return json(route, { configured });
    }
    if (pathname === "/api/settings")
      return json(route, {
        revision: 1,
        effective: true,
        settings: {
          sources: [],
          default_model: "fixture-model",
          detection: {
            threshold: 0.5,
            resolution: 0.8,
            kernel_size: 3,
            min_area_pixels: 10,
          },
          runtime: {},
          tiles: {},
          browser: {
            concurrency: 2,
            inactive_layers: 2,
            cache_mb: 64,
            idle_seconds: 30,
          },
        },
        history: [],
        diagnostics: {},
        error: "",
      });
    if (pathname === "/api/models")
      return json(route, [
        {
          id: "fixture-model",
          name: "Fixture",
          description: "",
          sha256: "fixture",
          status: "ready",
          enabled: true,
          error: "",
          mapping: {},
          inputs: null,
          outputs: null,
        },
      ]);
    if (pathname === "/api/ai/sessions" && request.method() === "POST")
      return json(route, { id: "session-fixture" });
    if (
      pathname === "/api/ai/sessions/session-fixture/actions/" &&
      request.method() === "POST"
    )
      return json(route, {});
    const ackMatch = pathname.match(
      /^\/api\/ai\/sessions\/session-fixture\/actions\/([^/]+)$/,
    );
    if (ackMatch && request.method() === "POST") {
      const id = ackMatch[1];
      const attempt = (ackAttempts.get(id) || 0) + 1;
      ackAttempts.set(id, attempt);
      const body = request.postDataJSON();
      if (ackProbe && id === "action-ack-retry" && attempt === 1)
        return json(route, { error: "fixture ACK unavailable" }, 503);
      acknowledgements.push({ id, body, attempt });
      return json(route, {});
    }
    if (
      pathname === "/api/ai/sessions/session-fixture/messages" &&
      request.method() === "POST"
    ) {
      const body = request.postDataJSON();
      messageWorkspaces.push(body.workspace);
      let command;
      if (ackProbe) {
        ackProbeSideEffects = 0;
        command = {
          action_id: "action-ack-retry",
          action: "select_region",
          args: { region_id: region.id },
        };
      } else if (activePhaseProbe) {
        if (activePhaseMessageCount++ === 0)
          command = {
            action_id: "action-active-phase-seed",
            action: "unsupported_action",
            args: {},
          };
        else
          command = {
            action_id: "action-active-phase-stale",
            action: "fly_to_bounds",
            expected_revision: messageWorkspaces[0]?.revision,
            args: { bounds },
          };
      } else {
        command =
          actionCommands[Math.min(messageCount++, actionCommands.length - 1)];
      }
      const events = [
        { type: "text_delta", data: { text: "正在解析请求。" } },
        { type: "plan", data: { message: "已生成执行计划" } },
        { type: "tool_started", data: { message: "正在调用 workspace 工具" } },
        {
          type: "tool_finished",
          data: {
            message: "工具调用完成",
            download_ref: "/api/tasks/fixture-task/result/change.geojson",
          },
        },
        {
          type: "tool_finished",
          data: {
            message: "外部引用已忽略",
            download_ref: "https://evil.example/result/change.geojson",
          },
        },
        {
          type: "tool_finished",
          data: {
            message: "路径穿越引用已忽略",
            download_ref:
              "/api/tasks/fixture-task%2f..%2fsecret/result/change.geojson",
          },
        },
        { type: "ui_command", data: command },
        { type: "text_delta", data: { text: "操作已完成" } },
        { type: "final", data: { status: "succeeded" } },
      ];
      workflowEvents = events.map((event, index) => ({
        id: index + 1,
        type: event.type,
        data: event.data,
      }));
      return sse(route, events);
    }
    if (pathname === "/api/ai/sessions/session-fixture") {
      if (delayedSessionSnapshot) {
        sessionSnapshotPending = true;
        await new Promise((resolve) => {
          releaseSessionSnapshot = resolve;
        });
        sessionSnapshotPending = false;
        releaseSessionSnapshot = null;
      }
      return json(route, {
        id: "session-fixture",
        messages: [{ role: "user", content: "恢复会话" }],
        workspace: restoreWorkspaceSnapshot
          ? snapshotWorkspace
          : { revision: "workspace-fixture", page: "/overview", phase_ids: [] },
        workflow_ids: [
          "workflow-fixture",
          ...(resumeWorkflows ? ["workflow-running"] : []),
          ...(pendingActionRecovery ? ["workflow-pending"] : []),
        ],
        actions: Object.fromEntries([
          ...acknowledgements.map((item) => [
            item.id,
            { id: item.id, status: item.body.status },
          ]),
          ...(pendingActionRecovery &&
          !acknowledgements.some(
            (item) => item.id === pendingRestoreCommand.action_id,
          )
            ? [
                [
                  pendingRestoreCommand.action_id,
                  {
                    id: pendingRestoreCommand.action_id,
                    command: pendingRestoreCommand,
                    status: "pending",
                  },
                ],
              ]
            : []),
          ...(terminalActionRecovery
            ? terminalCommands
                .slice(1)
                .map((command) => [
                  command.action_id,
                  { id: command.action_id, command, status: command.status },
                ])
            : []),
        ]),
      });
    }
    if (pathname === "/api/ai/workflows/workflow-fixture")
      return json(route, { id: "workflow-fixture", status: "succeeded" });
    if (pathname === "/api/ai/workflows/workflow-running")
      return json(route, { id: "workflow-running", status: "running" });
    if (pathname === "/api/ai/workflows/workflow-pending")
      return json(route, { id: "workflow-pending", status: "running" });
    if (pathname === "/api/ai/workflows/workflow-fixture/events")
      return json(route, {
        events: terminalActionRecovery
          ? terminalCommands.map((command, index) => ({
              id: index + 1,
              type: "ui_command",
              data: command,
            }))
          : workflowEvents,
      });
    if (pathname === "/api/ai/workflows/workflow-running/events") {
      const after = Number(parsed.searchParams.get("after") || 0);
      if (request.headers().accept?.includes("text/event-stream"))
        return sse(
          route,
          after < 2
            ? [
                {
                  type: "tool_progress",
                  data: { message: "刷新后工作流仍在运行" },
                },
                { type: "final", data: { message: "恢复的工作流已完成" } },
              ]
            : [],
        );
      return json(route, {
        events: [
          { id: 1, type: "tool_progress", data: { message: "正在后台处理" } },
        ],
      });
    }
    if (pathname === "/api/ai/workflows/workflow-pending/events") {
      const after = Number(parsed.searchParams.get("after") || 0);
      if (request.headers().accept?.includes("text/event-stream"))
        return sse(
          route,
          after < 1
            ? [{ type: "ui_command", data: pendingRestoreCommand }]
            : [
                { type: "ui_command", data: pendingRestoreCommand },
                { type: "final", data: { message: "恢复后的操作流已完成" } },
              ],
        );
      return json(route, {
        events: [{ id: 1, type: "ui_command", data: pendingRestoreCommand }],
      });
    }
    if (pathname === "/api/images/local" && request.method() === "POST")
      return json(route, { items: [], count: 0, batch_id: "fixture-batch" });
    if (pathname === "/api/images/local")
      return json(route, { items: [], count: 0 });

    if (
      pathname === "/py/local/catalog" ||
      pathname === "/py/local/catalog?refresh=true"
    )
      return json(route, {
        images: [],
        warning: null,
        bounds,
        boundary: polygon,
      });
    if (pathname === "/py/local/regions") {
      if (ackProbe) ackProbeSideEffects += 1;
      return json(route, regions);
    }
    if (pathname === `/py/local/regions/${region.id}/scenes`)
      return json(route, scenes);
    if (pathname.startsWith(`/py/local/regions/${region.id}/scenes`))
      return json(route, scenes);
    if (pathname === `/py/local/regions/${newRegion.id}/scenes`)
      return json(route, {
        region_id: newRegion.id,
        name: newRegion.properties.name,
        scenes: [],
        phases: [],
      });
    if (pathname.includes("/mosaic/") || pathname.includes("/tiles/"))
      return route.fulfill({ status: 404, body: "" });
    if (pathname === "/py/local/selection-pixels")
      return json(route, {
        pixel_width: 100,
        pixel_height: 100,
        pixel_count: 10000,
        area_m2: 6400,
      });
    if (pathname === "/py/overlay/image")
      return json(route, { image_b64: "", bounds, width: 1, height: 1 });
    if (pathname === "/py/overlay/mask")
      return json(route, { png_b64: "", bounds, width: 1, height: 1 });
    if (pathname.startsWith("/api/tasks/"))
      return json(route, {
        id: pathname.split("/").pop(),
        before_id: "fixture-before",
        after_id: "fixture-after",
        algorithm: "fixture",
        status: "succeeded",
        progress: 100,
      });
    if (pathname.startsWith("/api/images/")) {
      const id = pathname.split("/").pop();
      return json(route, {
        id,
        source: "local",
        phase: id === "fixture-before" ? "2025-01" : "2025-07",
        capture_date: id === "fixture-before" ? "2025-01-01" : "2025-07-01",
        file_path: "",
        thumb_path: "",
        bounds_west: bounds[0],
        bounds_south: bounds[1],
        bounds_east: bounds[2],
        bounds_north: bounds[3],
        created_at: "2025-01-01T00:00:00Z",
      });
    }
    return json(route, {});
  });
}

async function waitForAssistant(page) {
  await page.getByRole("button", { name: "打开 AI 助手" }).waitFor();
  await page.getByRole("button", { name: "打开 AI 助手" }).click();
  await page.getByRole("dialog", { name: "自然语言助手" }).waitFor();
}

async function getMapView(page) {
  return page.locator("[data-map-workspace]").evaluate((node) => {
    let component = node.__vueParentComponent;
    while (component && !component.exposed?.getView)
      component = component.parent;
    return component?.exposed?.getView() || null;
  });
}

async function sendAndWait(
  page,
  text,
  expectedFinalCount,
  expectedAckCount = expectedFinalCount,
) {
  const input = page.getByRole("textbox", { name: "输入给 AI 助手的消息" });
  await input.fill(text);
  await input.press("Enter");
  await page.waitForFunction(
    (count) =>
      document.querySelectorAll(".ai-message.is-final").length >= count,
    expectedFinalCount,
  );
  await waitForAckCount(page, expectedAckCount);
}

async function sendPrompt(page, text) {
  const input = page.getByRole("textbox", { name: "输入给 AI 助手的消息" });
  await input.fill(text);
  await input.press("Enter");
}

async function waitForAckCount(page, expectedAckCount) {
  const deadline = Date.now() + 5000;
  while (acknowledgements.length < expectedAckCount && Date.now() < deadline)
    await page.waitForTimeout(25);
  assert.ok(
    acknowledgements.length >= expectedAckCount,
    "UI command acknowledgement was not received",
  );
}

try {
  const report = { checks: [], layouts: [], acknowledgements: [] };
  {
    const page = await browser.newPage({
      viewport: { width: 1440, height: 900 },
    });
    page.setDefaultTimeout(15000);
    await fixtureForPage(page);
    await page.goto(`${url.split("/overview")[0]}/overview`);
    await page.waitForLoadState("networkidle");
    assert.equal(
      await page.getByRole("button", { name: "打开 AI 助手" }).count(),
      0,
      "unconfigured AI must stay hidden",
    );
    report.checks.push("configured gate");
    configured = true;
    await page.reload();
    await waitForAssistant(page);
    const dialog = page.getByRole("dialog", { name: "自然语言助手" });
    assert.ok(
      (
        await dialog.evaluate(
          (node) => getComputedStyle(node).transitionDuration,
        )
      )
        .split(",")
        .every((value) => value.trim() === "0.22s"),
    );
    await page.waitForTimeout(260);
    const desktopBox = await dialog.boundingBox();
    assert.ok(
      desktopBox &&
        desktopBox.width <= 680 &&
        desktopBox.width > 670 &&
        desktopBox.x > 300 &&
        desktopBox.y > 250,
    );
    const launcherBox = await page
      .getByRole("button", { name: "打开 AI 助手" })
      .boundingBox();
    const animationOrigin = await dialog.evaluate((node) => ({
      x: Number.parseFloat(node.style.getPropertyValue("--ai-origin-x")),
      y: Number.parseFloat(node.style.getPropertyValue("--ai-origin-y")),
    }));
    assert.ok(launcherBox && desktopBox);
    assert.ok(
      Math.abs(
        animationOrigin.x -
          (launcherBox.x +
            launcherBox.width / 2 -
            desktopBox.x -
            desktopBox.width / 2),
      ) < 4 &&
        Math.abs(
          animationOrigin.y -
            (launcherBox.y +
              launcherBox.height / 2 -
              desktopBox.y -
              desktopBox.height / 2),
        ) < 4,
      `panel transition origin must match the launcher center: ${JSON.stringify({ animationOrigin, launcherBox, desktopBox })}`,
    );
    report.layouts.push({
      name: "desktop",
      viewport: [1440, 900],
      box: desktopBox,
    });
    await page.locator('.app-nav .nav-item[href="/comparison"]').click();
    await page.waitForURL("**/comparison");
    assert.equal(
      await page.getByRole("button", { name: "打开 AI 助手" }).count(),
      0,
      "AI entry must be hidden outside Overview",
    );
    await page.locator('.app-nav .nav-item[href="/overview"]').click();
    await page.waitForURL("**/overview");
    await waitForAssistant(page);
    await page.emulateMedia({ reducedMotion: "reduce" });
    assert.equal(
      await dialog.evaluate(
        (node) => getComputedStyle(node).transitionDuration,
      ),
      "0s",
    );
    report.checks.push(
      "desktop launcher-origin expansion, Overview-only gate, and reduced motion",
    );
    await page.waitForFunction(
      () => document.querySelector(".ai-dialog") !== null,
    );
    // The fixture command sequence exercises map bounds, region, phase, and AOI adapters.
    for (const [index, prompt] of [
      "定位测试县",
      "选择测试县",
      "加载一月时相",
      "使用当前选区",
    ].entries()) {
      await sendAndWait(page, prompt, index + 1);
      await page.waitForTimeout(80);
      assert.equal(
        acknowledgements[index].body.status,
        "applied",
        `UI command ${index} should apply`,
      );
      if (index === 0) {
        await page.waitForFunction(() => {
          const node = document.querySelector("[data-map-workspace]");
          let component = node?.__vueParentComponent;
          while (component && !component.exposed?.getView)
            component = component.parent;
          const view = component?.exposed?.getView();
          return (
            view &&
            Math.abs(view.center.lat - 38.955) < 0.0001 &&
            Math.abs(view.center.lng - 115.975) < 0.0001
          );
        });
        const view = await getMapView(page);
        assert.ok(
          view &&
            Math.abs(view.center.lat - 38.955) < 0.0001 &&
            Math.abs(view.center.lng - 115.975) < 0.0001,
        );
        report.checks.push("WGS84 fly_to_bounds reaches expected map center");
      }
      if (index === 0)
        for (const kind of ["assistant", "plan", "tool", "final"])
          assert.ok(
            await page.locator(`.ai-message.is-${kind}`).count(),
            `${kind} SSE event should be visible`,
          );
    }
    assert.deepEqual(
      acknowledgements.map((item) => item.id),
      actionCommands.slice(0, 4).map((command) => command.action_id),
    );
    report.checks.push(
      "SSE text/plan/tool/final events and four UI command acknowledgements",
    );
    const safeLinks = await page
      .locator(".ai-download-link")
      .evaluateAll((nodes) =>
        nodes.map((node) => ({
          href: node.getAttribute("href"),
          text: node.textContent?.trim(),
        })),
      );
    assert.equal(safeLinks.length, 4);
    assert.ok(
      safeLinks.every(
        (link) =>
          link.href === "/api/tasks/fixture-task/result/change.geojson" &&
          link.text === "下载结果",
      ),
      "only same-origin, whitelisted result refs may become links",
    );
    report.checks.push(
      "safe result export link and rejected external/path traversal refs",
    );
    await page.screenshot({
      path: path.join(output, "desktop.png"),
      fullPage: true,
    });
    // Completed geometry and phases survive navigation within this document.
    assert.equal(await page.locator('.leaflet-cdp-area-pane path[stroke="#22d3ee"]').count(), 1);
    await page.locator('.app-nav .nav-item[href="/comparison"]').click();
    await page.locator('.app-nav .nav-item[href="/overview"]').click();
    await waitForAssistant(page);
    assert.equal(await page.locator('.ai-turn').count(), 4);
    assert.equal(await page.locator('.leaflet-cdp-area-pane path[stroke="#22d3ee"]').count(), 1);
    assert.equal(await page.locator('.phase-tab').count(), 1);
    const beforeReloadAcks = acknowledgements.length;
    const beforeRequests = [];
    page.on('request', (request) => {
      if (/\/api\/ai\/(sessions|workflows)/.test(new URL(request.url()).pathname))
        beforeRequests.push(request.url());
    });
    resumeWorkflows = true;
    pendingActionRecovery = true;
    restoreWorkspaceSnapshot = true;
    await page.evaluate(() => {
      sessionStorage.setItem('change-detection.ai.session', 'session-fixture');
      sessionStorage.setItem('change-detection.ai.context-revision', 'workspace-restored');
    });
    await page.reload();
    await waitForAssistant(page);
    await page.waitForLoadState('networkidle');
    assert.equal(await page.locator('.ai-turn').count(), 0);
    assert.equal(await page.locator('.leaflet-cdp-area-pane path[stroke="#22d3ee"]').count(), 0);
    assert.equal(await page.locator('.phase-tab').count(), 0);
    assert.equal(await page.getByRole('button', { name: '提交检测任务' }).isDisabled(), true);
    assert.equal(acknowledgements.length, beforeReloadAcks);
    assert.deepEqual(beforeRequests, [], 'reload must not read, reconnect, close or replay an old AI session');
    assert.deepEqual(await page.evaluate(() => [
      sessionStorage.getItem('change-detection.ai.session'),
      sessionStorage.getItem('change-detection.ai.context-revision'),
    ]), [null, null]);
    messageCount = 0;
    await sendAndWait(page, '刷新后的新请求', 1, beforeReloadAcks + 1);
    const freshWorkspace = messageWorkspaces.at(-1);
    assert.equal(freshWorkspace.region_id, undefined);
    assert.equal(freshWorkspace.aoi, undefined);
    assert.equal(freshWorkspace.bounds, undefined);
    assert.equal(freshWorkspace.task_id, undefined);
    assert.deepEqual(freshWorkspace.phase_ids, []);
    assert.notEqual(freshWorkspace.revision, 'workspace-restored');
    assert.equal(await page.evaluate(() => sessionStorage.getItem('change-detection.ai.session')), null);
    report.checks.push('SPA navigation preserves AI/AOI/phases; reload clears them after catalog scan without replaying pending actions');
    restoreWorkspaceSnapshot = false;
    await page.close();
  }
  for (const [index, expectedPath, prompt] of [
    [4, "/comparison", "恢复任务"],
    [5, "/reports", "查看结果"],
  ]) {
    messageCount = index;
    resumeWorkflows = false;
    pendingActionRecovery = false;
    const page = await browser.newPage({
      viewport: { width: 1280, height: 800 },
    });
    page.setDefaultTimeout(15000);
    await fixtureForPage(page);
    await page.goto(`${url.split("/overview")[0]}/overview`);
    await waitForAssistant(page);
    const expectedAcks = acknowledgements.length + 1;
    await sendPrompt(page, prompt);
    await waitForAckCount(page, expectedAcks);
    await page.waitForURL(`**${expectedPath}`);
    assert.equal(
      acknowledgements.at(-1).body.status,
      "applied",
      `${expectedPath} UI command should apply`,
    );
    report.checks.push(
      `${expectedPath.slice(1)} UI command restores task/result context`,
    );
    await page.close();
  }
  for (const [index, expectedCode, prompt] of [
    [6, "UI_CONTEXT_STALE", "过期上下文操作"],
    [7, "UI_ACTION_UNSUPPORTED", "拒绝未知操作"],
  ]) {
    messageCount = index;
    const page = await browser.newPage({
      viewport: { width: 1280, height: 800 },
    });
    page.setDefaultTimeout(15000);
    await fixtureForPage(page);
    await page.goto(`${url.split("/overview")[0]}/overview`);
    await waitForAssistant(page);
    await sendAndWait(page, prompt, 1, acknowledgements.length + 1);
    assert.equal(acknowledgements.at(-1).body.status, "rejected");
    assert.equal(acknowledgements.at(-1).body.error.code, expectedCode);
    report.checks.push(`${expectedCode} UI action rejection`);
    await page.close();
  }
  {
    const page = await browser.newPage({
      viewport: { width: 390, height: 844 },
    });
    page.setDefaultTimeout(15000);
    await fixtureForPage(page);
    await page.goto(`${url.split("/overview")[0]}/overview`);
    await waitForAssistant(page);
    await page.waitForTimeout(260);
    const launcher = page.getByRole("button", { name: "打开 AI 助手" });
    const launcherBox = await launcher.boundingBox();
    const dialogBox = await page
      .getByRole("dialog", { name: "自然语言助手" })
      .boundingBox();
    assert.ok(launcherBox && launcherBox.x < 30 && launcherBox.y > 760);
    assert.ok(
      dialogBox &&
        dialogBox.width <= 370 &&
        dialogBox.width > 365 &&
        dialogBox.x >= 10 &&
        dialogBox.y > 180,
    );
    assert.ok(
      dialogBox.y + dialogBox.height <= 844,
      "mobile dialog must fit above the safe-area bottom edge",
    );
    report.layouts.push({
      name: "mobile",
      viewport: [390, 844],
      launcher: launcherBox,
      box: dialogBox,
    });
    await page.screenshot({
      path: path.join(output, "mobile.png"),
      fullPage: true,
    });
    await page.close();
  }
  await runAiReliabilityChecks({
    browser,
    url,
    fixtureForPage,
    report,
    output,
  });
  await runAiDetectionChecks({ browser, url, fixtureForPage, report, region, bounds, polygon });
  report.acknowledgements = acknowledgements;
  await fs.writeFile(
    path.join(output, "report.json"),
    JSON.stringify(report, null, 2),
  );
  console.log(JSON.stringify(report, null, 2));
} catch (error) {
  for (const context of browser.contexts())
    for (const page of context.pages())
      await page
        .screenshot({ path: path.join(output, "failure.png"), fullPage: true })
        .catch(() => {});
  throw error;
} finally {
  await browser.close();
}
