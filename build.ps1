#!/usr/bin/env pwsh
# ============================================================
# Pipewright 构建脚本 (Windows PowerShell / pwsh)
#
# 自动生成版本元数据 ldflags(VERSION/COMMIT/DATE 取自 git),
# 无需手工拼写 $LDFLAGS,编译产物 -> dist/<Target>/pipewright[.exe]
#
# 用法:
#   .\build.ps1                      # 默认 linux-arm64
#   .\build.ps1 linux-x86_64         # linux/amd64
#   .\build.ps1 windows-x86_64       # windows/amd64 (.exe)
#   .\build.ps1 local                # 当前本机平台
#   .\build.ps1 linux-arm64 -RebuildFrontend   # 强制重建 web/dist 前端
# ============================================================
[CmdletBinding()]
param(
    # 目标平台:linux-arm64 | linux-x86_64 | windows-x86_64 | local
    [string]$Target = "linux-arm64",
    # 强制重新构建前端(web/dist);默认:已存在则跳过,不存在则自动构建
    [switch]$RebuildFrontend
)

$ErrorActionPreference = "Stop"
$root = $PSScriptRoot
$vPkg = "github.com/huangchengsir/pipewright/internal/version"

# ---------- 1. 目标平台 -> GOOS/GOARCH/扩展名 ----------
$targets = @{
    "linux-arm64"    = @{ goos = "linux";   goarch = "arm64"; ext = "" }
    "linux-x86_64"   = @{ goos = "linux";   goarch = "amd64"; ext = "" }
    "linux-amd64"    = @{ goos = "linux";   goarch = "amd64"; ext = "" }
    "windows-x86_64" = @{ goos = "windows"; goarch = "amd64"; ext = ".exe" }
    "windows-amd64"  = @{ goos = "windows"; goarch = "amd64"; ext = ".exe" }
}
if ($Target -eq "local") {
    $t = @{ goos = if ($IsWindows) { "windows" } else { "linux" }; goarch = "amd64"; ext = if ($IsWindows) { ".exe" } else { "" } }
    $dirName = "local"
} elseif ($targets.ContainsKey($Target)) {
    $t = $targets[$Target]
    $dirName = $Target
} else {
    throw "未知目标平台: $Target (支持: linux-arm64 / linux-x86_64 / windows-x86_64 / local)"
}

# ---------- 2. 自动生成版本元数据(与 Makefile 一致;git 失败回退源码态默认值) ----------
$version = (git describe --tags --always --dirty 2>$null | Select-Object -First 1)
if (-not $version) { $version = "dev" }
$commit = (git rev-parse --short HEAD 2>$null | Select-Object -First 1)
if (-not $commit) { $commit = "none" }
$date = [DateTime]::UtcNow.ToString("yyyy-MM-ddTHH:mm:ssZ")
$ldflags = "-s -w -X $vPkg.Version=$version -X $vPkg.Commit=$commit -X $vPkg.Date=$date"

Write-Host "==> 目标平台 : $dirName ($($t.goos)/$($t.goarch))"
Write-Host "==> 版本元数据:"
Write-Host "    VERSION = $version"
Write-Host "    COMMIT  = $commit"
Write-Host "    DATE    = $date"

# ---------- 3. 前端产物检查(go:embed web/dist 必须存在) ----------
$webDist = Join-Path $root "web/dist"
$needFrontend = $RebuildFrontend -or -not (Test-Path (Join-Path $webDist "index.html"))
if ($needFrontend) {
    Write-Host "==> 构建前端(web/dist)..."
    Push-Location (Join-Path $root "web")
    try {
        npm run build
        if ($LASTEXITCODE -ne 0) { throw "前端构建失败 (npm run build)" }
    } finally {
        Pop-Location
    }
} else {
    Write-Host "==> 复用已存在的前端产物 web/dist(如需重建请加 -RebuildFrontend)"
}

# ---------- 4. 交叉编译 ----------
$outDir = Join-Path $root "dist/$dirName"
$outFile = Join-Path $outDir ("pipewright" + $t.ext)
New-Item -ItemType Directory -Force -Path $outDir | Out-Null

$prevGOOS = $env:GOOS; $prevGOARCH = $env:GOARCH; $prevCGO = $env:CGO_ENABLED
$env:GOOS = $t.goos; $env:GOARCH = $t.goarch; $env:CGO_ENABLED = "0"
try {
    Write-Host "==> go build -ldflags ... -o $outFile"
    Push-Location $root
    try {
        go build -ldflags $ldflags -o $outFile ./cmd/pipewright
    } finally {
        Pop-Location
    }
    if ($LASTEXITCODE -ne 0) { throw "go build 失败" }
} finally {
    # 收尾:恢复本次会话的交叉编译环境变量,避免影响之后的本机编译
    $env:GOOS = $prevGOOS; $env:GOARCH = $prevGOARCH; $env:CGO_ENABLED = $prevCGO
}

Write-Host ""
Write-Host "构建成功: $outFile" -ForegroundColor Green
