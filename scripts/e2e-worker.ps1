#Requires -Version 7
# End-to-end loopback run of the desktop app: two agentlink-tray nodes in
# -no-tray mode, node-b answers requests with a fake echo agent, node-a sends a
# request and must receive the automatic reply.
# -RealClaude / -RealCodex run the installed `claude` / `codex` CLI instead of the fake agent (one real smoke);
# -AgentPath runs that program for it (saved as agent_path), e.g. the Codex app's own codex.exe.
# -WorkDir + -Prompt ask a real agent your own question in your own folder; the answer is printed, not checked.
[CmdletBinding()]
param([int]$TimeoutSeconds = 0, [switch]$RealClaude, [switch]$RealCodex, [string]$AgentPath = '', [string]$WorkDir = '', [string]$Prompt = '')

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
[Console]::OutputEncoding = [Text.Encoding]::UTF8

if ($RealClaude -and $RealCodex) { throw 'use -RealClaude or -RealCodex, not both' }
$real = $RealClaude -or $RealCodex
$custom = [bool]$Prompt
if ($custom -and -not ($real -and $WorkDir)) { throw '-Prompt needs -WorkDir and -RealClaude or -RealCodex' }
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
# The settings page's "Создать код" button: 6 letters/digits, the same on both sides.
$code = -join ((1..6) | ForEach-Object { 'ABCDEFGHJKLMNPQRSTUVWXYZ23456789'[(Get-Random -Maximum 32)] })
$work = New-Item -ItemType Directory -Force (Join-Path $data 'work')
Set-Content -Path (Join-Path $work 'note.txt') -Value 'kiwi'
if ($custom) { $work = Get-Item $WorkDir }
$prompt = if ($custom) { $Prompt } elseif ($real) { 'Read note.txt in the current folder. Reply with only the word pong followed by the word in that file.' } else { 'ping from node-a' }

$nodes = @{
    'node-a' = @{ listen = '127.0.0.1:7431'; api = '127.0.0.1:7531'; peer = 'node-b'; peerAddr = '127.0.0.1:7432'; handler = 'none' }
    'node-b' = @{ listen = '127.0.0.1:7432'; api = '127.0.0.1:7532'; peer = 'node-a'; peerAddr = '127.0.0.1:7431'; handler = $(if ($RealCodex) { 'codex' } else { 'claude' }) }
}
foreach ($name in $nodes.Keys) {
    $n = $nodes[$name]
    $dir = New-Item -ItemType Directory -Force (Join-Path $data $name)
    $settings = [ordered]@{
        node = $name; code = $code; peer_addr = $n.peerAddr; handler = $n.handler; work_dir = $work.FullName
        listen = $n.listen; api = $n.api
    }
    if ($name -eq 'node-b' -and -not $real) { $settings.handler_command = @($fake) }
    if ($name -eq 'node-b' -and $real -and $AgentPath) { $settings.agent_path = $AgentPath }
    $n.config = Join-Path $dir 'config.json'
    $settings | ConvertTo-Json | Set-Content -Path $n.config
    # A CLI config pointing at the same control API, for send/wait.
    $n.cli = Join-Path $dir 'cli.json'
    [ordered]@{
        node = $name; listen = $n.listen; api = $n.api; data_dir = 'data'; secret_env = 'UNUSED'
        areas = @(); peers = @(@{ addr = $n.peerAddr })
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

    # Wait for the reply in short slices, collecting the handler's activity
    # from node-a's inbox meanwhile (a real agent streams its tool calls).
    $activity = [Collections.Generic.List[string]]::new()
    $deadline = (Get-Date).AddSeconds($TimeoutSeconds)
    $lines = @()
    while ($lines.Count -eq 0) {
        if ((Get-Date) -gt $deadline) { throw "no reply in $TimeoutSeconds s" }
        foreach ($line in @(& $cli inbox --config $nodes['node-a'].cli --limit 5)) {
            $e = $line | ConvertFrom-Json
            if ($e.id -eq $id -and $e.PSObject.Properties['activity'] -and $e.activity -and -not $activity.Contains($e.activity)) {
                $activity.Add($e.activity)
            }
        }
        $lines = @(& $cli wait --config $nodes['node-a'].cli --timeout 1 | Where-Object { $_ })
        if ($LASTEXITCODE -notin 0, 2) { throw "wait on node-a exited $LASTEXITCODE" }
    }
    Write-Host "activity seen by node-a: $($activity -join ' | ')"
    if ($real -and $activity.Count -eq 0) { throw 'the real agent streamed no activity to the sender' }
    if ($lines.Count -ne 1) { throw "node-a received $($lines.Count) messages" }
    Write-Host "received: $($lines[0])"
    $reply = $lines[0] | ConvertFrom-Json
    $wantBody = if ($real) { 'pong kiwi' } else { 'echo: ping from node-a' }
    if ($custom) {
        if ($reply.from -ne 'node-b' -or $reply.reply_to -ne $id -or $reply.job_status -ne 'completed') { throw 'node-a got no answer' }
        Write-Host "answer: $($reply.body)"
    }
    elseif ($reply.from -ne 'node-b' -or $reply.reply_to -ne $id -or $reply.body.Trim().TrimEnd('.').ToLowerInvariant() -ne $wantBody) {
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
