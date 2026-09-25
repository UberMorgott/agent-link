[CmdletBinding()]
param(
    [Parameter(ValueFromRemainingArguments)]
    [string[]] $AgentLinkArguments
)

$ErrorActionPreference = 'Stop'
$candidates = [System.Collections.Generic.List[string]]::new()

if ($env:AGENTLINK_EXE) {
    $candidates.Add($env:AGENTLINK_EXE)
}

if ($env:APPDATA) {
    $marker = Join-Path $env:APPDATA 'agentlink\executable.path'
    if (Test-Path -LiteralPath $marker -PathType Leaf) {
        $candidates.Add((Get-Content -Raw -LiteralPath $marker).Trim().Trim('"'))
    }
}

foreach ($name in 'agentlink.exe', 'agentlink') {
    $command = Get-Command $name -CommandType Application -ErrorAction SilentlyContinue
    if ($command) {
        $candidates.Add($command.Source)
    }
}

$executable = $candidates |
    Where-Object { $_ -and (Test-Path -LiteralPath $_ -PathType Leaf) } |
    Select-Object -First 1

if (-not $executable) {
    Write-Error 'AgentLink is not installed. Start agentlink.exe once or set AGENTLINK_EXE to its absolute path.'
    exit 1
}

& $executable @AgentLinkArguments
exit $LASTEXITCODE
