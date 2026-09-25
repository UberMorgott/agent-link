$ErrorActionPreference = 'Stop'
$testDir = Join-Path ([System.IO.Path]::GetTempPath()) ("agentlink-plugin-test-" + [guid]::NewGuid().ToString('N'))
$fakeExe = Join-Path $testDir 'agentlink.cmd'
$output = Join-Path $testDir 'args.txt'
$launcher = Join-Path $PSScriptRoot 'agentlink.ps1'

try {
    New-Item -ItemType Directory -Path $testDir | Out-Null
    Set-Content -LiteralPath $fakeExe -Encoding Ascii -Value "@echo off`necho %* > `"%AGENTLINK_TEST_OUTPUT%`"`nexit /b 17"

    $env:AGENTLINK_EXE = $fakeExe
    $env:AGENTLINK_TEST_OUTPUT = $output
    & pwsh -NoProfile -File $launcher mcp --probe

    if ($LASTEXITCODE -ne 17) {
        throw "launcher exit code was $LASTEXITCODE, expected 17"
    }
    if ((Get-Content -Raw -LiteralPath $output).Trim() -ne 'mcp --probe') {
        throw 'launcher did not preserve arguments'
    }
    'agentlink plugin launcher: PASS'
}
finally {
    Remove-Item Env:AGENTLINK_EXE -ErrorAction SilentlyContinue
    Remove-Item Env:AGENTLINK_TEST_OUTPUT -ErrorAction SilentlyContinue
    Remove-Item -LiteralPath $testDir -Recurse -Force -ErrorAction SilentlyContinue
}
