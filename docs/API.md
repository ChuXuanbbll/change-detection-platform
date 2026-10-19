# 当前接口

按 2026-10-12 的 Go 路由/handler 和 Python router 核对。下文为当前约定，不沿用迁移报告的旧算法或上传接口。

所有浏览器管理请求通过 Go `/api`。请求错误保留 `error` / `code`，Python 校验细节保留在上游错误中。路径属于服务运行主机。模型、配置接口只接受本机、合法 Host/Origin 和 JSON 或二进制请求。

## 配置

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/settings` | `revision/settings/effective/error/history/diagnostics` |
| POST | `/api/settings/validate` | Settings 文档，返回异步操作 |
| POST | `/api/settings/apply` | `{expected_revision, settings}`，冲突 409 |
| POST | `/api/settings/rollback` | `{expected_revision, rollback_revision}`，生成新版本 |
| POST | `/api/settings/sync` | `{}`，重新将持久化版本同步到 Python |
| GET | `/api/settings/operations/:id` | `running/succeeded/failed`，结果或具体错误 |

Settings 包含 `sources/default_model/detection/runtime/tiles/browser`。字段、范围和默认值以 `backend-py/app/settings.py` 为准。来源 `sources` 为有序数组，字段 `id/name/path/year/enabled`；`year` 可省略或为 null。生效前准备失败不会替换旧配置。

## 模型

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/models` | 登记、哈希、状态、启用、映射、输入输出、错误 |
| POST | `/api/models/import` | JSON `{path,name}` 或 `application/octet-stream` 原始 ONNX；上传名用 `?name=` |
| POST | `/api/models/:id/validate` | `{mapping:{input_a,input_b,output}}`；真实 CUDA 验证 |
| PUT | `/api/models/:id` | `{name,description,enabled}` |
| DELETE | `/api/models/:id` | 移除未被配置/批次引用的登记，保留制品 |

导入和验证返回 202 与操作 ID；重复哈希返回 200 与已有模型。状态为 `unvalidated/needs_mapping/invalid/ready`，只有 ready 且启用的模型能创建新批次。

## 检测工作流

1. GET `/py/local/regions`，GET `/py/local/regions/:id/scenes` 获取区域、场景、同月时相。
2. POST `/api/images/local`：`bounds:[west,south,east,north]`、`region_id`、`scene_ids`、`model_id`、`config_revision`、`params`，以及可选 `aoi` 多边形和 UUID `progress_id`。成功裁切并原子持久化影像元数据后返回 201 与 `items/count/batch_id`；兼容日期查询无数据时可返回 200、空 `items`，不能据此提交检测。
3. POST `/api/tasks`：`batch_id/before_id/after_id`。实际算法与参数来自该批次快照；必须是同批次影像，队列满返回 503。
4. GET `/api/tasks/:id` 查询 `pending/running/succeeded/failed`、整数 `progress`、`stage`、错误及时间。准备阶段使用 GET `/api/images/local/progress/:id`；阶段、权重与记录有效期见 [检测进度](detection-progress.md)。
5. GET `/api/tasks/:id/result` 返回结果路径、`stats_json` 和 GeoJSON。统计包含 `execution`（版本、模型、哈希、批次、实际运行参数）与 `parameters`。
6. GET `/api/tasks/:id/result/mask.tif`、`change.geojson`、`stats.csv`、`change.zip` 下载。

成功状态和结果入库是同一事务。服务重启将未完成任务标记为中断失败，不自动重复计算。`/api/images/wayback` 是旧客户端到本地裁切的兼容别名，不包含网络影像下载器。

### 多边形选区

手绘 `aoi` 为 WGS84 GeoJSON Polygon，坐标顺序 `[经度, 纬度]`，仅支持单个无孔闭合环。示例：

```json
{"type":"Polygon","coordinates":[[[116.2573,39.4158],[116.2578,39.4158],[116.2573,39.4163],[116.2573,39.4158]]]}
```

