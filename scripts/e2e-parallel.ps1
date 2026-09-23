#Requires -Version 7
# End-to-end loopback run of parallel jobs, activity relay and the idle timeout:
# two agentlink desktop apps in -no-tray mode; node-b answers with max_jobs = 2 and
# a fake streaming agent (Claude stream-json) and a 4s idle timeout. node-a sends
# three requests at once: a long streaming job, a job that streams once and then
# hangs, and a short streaming job. Asserts: two jobs run at the same time, never
# three; the sender's inbox shows the agent's activity while jobs run; the hung
# job fails by the idle timeout while the other two complete.
[CmdletBinding()]
param([int]$TimeoutSeconds = 90)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
[Console]::OutputEncoding = [Text.Encoding]::UTF8

$root = Split-Path -Parent $PSScriptRoot
$bin = Join-Path $root 'bin'
$cli = Join-Path $bin 'agentlink.exe'
$tray = $cli # the desktop app is agentlink.exe without a command
$fake = Join-Path $bin 'fakeagent.exe'
$data = Join-Path $root '.data/e2e-parallel'

Write-Host '== build'
go -C $root build -o $cli ./cmd/agentlink
if ($LASTEXITCODE -ne 0) { throw 'build agentlink failed' }
go -C $root build -o $fake ./internal/worker/testdata/fakeagent
if ($LASTEXITCODE -ne 0) { throw 'build fakeagent failed' }

if (Test-Path $data) { Remove-Item -Recurse -Force $data }
$code = -join ((1..6) | ForEach-Object { 'ABCDEFGHJKLMNPQRSTUVWXYZ23456789'[(Get-Random -Maximum 32)] })
$work = New-Item -ItemType Directory -Force (Join-Path $data 'work')
$log = Join-Path $data 'runs.log'

