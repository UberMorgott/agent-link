# Tests swap.ps1 without Pester: pwsh -File scripts/swap.test.ps1
# Exit code 0 when every case passes.
$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
. (Join-Path $PSScriptRoot 'swap.ps1')

$dir = Join-Path ([IO.Path]::GetTempPath()) "agentlink-swap-$PID"
New-Item -ItemType Directory -Force $dir | Out-Null
$failed = 0
function Check([string]$Name, [bool]$Ok) {
    if ($Ok) { Write-Host "PASS $Name" } else { Write-Host "FAIL $Name"; $script:failed++ }
}
try {
    $exe = Join-Path $dir 'agentlink.exe'
    $built = Join-Path $dir 'built.exe'

    # Success: the new build replaces the live file, the old one is parked.
    Set-Content -LiteralPath $exe -Value 'old'
    Set-Content -LiteralPath $built -Value 'new'
    Install-Built $built $exe
    Check 'swap installs the new build' ((Get-Content -LiteralPath $exe) -eq 'new')
    Check 'swap parks the old build' ((Get-Content -LiteralPath (Join-Path $dir '.agentlink.exe.old')) -eq 'old')

    # Failure of the second move (the build is gone): the parked file returns.
    Remove-Item -LiteralPath (Join-Path $dir '.agentlink.exe.old') -Force
    $threw = $false
    try { Install-Built (Join-Path $dir 'missing.exe') $exe } catch { $threw = $true }
    Check 'failed swap reports the error' $threw
    Check 'failed swap restores the live file' ((Test-Path -LiteralPath $exe) -and (Get-Content -LiteralPath $exe) -eq 'new')
}
finally {
    Remove-Item -LiteralPath $dir -Recurse -Force -ErrorAction SilentlyContinue
}
exit $failed
