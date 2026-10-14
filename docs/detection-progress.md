# 检测进度

按 2026-10-12 的前端编排、Go 采样和 Python/模型快照实现核对。GPU 历史证据范围见 [系统状态](CURRENT_STATUS.md)；本轮未重新运行验收。

总览显示当前阶段和总体百分比。影像准备占 20%，相邻时相检测组均分其余 80%。这是流程完成度，不是预计耗时。只有所有组的结果成功落库后显示 100%；地图预览加载不计入检测完成条件。

## 接口

`POST /api/images/local` 使用同步裁切响应和可选 UUID 字段 `progress_id`。前端为每次提交生成新 ID，同时每秒查询 `GET /api/images/local/progress/:id`：

```json
{"id":"7bc91ad3-14b1-429c-b670-bf43d7691acd","status":"running","stage":"mosaicking","progress":47.5}
```

`progress` 是本次影像准备的 0–100 进度，乘 0.2 得到总体贡献。`status` 为 `running`、`succeeded` 或 `failed`。记录由当前 Go 实例管理，请求结束后保留 10 分钟；未创建、已过期或服务重启后返回 404。重复 ID 返回 409，避免重复请求覆盖已有进度。未提供 ID 的旧客户端行为保持不变；任务调度仍由前端顺序提交，刷新不会自动提交后续组。

`GET /api/tasks/:id` 返回 `stage`、整数 `progress` 和状态字段。任务进度和阶段持久化到 SQLite，启动时自动添加旧库缺少的 `stage` 列。

| 阶段 | 单组进度 |
| --- | --- |
| `queued` / `aligning` | 0–5 |
| `model_loading` | 5–10 |
| `inference` | 10–85，按实际完成切片汇总 |
| `reprojecting` | 85–90 |
| `postprocessing` | 90–94 |
| `vectorizing` | 94–98，按处理完的连通域计数，包含过滤掉的图斑 |
| `statistics` | 98–99 |
| `saving` / `completed` | 99 / 100，100 由结果落库事务提交 |

准备阶段包括 `preparing`、`reading_images`、`cropping`、`mosaicking` 和 `saving_images`。无法进一步计数的同步计算保持当前值，界面通过忙碌动画表示仍在执行。快速阶段可能在两次轮询之间完成，这是正常行为。

## 上报与故障语义

Python 在当前输出目录的 `progress/stage.json` 中写入阶段和百分比；模型通过可选 `--progress_dir` 参数把各 worker 的 `rank/world/done/total` 写入独立的 `worker-N.json`。快照使用临时文件和原子替换，同阶段最多每 250 毫秒写一次，worker 最后一批强制写入。Go 每 500 毫秒采样，只有所有 worker 的合法快照都存在时才汇总推理进度。

快照写入或读取失败不会中断检测，保留最后有效进度；终态与输出结果以 HTTP 响应和 SQLite 事务为准。结束采集会等待采集线程退出，数据库和准备记录同时拒绝迟到的进度更新。失败保留最后百分比和阶段，前端停止忙碌动画并展示错误。

前端每秒查询，失败时指数退避至 15 秒，查询超时为 10 秒；过期批次或已结束准备阶段的响应会被丢弃。进度条只在两个已确认值之间做 400 毫秒过渡，尊重减少动画设置，不以时间虚增。

## 验证

在项目根目录执行现有 Go 测试、Python unittest、前端构建及 `node --test scripts/test_frontend_workflow.mjs`。Go 测试覆盖中间快照、多 worker、终态保护和旧库迁移，Python 测试通过真实切片循环验证单切片、整批及最后不足整批情况。

Windows 真实 GPU 验收：

```powershell
backend-py/.venv-local/Scripts/python.exe -X utf8 scripts/validate_progress.py --browser
```

使用独立端口、数据库和 Vite 缓存；保留 `data/validation/progress-*/report.json`、采样数据和浏览器截图，结束后关闭自己启动的服务。浏览器部分需要 Chrome 和 Playwright，可用 `PLAYWRIGHT_MODULE` 指向已有安装。GPU 验收要求任务完成前至少观察到两个不同的推理中间百分比。
