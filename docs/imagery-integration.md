# 河北影像来源与兼容显示源

当前概览按影像来源添加月时相并请求合成瓦片，检测输入通过 [管理](MANAGEMENT.md) 登记；[本地影像](LOCAL_IMAGERY.md) 为检测数据主说明。行政树、边界版本、县界检测及来源映射见 [县级行政区知识库](administrative-boundaries.md)。

年度影像及外部显示接口仍保留在代码中，年度下拉框仅用于地图组件的非工作区分支。不要将其描述为当前概览主入口；是否接入主工作区或收缩兼容范围尚待用户选择。

## 区域键与边界

默认导航保留 14 个影像来源，原有点击、拖入整月时相的业务流程保留。全省地图显示这 14 个来源区域的边界与名称，支持鼠标交互进入；进入来源后显示其县级边界与县区名。普通县区导航只定位和筛选候选，不改变已加载时相或选区；明确选择“以某县边界作为检测范围”后才替换选区。当前没有单景检测像元硬上限。

| 名称 | region_id |
|---|---|
| 保定市 | 130600 |
| 沧州市 | 130900 |
| 承德市 | 130800 |
| 定州市 | 130682 |
| 邯郸市 | 130400 |
| 衡水市 | 131100 |
| 廊坊市 | 131000 |
| 秦皇岛市 | 130300 |
| 石家庄市 | 130100 |
| 唐山市 | 130200 |
| 辛集市 | 130181 |
| 邢台市 | 130500 |
| 雄安新区 | xiongan |
| 张家口市 | 130700 |

这是应用稳定键，不是统一地级行政区编码。定州、辛集使用县级代码；雄安为应用键。

当前轮廓来自天地图 2025-09 公开行政区划快照，位于 `data/boundaries/hebei-administrative.geojson`，由 Go 离线导入共享 SQLite 知识库。`hebei-source-groups.geojson` 是按县级归属生成的 14 个来源轮廓，`hebei-administrative-province.geojson` 为同一版本省界。雄安来源是容城、安新、雄县联合范围，不是另行取得的规划范围。公共可视化边界不能替代测绘级边界。

原 `hebei-regions.geojson`、DataV/AMap 快照及 `regions-provenance.json` 保留作历史参考。当前数据的来源 URL、响应 SHA-256、数据日期与采集日期记录在新快照中。离线重建来源与省界时从项目根目录运行：

```powershell
backend-py/.venv-local/Scripts/python.exe scripts/prepare_admin_boundaries.py
```

显式下载新版本的流程与校验条件见县级行政区知识库文档；边界变更后需重启服务导入并同步缓存。

## 当前主流程与兼容接口

| 请求 | 用途与范围 |
|---|---|
| GET `/py/local/regions` | 区域 GeoJSON、省界与省范围 |
| GET `/api/administrative-regions?level=county&source_id=...` | 共享知识库中的行政区边界；也支持 prefecture、province |
| GET `/py/local/regions/{region_id}/scenes` | 当前主流程场景和同月 `phases`，支持 `?refresh=true` |
| GET `/py/local/regions/{region_id}/mosaic/{z}/{x}/{y}.png?scene_ids=...&v=...` | 当前选定月时相合成瓦片 |
| GET `/py/local/regions/{region_id}/imagery?year=2025` | 兼容年度显示源；省略 year 取最新可用年 |
| GET `/py/local/regions/{region_id}/tiles/{scene}/{z}/{x}/{y}.png` | 区域单景瓦片 |
| GET `/py/local/tiles/{scene}/{z}/{x}/{y}.png` | 兼容单景瓦片 |

年度响应为 `region_id/name/years/selected_year/sources`；年份降序，无数据时 `sources=[]`，完全无年份时 `selected_year=null`。同年同覆盖块按 date、id 取最新版本，多个覆盖块可一起显示；不是当前检测的同月拼接规则。

## 本地栅格兼容清单

当前推荐在管理页登记本地/共享目录，按年份、行政区和月份目录建立索引；月份不从文件名猜测。旧 `catalog.json` 保留兼容，可在登记的影像目录中使用清单：

```json
{
  "images": [
    {
      "id": "scene-baoding-example",
      "file": "baoding.tif",
      "region_id": "130600",
      "region": "保定覆盖块A",
      "date": "2025-01-01",
      "date_precision": "year"
    }
  ]
}
```

ID/文件须稳定且可定位；年度日期只用于排序，不声称真实采集日。`region` 是年度兼容接口的覆盖块键，多个块需不同键。当前工作区检测仍按月时相组织，不应把仅知年度的锚点日期解释为真实月份。

栅格须有 CRS、至少三波段 uint8 RGB；前三波段用于当前展示/检测。16 位反射率、多光谱、灰度需另外约定转换或模型协议，不能直接宣称平台已支持。大栅格可准备带内部瓦片/overview 的 COG，伴随金字塔文件随原始数据单独交付；当前样例清单和可用性以服务目录响应为准，不在此固定机器影像计数。

## 外部 XYZ / TMS / WMS 兼容显示

在资源目录 `data/imagery/map-sources.json`（自定义部署相对 `ASSET_DIR`）登记；接口读取该文件不代表当前管理页支持编辑它：

```json
{
  "schema_version": 1,
  "sources": [
    {
      "id": "baoding-display-example",
      "region_id": "130600",
      "year": 2025,
      "type": "xyz",
      "url": "https://imagery.example.com/2025/{z}/{x}/{y}.png",
      "crs": "EPSG:3857",
      "attribution": "影像提供方"
    }
  ]
}
```

示例 URL 必须替换。ID 唯一；year 为 1900–9999 整数；可选 `bounds` 为 WGS84 `[west,south,east,north]`。替代版本声明相同 `mosaic_key`，按 `date/id` 取最新。URL 支持同源路径或 HTTP(S)，浏览器直连上游，需满足跨域、协议及鉴权条件，不能放私密凭据。

| 格式 | 当前兼容范围 |
|---|---|
| XYZ / TMS | EPSG:3857 标准全球网格，含 `{z}/{x}/{y}`；TMS 翻转 Y |
| WMS | 声明 `layers`，可选 version/format；支持 EPSG:3857 或 EPSG:4326 |
| WMTS | 仅可由提供方转成标准 XYZ 模板；其他矩阵尚无直接适配 |
| MBTiles / PMTiles | 需外部服务暴露 XYZ，当前不直接读取归档 |
| 无地理参考 PNG/JPEG | 需先配准或由服务提供明确网格 |

GCJ-02/BD-09 图源不能直接作为 WGS84 遥感影像叠加。外部显示源不进入月时相检测；需检测时另行提供当前协议支持的原始栅格。

## 刷新与验证

完整文件/清单准备后原子替换 JSON，避免半写入状态。清单按版本化文件元数据读取，目录刷新后场景/时相版本变化会失效相关缓存；边界仍需重启。只读目录缓存写到运行数据目录，不回写共享盘。

专项测试为 `backend-py/tests/test_regions.py`、`test_catalog_lifecycle.py` 和 `scripts/test_regions_browser.mjs`；主流程还需 `scripts/test_imagery_workspace_browser.mjs`，它验证真实影像浏览而非模型推理。执行条件见 [开发与测试](DEVELOPMENT.md)，本轮未运行。
