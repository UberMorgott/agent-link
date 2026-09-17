#Requires -Version 7
# End-to-end loopback run of the desktop app: two agentlink-tray nodes in
# -no-tray mode, node-b answers requests with a fake echo agent, node-a sends a
# request and must receive the automatic reply.
# -RealClaude / -RealCodex run the installed `claude` / `codex` CLI instead of the fake agent (one real smoke).
[CmdletBinding()]
param([int]$TimeoutSeconds = 0, [switch]$RealClaude, [switch]$RealCodex)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

if ($RealClaude -and $RealCodex) { throw 'use -RealClaude or -RealCodex, not both' }
$real = $RealClaude -or $RealCodex
if ($TimeoutSeconds -le 0) { $TimeoutSeconds = if ($real) { 300 } else { 30 } }

$root = Split-Path -Parent $PSScriptRoot
$bin = Join-Path $root 'bin'
$tray = Join-Path $bin 'agentlink-tray.exe'
$cli = Join-Path $bin 'agentlink.exe'
$fake = Join-Path $bin 'fakeagent.exe'
$data = Join-Path $root '.data/e2e-worker'

Write-Host '== build'
go -C $root build -ldflags '-H=windowsgui' -o $tray ./cmd/agentlink-tray
if ($LASTEXITCODE -ne 0) { throw 'build agentlink-tray failed' }
go -C $root build -o $cli ./cmd/agentlink
if ($LASTEXITCODE -ne 0) { throw 'build agentlink failed' }
go -C $root build -o $fake ./internal/worker/testdata/fakeagent
if ($LASTEXITCODE -ne 0) { throw 'build fakeagent failed' }

if (Test-Path $data) { Remove-Item -Recurse -Force $data }
$secret = -join ((1..32) | ForEach-Object { '{0:x2}' -f (Get-Random -Maximum 256) })
$work = New-Item -ItemType Directory -Force (Join-Path $data 'work')
Set-Content -Path (Join-Path $work 'note.txt') -Value 'kiwi'
$prompt = if ($real) { 'Read note.txt in the current folder. Reply with only the word pong followed by the word in that file.' } else { 'ping from node-a' }

$nodes = @{
    'node-a' = @{ listen = '127.0.0.1:7431'; api = '127.0.0.1:7531'; peer = 'node-b'; peerAddr = '127.0.0.1:7432'; handler = 'none' }
    'node-b' = @{ listen = '127.0.0.1:7432'; api = '127.0.0.1:7532'; peer = 'node-a'; peerAddr = '127.0.0.1:7431'; handler = $(if ($RealCodex) { 'codex' } else { 'claude' }) }
}
foreach ($name in $nodes.Keys) {
    $n = $nodes[$name]
    $dir = New-Item -ItemType Directory -Force (Join-Path $data $name)
    $settings = [ordered]@{
        node = $name; listen = $n.listen; peer_name = $n.peer; peer_addr = $n.peerAddr; secret = $secret
        areas = @(); handler = $n.handler; work_dir = $work.FullName; autostart = $false; api = $n.api
    }
    if ($name -eq 'node-b' -and -not $real) { $settings.handler_command = @($fake) }
    $n.config = Join-Path $dir 'config.json'
    $settings | ConvertTo-Json | Set-Content -Path $n.config
    # A CLI config pointing at the same control API, for send/wait.
    $n.cli = Join-Path $dir 'cli.json'
    [ordered]@{
        node = $name; listen = $n.listen; api = $n.api; data_dir = 'data'; secret_env = 'UNUSED'
        areas = @(); peers = @(@{ name = $n.peer; addr = $n.peerAddr })
    } | ConvertTo-Json | Set-Content -Path $n.cli
}

$procs = @()
try {
    Write-Host '== start nodes'
    foreach ($name in 'node-a', 'node-b') {
        $procs += Start-Process -FilePath $tray -ArgumentList '-no-tray', '-config', $nodes[$name].config -PassThru
    }
    foreach ($name in 'node-a', 'node-b') {
        $deadline = (Get-Date).AddSeconds($TimeoutSeconds)
        while ($true) {
            & $cli inbox --config $nodes[$name].cli *> $null
            if ($LASTEXITCODE -eq 0) { break }
            if ((Get-Date) -gt $deadline) { throw "$name did not come up" }
            Start-Sleep -Milliseconds 200
        }
    }

    Write-Host '== request node-a -> node-b'
    $id = "$(& $cli send --config $nodes['node-a'].cli --to node-b --body $prompt)".Trim()
    if ($LASTEXITCODE -ne 0) { throw 'send failed' }
    Write-Host "request id: $id"

    $out = & $cli wait --config $nodes['node-a'].cli --timeout $TimeoutSeconds
    if ($LASTEXITCODE -ne 0) { throw "wait on node-a exited $LASTEXITCODE" }
    $lines = @($out)
    if ($lines.Count -ne 1) { throw "node-a received $($lines.Count) messages" }
    Write-Host "received: $($lines[0])"
    $reply = $lines[0] | ConvertFrom-Json
    $wantBody = if ($real) { 'pong kiwi' } else { 'echo: ping from node-a' }
    if ($reply.from -ne 'node-b' -or $reply.reply_to -ne $id -or $reply.body.Trim().TrimEnd('.').ToLowerInvariant() -ne $wantBody) {
        throw 'node-a got the wrong auto-reply'
    }

    # Replies must not trigger the handler: send one to node-b and expect nothing back.
    & $cli send --config $nodes['node-a'].cli --to node-b --body 'thanks' --reply-to $reply.id | Out-Null
    & $cli wait --config $nodes['node-a'].cli --timeout 3 | Out-Null
    if ($LASTEXITCODE -ne 2) { throw "a reply triggered the handler (wait exited $LASTEXITCODE)" }

    Write-Host 'E2E WORKER PASS'
}
finally {
    $procs | Where-Object { $_ -and -not $_.HasExited } | Stop-Process -Force
}
