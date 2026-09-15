# 覆盖率门禁:先跑全量测试产出 coverprofile,再交给 cmd/coveragegate 判定阈值。
#
# 为什么这两步必须由同一个脚本串起来(而不是在 CI 里各写一行):
#  1. 环境变量(GOCACHE / GOMODCACHE / GOPROXY / DSN)必须与"跑测试的人"完全一致,
#     否则同一份代码在 CI 与本地会算出不同的覆盖率,门禁红绿全凭环境;
#  2. `go test` 失败时退出码**必须先于门禁返回**:测试挂了照样会写出一份
#     "看起来完整"的 profile(没被覆盖的语句照样记进去),若继续判定阈值,
#     就会拿一份残缺数据发绿灯 —— 那正是覆盖率门禁最坏的失效方式。
#
# 用法(任意工作目录均可,脚本自己定位 server 目录):
#   powershell -ExecutionPolicy Bypass -File server\scripts\coverage-gate.ps1
#
# 退出码:0 = 测试全绿且所有阈值达标;否则透传失败一方的退出码
# (测试失败 → go test 的码;阈值未达标 → coveragegate 的 1)。
#
# 本文件必须保存为**带 BOM 的 UTF-8**:Windows PowerShell 5.1 默认按 ANSI
# 代码页读脚本,没有 BOM 时上面的中文会变成乱码,报错信息也就没人看得懂。

$ErrorActionPreference = 'Stop'

# 控制台按 UTF-8 输出,否则 5.1 下 Go 程序打的中文会乱码。
try { [Console]::OutputEncoding = [System.Text.Encoding]::UTF8 } catch { }

# 与仓库其它脚本保持一致的缓存/代理设置:默认 %LOCALAPPDATA% 下的缓存
# 在 CI 上是临时的,每次冷编译要几分钟;统一指到仓库内的目录可以让缓存复用。
$env:GOCACHE = 'D:\WorkSpace\GO\.gocache'
$env:GOMODCACHE = 'D:\WorkSpace\GO\.gomodcache'
$env:GOPROXY = 'https://goproxy.cn,direct'
$env:GOSUMDB = 'off'
$env:NETDISK_TEST_DSN = 'host=127.0.0.1 port=5432 dbname=netdisk_test user=netdisk password=Netdisk_LocalDev_2026! sslmode=disable'

# go 二进制:优先用仓库约定的固定路径(本地/CI 一致),找不到再退回 PATH。
$goExe = 'D:\Develop\go\bin\go.exe'
if (-not (Test-Path -LiteralPath $goExe)) { $goExe = 'go' }

$serverDir = Split-Path -Parent $PSScriptRoot
# profile 放系统临时目录并保留(不删):门禁红了要能立刻用
# `go tool cover -func=<profile>` 复查到底是哪些函数掉了。
$coverProfile = Join-Path ([System.IO.Path]::GetTempPath()) ('netdisk-cover-' + [guid]::NewGuid().ToString('N') + '.out')

Push-Location $serverDir
try {
    Write-Host "==> go test ./... -count=1 -covermode=set"
    Write-Host "    profile: $coverProfile"
    & $goExe test ./... -count=1 -coverprofile=$coverProfile -covermode=set
    $testExit = $LASTEXITCODE
    if ($testExit -ne 0) {
        Write-Host "FAIL: 测试未全部通过(退出码 $testExit),profile 不可信,门禁结论作废。" -ForegroundColor Red
        exit $testExit
    }

    Write-Host '==> go run ./cmd/coveragegate -config cmd/coveragegate/thresholds.json'
    & $goExe run ./cmd/coveragegate -profile $coverProfile -config cmd/coveragegate/thresholds.json
    $gateExit = $LASTEXITCODE
    if ($gateExit -eq 0) {
        Write-Host "==> 门禁通过(profile 已保留,可用 go tool cover -func=$coverProfile 复查)" -ForegroundColor Green
    } else {
        Write-Host "==> 门禁未通过(退出码 $gateExit)" -ForegroundColor Red
    }
    exit $gateExit
}
finally {
    Pop-Location
}
