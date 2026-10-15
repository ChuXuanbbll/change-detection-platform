# AI Tool Use 与 Skill 基础建设

本文档描述首期自然语言助手的稳定边界。AI Gateway 只向模型暴露注册过的 Tool；模型不能访问数据库、文件系统、Python 私有接口或原始 HTTP URL。Tool 返回 opaque ID、结构化统计和受控下载引用，源影像、源目录、模型文件和模型登记信息保持只读。

## 启用方式

Go 服务使用 OpenAI-compatible Chat Completions provider。启动 Go 服务前设置：

```text
AI_API_BASE=https://your-provider.example/v1
AI_API_KEY=...
AI_MODEL=...
AI_TIMEOUT_MS=45000
```

未同时配置这三个必需值时，`GET /api/ai/status` 返回 `configured=false`，Overview 不显示 AI 入口。实际部署应把密钥放在进程环境或受控密钥注入系统，不写入前端、仓库或 AI session。

## 业务链路

```text
Overview AI 球
  -> POST /api/ai/sessions/:id/messages
  -> Go AI Gateway
  -> LLM provider adapter
  -> 版本化 Skill 状态机
  -> Tool Registry（JSON Schema 2020-12）
  -> 领域服务 / 既有 Go-Python API
  -> SSE 事件、UI command、workflow 状态
```

刷新网页视作开启新的工作区：清空 AI 对话、选区、已加载时相和前端任务状态，不从旧 session 恢复工作区或重放 UI 操作；旧版本保存的浏览器会话键会在启动时移除。同一页面生命周期内，切页保留工作区，SSE 断线仍使用 `Last-Event-ID` 恢复当前订阅。刷新不会重启后端，也不会取消已提交的服务端检测任务。

模型调用将网络故障、超时、限流和服务端暂时故障最多尝试三次（含首次），只重发当前模型请求，不重新执行已完成的工具。退避以 1、2 秒为基准并加入抖动，遵循 `Retry-After`，单次等待最多 30 秒；一次模型调用的总预算为 `3 × AI_TIMEOUT_MS + 30 秒`。鉴权、额度、参数和响应格式错误直接返回分类后的中文原因；服务端日志只记录脱敏的错误码、上游请求标识、HTTP 状态、阶段及尝试次数。

只读工具仅在 `effect=query`、`retry_policy.safe=true` 且错误可重试时最多尝试三次，始终复用原工具 request ID；未指定超时时只读工具每次默认 45 秒。准备、提交和界面操作不自动重放。SSE 在没有终态时断开，会最多自动重连三次，复用完整原请求和事件游标；耗尽后可选择“恢复连接”。这只恢复订阅，不重跑失败的业务。

“新对话”关闭旧 session、取消旧 AI 编排及后续界面操作，并保留当前地图、选区、时相和已提交检测。新 session 不携带旧聊天。关闭接口幂等，关闭后的 session 拒绝新消息；已提交的检测继续运行，可在任务页查看。关闭旧会话失败时界面保留旧对话并明确提示，不能把未确认的停止显示为成功。

## Tool 与 Skill 规则

每个 Tool 由 `name`、`version`、输入/输出 JSON Schema、effect、超时、重试策略和最大输出大小组成。输入 schema 拒绝未知字段；执行器再次校验领域约束；输出经过路径、密钥、SQL、shell 和 traceback 脱敏。

只读 Tool 包括：

```text
workspace.context, region.search, imagery.catalog, imagery.scenes,
imagery.phase, model.list, image.list, task.search, task.status,
result.summary, result.patches.query
```

准备与计算 Tool 包括：

```text
selection.validate, detection.plan.create, imagery.prepare,
detection.workflow.submit, operation.status, result.export.prepare
```

UI command 只允许选择区域、加载时相、设置已有 AOI、地图定位、打开任务/结果/报告及通过共享工作区入口提交检测。没有源影像导入/删除、模型修改、系统配置修改、通用文件系统、shell、SQL 或任意 HTTP Tool。

