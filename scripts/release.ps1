# Builds stripped, UPX-packed Windows release binaries into dist/ and,
# with -Publish, creates the GitHub release or replaces its assets.
param(
    [Parameter(Mandatory)][string]$Version,
    [switch]$Publish
)
$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$root = Split-Path $PSScriptRoot -Parent
$dist = Join-Path $root 'dist'
if (-not (Get-Command upx -ErrorAction SilentlyContinue)) { throw 'upx is not on PATH (winget install upx.upx)' }
New-Item -ItemType Directory -Force $dist | Out-Null

$env:GOOS = 'windows'; $env:GOARCH = 'amd64'; $env:CGO_ENABLED = '0'
$targets = @(
    @{ Pkg = './cmd/agentlink'; Out = 'agentlink.exe'; LdFlags = '-s -w' },
    @{ Pkg = './cmd/agentlink-tray'; Out = 'agentlink-tray.exe'; LdFlags = '-s -w -H=windowsgui' }
)
$assets = foreach ($t in $targets) {
    $exe = Join-Path $dist $t.Out
    Remove-Item $exe -ErrorAction SilentlyContinue
    go build -C $root -trimpath -ldflags $t.LdFlags -o $exe $t.Pkg
    if ($LASTEXITCODE) { throw "go build failed for $($t.Pkg)" }
    $stripped = (Get-Item $exe).Length
    upx --best --lzma -q $exe | Out-Null
    if ($LASTEXITCODE) { throw "upx failed for $exe" }
    upx -t -q $exe | Out-Null
    if ($LASTEXITCODE) { throw "upx test failed for $exe" }
    '{0}: {1:N0} -> {2:N0} bytes' -f $t.Out, $stripped, (Get-Item $exe).Length | Write-Host
    $exe
}

# Smoke: the packed CLI must still start.
& (Join-Path $dist 'agentlink.exe') --help *> $null
if ($LASTEXITCODE -gt 2) { throw 'packed agentlink.exe failed to start' }

if ($Publish) {
    $tag = "v$($Version.TrimStart('v'))"
    gh release view $tag --repo UberMorgott/agent-link *> $null
    if ($LASTEXITCODE -eq 0) {
        gh release upload $tag @assets --clobber --repo UberMorgott/agent-link
    } else {
        gh release create $tag @assets --repo UberMorgott/agent-link --title $tag --generate-notes
    }
    if ($LASTEXITCODE) { throw "gh release failed for $tag" }
}
