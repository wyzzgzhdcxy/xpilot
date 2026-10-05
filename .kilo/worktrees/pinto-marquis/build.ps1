# xpilot 打包脚本：构建单文件 exe 到工具箱目录。
# 本文件为 UTF-8 with BOM 编码，Windows PowerShell 5.1 与 PowerShell 7 均可正确解析中文路径。
$ErrorActionPreference = 'Stop'

$TargetDir = 'E:\application\我的工具箱'
$Target = Join-Path $TargetDir 'xpilot.exe'

if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
    Write-Host '[ERROR] "go" not found in PATH. Install Go first.' -ForegroundColor Red
    exit 1
}

if (-not (Test-Path $TargetDir)) {
    New-Item -ItemType Directory -Path $TargetDir -Force | Out-Null
}

Write-Host "Building xpilot: $Target"
go build -trimpath -ldflags "-s -w -H=windowsgui" -o $Target .
if ($LASTEXITCODE -ne 0) {
    Write-Host '[ERROR] Build failed. If "Access is denied", stop the running xpilot.exe first.' -ForegroundColor Red
    exit 1
}

$size = (Get-Item $Target).Length
Write-Host ("Done: {0} ({1:N0} bytes)" -f $Target, $size)
