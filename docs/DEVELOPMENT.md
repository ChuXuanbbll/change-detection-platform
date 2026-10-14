# 开发与测试

按 2026-10-12 代码入口与现有脚本核对。本文提供验证方法，不表示本轮已运行命令。当前能力见 [系统状态](CURRENT_STATUS.md)，未确认的未来需求见 [开发待办](ROADMAP.md)。

## 运行边界

Windows GPU 为主要运行环境。三个服务默认只监听本机：Vite 5173、Go 8080、Python 8090。解释器、端口、数据库目录是部署参数，业务参数在「管理」持久化。安装见 [环境说明](../ENVIRONMENT_SETUP.md)，生效机制见 [管理说明](MANAGEMENT.md)。

Python 与 Go 必须共享同一绝对 `DATA_DIR`；原生默认是项目 `backend-go/data`。资源目录 `ASSET_DIR` 默认为项目 `data`。Go 创建随机 `control.token`，内部计算与管理请求携带令牌；不要把它发给浏览器或写入日志。

## 代码入口

| 层 | 入口 | 职责 |
|---|---|---|
| 页面 | `frontend/src/App.vue` / `router/index.ts` | 四页 KeepAlive 与共享工作区 |
| 批次 | `composables/useDetection.ts` | 顺序提交、轮询容错、异步预览 |
| 配置 | `utils/settings.ts` / `views/Management.vue` | 版本、操作状态、模型、来源表单 |
| Go | `backend-go/internal/api/management.go` | 本机管理、配置事务、模型导入、批次快照 |
| SQLite | `internal/store/settings.go` / `transactions.go` | 迁移、恢复、原子任务完成 |
| Python | `backend-py/app/routers/admin.py` / `settings.py` | 严格校验、目录准备、瓦片进程切换 |
| 影像 | `fetchers/local.py` / `imagery_index.py` | 稳定身份、目录扫描、统一有效像元拼接 |
| 模型 | `algorithms/hebei_onnx.py` / `utils/model_process.py` | ONNX 适配、拥有的进程树、推理超时 |

模型适配与后处理分离。注册协议保留给兼容 ONNX 模型；传统算法与 HTTP 模型已删除。不得在浏览器传入任意模型路径，执行元数据只能从服务端批次快照生成。

## 验证命令

在项目根目录：

```powershell
cd frontend
npm ci
npm run build
npm run format:check
cd ../backend-go
$env:GOCACHE = Join-Path (Get-Location) '.cache/go-build'
go test ./...
go vet ./...
cd ../backend-py
$env:LOCAL_IMAGERY_DIR = (Resolve-Path ../data/imagery).Path
.venv-local/Scripts/python.exe -X utf8 -m unittest discover -s tests -q
.venv-local/Scripts/python.exe -m pip install -r requirements-dev.txt
.venv-local/Scripts/python.exe -m ruff check app
.venv-local/Scripts/python.exe -m ruff format --check app/settings.py app/routers/admin.py app/utils/model_process.py app/utils/probe_model.py tests/test_settings.py
cd ..
node --test scripts/test_frontend_workflow.mjs scripts/test_province.mjs scripts/test_region_view.mjs
backend-py/.venv-local/Scripts/python.exe -X utf8 scripts/validate_management.py --browser
backend-py/.venv-local/Scripts/python.exe -X utf8 scripts/benchmark_alignment.py
```

测试分为静态/单元、确定性浏览器夹具、真实影像浏览、真实 GPU 后端四种证据，不能互相替代。针对进度和多边形，可分别运行 `scripts/validate_progress.py --browser`、`scripts/validate_polygon.py`，均需可用 CUDA；运行前阅读脚本的数据和端口设置。`scripts/smoke_native.py` 是管理 GPU 验收兼容入口，名称不表示 CPU 测试。

Python 栅格测试应显式使用项目样例或隔离夹具：unittest 前设置 `LOCAL_IMAGERY_DIR` 为项目 `data/imagery` 的绝对路径，避免依赖初始 Y 盘。文档改动只核对引用、默认值、命令与代码一致性，无需重跑模型或改动运行数据库。

浏览器脚本需要 Playwright 与 Chrome；可用 `PLAYWRIGHT_MODULE` 指向已有安装，`FRONTEND_URL` 指定隔离 Vite 服务。`validate_management.py` 自建隔离 Python/Go/Vite 服务及数据库，结束后关闭它们，不修改现有工作实例；保留 `data/validation/management-*` 报告。

同仓库同时运行多个 Vite 实例时，为额外实例设置独立 `VITE_CACHE_DIR`（例如 `../.cache/vite-review`），避免不同配置覆盖预构建缓存并造成模块重复实例。隔离验收脚本自动设置。

Prettier 与 Ruff 固定版本。格式检查当前覆盖管理模块，扩展范围应与功能修改分开；`npm run format:management` 可格式化对应前端文件。Go 继续使用 `gofmt`。

`benchmark_alignment.py` 对照提交 `17acd04` 的原始对齐函数与现函数，在同机合成数据上比较时间、峰值 RSS、文件大小及逐像元/掩膜哈希。它测量一个栅格步骤，不代表完整 GPU 吞吐。

## 修改约束

Vue 前端遵守 [前端开发规范](FRONTEND_STANDARDS.md) 与 [frontend/AGENTS.md](../frontend/AGENTS.md)。构建包含严格模板和未使用代码检查；事件清理需要覆盖临时 DOM 重建、KeepAlive 停用/恢复及卸载。

- 参数/来源/模型修改后，旧批次必须保持快照；数据库失败不能返回成功。
- 非幂等裁切与检测禁止网络隐式重试；GET 状态轮询可重试。
- 未经需求确认，不新增像元硬上限、降精度或关页续跑；当前行为与未来候选项保持分开。
- 四邻域最小组件过滤、八邻域矢量统计有专项回归，不能混为同一规则。
- 测试用明确夹具或隔离数据库，不能依赖开发者当前映射盘或改动其任务。
- Node 的地图组件测试隔离导航对象；真正 Leaflet 拖拽、缩放和惯性约束由 `test_region_navigation_browser.mjs` 验证。
