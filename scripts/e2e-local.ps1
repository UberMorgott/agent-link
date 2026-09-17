#Requires -Version 7
# End-to-end loopback run: two serve processes, a->b message, b->a reply.
[CmdletBinding()]
param([int]$TimeoutSeconds = 15)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$root = Split-Path -Parent $PSScriptRoot
$bin = Join-Path $root 'bin/agentlink.exe'
$cfgA = Join-Path $root 'examples/node-a.json'
$cfgB = Join-Path $root 'examples/node-b.json'
$data = Join-Path $root '.data'

function Invoke-Agentlink {
    param([Parameter(ValueFromRemainingArguments)][string[]]$ArgList)
    $out = & $bin @ArgList
    [pscustomobject]@{ Code = $LASTEXITCODE; Output = $out }
}

function Wait-Api([string]$cfg) {
    $deadline = (Get-Date).AddSeconds($TimeoutSeconds)
    while ((Get-Date) -lt $deadline) {
        & $bin inbox --config $cfg *> $null
        if ($LASTEXITCODE -eq 0) { return }
        Start-Sleep -Milliseconds 200
    }
    throw "control API for $cfg did not come up"
}

function Receive-One([string]$cfg) {
    $r = Invoke-Agentlink wait --config $cfg --timeout $TimeoutSeconds
    if ($r.Code -ne 0) { throw "wait on $cfg exited $($r.Code)" }
    $lines = @($r.Output)
    if ($lines.Count -ne 1) { throw "wait on $cfg returned $($lines.Count) messages" }
    Write-Host "received: $($lines[0])"
    $lines[0] | ConvertFrom-Json
}

Write-Host '== build'
go -C $root build -o $bin ./cmd/agentlink
if ($LASTEXITCODE -ne 0) { throw 'go build failed' }

if (Test-Path $data) { Remove-Item -Recurse -Force $data }
New-Item -ItemType Directory -Force $data | Out-Null
$env:AGENTLINK_SECRET = -join ((1..32) | ForEach-Object { '{0:x2}' -f (Get-Random -Maximum 256) })

$procs = @()
try {
    Write-Host '== start nodes'
    foreach ($n in @(@('node-a', $cfgA), @('node-b', $cfgB))) {
        $procs += Start-Process -FilePath $bin -ArgumentList 'serve', '--config', $n[1] -PassThru -NoNewWindow `
            -RedirectStandardOutput (Join-Path $data "$($n[0]).out.log") `
            -RedirectStandardError (Join-Path $data "$($n[0]).err.log")
    }
    Wait-Api $cfgA
    Wait-Api $cfgB

    Write-Host '== send node-a -> node-b'
    $sent = Invoke-Agentlink send --config $cfgA --to node-b --body 'hello from node-a'
    if ($sent.Code -ne 0) { throw 'send a->b failed' }
    $id = "$($sent.Output)".Trim()
    Write-Host "sent id: $id"

    $msg = Receive-One $cfgB
    if ($msg.id -ne $id -or $msg.from -ne 'node-a' -or $msg.body -ne 'hello from node-a') { throw 'node-b got the wrong message' }

    Write-Host '== reply node-b -> node-a'
    $reply = Invoke-Agentlink send --config $cfgB --to node-a --body 'hello back from node-b' --reply-to $id
    if ($reply.Code -ne 0) { throw 'send b->a failed' }

    $back = Receive-One $cfgA
    if ($back.from -ne 'node-b' -or $back.reply_to -ne $id) { throw 'node-a got the wrong reply' }

    $empty = Invoke-Agentlink wait --config $cfgA --timeout 1
    if ($empty.Code -ne 2) { throw "empty wait exited $($empty.Code), want 2" }

    Write-Host 'E2E PASS'
}
finally {
    $procs | Where-Object { $_ -and -not $_.HasExited } | Stop-Process -Force
    Remove-Item Env:AGENTLINK_SECRET -ErrorAction SilentlyContinue
}
