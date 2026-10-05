#requires -Version 5.1
<#
.SYNOPSIS
    本地打包本项目 Go 命令行工具，输出到 build/bin/，并部署到 D:\app_mgr\my

.DESCRIPTION
    用法:
        .\scripts\build-local.ps1                   # 标准打包 + 部署
        .\scripts\build-local.ps1 -Clean            # 清理 build/bin/
        .\scripts\build-local.ps1 -NoDeploy         # 只打包不部署
        .\scripts\build-local.ps1 -DeployDir D:\xxx # 自定义部署目录（默认 $env:app_output_dir，兜底 D:\app_mgr\my\my）
        .\scripts\build-local.ps1 -OutputDir dist   # 自定义输出目录
        .\scripts\build-local.ps1 -DryRun           # 只预览不执行

    输出:
        build/bin/<module-last-segment>.exe         <- 产物（go build -trimpath -ldflags "-s -w"）
        D:\app_mgr\my\<module-last-segment>.exe        <- 部署副本（拷贝前自动停止同名进程）

    说明:
        - 产物名自动从 go.mod 第一行 module 路径的最后一段获取（例如 module wcj-go-http → wcj-go-http.exe）
        - 当前模板默认传 -H windowsgui（无控制台窗口的 UI 程序）
        - 默认 DeployDir = D:\app_mgr\my；如需改为其他目录，调用时传 -DeployDir
        - 部署前自动 Stop-Process 同名进程，避免文件占用
    部署目录（app_output_dir）:
        取值优先级：-DeployDir 参数 > 环境变量 app_output_dir > 内置兜底 D:\app_mgr\my
        查看当前值：$env:app_output_dir
        修改：[Environment]::SetEnvironmentVariable('app_output_dir', 'D:\app_mgr\my', 'User')
#>

[CmdletBinding()]
param(
    [switch]$Clean,
    [switch]$NoDeploy,
    [string]$DeployDir,       # 部署目录；留空则取环境变量 app_output_dir，兜底 D:\app_mgr\my
    [string]$OutputDir = "build\bin",
    [switch]$DryRun
)

$ErrorActionPreference = 'Stop'

# --- resolve deploy dir ------------------------------------------------------
# 部署目录取值优先级：-DeployDir 参数 > 环境变量 app_output_dir > 内置兜底目录
$FallbackDeployDir = "D:\app_mgr\my"
$envDeployDir = [Environment]::GetEnvironmentVariable('app_output_dir')
$envDeployDir = if ($null -ne $envDeployDir) { $envDeployDir.Trim() } else { '' }

if ($DeployDir) {
    $DeployDirSource = '-DeployDir 参数'
} elseif ($envDeployDir) {
    $DeployDir = $envDeployDir
    $DeployDirSource = '环境变量 app_output_dir'
} else {
    $DeployDir = $FallbackDeployDir
    $DeployDirSource = '脚本内置默认值'
}

