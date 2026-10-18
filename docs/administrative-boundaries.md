# 县级行政区知识库与影像来源

影像来源保留原有 14 个入口、ID 和影像归属。行政区是独立的省／地级市／县级树；`parent_code` 不用于确定影像来源。全省地图显示 14 个影像来源区域的边界和名称，均可通过鼠标交互进入；进入来源后显示对应县区边界和名字。知识库仍保留 11 个地级市的行政区层级。

## 数据与版本

- 来源：[国家基础地理信息中心天地图行政区划](https://cloudcenter.tianditu.gov.cn/administrativeDivision)，页面声明数据更新于 **2025-09**。这不是 2026 年实时行政区变更清单。
- 本地快照 `data/boundaries/hebei-administrative.geojson` 包含 1 省、11 个地级市、167 个县级区域，保留源接口坐标，没有进行 GCJ-02 逆转换。父子关系记录来源的导航层级，不把代管关系描述为法定隶属关系。
- 快照记录发布者、页面、公开接口 URL、原始响应 SHA256、采集时间、数据日期和用途说明。天地图声明这些轮廓仅供地图可视化；本系统把这个已知版本的公开轮廓用作检测空间掩膜，不声称法定勘界或测绘精度。
- SQLite `meta.db` 的 `administrative_datasets`、`administrative_regions` 保存完整几何和来源元数据。版本为数据集 ID 加快照全文 SHA256。Go 启动时离线、幂等、事务导入；历史版本保留。Python 只读同一个数据库。
- 原 DataV 数据及源影像、模型不改动。`hebei-source-groups.geojson` 是新县界按业务归属求并集得到的 14 个来源轮廓；省界由同一快照的 11 市求并集。

特殊归属：雄安来源包含容城 `130629`、安新 `130632`、雄县 `130638`；保定来源排除这三县及定州 `130682`；石家庄来源排除辛集 `130181`。定州、辛集保持独立来源。省级知识库仍保留原行政树父节点，并未把雄安伪造为第 12 个地级市。

## 浏览与检测

1. 点击来源、拖入整月时相的流程保留。选择县区只定位、画轮廓，影像来源列表的标题、月份和可用影像张数保持不变，保留已加载时相与手绘选区。
2. 空间筛选以县界与场景 WGS84 覆盖包围盒的正面积交集判断，是**候选**筛选，不保证县区内存在有效像元。此筛选供接口查询使用，县区浏览不触发列表筛选或显示县内候选张数。拖入继续加载完整来源月时相，使用原场景集及瓦片版本。
3. 点击“以某县界作为检测范围”，或明确要求 AI 按完整县界检测，才替换检测选区。使用 `admin_code` 和 `admin_version` 查询数据库；完整 Polygon／MultiPolygon 的全部部件与孔洞一路保留到裁切、有效像元掩膜和结果。
4. Go 与 Python 都核对县级、来源、版本以及提交的几何。不同来源、未知版本、包围盒代替县界等请求被拒绝。AI 的 `region.search` 返回可信代码、来源和版本；只有 `use_admin_boundary=true` 才由服务器绑定县界引用，模型不生成县界坐标。
5. 县界是完整目标范围，栅格仅计算所选日期影像的实际有效覆盖，未覆盖处不补数据。任一时相完全无覆盖时失败并提示，不能把没有影像解读为没有变化。县界可能很大，实际资源消耗取决于分辨率和覆盖范围。

## API 与更新

- `GET /api/administrative-regions?level=prefecture|county|province&source_id=...`
- `GET /api/administrative-regions/{六位代码}?version=...`
- `GET /py/local/regions/{来源ID}/scenes?admin_code=...`
- 裁切／像元尺寸请求新增可选 `admin_code`、`admin_version`。手绘选区继续使用单一无孔 Polygon。

在项目根目录显式更新：

```powershell
backend-py/.venv-local/Scripts/python.exe scripts/prepare_admin_boundaries.py --download --data-date 2025-09
```

不带 `--download` 只从现有快照离线重建来源与省界。下载必须先核对来源页面的数据日期，再传入 `--data-date`，不能把采集日期当作数据日期。应用启动不访问远程边界接口。更新时核对树与边界代码、几何有效性、父子覆盖、来源分组和数据日期；节点数变化时脚本暂停发布，需审查变更并调整快照校验；不能用猜测补齐。容器通过只读边界目录挂载读取；Go 与 Python 必须共享同一 `DATA_DIR/meta.db`。

验证入口：`go test ./internal/store ./internal/api ./internal/ai`、Python `tests.test_administrative`、`node scripts/test_admin_boundaries_browser.mjs`。浏览器使用真实入库快照形状和隔离的影像／任务夹具，不触发真实全县推理。
