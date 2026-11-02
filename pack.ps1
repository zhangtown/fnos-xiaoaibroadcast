# 打包 xiaoai-broadcast.fpk（飞牛原生应用，统一网关 / unix socket 模式）
#
# 用法：pwsh -NoProfile -File pack.ps1
# 依赖：Go（交叉编译 linux/amd64）、GNU tar（Git for Windows 自带）
# 产物：dist\xiaoai-broadcast-<version>.fpk
#
# 为什么不用 fnpack：本机上 fnpack.exe 只要一 mkdir 就报 "Access is denied"
# （安全软件拦第三方未签名程序的写入，连 `fnpack create` 都建不出目录）。
# fpk 本身就是 tar.gz，内含 app.tgz + manifest + cmd/ config/ wizard/ ICON*.PNG，
# 所以用 GNU tar 按同一结构打出来即可；--mode=0755 保证 cmd/main 等脚本可执行。
$ErrorActionPreference = 'Stop'
$root = $PSScriptRoot
$pkg  = Join-Path $root 'packaging\fnos\xiaoai-broadcast'
$dist = Join-Path $root 'dist'
New-Item -ItemType Directory -Force -Path $dist | Out-Null

# GNU tar 是 MSYS 程序，只认 /d/... 形式路径，且需要同目录的 gzip
$gitUsr = 'C:\Program Files\Git\usr\bin'
$tar = Join-Path $gitUsr 'tar.exe'
if (-not (Test-Path $tar)) { throw "找不到 GNU tar：$tar" }
$env:PATH = "$gitUsr;$env:PATH"
function To-Msys([string]$p) {
    $full = [System.IO.Path]::GetFullPath($p)
    if ($full -match '^([A-Za-z]):\\(.*)$') { return '/' + $matches[1].ToLower() + '/' + ($matches[2] -replace '\\', '/') }
    return ($full -replace '\\', '/')
}

$manifest = Join-Path $pkg 'manifest'
$version = (Get-Content $manifest | Where-Object { $_ -match '^version=' } | Select-Object -First 1) -replace '^version=', ''
if (-not $version) { throw 'manifest 里没有 version=' }
"== version: $version"

# ---- 1. 图标（在编译之前跑，保证包里图标与代码同批）----
& (Join-Path $root 'tools\mkicon.ps1') | Out-Null

# ---- 2. 交叉编译 ----
# go 的位置按「环境变量 → PATH」找；不写死路径，免得把本机用户名带进仓库
$go = $env:GO_EXE
if (-not $go) {
    $cmd = Get-Command go -ErrorAction SilentlyContinue
    if ($cmd) { $go = $cmd.Source }
}
if (-not $go -or -not (Test-Path $go)) {
    throw '找不到 go：把 Go 加进 PATH，或设环境变量 GO_EXE 指向 go.exe（例如 $env:GO_EXE="C:\go\bin\go.exe"）'
}
$env:GOOS = 'linux'; $env:GOARCH = 'amd64'; $env:CGO_ENABLED = '0'
$outBin = Join-Path $pkg 'app\bin\xiaoai-broadcast'
Push-Location $root
try {
    & $go build -trimpath -ldflags "-s -w -X main.version=$version" -o $outBin .
    if ($LASTEXITCODE -ne 0) { throw 'go build 失败' }
} finally { Pop-Location }
"== built $([math]::Round((Get-Item $outBin).Length / 1MB, 2)) MB"

$pkgMs  = To-Msys $pkg
$distMs = To-Msys $dist

# ---- 3. app.tgz（app/ 目录的 tar 包）----
$appTgz = Join-Path $pkg 'app.tgz'
if (Test-Path $appTgz) { Remove-Item $appTgz -Force }
& $tar --mode=0755 --owner=0 --group=0 --numeric-owner -czf "$pkgMs/app.tgz" -C "$pkgMs/app" .
if ($LASTEXITCODE -ne 0) { throw '打 app.tgz 失败' }

# ---- 4. fpk（外层 tar.gz）----
$fpkName = "xiaoai-broadcast-$version.fpk"
$fpk = Join-Path $dist $fpkName
if (Test-Path $fpk) { Remove-Item $fpk -Force }
try {
    & $tar --mode=0755 --owner=0 --group=0 --numeric-owner -czf "$distMs/$fpkName" -C "$pkgMs" `
        app.tgz cmd config manifest ICON.PNG ICON_256.PNG
    if ($LASTEXITCODE -ne 0) { throw '打 fpk 失败' }
} finally {
    Remove-Item $appTgz -Force -ErrorAction SilentlyContinue
}
"== fpk: $fpk  ($([math]::Round((Get-Item $fpk).Length / 1MB, 2)) MB)"

# ---- 5. 自检：结构 + 可执行位 ----
"== 外层内容："
& $tar -tzvf (To-Msys $fpk)
"== app.tgz 里的关键文件："
$tmpTgz = Join-Path $env:TEMP 'xb-app.tgz'
& $tar -xzOf (To-Msys $fpk) app.tgz > $tmpTgz
& $tar -tzvf (To-Msys $tmpTgz) | Select-String 'bin/xiaoai-broadcast|ui/config'
Remove-Item $tmpTgz -Force -ErrorAction SilentlyContinue