$nodes = @{
    'node-a' = @{ listen = '127.0.0.1:7441'; api = '127.0.0.1:7541'; peerAddr = '127.0.0.1:7442'; handler = 'none' }
    'node-b' = @{ listen = '127.0.0.1:7442'; api = '127.0.0.1:7542'; peerAddr = '127.0.0.1:7441'; handler = 'claude' }
}
foreach ($name in $nodes.Keys) {
    $n = $nodes[$name]
    $dir = New-Item -ItemType Directory -Force (Join-Path $data $name)
    $settings = [ordered]@{
        node = $name; code = $code; peer_addr = $n.peerAddr; handler = $n.handler; work_dir = $work.FullName
        listen = $n.listen; api = $n.api
    }
    if ($name -eq 'node-b') { $settings.handler_command = @($fake); $settings.max_jobs = 2 }
    $n.config = Join-Path $dir 'config.json'
    $settings | ConvertTo-Json | Set-Content -Path $n.config
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
        $trayArgs = @('-no-tray', '-config', $nodes[$name].config)
        if ($name -eq 'node-b') { $trayArgs += @('-handler-idle-timeout', '4s') }
        $procs += Start-Process -FilePath $tray -ArgumentList $trayArgs -PassThru -WindowStyle Hidden
    }
    foreach ($name in 'node-a', 'node-b') {
        $deadline = (Get-Date).AddSeconds(30)
        while ($true) {
            & $cli inbox --config $nodes[$name].cli *> $null
            if ($LASTEXITCODE -eq 0) { break }
            if ((Get-Date) -gt $deadline) { throw "$name did not come up" }
            Start-Sleep -Milliseconds 200
        }
    }

    Write-Host '== three requests node-a -> node-b'
    $ids = [ordered]@{}
    foreach ($p in @("stream 10 $log long", "stall $log hung", "stream 3 $log short")) {
        $label = ($p -split ' ')[-1]
        # One chat per request: a chat's session takes one turn at a time, separate chats run in parallel.
        $chat = "$(& $cli chat new --config $nodes['node-a'].cli --with node-b)".Trim()
        if ($LASTEXITCODE -ne 0) { throw "chat new $label failed" }
        $ids[$label] = "$(& $cli send --config $nodes['node-a'].cli --chat $chat --ask node-b --body $p)".Trim()
        if ($LASTEXITCODE -ne 0) { throw "send $label failed" }
    }
    Write-Host "request ids: $(($ids.GetEnumerator() | ForEach-Object { "$($_.Key)=$($_.Value)" }) -join ' ')"

    # Collect replies with wait while polling the sender's inbox for activity.
    $replies = @{}
    $activity = @{}
    $deadline = (Get-Date).AddSeconds($TimeoutSeconds)
    while ($replies.Count -lt 3) {
        if ((Get-Date) -gt $deadline) { throw "only $($replies.Count) replies in $TimeoutSeconds s" }
        foreach ($line in @(& $cli inbox --config $nodes['node-a'].cli --limit 20)) {
            $e = $line | ConvertFrom-Json
            if ($e.direction -eq 'out' -and $e.PSObject.Properties['activity'] -and $e.activity) {
                $label = ($ids.GetEnumerator() | Where-Object { $_.Value -eq $e.id } | Select-Object -First 1).Key
                if ($label) {
                    if (-not $activity.ContainsKey($label)) { $activity[$label] = [Collections.Generic.List[string]]::new() }
                    if (-not $activity[$label].Contains($e.activity)) { $activity[$label].Add($e.activity) }
                }
            }
        }
        foreach ($line in @(& $cli wait --config $nodes['node-a'].cli --timeout 1)) {
            if (-not $line) { continue }
            $m = $line | ConvertFrom-Json
            $replies[$m.reply_to] = $m
            Write-Host "reply to $($m.reply_to.Substring(0, 8)): $($m.job_status) $($m.body)"
        }
    }

    Write-Host '== activity seen by the sender'
    foreach ($k in $activity.Keys) { Write-Host "  ${k}: $($activity[$k] -join ' | ')" }
    if (-not $activity.ContainsKey('long') -or $activity['long'].Count -lt 2) { throw 'no running activity reached the sender for the long job' }
    if (-not ($activity['long'] | Where-Object { $_ -like 'Read long-*.md' })) { throw "unexpected activity: $($activity['long'])" }

    $long, $hung, $short = $replies[$ids['long']], $replies[$ids['hung']], $replies[$ids['short']]
    if ($long.job_status -ne 'completed' -or $long.body -ne 'done long') { throw "long job: $($long | ConvertTo-Json -Compress)" }
    if ($short.job_status -ne 'completed' -or $short.body -ne 'done short') { throw "short job: $($short | ConvertTo-Json -Compress)" }
    if ($hung.job_status -ne 'failed' -or $hung.body -notlike '*агент завис (нет активности 4s)*') { throw "hung job: $($hung | ConvertTo-Json -Compress)" }

    Write-Host '== concurrency from the run log'
    $events = Get-Content $log | ForEach-Object { $f = $_ -split ' '; [pscustomobject]@{ what = $f[0]; label = $f[1]; at = [long]$f[2] } }
    $events | ForEach-Object { Write-Host "  $($_.what) $($_.label) $($_.at)" }
    $at = { param($w, $l) ($events | Where-Object { $_.what -eq $w -and $_.label -eq $l }).at }
    # long and hung start together; short starts only when hung's slot frees up (after its idle kill).
    if ((& $at 'start' 'hung') -ge (& $at 'end' 'long')) { throw 'long and hung did not run at the same time' }
    if ((& $at 'start' 'short') -lt (& $at 'start' 'hung') + 3500) { throw 'a third job started while two were running' }
    if ((& $at 'start' 'short') -ge (& $at 'end' 'long')) { throw 'short did not run next to long' }

    Write-Host 'E2E PARALLEL PASS'
}
finally {
    $procs | Where-Object { $_ -and -not $_.HasExited } | Stop-Process -Force
    Get-CimInstance Win32_Process -Filter "Name = 'fakeagent.exe'" | Where-Object { $_.CommandLine -like "*$bin*" } |
        ForEach-Object { Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue }
}