# 去掉结尾多余的分隔符，避免拼出 "D:\app_mgr\my\\app.exe"；盘符根目录保留
$DeployDir = $DeployDir.Trim().TrimEnd('\', '/')
if ($DeployDir -match '^[A-Za-z]:$') { $DeployDir += '\' }

function Write-Step($t) { Write-Host "`n==> $t" -ForegroundColor Cyan }
function Write-Ok($t)   { Write-Host $t -ForegroundColor Green }
function Write-Warn($t) { Write-Host $t -ForegroundColor Yellow }

function Require-Command($cmd) {
    if (-not (Get-Command $cmd -ErrorAction SilentlyContinue)) {
        throw "未找到命令: $cmd，请先安装并加入 PATH"
    }
}

# 停止指定进程（不带 .exe 后缀），用于解锁目标 EXE
function Stop-ExeProcess([string]$name) {
    $procs = Get-Process -Name $name -ErrorAction SilentlyContinue
    if ($procs) {
        Write-Host "  停止进程: $name (PID: $(($procs.Id) -join ', '))" -ForegroundColor Yellow
        if ($DryRun) {
            Write-Host "  (DRYRUN) Stop-Process: $name"
        } else {
            $procs | Stop-Process -Force -ErrorAction SilentlyContinue
            Start-Sleep -Milliseconds 500
        }
    }
}

$root = Split-Path $PSScriptRoot -Parent

# --- 从 go.mod 读取 module 路径 ---------------------------------------------
$goModPath = Join-Path $root "go.mod"
if (-not (Test-Path -LiteralPath $goModPath)) {
    throw "未找到 go.mod: $goModPath"
}
$moduleLine = (Get-Content -LiteralPath $goModPath -Encoding UTF8 -First 1).Trim()
if ($moduleLine -notmatch '^module\s+(\S+)\s*$') {
    throw "go.mod 第一行格式异常: $moduleLine"
}
$modulePath = $Matches[1]
$productName = Split-Path -Path $modulePath -Leaf
$exeName = "$productName.exe"
Write-Host "产物名（go.mod module 末段）: $exeName"
Write-Host "module: $modulePath"

# --- preflight ---------------------------------------------------------------
Write-Step "检查构建环境"
$knownToolDirs = @(
    "D:\application\golang\go\bin",
    "$env:USERPROFILE\go\bin",
    "$env:LOCALAPPDATA\Programs\Go\bin"
) | Where-Object { $_ -and (Test-Path -LiteralPath $_) }

$pathEntries = @($env:Path -split ';' | Where-Object { $_ } | ForEach-Object { $_.TrimEnd('\') })
$added = @()
foreach ($d in $knownToolDirs) {
    $norm = $d.TrimEnd('\')
    if ($pathEntries -notcontains $norm) {
        $env:Path = "$d;$env:Path"
        $added += $d
    }
}
if ($added.Count -gt 0) {
    Write-Warn "已自动追加 PATH: $($added -join '; ')"
}

Require-Command go
Write-Host "  $(& go version)"

# --- output dir --------------------------------------------------------------
$binDir = if ([System.IO.Path]::IsPathRooted($OutputDir)) { $OutputDir } else { Join-Path $root $OutputDir }
if ($Clean -and (Test-Path -LiteralPath $binDir)) {
    Write-Step "清理: $binDir"
    if (-not $DryRun) { Remove-Item -LiteralPath $binDir -Recurse -Force }
}
if (-not (Test-Path -LiteralPath $binDir)) {
    if ($DryRun) {
        Write-Host "  (DRYRUN) 创建目录: $binDir"
    } else {
        New-Item -ItemType Directory -Path $binDir -Force | Out-Null
    }
}
Write-Host "输出目录: $binDir"

# --- build -------------------------------------------------------------------
$out = Join-Path $binDir $exeName
$ldflags = "-s -w -H windowsgui"

Write-Step "go build -> $exeName"
Push-Location $root
try {
    # 使用脚本专属临时 GOCACHE，绕开宿主机 GOCACHE 残留版本不匹配
    if (-not $DryRun) {
        $scriptCache = Join-Path $env:TEMP "go-build-xpilot"
        if (Test-Path -LiteralPath $scriptCache) {
            Remove-Item -LiteralPath $scriptCache -Recurse -Force -ErrorAction SilentlyContinue
        }
        New-Item -ItemType Directory -Path $scriptCache -Force | Out-Null
        $env:GOCACHE = $scriptCache
        Write-Host "  临时 GOCACHE: $scriptCache"
    }
    if ($DryRun) {
        Write-Host "  (DRYRUN) 在 $root 执行: go build -trimpath -ldflags '$ldflags' -o '$out' ."
    } else {
        & go build -trimpath -ldflags $ldflags -o $out .
        if ($LASTEXITCODE -ne 0) { throw "go build 失败 (exit=$LASTEXITCODE)" }
    }
} finally {
    Pop-Location
}

if (-not $DryRun -and -not (Test-Path -LiteralPath $out)) {
    throw "未找到预期产物: $out"
}
Write-Ok "  ✓ 已生成: $out"

# --- deploy ------------------------------------------------------------------
if ($NoDeploy) {
    Write-Warn "已跳过部署 (-NoDeploy)"
} else {
    Write-Step "部署: $DeployDir  (来源: $DeployDirSource)"
    if (-not (Test-Path -LiteralPath $DeployDir)) {
        if ($DryRun) {
            Write-Host "  (DRYRUN) 创建目录: $DeployDir"
        } else {
            New-Item -ItemType Directory -Path $DeployDir -Force | Out-Null
        }
    }

    Stop-ExeProcess $productName

    if ($DryRun) {
        Write-Host "  (DRYRUN) Copy-Item $out -> $(Join-Path $DeployDir $exeName)"
    } else {
        Copy-Item -LiteralPath $out -Destination (Join-Path $DeployDir $exeName) -Force
        Write-Ok "  ✓ 已部署: $(Join-Path $DeployDir $exeName)"
    }
}

# --- summary -----------------------------------------------------------------
Write-Step "打包完成"
if (Test-Path -LiteralPath $binDir) {
    Get-ChildItem -LiteralPath $binDir -File | Sort-Object Name |
        Select-Object Name, @{n='Size(MB)';e={[math]::Round($_.Length/1MB,2)}} |
        Format-Table -AutoSize | Out-String | Write-Host
}
Write-Host "本地产物: $out" -ForegroundColor Green
if (-not $NoDeploy -and $DeployDir) {
    Write-Host "部署位置: $(Join-Path $DeployDir $exeName)" -ForegroundColor Green
}