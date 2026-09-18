# Builds stripped, UPX-packed release executables and checksums.txt into dist/
# and, with -Publish, creates the GitHub release or replaces its assets.
# .github/workflows/release.yml runs this same script on a pushed v* tag.
#
# Asset names (internal/selfupdate.AssetName must match):
#   windows/amd64  agentlink.exe, agentlink-tray.exe
#   linux/amd64    agentlink-linux-amd64, agentlink-tray-linux-amd64
# checksums.txt is `sha256sum` output over the packed files; self-update
# refuses any release without it.
param(
    [Parameter(Mandatory)][string]$Version,
    [switch]$Publish
)
$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$Version = $Version.TrimStart('v')
if ($Version -notmatch '^\d+\.\d+\.\d+$') { throw "version $Version is not X.Y.Z" }
$root = Split-Path $PSScriptRoot -Parent
$dist = Join-Path $root 'dist'
if (-not (Get-Command upx -ErrorAction SilentlyContinue)) { throw 'upx is not on PATH (winget install upx.upx)' }
New-Item -ItemType Directory -Force $dist | Out-Null
Remove-Item (Join-Path $dist 'checksums.txt') -ErrorAction SilentlyContinue

$ldVersion = "-X github.com/UberMorgott/agent-link/internal/selfupdate.Version=$Version"
$targets = foreach ($os in 'windows', 'linux') {
    $ext = if ($os -eq 'windows') { '.exe' } else { '-linux-amd64' }
    @{ OS = $os; Pkg = './cmd/agentlink'; Out = "agentlink$ext"; LdFlags = "-s -w $ldVersion" }
    $gui = if ($os -eq 'windows') { ' -H=windowsgui' } else { '' }
    @{ OS = $os; Pkg = './cmd/agentlink-tray'; Out = "agentlink-tray$ext"; LdFlags = "-s -w$gui $ldVersion" }
}
$env:GOARCH = 'amd64'; $env:CGO_ENABLED = '0'
$assets = foreach ($t in $targets) {
    $env:GOOS = $t.OS
    $exe = Join-Path $dist $t.Out
    Remove-Item $exe -ErrorAction SilentlyContinue
    go build -C $root -trimpath -ldflags $t.LdFlags -o $exe $t.Pkg
    if ($LASTEXITCODE) { throw "go build failed for $($t.Pkg) ($($t.OS))" }
    $stripped = (Get-Item $exe).Length
    upx --best --lzma -q $exe | Out-Null
    if ($LASTEXITCODE) { throw "upx failed for $exe" }
    upx -t -q $exe | Out-Null
    if ($LASTEXITCODE) { throw "upx test failed for $exe" }
    '{0}: {1:N0} -> {2:N0} bytes' -f $t.Out, $stripped, (Get-Item $exe).Length | Write-Host
    $exe
}
Remove-Item Env:GOOS, Env:GOARCH, Env:CGO_ENABLED

# `sha256sum` format with LF line endings, as internal/selfupdate parses it.
$sums = Join-Path $dist 'checksums.txt'
$lines = foreach ($a in $assets) { '{0}  {1}' -f (Get-FileHash $a -Algorithm SHA256).Hash.ToLower(), (Split-Path $a -Leaf) }
[IO.File]::WriteAllText($sums, ($lines -join "`n") + "`n")
$assets += $sums

# Smoke: the packed CLI for this machine must start and know its version.
$cli = Join-Path $dist ($IsWindows ? 'agentlink.exe' : 'agentlink-linux-amd64')
$got = & $cli version
if ($LASTEXITCODE -or $got -ne $Version) { throw "packed $cli reports '$got', want $Version" }

if ($Publish) {
    $tag = "v$Version"
    gh release view $tag --repo UberMorgott/agent-link *> $null
    if ($LASTEXITCODE -eq 0) {
        gh release upload $tag @assets --clobber --repo UberMorgott/agent-link
    } else {
        gh release create $tag @assets --repo UberMorgott/agent-link --title $tag --generate-notes
    }
    if ($LASTEXITCODE) { throw "gh release failed for $tag" }
}