提供 `aoi` 时必须指定 `region_id`；至少三个不同顶点，禁止自交、重复顶点、零面积及越出当前行政区（包括包围行政区孔洞）。非法几何返回 400。仅容许 `1e-9` 度边界数值误差，不自动修复几何。保留必填的合法 `bounds` 字段以兼容旧接口；实际裁切包围盒由多边形计算，省略 `aoi` 沿用矩形规则。

Python `/py/local/fetch`、兼容的 `/py/wayback/fetch` 和 `/py/local/selection-pixels` 使用相同 `aoi` 字段。像元预估返回矩形网格的 `width/height/pixels`，不表示多边形内有效像元数，且不读取源影像像元。

多边形裁切响应额外返回已验证的 `aoi`；Go 对照请求检查该确认字段，缺失或不匹配返回 502 `AOI_NOT_CONFIRMED` 并清理失败批次。前端还核对每张裁切影像的 `meta_json.aoi/region_id`，防止旧 Go 服务忽略几何后继续提交矩形检测。矩形请求无需确认字段。

裁切及检测产物仍为矩形 GeoTIFF，内部有效掩膜区分多边形外像元与区内有效黑色像元。最终变化和置信度栅格保留共同有效掩膜，区域外填零；变化比例和直方图仅统计共同有效像元。矢量结果描述按像元中心筛选后的像元覆盖范围。缩略图按有效掩膜透明显示；影像 `meta_json` 包含 `aoi/region_id/bounds`，其中 `bounds` 为实际裁切网格范围。

### 县级行政区边界

GET `/api/administrative-regions?level=province|prefecture|county&source_id=...` 查询共享 SQLite 知识库；GET `/api/administrative-regions/:code?version=...` 获取指定代码／版本的完整边界。来源、数据日期、特殊来源映射与更新方法见 [行政区知识库](administrative-boundaries.md)。

县界检测在 POST `/api/images/local` 和 `/py/local/selection-pixels` 中传入 `admin_code`、`admin_version`，并使用所查边界的合法 `bounds`、对应 `region_id`（影像来源 ID）及 `scene_ids`。县界由服务端数据库解析，省略版本时冻结当前版本。允许该可信边界包含 MultiPolygon 和孔洞；如另传 `aoi`，必须与入库边界完全一致。来源／版本不匹配或轮廓被替换时拒绝；不允许用包围盒代替边界。裁切影像元数据保留 `admin_code/admin_version/aoi`，前端核对这些字段后才继续检测。

GET `/py/local/regions/:source_id/scenes?admin_code=...` 返回与县界相交的候选月份。`matching_scene_count` 表示县内候选场景数；时相仍保留整个来源月份的场景集和版本，拖入行为不变。县区浏览不替换已加载时相或检测选区。

## 展示

- GET `/py/local/catalog` 与 `?refresh=true`：目录及可用性诊断。
- GET `/py/local/regions/:id/mosaic/{z}/{x}/{y}.png?scene_ids=...&v=...`：同月合成，场景 ID 必填、版本可选；过期版本返回 409。
- POST `/py/local/selection-pixels`：JSON `bounds/region_id/scene_ids` 与可选 `aoi`；预计网格像元数，不做容量硬拒绝。
- GET `/api/images`：裁切影像登记，支持 `source/phase/limit`；GET `/api/images/:id/preview` 按需生成缩略图，失败可重试。
- DELETE `/api/images/:id`：删除裁切影像登记及其影像/缩略图文件，不是管理来源删除或历史结果回收。
- POST `/py/overlay/image`、POST `/py/overlay/mask`：受管数据目录内预览；前者使用 `image_path`、可选 `max_edge`，后者使用 `mask_path`、可选颜色和透明度。
- GET `/api/health`、Go `/health`、Python `/`：服务健康，不等于配置生效或 CUDA 可用。

年度影像、单景瓦片与外部显示源兼容接口见 [行政区与显示源](imagery-integration.md)。当前概览主流程使用 `/scenes` 的 `phases` 与 `/mosaic`。

`/py/admin/*`、`/py/detect` 和原始计算写入口只允许 Go 内部令牌请求，浏览器不直接调用。算法列表 `/api/algorithms` 表示适配协议，模型选择以 `/api/models` 为准。
