# xpilot 打包脚本：构建单文件 exe，停掉旧进程，替换到工具箱目录，再拉起新进程。
#
# 本文件为 UTF-8 with BOM 编码，Windows PowerShell 5.1 与 PowerShell 7 均可正确解析中文路径。
#
# 完整流程：检查并结束运行中的 xpilot → 构建 → 复制到目标目录 → 启动新进程。
# 之所以要「先停后建」，是因为运行中的 exe 会锁住自己的映像文件，
# 直接 go build -o 到目标路径会报 "Access is denied"。
$ErrorActionPreference = 'Stop'

$TargetDir = 'E:\app_mgr'
$Target    = Join-Path $TargetDir 'xpilot.exe'
$ProcName  = 'xpilot'

if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
    Write-Host '[ERROR] "go" not found in PATH. Install Go first.' -ForegroundColor Red
    exit 1
}

if (-not (Test-Path $TargetDir)) {
    New-Item -ItemType Directory -Path $TargetDir -Force | Out-Null
}

# ---------- 1. 停掉正在运行的实例 ----------
# 先看有没有在跑；有就结束，等它真正退出后再继续 —— 否则文件仍被锁着，
# 复制步骤会失败（进程退出和句柄释放之间有时间差）。
$running = Get-Process -Name $ProcName -ErrorAction SilentlyContinue
if ($running) {
    $pids = ($running | Select-Object -ExpandProperty Id) -join ', '
    Write-Host "Stopping running xpilot (PID: $pids)..." -ForegroundColor Yellow
    $running | Stop-Process -Force -ErrorAction SilentlyContinue

    # 最多等 10 秒确认句柄释放；还在就报错退出，不冒险覆盖
    $waited = 0
    while ((Get-Process -Name $ProcName -ErrorAction SilentlyContinue) -and $waited -lt 100) {
        Start-Sleep -Milliseconds 100
        $waited++
    }
    if (Get-Process -Name $ProcName -ErrorAction SilentlyContinue) {
        Write-Host '[ERROR] xpilot did not exit within 10s. Kill it manually and retry.' -ForegroundColor Red
        exit 1
    }
    Write-Host 'Stopped.' -ForegroundColor Green
} else {
    Write-Host 'No running xpilot found.'
}

# ---------- 2. 构建到临时文件 ----------
# 先构建到临时路径、再原子移动到目标，避免构建失败时把目标位置留下一个半截文件。
$Staging = Join-Path $env:TEMP 'xpilot-build.exe'
Write-Host "Building xpilot..."
Push-Location $PSScriptRoot
try {
    go build -trimpath -ldflags "-s -w -H=windowsgui" -o $Staging .
    if ($LASTEXITCODE -ne 0) {
        Write-Host '[ERROR] Build failed.' -ForegroundColor Red
        exit 1
    }
} finally {
    Pop-Location
}

# ---------- 3. 替换到目标目录 ----------
Move-Item -Path $Staging -Destination $Target -Force
$size = (Get-Item $Target).Length
Write-Host ("Copied: {0} ({1:N0} bytes)" -f $Target, $size) -ForegroundColor Green

# ---------- 4. 拉起新进程 ----------
# 用 Start-Process 启动（-H=windowsgui 构建，无控制台窗口）。
Start-Process -FilePath $Target
Start-Sleep -Milliseconds 800
$now = Get-Process -Name $ProcName -ErrorAction SilentlyContinue
if ($now) {
    Write-Host ("Started xpilot (PID: {0})." -f (($now | Select-Object -ExpandProperty Id) -join ', ')) -ForegroundColor Green
} else {
    Write-Host '[WARN] xpilot started but process not found. Check logs for startup errors.' -ForegroundColor Yellow
}

Write-Host 'Done.' -ForegroundColor Green
