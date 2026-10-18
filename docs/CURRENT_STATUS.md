# 当前系统状态

按 2026-10-12 代码基线 `ce3d52d`、配置及现存本机报告核对。本次只整理文档，没有重新执行产品测试、模型推理或部署验收。

## 已实现

| 能力 | 当前行为 | 主要依据 |
|---|---|---|
| 四页工作区 | 概览、对比、报告、管理；KeepAlive 与应用级共享工作区 | `frontend/src/router/index.ts`、`composables/useWorkspace.ts` |
| 行政区与月时相 | 14 个影像来源；全省显示 14 个来源区域，来源内显示县区；共享知识库保留 11 市、167 县，支持可信完整县界检测（2026-10-16 更新） | [行政区知识库](administrative-boundaries.md)、`fetchers/regions.py`、`fetchers/local.py` |
| 检测选区 | 矩形、无孔单环 Polygon，顶点/边编辑；服务端几何与行政区覆盖校验 | `MapBasemap.vue`、`utils/selectionGeometry.ts` |
| 版本化管理 | 模型、多来源、检测/性能配置、历史与回退；Go/Python 同版本后允许新批次 | `internal/api/management.go`、`app/settings.py` |
| 模型执行 | 平台只有 `hebei_onnx` 适配器；已验证且启用的兼容 ONNX，CUDA 子进程执行 | `algorithms/hebei_onnx.py`、`utils/model_process.py` |
| 批次快照 | 裁切时冻结模型哈希、映射、版本和最终参数，后续任务读取同一快照 | `internal/api/images.go`、`tasks.go` |
| 任务与进度 | 浏览器顺序提交相邻组；Go 队列；准备 20%、检测 80%；结果/成功状态同事务 | `useDetection.ts`、`internal/progress`、`store/transactions.go` |
| 地图瓦片 | 渲染进程、共享任务、取消等待、503/429 重试、409 刷新，低清预加载与时相缓存 | `utils/tile_queue.py`、`utils/tile_worker.py`、`utils/phaseLayerCache.ts` |
| 结果 | 三图联动、热点、分页图斑、置信度直方图；TIFF/GeoJSON/CSV/Shapefile ZIP | `Comparison.vue`、`Reports.vue`、`internal/api/tasks.go` |

前端简称路径相对 `frontend/src`，Python 算法/工具/影像路径相对 `backend-py/app`，Go 路径相对 `backend-go`。具体规则见 [文档索引](README.md)。

检测输入为本地/服务端可读共享目录中的有 CRS、至少三波段的 uint8 RGB 栅格。同月拼接优先使用分辨率较细的有效像元，同分辨率按来源顺序和相对路径。年度 XYZ/TMS/WMS 显示接口仍在，概览主流程使用月时相合成；外部显示源不会自动成为检测输入。

## 运行与默认值

- 原生入口：前端 5173、Go 8080、Python 8090；本机管理使用回环访问与内部控制令牌。
- `DATA_DIR` 原生默认为项目 `backend-go/data`，`ASSET_DIR` 默认为项目 `data`；启动器固定前者，手动部署须保证 Go/Python 一致。
- 首次影像来源默认 `Y:\河北影像\2025年`，之后以 SQLite 生效配置为准；不保证其他机器存在该映射盘。
- 检测默认：阈值 0.5、输出分辨率 0.8 米、核大小 3、最小面积 10 像元；GPU 0、批大小 1、预取 2、超时 3600 秒、检测 worker 1、队列 128。
- 瓦片默认：4 进程、8 在途、等待 1000 ms、512 项/64 MiB PNG 缓存、每进程 8 句柄/128 MiB GDAL 缓存；浏览器默认 4 请求、3 个非活动时相、128 MiB、300 秒闲置。

依据为 `backend-py/app/settings.py`。`backend-go/config.yaml` 的历史 `queue.workers: 2` 不是生效并发依据，管理同步后由 `runtime.workers` 控制。已有 25 ms 配置不会随新版默认值自动更新。

## 已知边界

- 切页继续批次；刷新/关闭丢失尚未提交组的编排。已提交任务继续；Go 重启将未完成任务标为中断失败，不自动重算。没有服务端批次续跑或前端历史批次恢复入口。
- 平台检测没有 CPU 模式。独立 CLI 保留 CPU/auto 和多 GPU 参数；平台快照只选择单个 GPU，不能将 CLI 能力当作产品入口。
- 没有检测像元硬上限或自动降精度；拼接、重投影、后处理、连通域仍可能持有整幅数组，资源不足会失败。
- 不提供孔洞或多个离散选区绘制，不直接接入任意波段/反射率模型、HTTP 模型、旧传统算法或联网 Wayback 下载。
- CUDA 校验不证明训练归一化、类别含义与检测精度正确；平均置信度不等于准确率。
- 管理操作有状态和错误，没有全过程持久化进度百分比；没有自动模型/结果垃圾回收或多用户权限系统。
- 启动探测失败后仍可开放管理入口，末尾提示与 `-CheckOnly` 退出码不是 CUDA 验收结论。

## 验证证据与范围

本轮读取以下已有报告，未重新运行；它们不构成当前全部代码的完整验收：

| 本机证据 | 范围 |
|---|---|
| `data/validation/management-2w4q99u6/report.json` | `passed/cuda`，配置版本、快照、运行设置、真实推理及四种导出 |
| `data/validation/progress-pf8g_da_/report.json` | `passed/cuda`，推理中间进度 16、60 |
| `data/validation/polygon-cuda-bhgwokpx/report.json` | `passed/cuda`，多边形裁切、检测和导出 |
| `data/validation/review-20261012/workspace-selector-diagnostic/report.json` | 三个视口的确定性浏览器夹具通过，不验证推理 |
| `data/validation/review-20261012/tile-retry/report.json` | 最大并发 4、取消及过期版本不重试 |
| `data/validation/imagery-workspace-final/report.json` | 真实影像浏览与预热切换，`modelInference=false`；未清空 OS/后端缓存 |

Docker/Linux GPU、真实 UNC 断网恢复、完整大范围长时间 GPU/瓦片共存负载仍缺少完成实测的依据。阶段报告中的测试数量、审计数量与性能样本为历史结论，本轮未再次运行。候选工作见 [开发待办](ROADMAP.md)。
