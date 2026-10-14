param(
    [switch]$CheckOnly,
    [ValidateSet('cuda')][string]$Provider = 'cuda'
)

$ErrorActionPreference = 'Stop'
$projectRoot = $PSScriptRoot
$python = Join-Path $projectRoot 'backend-py\.venv-local\Scripts\python.exe'

function Test-Port([int]$Port) {
    foreach ($hostAddress in @('127.0.0.1', '::1')) {
        $address = [System.Net.IPAddress]::Parse($hostAddress)
        $client = New-Object System.Net.Sockets.TcpClient($address.AddressFamily)
        try {
            $pending = $client.BeginConnect($address, $Port, $null, $null)
            if ($pending.AsyncWaitHandle.WaitOne(500)) {
                $client.EndConnect($pending)
                return $true
            }
        } catch { }
        finally { $client.Dispose() }
    }
    return $false
}

function Quote-PS([string]$Value) {
    return "'" + $Value.Replace("'", "''") + "'"
}

function Add-ExistingPath([System.Collections.Generic.List[string]]$Paths, [string]$Path) {
    if ([string]::IsNullOrWhiteSpace($Path)) { return }
    if (-not (Test-Path -LiteralPath $Path -PathType Container)) { return }
    $resolved = (Resolve-Path -LiteralPath $Path).Path
    if (-not $Paths.Contains($resolved)) { $Paths.Add($resolved) }
}

function Add-CudaRuntimePath([System.Collections.Generic.List[string]]$Paths, [string]$Path) {
    if (-not (Test-Path -LiteralPath $Path -PathType Container)) { return }
    $required = @(
        'cublasLt64_12.dll', 'cublas64_12.dll', 'cufft64_11.dll',
        'cudart64_12.dll', 'cudnn64_9.dll'
    )
    $complete = $true
    foreach ($name in $required) {
        if (-not (Test-Path -LiteralPath (Join-Path $Path $name))) {
            $complete = $false
            break
        }
    }
    if ($complete) { Add-ExistingPath $Paths $Path }
}

