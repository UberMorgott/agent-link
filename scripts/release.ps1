# Builds the stripped, UPX-packed release executable into dist/ and, with
# -Publish, creates the GitHub release (notes from scripts/release-notes.ps1)
# or replaces its asset. Releases are built and published locally with this
# script; .github/workflows/release.yml runs it only when started by hand.
#
# A release has one file per platform; for now only Windows is released:
#   windows/amd64  agentlink.exe   (internal/selfupdate.AssetName must match)
# Self-update verifies the download against the sha256 "digest" the GitHub
# releases API reports for the asset, so no checksums file is published.
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

$ldVersion = "-X github.com/UberMorgott/agent-link/internal/selfupdate.Version=$Version"
$exe = Join-Path $dist 'agentlink.exe'

# dist/agentlink.exe is live: the tray app, MCP servers and hooks run it and
# may start it at any moment, which locks the file (upx: Permission denied).
# Build, pack and test in a private folder on the same volume, then swap the
# result in the way the self-updater does: the running file is renamed aside
# (a running executable cannot be deleted, only renamed) and removed later by
# the app's cleanup (internal/selfupdate OldPath and its numbered slots).
$work = Join-Path $dist ".build-$PID"
$built = Join-Path $work 'agentlink.exe'
New-Item -ItemType Directory -Force $work | Out-Null

function Move-Aside([string]$Path) {
    if (-not (Test-Path -LiteralPath $Path)) { return }
    $old = Join-Path (Split-Path $Path -Parent) ('.' + (Split-Path $Path -Leaf) + '.old')
    for ($slot = 0; $slot -le 64; $slot++) {
        $target = if ($slot) { "$old.$slot" } else { $old }
        if (Test-Path -LiteralPath $target) {
            # Still running an older build: try the next slot.
            try { Remove-Item -LiteralPath $target -Force -ErrorAction Stop } catch { continue }
        }
        [IO.File]::Move($Path, $target)
        return
    }
    throw "no free slot to move $Path aside"
}

try {
    $env:GOOS = 'windows'; $env:GOARCH = 'amd64'; $env:CGO_ENABLED = '0'
    try {
        go build -C $root -trimpath -ldflags "-s -w $ldVersion" -o $built ./cmd/agentlink
        if ($LASTEXITCODE) { throw 'go build failed for ./cmd/agentlink (windows/amd64)' }
    }
    finally {
        $env:GOOS = $null; $env:GOARCH = $null; $env:CGO_ENABLED = $null
    }
    $stripped = (Get-Item $built).Length
    upx --best --lzma -q $built | Out-Null
    if ($LASTEXITCODE) { throw "upx failed for $built" }
    upx -t -q $built | Out-Null
    if ($LASTEXITCODE) { throw "upx test failed for $built" }
    '{0}: {1:N0} -> {2:N0} bytes' -f (Split-Path $exe -Leaf), $stripped, (Get-Item $built).Length | Write-Host

    # Smoke: the packed executable must start as the CLI, know its version and
    # report it on stdout with exit code 0.
    if ($IsWindows) {
        $got = & $built version
        if ($LASTEXITCODE -or $got -ne $Version) { throw "packed $built reports '$got', want $Version" }
    }

    Move-Aside $exe
    [IO.File]::Move($built, $exe)
}
finally {
    Remove-Item -LiteralPath $work -Recurse -Force -ErrorAction SilentlyContinue
}
$assets = @($exe)
if ($Publish) {
    $tag = "v$Version"
    gh release view $tag --repo UberMorgott/agent-link *> $null
    if ($LASTEXITCODE -eq 0) {
        gh release upload $tag @assets --clobber --repo UberMorgott/agent-link
    } else {
        $notes = Join-Path $dist 'notes.md'
        & (Join-Path $PSScriptRoot 'release-notes.ps1') -Tag $tag | Set-Content -Encoding utf8NoBOM $notes
        gh release create $tag @assets --repo UberMorgott/agent-link --title $tag --notes-file $notes
    }
    if ($LASTEXITCODE) { throw "gh release failed for $tag" }
}
