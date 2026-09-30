# Tests the publish guard of release.ps1 without Pester: pwsh -File scripts/release.test.ps1
# Exit code 0 when every case passes. Nothing is built or published: each
# case stops at the guard, in a throwaway repository.
$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$dir = Join-Path ([IO.Path]::GetTempPath()) "agentlink-release-$PID"
$failed = 0
function Check([string]$Name, [bool]$Ok) {
    if ($Ok) { Write-Host "PASS $Name" } else { Write-Host "FAIL $Name"; $script:failed++ }
}
# Publish runs release.ps1 -Publish in the throwaway repository; its error text.
function Publish {
    $out = pwsh -NoProfile -File (Join-Path $dir 'scripts/release.ps1') -Version 9.9.9 -Publish 2>&1
    if ($LASTEXITCODE -eq 0) { return '<no error>' }
    return ($out | Out-String)
}
try {
    New-Item -ItemType Directory -Force (Join-Path $dir 'scripts') | Out-Null
    foreach ($f in 'release.ps1', 'swap.ps1', 'release-notes.ps1') {
        Copy-Item (Join-Path $PSScriptRoot $f) (Join-Path $dir 'scripts')
    }
    git -C $dir init -q
    git -C $dir -c user.name=t -c user.email=t@t add -A
    git -C $dir -c user.name=t -c user.email=t@t commit -q -m one
    Set-Content -LiteralPath (Join-Path $dir 'untracked.txt') -Value 'x'

    Check 'no tag is refused' ((Publish) -match 'tag v9\.9\.9 does not exist')

    git -C $dir tag v9.9.9
    git -C $dir -c user.name=t -c user.email=t@t commit -q --allow-empty -m two
    Check 'HEAD past the tag is refused' ((Publish) -match 'is not v9\.9\.9')

    git -C $dir checkout -q v9.9.9
    Add-Content -LiteralPath (Join-Path $dir 'scripts/swap.ps1') -Value '# changed'
    Check 'a changed tracked file is refused' ((Publish) -match 'has changes')

    git -C $dir checkout -q -- scripts/swap.ps1
    Check 'the tagged commit passes the guard' ((Publish) -notmatch 'has changes|does not exist|is not v9')
}
finally {
    Remove-Item -LiteralPath $dir -Recurse -Force -ErrorAction SilentlyContinue
}
exit $failed
