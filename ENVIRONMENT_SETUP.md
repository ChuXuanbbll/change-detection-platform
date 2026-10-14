# 环境安装与原生启动

按 2026-10-12 代码基线 `ce3d52d` 核对。平台检测要求 NVIDIA CUDA；Python 服务可安装 CPU 版 ONNX Runtime，用于结构检查等操作，模型推理由独立 GPU 解释器执行。模型或影像来源不可用时服务仍可提供管理入口，基础依赖与解释器仍须存在。

容器部署见 [容器说明](deploy/README.md)。历史 CPU 迁移验证已归档，不代表当前平台支持 CPU 检测。

## 安装依赖

现有环境说明使用 Python 3.13、Node.js 24；Go 模块要求 1.22 或更新。其他版本兼容性需另行验证。以下命令从项目根目录执行：

```powershell
python -m venv backend-py/.venv-local
backend-py/.venv-local/Scripts/python.exe -m pip install -r backend-py/requirements.lock.txt --index-url https://pypi.org/simple
Push-Location frontend
npm ci --registry=https://registry.npmjs.org
Pop-Location
Push-Location backend-go
go mod download
Pop-Location

backend-py/.venv-local/Scripts/python.exe -m venv model-runtime/.venv-gpu
model-runtime/.venv-gpu/Scripts/python.exe -m pip install -r model-runtime/requirements.windows-gpu.txt --index-url https://pypi.org/simple
```

服务环境使用 `backend-py/requirements.lock.txt`；`requirements.txt` 是宽版本依赖列表，其历史 CPU 注释不改变平台限制。`deploy/environment-observed-windows.txt` 仅为迁移时环境快照。不要在同一环境同时安装 `onnxruntime` 与 `onnxruntime-gpu`。

Windows GPU 环境还需兼容的 NVIDIA 驱动、CUDA 12 / cuDNN 9 DLL。启动器解释器选择顺序为：`CHANGE_ONNX_PYTHON` → 项目 `model-runtime/.venv-gpu` → Conda `change_onnx` → 平台解释器。最后一项如果只有 CPU ORT，不能执行检测。

启动器查找模型环境及 Conda/PyTorch 中的 DLL；其他位置可在启动前指定：

```powershell
$env:CHANGE_CUDA_DLL_DIRS = 'D:\实际的CUDA12与cuDNN9运行库目录'
```

替换为实际路径；多目录用分号分隔。手动启动 Python 时可设置 `CHANGE_CUDA_DLL_DIR` 并将必要 DLL 目录加入 `PATH`，不会执行启动器自动发现逻辑。

## 启动与首次配置

```powershell
.\start-all.cmd -CheckOnly
.\start-all.cmd
```

启动器只接受 `-Provider cuda`，默认也是 cuda。它检查依赖并尝试加载内置 `model-runtime/model/enforcement_2.onnx`；模型探测失败会警告并继续，`-CheckOnly` 返回 0 或末尾提示不能替代管理页的真实 CUDA 验证。

入口为 `http://localhost:5173`，Go 8080、Python 8090。后台服务日志位于 `data/logs`。占用端口会被跳过，不保证原服务已加载最新代码；确需重启时使用项目停止/启动脚本，停止会中断在途工作。

首次进入「管理」，验证内置模型或导入并验证兼容 ONNX，配置可访问的影像目录并应用。初始来源来自 `LOCAL_IMAGERY_DIR`，未设置时为 `Y:\河北影像\2025年`；可在首次启动前指定项目 `data/imagery` 样例目录。持久化后来源、模型、检测和性能参数以 SQLite 生效版本为准，修改旧环境变量不会覆盖它们。详见 [管理说明](docs/MANAGEMENT.md)。

启动器固定 `DATA_DIR` 为项目 `backend-go/data`，固定服务端口和 Python 上游地址；资源默认位于项目 `data`。自定义运行目录或端口需手动部署。

## 手动启动

在各终端先进入项目根目录。Python 与 Go 使用相同绝对 `DATA_DIR`；Python 只启动一个 Uvicorn API worker。

Python 终端：

```powershell
$env:DATA_DIR = Join-Path (Get-Location) 'backend-go/data'
$env:CHANGE_ONNX_PYTHON = (Resolve-Path model-runtime/.venv-gpu/Scripts/python.exe).Path
$env:CHANGE_ONNX_PROVIDER = 'cuda'
# 按实际环境设置 CHANGE_CUDA_DLL_DIR 和 PATH。
Set-Location backend-py
.venv-local/Scripts/python.exe -m uvicorn app.main:app --host 127.0.0.1 --port 8090
```

Go 终端：

```powershell
$env:DATA_DIR = Join-Path (Get-Location) 'backend-go/data'
$env:PY_ALGO_URL = 'http://127.0.0.1:8090'
Set-Location backend-go
$env:GOCACHE = Join-Path (Get-Location) '.cache/go-build'
go run ./cmd/server
```

前端终端：

```powershell
Set-Location frontend
npm run dev
```

## 验证与数据交付

检查与隔离验收入口见 [开发与测试](docs/DEVELOPMENT.md)。`scripts/validate_management.py --browser` 会创建隔离服务/数据库并执行真实 GPU 检测；`scripts/smoke_native.py` 当前只是该 GPU 验收的兼容入口。

源码克隆不包含完整数据备份。模型、原始影像、金字塔伴随文件、`DATA_DIR` 的数据库和结果需单独交付。本次仅核对说明，没有重新安装依赖、重启服务或验证部署环境；旧说明见 [归档索引](docs/archive/README.md)。
