param([switch]$GPU)
$ErrorActionPreference = 'Stop'
Push-Location $PSScriptRoot
try {
    $compose = @('compose', '-f', 'docker-compose.yml')
    $pythonImage = 'change-detection-python:gpu'
    if ($GPU) {
        $compose += @('-f', 'docker-compose.gpu.yml')
        $pythonImage = 'change-detection-python:gpu'
    }
    & docker @compose build
    if ($LASTEXITCODE -ne 0) { throw 'Image build failed' }
    & docker pull python:3.13-slim-bookworm
    if ($LASTEXITCODE -ne 0) { throw 'Initialization image pull failed' }
    & docker image save -o environment-images.tar change-detection-frontend:local change-detection-go:local $pythonImage python:3.13-slim-bookworm
    if ($LASTEXITCODE -ne 0) { throw 'Image export failed' }
    Get-FileHash environment-images.tar -Algorithm SHA256 | Format-List
} finally { Pop-Location }
