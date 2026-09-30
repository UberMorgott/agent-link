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
$tag = "v$Version"

# A published release is built from its tag's commit, nothing else: a clean
# tree (untracked files aside) whose HEAD is the tag.
if ($Publish) {
    $changed = git -C $root status --porcelain --untracked-files=no
    if ($LASTEXITCODE) { throw "git status failed in $root" }
    if ($changed) { throw "the working tree of $root has changes; a release is built from its tag only:`n$($changed -join "`n")" }
    $tagged = git -C $root rev-parse --verify --quiet "$tag^{commit}"
    if (-not $tagged) { throw "tag $tag does not exist: tag the release commit first" }
    $head = git -C $root rev-parse HEAD
    if ($head -ne $tagged) { throw "HEAD $head is not $tag ($tagged): check out the tagged commit" }
}
if (-not (Get-Command upx -ErrorAction SilentlyContinue)) { throw 'upx is not on PATH (winget install upx.upx)' }
New-Item -ItemType Directory -Force $dist | Out-Null

$ldVersion = "-X github.com/UberMorgott/agent-link/internal/selfupdate.Version=$Version"
$exe = Join-Path $dist 'agentlink.exe'

# dist/agentlink.exe is live: the tray app, MCP servers and hooks run it and
# may start it at any moment, which locks the file (upx: Permission denied).
# Build, pack and test in a private folder on the same volume, then swap the
# result in the way the self-updater does (swap.ps1).
$work = Join-Path $dist ".build-$PID"
$built = Join-Path $work 'agentlink.exe'
New-Item -ItemType Directory -Force $work | Out-Null
. (Join-Path $PSScriptRoot 'swap.ps1')

try {
    # The target is set for this build only; the caller's own values return.
    $goEnv = @{ GOOS = 'windows'; GOARCH = 'amd64'; CGO_ENABLED = '0' }
    $saved = @{}
    foreach ($k in $goEnv.Keys) {
        $saved[$k] = [Environment]::GetEnvironmentVariable($k)
        [Environment]::SetEnvironmentVariable($k, $goEnv[$k])
    }
    try {
        go build -C $root -trimpath -ldflags "-s -w $ldVersion" -o $built ./cmd/agentlink
        if ($LASTEXITCODE) { throw 'go build failed for ./cmd/agentlink (windows/amd64)' }
    }
    finally {
        foreach ($k in $saved.Keys) { [Environment]::SetEnvironmentVariable($k, $saved[$k]) }
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

    Install-Built $built $exe
}
finally {
    Remove-Item -LiteralPath $work -Recurse -Force -ErrorAction SilentlyContinue
}
$assets = @($exe)
if ($Publish) {
    gh release view $tag --repo UberMorgott/agent-link *> $null
    if ($LASTEXITCODE -eq 0) {
        gh release upload $tag @assets --clobber --repo UberMorgott/agent-link
    } else {
        $notes = Join-Path $dist 'notes.md'
        & (Join-Path $PSScriptRoot 'release-notes.ps1') -Tag $tag -Root $root | Set-Content -Encoding utf8NoBOM $notes
        gh release create $tag @assets --repo UberMorgott/agent-link --title $tag --notes-file $notes --verify-tag
    }
    if ($LASTEXITCODE) { throw "gh release failed for $tag" }
}
