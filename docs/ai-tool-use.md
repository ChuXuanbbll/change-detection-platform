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

聊天连接关闭不会取消已启动的服务端 workflow。刷新页面后，session、workflow 和 SSE 事件可通过 session/workflow 接口恢复；SSE 使用 `Last-Event-ID` 继续读取未消费事件。

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

UI command 只允许选择区域、加载时相、设置已有 AOI、地图定位、打开任务/结果/报告。没有源影像导入/删除、模型修改、系统配置修改、通用文件系统、shell、SQL 或任意 HTTP Tool。

Skill 文件位于 `backend-go/internal/ai/skills/`。Skill 是有限步骤状态机，执行器负责前置步骤、最大步数、超时、重试和持久化；LLM 只负责自然语言槽位填充和结果说明。

## 可支持的用户输入假设预案

自然语言先映射为 `operation`、`admin_path`、`region_id`、`aoi`、`date_range`、`phases`、`pair_policy`、`model_id`、`result_filter`、`output_format` 和 `ui_context`。缺少关键槽位时必须追问，不能猜测地图中心、坐标、模型或日期。

### 行政区

“帮我查河北省保定市雄县 2025 年 1 月到 7 月的变化”先调用 `region.search`。唯一匹配才继续；重名或层级不完整时返回候选。行政区名称不会自动扩大到上级区域。

行政区边界只有在单个、闭合、无孔、非自交 Polygon 时才可作为检测 AOI。MultiPolygon、有孔或无效环必须要求用户选择独立子区域或绘制合法区域，不能取包围盒。

### 日期与时相

“2025 年 1 月到 7 月”默认使用范围内最早和最晚有效时相，并展示实际日期、日期精度和缺失月份。“逐月检查”使用相邻时相组，缺失月份不伪造数据；“只比较 1 月和 7 月”只创建指定的一组。相对日期以目录最新有效日期为基准，并说明实际起止日期。

日期值必须携带 `precision=day|month|year`。目录只有月份或年份精度时，系统不生成虚构的具体日。

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
GET  /api/ai/workflows/:id
GET  /api/ai/workflows/:id/events
POST /api/ai/workflows/:id/cancel
```

Tool 直接调用使用 `tool-call.v1`，所有响应使用 `tool-result.v1`。真实云端 provider 之外，可通过 Go runtime 的 Provider 注入点使用 mock provider 做 Tool Call、UI ack、重试、幂等和 SSE 断线恢复测试。