try {
    foreach ($relative in @(
        'backend-py\.venv-local\Scripts\python.exe',
        'frontend\node_modules\vite\bin\vite.js'
    )) {
        if (-not (Test-Path -LiteralPath (Join-Path $projectRoot $relative))) {
            throw "Missing required file: $relative"
        }
    }
    $go = (Get-Command go.exe -ErrorAction Stop).Source
    $node = (Get-Command node.exe -ErrorAction Stop).Source
    & $python -c 'import fastapi, uvicorn, rasterio, numpy, scipy, skimage, shapely, shapefile, requests, PIL, onnxruntime, onnx'
    if ($LASTEXITCODE -ne 0) { throw 'Python dependencies are incomplete.' }

    # The platform environment and the model runtime have different dependency
    # sets. Keep an explicit model interpreter if supplied, otherwise prefer the
    # documented Conda environment and fall back to the platform interpreter.
    $modelPython = $env:CHANGE_ONNX_PYTHON
    $projectModelPython = Join-Path $projectRoot 'model-runtime\.venv-gpu\Scripts\python.exe'
    if ([string]::IsNullOrWhiteSpace($modelPython) -and (Test-Path -LiteralPath $projectModelPython)) {
        $modelPython = $projectModelPython
    }
    $condaBase = $null
    $conda = Get-Command conda.exe -ErrorAction SilentlyContinue
    if ($Provider -eq 'cuda' -and $conda) {
        try {
            $condaBase = (& $conda.Source info --base 2>$null).Trim()
        } catch {
            $condaBase = $null
        }
    }
    if ($Provider -eq 'cuda' -and [string]::IsNullOrWhiteSpace($modelPython) -and $condaBase) {
        $candidate = Join-Path $condaBase 'envs\change_onnx\python.exe'
        if (Test-Path -LiteralPath $candidate) { $modelPython = $candidate }
    }
    if ([string]::IsNullOrWhiteSpace($modelPython)) { $modelPython = $python }
    if (-not (Test-Path -LiteralPath $modelPython -PathType Leaf)) {
        throw "Model Python interpreter not found: $modelPython"
    }

    # ONNX Runtime 1.23.x requires CUDA 12 / cuDNN 9 DLL names. CUDA 13 on
    # PATH is not compatible, so discover a complete runtime directory from
    # the model env or an installed Conda/PyTorch environment.
    if ($Provider -eq 'cuda') {
    $cudaDllDirs = New-Object 'System.Collections.Generic.List[string]'
    if ($env:CHANGE_CUDA_DLL_DIRS) {
        foreach ($dir in ($env:CHANGE_CUDA_DLL_DIRS -split ';')) {
            Add-ExistingPath $cudaDllDirs $dir.Trim()
        }
    }
    $modelEnvRoot = Split-Path (Split-Path $modelPython -Parent) -Parent
    $runtimeRoots = @($modelEnvRoot)
    if ($condaBase) {
        $runtimeRoots += $condaBase
        $envsRoot = Join-Path $condaBase 'envs'
        if (Test-Path -LiteralPath $envsRoot -PathType Container) {
            $runtimeRoots += @(Get-ChildItem -LiteralPath $envsRoot -Directory | Select-Object -ExpandProperty FullName)
        }
    }
    foreach ($root in ($runtimeRoots | Select-Object -Unique)) {
        Add-CudaRuntimePath $cudaDllDirs (Join-Path $root 'Lib\site-packages\torch\lib')
        $nvidiaRoot = Join-Path $root 'Lib\site-packages\nvidia'
        if (Test-Path -LiteralPath $nvidiaRoot -PathType Container) {
            foreach ($bin in @(Get-ChildItem -LiteralPath $nvidiaRoot -Directory -Recurse -ErrorAction SilentlyContinue |
                    Where-Object { $_.Name -eq 'bin' } | Select-Object -ExpandProperty FullName)) {
                Add-ExistingPath $cudaDllDirs $bin
            }
        }
    }
    $completeRuntime = $null
    foreach ($dir in $cudaDllDirs) {
        $hasAll = $true
        foreach ($name in @('cublasLt64_12.dll', 'cublas64_12.dll', 'cufft64_11.dll', 'cudart64_12.dll', 'cudnn64_9.dll')) {
            if (-not (Test-Path -LiteralPath (Join-Path $dir $name))) { $hasAll = $false; break }
        }
        if ($hasAll) { $completeRuntime = $dir; break }
    }
    if ($cudaDllDirs.Count -gt 0) {
        $env:PATH = (($cudaDllDirs -join ';') + ';' + $env:PATH)
    }
    if ($completeRuntime) {
        $env:CHANGE_CUDA_DLL_DIR = $completeRuntime
        Write-Host "CUDA runtime DLLs: $completeRuntime"
    } else {
        Write-Host 'CUDA 12/cuDNN 9 runtime directory was not found; GPU startup probe may fail.' -ForegroundColor Yellow
    }
    }
    $env:CHANGE_ONNX_PROVIDER = $Provider
    $env:CHANGE_ONNX_PYTHON = $modelPython
    if ([string]::IsNullOrWhiteSpace($env:LOCAL_IMAGERY_DIR)) {
        $env:LOCAL_IMAGERY_DIR = 'Y:\河北影像\2025年'
    }
    if (-not (Test-Path -LiteralPath $env:LOCAL_IMAGERY_DIR -PathType Container)) {
        Write-Warning "Initial imagery directory is unavailable. Repair sources in Management: $env:LOCAL_IMAGERY_DIR"
    }
    $env:CHANGE_MODEL_PATH = Join-Path $projectRoot 'model-runtime\model\enforcement_2.onnx'

$gpuProbe = @'
import os, sys
import onnxruntime as ort
model = sys.argv[1]
directory = os.environ.get('CHANGE_CUDA_DLL_DIR', '')
provider = os.environ['CHANGE_ONNX_PROVIDER']
if provider == 'cuda' and hasattr(ort, 'preload_dlls'):
    ort.preload_dlls(directory=directory)
expected = 'CUDAExecutionProvider' if provider == 'cuda' else 'CPUExecutionProvider'
session = ort.InferenceSession(model, providers=[expected])
active = session.get_providers()
if expected not in active:
    raise RuntimeError(expected + ' did not start; active providers: ' + repr(active))
print(expected + ' OK')
'@
    & $modelPython -c $gpuProbe $env:CHANGE_MODEL_PATH
    if ($LASTEXITCODE -ne 0) {
        Write-Warning "$Provider model runtime check failed. Services will start for management and diagnostics."
    }

    $env:PY_ALGO_URL = 'http://127.0.0.1:8090'
    $env:GO_API_PORT = '8080'
    $env:CONFIG_PATH = Join-Path $projectRoot 'backend-go\config.yaml'
    $env:DATA_DIR = Join-Path $projectRoot 'backend-go\data'
    $env:GOCACHE = Join-Path $projectRoot 'backend-go\.cache\go-build'
    $logDir = Join-Path $projectRoot 'data\logs'
    if (-not $CheckOnly) { New-Item -ItemType Directory -Force -Path $logDir | Out-Null }

    $services = @(
        @{ Name = 'Python'; Port = 8090; Directory = 'backend-py'; Command = "& $(Quote-PS $python) -m uvicorn app.main:app --host 127.0.0.1 --port 8090" },
        @{ Name = 'Go API'; Port = 8080; Directory = 'backend-go'; Command = "& $(Quote-PS $go) run ./cmd/server" },
        @{ Name = 'Frontend'; Port = 5173; Directory = 'frontend'; Command = "& $(Quote-PS $node) ./node_modules/vite/bin/vite.js --host localhost --port 5173 --strictPort" }
    )
    foreach ($service in $services) {
        $occupied = Test-Port $service.Port
        if ($occupied) {
            Write-Host "Port $($service.Port) is occupied; skipping $($service.Name). Verify it belongs to this project."
            continue
        }
        if ($CheckOnly) {
            Write-Host "$($service.Name): prerequisites OK; port $($service.Port) available."
            continue
        }
        $directory = Join-Path $projectRoot $service.Directory
        $shell = (Get-Command powershell.exe -ErrorAction Stop).Source
        $logName = $service.Name.Replace(' ', '-').ToLowerInvariant()
        $command = "Set-Location -LiteralPath $(Quote-PS $directory); " + $service.Command +
            " 1> $(Quote-PS (Join-Path $logDir "$logName.log")) 2> $(Quote-PS (Join-Path $logDir "$logName.error.log"))"
        $encoded = [Convert]::ToBase64String([Text.Encoding]::Unicode.GetBytes($command))
        # Inherit the environment directly: Windows PowerShell's Start-Process
        # can fail while merging PATH/Path entries from the machine and user.
        $startInfo = New-Object System.Diagnostics.ProcessStartInfo
        $startInfo.FileName = $shell
        $startInfo.Arguments = "-NoProfile -EncodedCommand $encoded"
        $startInfo.WorkingDirectory = $directory
        $startInfo.UseShellExecute = $false
        $startInfo.CreateNoWindow = $true
        $startInfo.WindowStyle = [System.Diagnostics.ProcessWindowStyle]::Hidden
        $process = [System.Diagnostics.Process]::Start($startInfo)
        Write-Host "Started $($service.Name) (launcher PID $($process.Id)); logs: $logDir"
    }
    if ($CheckOnly) { exit 0 }

    $deadline = (Get-Date).AddSeconds(60)
    do {
        $pendingServices = @($services | Where-Object { -not (Test-Port $_.Port) })
        if ($pendingServices.Count -eq 0) { break }
        Start-Sleep -Seconds 1
    } while ((Get-Date) -lt $deadline)
    if ($pendingServices.Count -gt 0) {
        throw "Ports not ready: $(($pendingServices.Port) -join ', '). Check logs in $logDir."
    }
    $pyHealth = Invoke-RestMethod 'http://127.0.0.1:8090/' -TimeoutSec 15
    if ($pyHealth.service -ne 'change-detection-backend-py') { throw 'Port 8090 is not the expected Python service.' }
    $apiHealth = Invoke-RestMethod 'http://localhost:5173/api/health' -TimeoutSec 15
    if ($apiHealth.status -ne 'ok') { throw 'Frontend proxy cannot reach the Go API.' }
    $null = Invoke-RestMethod 'http://localhost:5173/api/algorithms' -TimeoutSec 15
    # Initial metadata discovery over the mapped drive can exceed one minute.
    try { $null = Invoke-RestMethod 'http://localhost:5173/py/local/catalog' -TimeoutSec 300 } catch { Write-Warning 'Imagery unavailable. Configure sources in Management.' }
    Write-Host 'Services, frontend proxies, algorithms and local imagery are ready. Open http://localhost:5173/'
    Write-Host "ONNX Runtime $Provider provider check passed. Service logs: $logDir"
} catch {
    Write-Host "Startup failed: $($_.Exception.Message)" -ForegroundColor Red
    exit 1
}