Skill 位于 `backend-go/internal/ai/skills/<skill-name>/`，每个目录包含标准 `SKILL.md`（YAML frontmatter 的 name、description 和 Markdown 正文）及按需参考 `references/*.md`。平台扩展 `runtime.yaml` 只定义稳定 ID、版本、闭合 schema、工具权限、前置条件和预算，不存放行为指令。完整目录可独立阅读、版本化及分发，正文与参考不写入 Go 字符串。通用选择、意图和执行提示词在相邻 `prompts/*.md`。

采用 [Agent Skills 渐进披露](https://learn.chatgpt.com/docs/build-skills)：`skill_selection` 阶段只提供名称、描述及选择用 ID；选中后，`intent` 阶段才加载对应 SKILL.md 正文与槽位 schema；`execution` 仅使用当前 Skill 的工具。条件参考不自动注入，模型通过受限 `skill.read_reference(path)` 读取当前包内指定参考，禁止跨包和任意文件路径。`GET /api/ai/skills` 也只返回元数据。

默认将目录嵌入 Go 二进制；更新后需重建并重启。开发或独立分发可将 `AI_SKILLS_DIR` 指向包含这些 Skill 目录的绝对路径，启动时校验并加载；更新文件后重启 Go 即可，无需重新编译。加载失败直接报错，不静默退回旧包。执行器负责权限、前置条件、最大调用次数和持久化；模型负责选 Skill、填槽位、选择必要工具及说明结果。

## 可支持的用户输入假设预案

自然语言先映射为 `operation`、`admin_path`、`region_id`、`aoi`、`date_range`、`phases`、`pair_policy`、`model_id`、`result_filter`、`output_format` 和 `ui_context`。缺少关键槽位时必须追问，不能猜测地图中心、坐标、模型或日期。

正常自然语言请求先通过无副作用的 `select_skills` 选择相关能力，再仅加载选中 Skill 进行 `route_intent` 语义解析。解析依据同一会话的用户与最终回答、当前工作区和待澄清槽位，返回最多四个有序操作；槽位经过对应 Skill 的闭合 schema 校验。没有明确检测意图时不默认进入检测，“查看上次检测结果”“导出变化图斑”分别进入结果查询和导出。后续执行仍验证资源及前置条件，不把上一轮或上一操作的工具成功状态当作本操作前置成功。

执行中需要补充信息时通过 `request_clarification` 明确暂停当前及后续操作。组合请求只能继承前一操作返回的已验证任务引用；多个任务需要用户选择，不能转而使用旧工作区任务。运行过程按聊天 workflow 分组，固定高度显示摘要，结束后整体折叠；最终 Markdown、错误及受控下载链接始终保留在折叠区域外。Markdown 禁用原始 HTML、图片加载和危险链接协议。

### 行政区

“帮我查河北省保定市雄县 2025 年 1 月到 7 月的变化”先调用 `region.search`。唯一匹配才继续；重名或层级不完整时返回候选。行政区名称不会自动扩大到上级区域。

行政区边界只有在单个、闭合、无孔、非自交 Polygon 时才可作为检测 AOI。MultiPolygon、有孔或无效环必须要求用户选择独立子区域或绘制合法区域，不能取包围盒。

### 日期与时相

“2025 年 1 月到 7 月”默认使用范围内最早和最晚有效时相，并展示实际日期、日期精度和缺失月份。“逐月检查”使用相邻时相组，缺失月份不伪造数据；“只比较 1 月和 7 月”只创建指定的一组。相对日期以目录最新有效日期为基准，并说明实际起止日期。

日期值必须携带 `precision=day|month|year`。目录只有月份或年份精度时，系统不生成虚构的具体日。

### 共享检测入口

变化检测 Skill 不向模型开放 `detection.plan.create → imagery.prepare → operation.status → detection.workflow.submit` 底层链。区域、AOI 和参数已经就绪时，正常路径是 `workspace.context → ui.submit_detection`。底层领域工具仍供受控集成及 API 回归使用。

`ui.submit_detection` 的模型输入只允许 expected_revision；日期、区域、AOI、模型和检测参数由服务端绑定本轮校验后的槽位，执行模型不能改写这些条件。前端命令和右下角按钮都调用 `useWorkspace.submitDetection`。例如“2025-9/2025-3 两个时相对比”归一为 phase_dates=[2025-03,2025-09]，从四时相目录中精确替换为两个，不追加中间月份。已有框选和面板自定义参数保持；只有明确槽位覆盖相应参数。准备与任务创建后返回 task_id 和 pair_count，持续轮询由共享工作区负责，AI 不等待推理完成或再次提交。

工作区版本由同步观察器统一生成，回执返回操作后稳定版本及最新快照；模型使用回执版本继续。面板参数变化也使旧版本失效。无变化操作不更新版本；同一操作重复失败必须修正输入或澄清，达到调用上限立即失败，检测没有创建任务时不能以文本回答伪装成功。

### 当前工作区与已加载时相

“看我刚才框选的区域”使用 `workspace.context`。没有合法 AOI 时要求用户绘制或提供行政区。使用当前加载影像前，校验区域一致、场景仍可用、工作区版本未过期、空间范围有交集且模型可用。

### 模型、筛选和解释

“用默认模型”只能使用当前配置中已验证且启用的模型；指定模型必须匹配 `model.list` 返回的 opaque `model_id`。系统当前只提供变化掩膜和模型分数，不能把结果解释成建筑、道路或违法占地分类。

“只看面积大于 500 平方米的变化，列出最大的 20 个”由 `result.patches.query(area_min=500, sort=area_desc, limit=20)` 服务端筛选，模型不读取全量 GeoJSON 自行计算。

### 导出、历史与失败恢复

“把结果导出成 Shapefile”调用 `result.export.prepare`，返回受控下载引用，不返回文件路径。“继续上次检测”使用 `task.search` 或 workflow ID 查找状态；重试只创建失败时相组的新任务，保留原任务和结果引用。

“这附近”“最近”“全部区域”“高分辨率”“效果最好”“重点区域”等表达在没有唯一工作区上下文时必须澄清。

## 资源与状态引用

`plan_id` 冻结检测计划，`operation_id` 表示影像准备，`workflow_id` 表示 AI/检测编排，`batch_id` 表示已冻结配置和模型的影像批次，`task_id` 表示单组检测，`result_id` 表示结构化结果。重复 request ID 加规范化输入只执行一次；配置、目录或模型版本变化返回 `PLAN_STALE`，不会静默替换资源。

允许写入的只有派生影像、检测任务、检测结果和导出引用。首期按单机/内网单用户运行，不包含权限、审批和多租户隔离。

## 验证入口

```text
GET  /api/ai/status
GET  /api/ai/tools
GET  /api/ai/skills
POST /api/ai/sessions
POST /api/ai/sessions/:id/messages
GET  /api/ai/sessions/:id
POST /api/ai/sessions/:id/close
GET  /api/ai/workflows/:id
GET  /api/ai/workflows/:id/events
POST /api/ai/workflows/:id/cancel
```

Tool 直接调用使用 `tool-call.v1`，所有响应使用 `tool-result.v1`。真实云端 provider 之外，可通过 Go runtime 的 Provider 注入点使用 mock provider 做 Tool Call、UI ack、重试、幂等和 SSE 断线恢复测试。

每条新消息首先发出持久化 `workflow_started` 事件，包含 `session_id`、`workflow_id` 和 `request_id`；`final` 明确携带 `message` 和状态，取消使用独立 `cancelled` 终态。持久化消息通过可选 `workflow_id` 与 `phase=intermediate|final` 关联过程和结果，旧记录仍可恢复。新消息的 request ID 绑定规范化文本、完整工作区快照及显式 Skill ID，重连不得替换输入。浏览器验收运行 `scripts/test_ai_browser.mjs`，其中集成 `test_ai_browser_reliability.mjs` 的实时 SSE 检查；验收使用隔离夹具，不调用真实模型或创建真实检测。
