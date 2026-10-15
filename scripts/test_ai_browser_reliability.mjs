import assert from "node:assert/strict";
import http from "node:http";
import path from "node:path";

// A real local SSE socket lets browser checks observe intermediate layout and
// disconnection. Playwright route.fulfill buffers the entire response.
export async function runAiReliabilityChecks({
  browser,
  url,
  fixtureForPage,
  report,
  output,
}) {
  const handlers = new Map();
  const sockets = new Set();
  const server = http.createServer(async (request, response) => {
    response.setHeader("Access-Control-Allow-Origin", new URL(url).origin);
    response.setHeader("Access-Control-Allow-Credentials", "true");
    response.setHeader(
      "Access-Control-Allow-Headers",
      "content-type,last-event-id",
    );
    if (request.method === "OPTIONS") {
      response.writeHead(204);
      response.end();
      return;
    }
    const handler = handlers.get(request.url);
    if (!handler) {
      response.writeHead(404);
      response.end();
      return;
    }
    let body = "";
    for await (const chunk of request) body += chunk;
    response.writeHead(200, {
      "Content-Type": "text/event-stream",
      "Cache-Control": "no-cache",
    });
    response.flushHeaders();
    try {
      await handler({ request, response, body });
    } catch (error) {
      response.destroy(error);
    }
  });
  server.on("connection", (socket) => {
    sockets.add(socket);
    socket.on("close", () => sockets.delete(socket));
  });
  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
  const origin = `http://127.0.0.1:${server.address().port}`;
  const endpoint = `${url.split("/overview")[0]}/overview`;
  const send = async (page, text) => {
    await page
      .getByRole("textbox", { name: "输入给 AI 助手的消息" })
      .fill(text);
    await page.getByRole("button", { name: "发送消息" }).click();
  };
  const createPage = async (
    name,
    handler,
    viewport = { width: 1440, height: 900 },
  ) => {
    const page = await browser.newPage({ viewport });
    page.setDefaultTimeout(15_000);
    await fixtureForPage(page);
    let sessionCount = 0;
    const bodies = [];
    const closes = [];
    const acks = [];
    let closeFailure = false;
    await page.route("**/api/ai/sessions", (route) =>
      route.fulfill({
        status: 200,
        json: { id: `${name}-session-${++sessionCount}` },
      }),
    );
    await page.route("**/api/ai/sessions/*/close", (route) => {
      closes.push(route.request().url());
      return route.fulfill(
        closeFailure
          ? { status: typeof closeFailure === "number" ? closeFailure : 503, json: { error: "fixture close unavailable" } }
          : { status: 200, json: { status: "closed" } },
      );
    });
    await page.route("**/api/ai/sessions/*/actions/*", (route) => {
      acks.push({
        url: route.request().url(),
        body: route.request().postDataJSON(),
      });
      return route.fulfill({ status: 200, json: {} });
    });
    handlers.set(`/${name}`, handler);
    await page.route("**/api/ai/sessions/*/messages", async (route) => {
      bodies.push({
        url: route.request().url(),
        body: route.request().postData(),
        cursor: route.request().headers()["last-event-id"],
      });
      await route.continue({ url: `${origin}/${name}` });
    });
    await page.goto(endpoint);
    await page.getByRole("button", { name: "打开 AI 助手" }).click();
    await page.getByRole("dialog", { name: "自然语言助手" }).waitFor();
    return {
      page,
      bodies,
      closes,
      acks,
      setCloseFailure: (value) => {
        closeFailure = value;
      },
    };
  };
  const writer = ({ response, body }, workflowId = "workflow-live") => {
    let id = 0;
    const emit = (type, data, eventId) => {
      id = eventId ?? id + 1;
      response.write(
        `id: ${id}\nevent: ${type}\ndata: ${JSON.stringify(data)}\n\n`,
      );
    };
    const payload = JSON.parse(body);
    emit("workflow_started", {
      workflow_id: workflowId,
      request_id: payload.request_id,
    });
    return { emit, end: () => response.end(), payload };
  };
  try {
    for (const [name, viewport, expectedHeight] of [
      ["live-desktop", { width: 1440, height: 900 }, 160],
      ["live-mobile", { width: 390, height: 844 }, 120],
    ]) {
      let stream;
      const markdown =
        "## 检测结果\n\n- **发现变化**\n- 第二项\n\n| 区域 | 面积 |\n|---|---|\n| 测试县 | 123 |\n\n```json\n" +
        "x".repeat(180) +
        '\n```\n\n<script>window.__aiXss=1</script>\n<img src=x onerror="window.__aiXss=1">\n\n[危险](javascript:alert(1)) [文件](file:///secret) ![图片](https://evil.example/track.png) [链接](https://example.org)\n\n' +
        "结果说明。\n\n".repeat(20);
      const context = await createPage(
        name,
        async (request) => {
          stream = writer(request, `${name}-workflow`);
          stream.emit("plan", { message: "正在执行查询计划" });
          stream.emit("text_delta", { text: "查询当前区域和时间范围。" });
          for (let index = 0; index < 35; index++)
            stream.emit("tool_progress", {
              message: `已读取第 ${index} 组数据`,
            });
          stream.emit("tool_progress", {
            message: "服务暂时不可用，正在重试",
            stage: "execution",
            attempt: 2,
            max_attempts: 3,
            delay_ms: 200,
          });
          stream.emit("tool_finished", {
            message: "导出准备完成",
            download_ref: "/api/tasks/live-task/result/stats.csv",
          });
        },
        viewport,
      );
      const { page } = context;
      await send(page, "查询当前区域的变化");
      await page.locator(".ai-process-viewport .ai-message").nth(30).waitFor();
      const process = page.locator(".ai-process-viewport");
      assert.equal(
        await process.evaluate((node) => node.clientHeight),
        expectedHeight,
      );
      assert.ok(
        await process.evaluate((node) => node.scrollHeight > node.clientHeight),
      );
      assert.equal(
        await page.locator(".ai-turn-process").evaluate((node) => node.open),
        true,
      );
      await page
        .getByText("正在重试（2/3），0.2 秒后继续", { exact: true })
        .waitFor();
      await process.evaluate((node) => {
        node.scrollTop = 0;
        node.dispatchEvent(new Event("scroll"));
      });
      stream.emit("tool_progress", { message: "新增进度保持阅读位置" });
      await page.getByText("新增进度保持阅读位置", { exact: true }).waitFor();
      assert.equal(await process.evaluate((node) => node.scrollTop), 0);
      assert.equal(
        await process.evaluate((node) => node.clientHeight),
        expectedHeight,
      );
      stream.emit("text_delta", { text: markdown });
      stream.emit("final", { status: "succeeded", message: markdown });
      stream.end();
      await page.locator(".ai-markdown h2").waitFor();
      assert.equal(
        await page.locator(".ai-turn-process").evaluate((node) => node.open),
        false,
      );
      assert.equal(await process.isVisible(), false);
      assert.equal(await page.locator(".ai-markdown li").count(), 2);
      assert.equal(await page.locator(".ai-markdown table").count(), 1);
      assert.equal(await page.locator(".ai-markdown pre code").count(), 1);
      assert.equal(
        await page
          .locator(
            ".ai-markdown script, .ai-markdown img, .ai-markdown [onerror]",
          )
          .count(),
        0,
      );
      assert.equal(await page.evaluate(() => window.__aiXss), undefined);
      assert.ok(
        (
          await page
            .locator(".ai-markdown a")
            .evaluateAll((links) =>
              links.map((link) => link.getAttribute("href")),
            )
        ).every(
          (href) => !/^(javascript|data|vbscript|file):/i.test(href || ""),
        ),
      );
      assert.equal(
        await page.getByRole("link", { name: "下载结果" }).isVisible(),
        true,
      );
      assert.equal(
        await page
          .locator(".ai-message-list")
          .evaluate((node) => node.scrollWidth <= node.clientWidth),
        true,
      );
      assert.equal(
        await page
          .locator(".ai-markdown pre")
          .evaluate((node) => node.scrollWidth > node.clientWidth),
        true,
      );
      await page.locator(".ai-turn-process > summary").click();
      assert.equal(await process.isVisible(), true);
      await page.screenshot({
        path: path.join(output, `${name}.png`),
        fullPage: true,
      });
      report.checks.push(
        `${name}: fixed process height, reading position, auto-collapse, Markdown safety and visible exports`,
      );
      await page.close();
    }

    {
      let attempts = 0;
      const context = await createPage("reconnect", async (request) => {
        attempts += 1;
        const stream = writer(request, "workflow-reconnect");
        stream.emit("text_delta", { text: "唯一中间文本" });
        stream.emit("tool_finished", { message: "唯一工具结果" });
        if (attempts === 1) {
          stream.end();
          return;
        }
        stream.emit("final", {
          status: "succeeded",
          message: "恢复后的最终结果",
        });
        stream.end();
      });
      await send(context.page, "测试连接恢复");
      await context.page
        .getByText("恢复后的最终结果", { exact: true })
        .waitFor();
      assert.equal(attempts, 2);
      assert.equal(context.bodies[0].body, context.bodies[1].body);
      assert.equal(context.bodies[1].cursor, "3");
      assert.equal(
        await context.page.locator(".ai-message.is-user").count(),
        1,
      );
      assert.equal(
        await context.page.getByText("唯一中间文本", { exact: true }).count(),
        1,
      );
      assert.equal(
        await context.page.getByText("唯一工具结果", { exact: true }).count(),
        1,
      );
      report.checks.push(
        "SSE EOF reconnect keeps original body/request ID and cursor, deduplicates workflow events",
      );
      await context.page.close();
    }

    {
      let attempts = 0;
      let recover = false;
      const context = await createPage("exhausted", async (request) => {
        attempts += 1;
        const stream = writer(request, "workflow-exhausted");
        stream.emit("text_delta", { text: "保留部分响应" });
        if (recover)
          stream.emit("final", {
            status: "succeeded",
            message: "手动恢复成功",
          });
        stream.end();
      });
      await send(context.page, "测试重连耗尽");
      await context.page.getByRole("button", { name: "恢复连接" }).waitFor();
      assert.equal(
        attempts,
        4,
        "initial connection plus at most three reconnects",
      );
      assert.equal(
        await context.page.locator(".ai-turn").getAttribute("data-status"),
        "interrupted",
      );
      assert.equal(
        await context.page
          .locator(".ai-turn-process")
          .evaluate((node) => node.open),
        false,
      );
      recover = true;
      await context.page.getByRole("button", { name: "恢复连接" }).click();
      await context.page.getByText("手动恢复成功", { exact: true }).waitFor();
      assert.equal(attempts, 5);
      assert.ok(
        context.bodies.every((item) => item.body === context.bodies[0].body),
      );
      assert.equal(
        await context.page.getByText("保留部分响应", { exact: true }).count(),
        1,
      );
      report.checks.push(
        "bounded reconnect exhaustion and explicit recovery retain one logical request",
      );
      await context.page.close();
    }

    {
      let oldStream;
      const context = await createPage("new-conversation", async (request) => {
        const body = JSON.parse(request.body);
        const stream = writer(request, `workflow-${body.request_id}`);
        if (body.text === "旧任务") {
          oldStream = stream;
          stream.emit("plan", { message: "旧任务仍在处理" });
          stream.emit("ui_command", {
            action_id: "old-delayed-region",
            action: "select_region",
            args: { region_id: "fixture-new-region" },
          });
        } else {
          stream.emit("final", {
            status: "succeeded",
            message: "新任务独立完成",
          });
          stream.end();
        }
      });
      const { page } = context;
      let releaseRegion;
      let regionPending = false;
      await page.route("**/py/local/regions", async (route) => {
        regionPending = true;
        await new Promise((resolve) => {
          releaseRegion = resolve;
        });
        await route
          .fulfill({
            status: 200,
            json: {
              features: [
                {
                  type: "Feature",
                  id: "fixture-new-region",
                  geometry: {
                    type: "Polygon",
                    coordinates: [
                      [
                        [116.01, 38.95],
                        [116.02, 38.95],
                        [116.02, 38.96],
                        [116.01, 38.96],
                        [116.01, 38.95],
                      ],
                    ],
                  },
                  properties: {
                    region_id: "fixture-new-region",
                    name: "旧异步区域",
                    kind: "county",
                  },
                },
              ],
            },
          })
          .catch(() => {});
      });
      await send(page, "旧任务");
      await page.getByText("旧任务仍在处理", { exact: true }).waitFor();
      while (!regionPending) await page.waitForTimeout(20);
      const firstBody = JSON.parse(context.bodies[0].body);
      await page.getByRole("button", { name: "新对话", exact: true }).click();
      await page.getByText("描述要查询的区域、时相或检测结果").waitFor();
      assert.equal(context.closes.length, 1);
      releaseRegion();
      oldStream.emit("ui_command", {
        action_id: "old-late-navigation",
        action: "open_report",
        args: {},
      });
      oldStream.emit("final", {
        status: "succeeded",
        message: "旧任务最终结果不应出现",
      });
      oldStream.end();
      await send(page, "新任务");
      await page.getByText("新任务独立完成", { exact: true }).waitFor();
      await page.waitForTimeout(150);
      const secondBody = JSON.parse(context.bodies[1].body);
      assert.notEqual(context.bodies[0].url, context.bodies[1].url);
      assert.equal(
        secondBody.workspace.region_id,
        firstBody.workspace.region_id,
      );
      assert.deepEqual(
        secondBody.workspace.phase_ids,
        firstBody.workspace.phase_ids,
      );
      assert.equal(await page.locator(".ai-message.is-user").count(), 1);
      assert.equal(
        await page.getByText("旧任务最终结果不应出现", { exact: true }).count(),
        0,
      );
      assert.equal(context.acks.length, 0);
      assert.ok(page.url().endsWith("/overview"));
      report.checks.push(
        "new conversation during a task isolates delayed region callbacks, ACKs, late events and navigation while preserving workspace",
      );
      await page.close();
    }

    {
      let stream;
      let recover = false;
      const context = await createPage("close-failure", async (request) => {
        stream = writer(request, "workflow-close-failure");
        stream.emit("plan", { message: "关闭失败前的任务" });
        if (recover) {
          stream.emit("final", {
            status: "succeeded",
            message: "恢复旧任务成功",
          });
          stream.end();
        }
      });
      context.setCloseFailure(true);
      await send(context.page, "不要丢失关闭错误");
      await context.page
        .getByText("关闭失败前的任务", { exact: true })
        .waitFor();
      await context.page
        .getByRole("button", { name: "新对话", exact: true })
        .click();
      await context.page
        .getByRole("alert")
        .filter({ hasText: "旧任务可能仍在运行" })
        .waitFor();
      assert.equal(
        await context.page.locator(".ai-message.is-user").count(),
        1,
      );
      assert.equal(
        await context.page
          .getByRole("button", { name: "恢复连接" })
          .isVisible(),
        true,
      );
      recover = true;
      await context.page.getByRole("button", { name: "恢复连接" }).click();
      await context.page.getByText("恢复旧任务成功", { exact: true }).waitFor();
      assert.equal(context.bodies[0].body, context.bodies[1].body);
      context.setCloseFailure(404);
      await context.page
        .getByRole("button", { name: "新对话", exact: true })
        .click();
      await context.page
        .getByText("描述要查询的区域、时相或检测结果")
        .waitFor();
      assert.equal(context.closes.length, 2);
      stream.end();
      report.checks.push(
        "503 session close retains recoverable state; 404 close clears the missing session and allows a new conversation",
      );
      await context.page.close();
    }

    {
      const streams = [];
      const context = await createPage('reload-running', async (request) => {
        const stream = writer(request, 'workflow-before-reload');
        streams.push(stream);
        const body = JSON.parse(request.body);
        if (body.text === '刷新前正在执行') stream.emit('plan', { message: '旧任务仍在执行' });
        else {
          stream.emit('final', { status: 'succeeded', message: '刷新后独立完成' });
          stream.end();
        }
      });
      const { page } = context;
      await send(page, '刷新前正在执行');
      await page.getByText('旧任务仍在执行', { exact: true }).waitFor();
      const previousSessionUrl = context.bodies[0].url;
      await page.reload();
      await page.getByRole('button', { name: '打开 AI 助手' }).click();
      await page.getByText('描述要查询的区域、时相或检测结果').waitFor();
      assert.equal(await page.getByRole('button', { name: '恢复连接' }).count(), 0);
      assert.equal(await page.getByRole('alert').count(), 0);
      assert.equal(context.bodies.length, 1, 'refresh must not reconnect the old stream');
      await send(page, '刷新后的独立任务');
      await page.getByText('刷新后独立完成', { exact: true }).waitFor();
      assert.notEqual(context.bodies[1].url, previousSessionUrl);
      assert.equal(await page.locator('.ai-turn').count(), 1);
      assert.equal(context.closes.length, 0);
      report.checks.push('reload during a running turn starts an empty assistant and creates a fresh session on the next message');
      for (const stream of streams) stream.end();
      await page.close();
    }

    {
      let stream;
      const context = await createPage("ack-cancel", async (request) => {
        stream = writer(request, "workflow-ack-cancel");
        stream.emit("ui_command", {
          action_id: "old-ack-retry",
          action: "unsupported_action",
          args: {},
        });
      });
      let attempts = 0;
      await context.page.route(
        "**/api/ai/sessions/*/actions/old-ack-retry",
        (route) => {
          attempts += 1;
          return route.fulfill({
            status: 503,
            json: { error: "fixture ACK unavailable" },
          });
        },
      );
      await send(context.page, "取消旧回执重试");
      while (!attempts) await context.page.waitForTimeout(10);
      await context.page
        .getByRole("button", { name: "新对话", exact: true })
        .click();
      await context.page
        .getByText("描述要查询的区域、时相或检测结果")
        .waitFor();
      const afterClose = attempts;
      await context.page.waitForTimeout(550);
      assert.equal(
        attempts,
        afterClose,
        "old ACK backoff must end when the session changes",
      );
      stream.end();
      report.checks.push(
        "conversation reset aborts ACK backoff without submitting an old callback to the new session",
      );
      await context.page.close();
    }

    {
      const context = await createPage("fatal-error", async () => {});
      let attempts = 0;
      await context.page.route("**/api/ai/sessions/*/messages", (route) => {
        attempts += 1;
        return route.fulfill({
          status: 401,
          json: { code: "AI_AUTH_FAILED", message: "AI 服务认证失败" },
        });
      });
      await send(context.page, "认证错误不要无限重试");
      await context.page
        .getByRole("alert")
        .filter({ hasText: "AI 服务认证失败" })
        .waitFor();
      await context.page.waitForTimeout(600);
      assert.equal(attempts, 1);
      assert.equal(
        await context.page.getByRole("button", { name: "恢复连接" }).count(),
        0,
      );
      report.checks.push(
        "non-retryable HTTP authentication errors stop immediately with a visible failure",
      );
      await context.page.close();
    }

    {
      const context = await createPage("cancelled", async (request) => {
        const stream = writer(request, "workflow-cancelled");
        stream.emit("plan", { message: "取消前进度" });
        stream.emit("cancelled", { message: "用户取消了任务" });
        stream.end();
      });
      await send(context.page, "取消终态");
      await context.page.getByText("用户取消了任务", { exact: true }).waitFor();
      assert.equal(
        await context.page.locator(".ai-turn").getAttribute("data-status"),
        "cancelled",
      );
      assert.equal(
        await context.page
          .locator(".ai-turn-process")
          .evaluate((node) => node.open),
        false,
      );
      assert.equal(context.bodies.length, 1);
      report.checks.push(
        "dedicated cancellation event ends and collapses the turn without retry",
      );
      await context.page.close();
    }

    {
      const context = await createPage("error-refs", async (request) => {
        const stream = writer(request, "workflow-error-refs");
        stream.emit("tool_finished", {
          tool: "detection.workflow.submit",
          status: "pending",
          data: {
            task_ids: ["existing-task-1", "existing-task-2"],
            workflow_id: "submitted-workflow",
            operation_id: "prepare-1",
            download_ref: "/api/tasks/existing-task-1/result/stats.csv",
            file_path: "must-not-display-private-path",
          },
        });
        stream.emit("tool_progress", {
          stage: "tool.detection.workflow.submit",
          attempt: 3,
          max_attempts: 3,
          delay_ms: 800,
        });
        stream.emit("error", {
          code: "AI_REQUEST_FAILED",
          message: "模型请求重试后仍失败",
          refs: [
            {
              task_id: "existing-task-1",
              result_id: "existing-result",
              download_ref: "/api/tasks/existing-task-1/result/stats.csv",
            },
            {
              task_ids: ["existing-task-3", "../secret"],
              operation_id: "/private/file",
              download_ref: "https://evil.example/result.zip",
            },
          ],
        });
        stream.end();
      });
      const { page } = context;
      await send(page, "提交后模型错误仍保留任务引用");
      await page
        .getByRole("alert")
        .filter({ hasText: "模型请求重试后仍失败" })
        .waitFor();
      assert.equal(
        await page.locator(".ai-turn-process").evaluate((node) => node.open),
        false,
      );
      const references = page.locator(".ai-resource-refs");
      assert.equal(await references.isVisible(), true);
      assert.deepEqual(await references.locator("p").allTextContents(), [
        "工作流：submitted-workflow",
        "准备操作：prepare-1",
        "任务：existing-task-1",
        "任务：existing-task-2",
        "结果：existing-result",
        "任务：existing-task-3",
      ]);
      assert.equal(
        await page.getByRole("link", { name: "下载结果" }).count(),
        1,
      );
      assert.equal(
        await page.getByRole("link", { name: "下载结果" }).isVisible(),
        true,
      );
      assert.equal(
        await references.getByText("must-not-display-private-path").count(),
        0,
      );
      assert.equal(context.acks.length, 0);
      assert.ok(page.url().endsWith("/overview"));
      assert.equal(context.bodies.length, 1);
      assert.equal(
        await page
          .getByText("detection.workflow.submit · 已提交，处理中", {
            exact: true,
          })
          .count(),
        1,
      );
      await page.locator(".ai-turn-process > summary").click();
      await page
        .getByText("正在重试（3/3），0.8 秒后继续", { exact: true })
        .waitFor();
      report.checks.push(
        "terminal provider failure retains safe task/result/workflow/operation refs and exports from pending tool data and error refs outside collapsed history",
      );
      await page.close();
    }

    for (const scenario of ["catalog", "task"]) {
      let oldStream;
      let pending = false;
      let release;
      const context = await createPage(`late-${scenario}`, async (request) => {
        const body = JSON.parse(request.body);
        const stream = writer(request, `workflow-late-${body.request_id}`);
        if (body.text === "等待旧操作") {
          oldStream = stream;
          stream.emit(
            "ui_command",
            scenario === "catalog"
              ? {
                  action_id: "old-catalog",
                  action: "select_region",
                  args: { region_id: "fixture-region" },
                }
              : {
                  action_id: "old-task",
                  action: "show_task",
                  args: { task_id: "fixture-old-task" },
                },
          );
        } else {
          stream.emit("final", {
            status: "succeeded",
            message: "晚响应隔离成功",
          });
          stream.end();
        }
      });
      const { page } = context;
      await page.route(
        scenario === "catalog"
          ? "**/py/local/regions/fixture-region/scenes"
          : "**/api/tasks/fixture-old-task",
        async (route) => {
          pending = true;
          await new Promise((resolve) => {
            release = resolve;
          });
          await route
            .fulfill({
              status: 200,
              json:
                scenario === "catalog"
                  ? {
                      region_id: "fixture-region",
                      phases: [
                        {
                          id: "late-phase",
                          region_id: "fixture-region",
                          month: "2024-01",
                          available: true,
                          scenes: [],
                          version: "late",
                        },
                      ],
                    }
                  : {
                      id: "fixture-old-task",
                      before_id: "fixture-before",
                      after_id: "fixture-after",
                      algorithm: "fixture",
                      status: "succeeded",
                    },
            })
            .catch(() => {});
        },
      );
      await send(page, "等待旧操作");
      while (!pending) await page.waitForTimeout(20);
      await page.getByRole("button", { name: "新对话", exact: true }).click();
      await page.getByText("描述要查询的区域、时相或检测结果").waitFor();
      release();
      oldStream.end();
      await send(page, "新的独立请求");
      await page.getByText("晚响应隔离成功", { exact: true }).waitFor();
      assert.ok(page.url().endsWith("/overview"));
      assert.equal(context.acks.length, 0);
      assert.equal(
        JSON.parse(context.bodies[1].body).workspace.task_id,
        undefined,
      );
      assert.ok(
        !JSON.parse(context.bodies[1].body).workspace.phase_ids.includes(
          "late-phase",
        ),
      );
      report.checks.push(
        `${scenario} async work cannot mutate workspace or ACK after conversation reset`,
      );
      await page.close();
    }

    {
      const page = await browser.newPage({
        viewport: { width: 1280, height: 800 },
      });
      page.setDefaultTimeout(15_000);
      await fixtureForPage(page);
      await page.addInitScript(() => {
        sessionStorage.setItem('change-detection.ai.session', 'session-history');
        sessionStorage.setItem('change-detection.ai.context-revision', 'workspace-old');
      });
      let staleSessionRequests = 0;
      await page.route('**/api/ai/sessions/session-history**', (route) => {
        staleSessionRequests += 1;
        return route.fulfill({ status: 404, json: { error: 'not found' } });
      });
      await page.goto(endpoint);
      await page.getByRole("button", { name: "打开 AI 助手" }).click();
      await page.waitForLoadState('networkidle');
      assert.equal(await page.locator('.ai-turn').count(), 0);
      assert.equal(staleSessionRequests, 0);
      assert.equal(await page.getByRole('alert').count(), 0);
      await page.getByRole('button', { name: '新对话', exact: true }).click();
      assert.equal(staleSessionRequests, 0);
      assert.equal(await page.getByRole('alert').count(), 0);
      await page.getByRole('textbox', { name: '输入给 AI 助手的消息' }).fill('新的独立对话');
      assert.equal(await page.getByRole('button', { name: '发送消息' }).isEnabled(), true);
      assert.equal(await page.evaluate(() => sessionStorage.getItem('change-detection.ai.session')), null);
      assert.equal(await page.getByText('旧任务中间说明', { exact: true }).count(), 0);
      report.checks.push('legacy stored session is discarded on startup without restoring workflow history');
      await page.close();
    }
  } finally {
    for (const socket of sockets) socket.destroy();
    await new Promise((resolve) => server.close(resolve));
  }
}
