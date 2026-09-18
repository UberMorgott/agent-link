#Requires -Version 7
# Two desktop apps on one machine as two people. Each agentlink-tray runs
# headless (-no-tray) with its own settings folder and API port, never touching
# %APPDATA%\agentlink or the autostart entry. Setup, sending, reading the inbox
# and quitting all go through the web UI endpoints the way the pages call them
# (per-run token from the page, same-origin headers).
#
# Checks: a->b request answered by b's fake echo agent and shown in a's inbox;
# quitting b during the first of three slow jobs leaves that agent running, and
# restarting b reattaches to it: all three complete (each run once, in order)
# with queued/running shown on a meanwhile; saving settings while a job runs
# leaves its agent running; an agent killed while b is down (a reboot) starts
# over once, killed again the job fails with a reply, and no agent processes
# are left behind; a restarted app keeps its inbox history; Quit exits both.
# -Address binds both peer listeners to that IP (e.g. the ZeroTier address).
[CmdletBinding()]
param([string]$Address = '127.0.0.1', [int]$TimeoutSeconds = 30)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$root = Split-Path -Parent $PSScriptRoot
$bin = Join-Path $root 'bin'
$tray = Join-Path $bin 'agentlink-tray.exe'
$fake = Join-Path $bin 'fakeagent.exe'
$data = Join-Path $root '.data/e2e-tray'

function Get-RealState {
    $dir = Join-Path $env:APPDATA 'agentlink'
    $files = if (Test-Path $dir) {
        Get-ChildItem -Recurse -Force $dir | ForEach-Object { "$($_.FullName)|$($_.PSIsContainer ? 'dir' : $_.Length)|$($_.LastWriteTimeUtc.Ticks)" }
    } else { 'absent' }
    $run = (Get-ItemProperty 'HKCU:\Software\Microsoft\Windows\CurrentVersion\Run' -Name agentlink -ErrorAction SilentlyContinue)?.agentlink
    "$($files -join "`n")`nrun=$run"
}

function Wait-Until([scriptblock]$Condition, [string]$What) {
    $deadline = (Get-Date).AddSeconds($TimeoutSeconds)
    while ($true) {
        $v = try { & $Condition } catch { $null }
        if ($v) { return $v }
        if ((Get-Date) -gt $deadline) { throw "timed out waiting for $What" }
        Start-Sleep -Milliseconds 250
    }
}

# Start-Node launches a tray app and returns a handle with the page token.
function Start-Node([hashtable]$n, [switch]$NoApiFlag) {
    $argv = @('-no-tray', '-config', $n.config)
    if (-not $NoApiFlag) { $argv += '-api', $n.api }
    $n.proc = Start-Process -FilePath $tray -ArgumentList $argv -PassThru
    $base = "http://$($n.api)"
    $page = Wait-Until { Invoke-WebRequest -Uri "$base/ui/settings" -TimeoutSec 2 } "$($n.name) web UI"
    if ($page.Content -notmatch 'name="agentlink-token" content="([0-9a-f]+)"') { throw "$($n.name): no token in page" }
    $n.headers = @{ 'X-Agentlink-Token' = $Matches[1]; 'Origin' = $base; 'Sec-Fetch-Site' = 'same-origin' }
}

function Invoke-Ui([hashtable]$n, [string]$Method, [string]$Path, $Body) {
    $p = @{ Method = $Method; Uri = "http://$($n.api)/ui/api/$Path"; Headers = $n.headers; TimeoutSec = 60 }
    if ($null -ne $Body) { $p.Body = ($Body | ConvertTo-Json -Compress); $p.ContentType = 'application/json' }
    Invoke-RestMethod @p
}

function Save-Settings([hashtable]$n) {
    $r = Invoke-Ui $n POST settings $n.settings
    if (-not $r.saved -or ($r.PSObject.Properties.Name -contains 'error' -and $r.error)) { throw "$($n.name): save failed: $($r | ConvertTo-Json -Compress)" }
}

function Stop-Node([hashtable]$n) {
    $r = Invoke-Ui $n POST quit
    if (-not $r.quitting) { throw "$($n.name): quit refused" }
    if (-not $n.proc.WaitForExit(15000)) { throw "$($n.name) did not exit after quit" }
    Write-Host "$($n.name) exited with code $($n.proc.ExitCode)"
}

function Get-Inbox([hashtable]$n) { @((Invoke-Ui $n GET inbox) | ForEach-Object { $_ }) }

# Find-Reply returns the inbound reply to id from n's inbox (entries omit empty reply_to).
function Find-Reply([hashtable]$n, [string]$Id) {
    Get-Inbox $n | Where-Object { $_.direction -eq 'in' -and $_.PSObject.Properties['reply_to']?.Value -eq $Id }
}

# Get-JobStatus returns the latest job status node-b reported for n's outbound request id.
function Get-JobStatus([hashtable]$n, [string]$Id) {
    $e = Get-Inbox $n | Where-Object { $_.direction -eq 'out' -and $_.id -eq $Id }
    ${e}?.PSObject.Properties['job_status']?.Value
}

function Get-Leftovers([string]$Marker) {
    @(Get-CimInstance Win32_Process -Filter "Name='fakeagent.exe'" | Where-Object { $_.ExecutablePath -eq $fake -or $_.CommandLine -like "*$Marker*" })
}

Write-Host '== build'
go -C $root build -ldflags '-H=windowsgui' -o $tray ./cmd/agentlink-tray
if ($LASTEXITCODE -ne 0) { throw 'build agentlink-tray failed' }
go -C $root build -o $fake ./internal/worker/testdata/fakeagent
if ($LASTEXITCODE -ne 0) { throw 'build fakeagent failed' }

$realBefore = Get-RealState
if (Test-Path $data) { Remove-Item -Recurse -Force $data }
$work = New-Item -ItemType Directory -Force (Join-Path $data 'work')
# The settings page's "Создать код" button: 6 letters/digits; b types it in lower case.
$code = -join ((1..6) | ForEach-Object { 'ABCDEFGHJKLMNPQRSTUVWXYZ23456789'[(Get-Random -Maximum 32)] })
$marker = "agentlink-e2e-$(Get-Random)"

$a = @{ name = 'node-a'; port = 7441; api = '127.0.0.1:7541' }
$b = @{ name = 'node-b'; port = 7442; api = '127.0.0.1:7542' }
foreach ($pair in @(@($a, $b), @($b, $a))) {
    $n, $peer = $pair
    $dir = New-Item -ItemType Directory -Force (Join-Path $data $n.name)
    $n.config = Join-Path $dir 'config.json'
    # The minimal form: name, code, peer address, handler, folder; "Мой адрес" from
    # "Дополнительно" only because both people share one machine.
    $n.settings = [ordered]@{
        node = $n.name; code = $code; peer_addr = "${Address}:$($peer.port)"; handler = 'none'
        work_dir = $work.FullName; listen = "${Address}:$($n.port)"
    }
}
$b.settings.handler = 'claude'
$b.settings.max_jobs = 1 # jobs run one after another, in order
$b.settings.code = $code.ToLower()
# The agent command is not editable in the UI; the only pre-seeded field is b's fake agent.
@{ handler_command = @($fake) } | ConvertTo-Json | Set-Content -Path $b.config

try {
    Write-Host "== start two apps (peer address $Address)"
    Start-Node $a
    Start-Node $b
    Write-Host '== first save with only a name: saved, waiting for a code'
    $r = Invoke-Ui $a POST settings @{ node = $a.name }
    if (-not $r.saved) { throw "name-only save failed: $($r | ConvertTo-Json -Compress)" }
    $st = Invoke-Ui $a GET status
    if ($st.running -or $st.problem -ne 'link.no_code') { throw "unexpected status after a name-only save: $($st | ConvertTo-Json -Compress)" }
    Save-Settings $a
    Save-Settings $b
    foreach ($n in $a, $b) {
        $st = Wait-Until { $s = Invoke-Ui $n GET status; if ($s.connected) { $s } } "$($n.name) connected"
        Write-Host "$($n.name): $($st | ConvertTo-Json -Compress)"
    }

    Write-Host '== request node-a -> node-b, auto-reply in node-a inbox'
    $req = Invoke-Ui $a POST send @{ to = 'node-b'; body = 'ping from node-a' }
    $reply = Wait-Until { Find-Reply $a $req.id } 'auto-reply'
    Write-Host "reply: $($reply.body)"
    if ($reply.from -ne 'node-b' -or $reply.body -ne 'echo: ping from node-a') { throw 'wrong auto-reply' }

    Write-Host '== quit node-b during job 1 of 3, restart: job 1 agent survives, all completed, each run once'
    $runlog = Join-Path $data 'runs.log'
    $jobs = @(1..3 | ForEach-Object { Invoke-Ui $a POST send @{ to = 'node-b'; body = "slow 3 $runlog job$_" } })
    Wait-Until { (Test-Path $runlog) -and (Get-Content $runlog) -contains 'job1' } 'job 1 running' | Out-Null
    $seen = Wait-Until {
        $st = @($jobs | ForEach-Object { Get-JobStatus $a $_.id })
        if ($st[0] -eq 'running' -and $st[1] -eq 'queued' -and $st[2] -eq 'queued') { $st -join ',' }
    } 'node-a sees job 1 running and jobs 2-3 queued'
    Write-Host "statuses on node-a before quit: $seen"
    $job1 = @(Get-CimInstance Win32_Process -Filter "Name='fakeagent.exe'" | Where-Object { $_.ExecutablePath -eq $fake })
    if ($job1.Count -ne 1) { throw "expected one agent for job 1, found $($job1.Count)" }
    Stop-Node $b
    if (-not (Get-Process -Id $job1[0].ProcessId -ErrorAction SilentlyContinue)) { throw 'quitting node-b killed the running agent' }
    if (@($jobs | Where-Object { Find-Reply $a $_.id }).Count) { throw 'a reply was sent for a running or queued job' }
    Start-Node $b -NoApiFlag
    Wait-Until { @($jobs | Where-Object { (Get-JobStatus $a $_.id) -eq 'completed' }).Count -eq 3 } 'three completed replies' | Out-Null
    foreach ($i in 0..2) {
        $e = Get-Inbox $a | Where-Object { $_.direction -eq 'out' -and $_.id -eq $jobs[$i].id }
        if ($e.answer -ne "echo: slow 3 $runlog job$($i + 1)") { throw "wrong answer for job $($i + 1): $($e.answer)" }
    }
    $runs = (Get-Content $runlog) -join ','
    Write-Host "agent runs: $runs"
    if ($runs -ne 'job1,job2,job3') { throw "unexpected agent runs: $runs" }

    Write-Host '== save settings on node-b while its job runs: the agent keeps running'
    $job = Invoke-Ui $a POST send @{ to = 'node-b'; body = "tree $marker" }
    $first = Wait-Until { $p = @(Get-Leftovers $marker); if ($p.Count -ge 2) { , $p.ProcessId } } 'attempt 1 and its child running'
    Save-Settings $b
    Wait-Until { (Invoke-Ui $b GET status).connected } 'node-b reconnected after the save' | Out-Null
    Start-Sleep -Seconds 1
    $now = @((Get-Leftovers $marker).ProcessId)
    if (@($first | Where-Object { $_ -notin $now }).Count -or $now.Count -ne $first.Count) { throw "a settings save changed the agent processes: $($first -join ',') -> $($now -join ',')" }
    if (Find-Reply $a $job.id) { throw 'a settings save replied' }

    Write-Host '== agent killed while node-b is down: started over once, killed again: failed'
    foreach ($round in 1, 2) {
        Stop-Node $b
        Get-Leftovers $marker | ForEach-Object { Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue }
        Wait-Until { -not @(Get-Leftovers $marker).Count } 'agent processes gone' | Out-Null
        Start-Node $b -NoApiFlag
        if ($round -eq 1) {
            Wait-Until { @(Get-Leftovers $marker).Count -ge 2 } 'attempt 2 and its child running' | Out-Null
            if (Find-Reply $a $job.id) { throw 'a killed agent replied instead of starting over' }
        }
    }
    $int = Wait-Until { Find-Reply $a $job.id } 'failed reply'
    Write-Host "reply: $($int.job_status) $($int.body)"
    if ($int.job_status -ne 'failed' -or $int.body -ne 'agentlink: handler was interrupted twice (the agent or the app stopped mid-run)') { throw 'wrong failed reply' }
    Start-Sleep -Seconds 1
    $left = @(Get-Leftovers $marker)
    if ($left.Count) { throw "agent processes left after interruption: $($left.ProcessId -join ', ')" }

    Write-Host '== restart node-a keeps inbox history'
    $before = Get-Inbox $a
    Stop-Node $a
    Start-Node $a -NoApiFlag # the API port must come from the saved settings now
    Wait-Until { (Invoke-Ui $a GET status).running } 'node-a running after restart' | Out-Null
    $after = Get-Inbox $a
    $missing = @($before.id | Where-Object { $_ -notin $after.id })
    if ($missing.Count) { throw "inbox lost entries after restart: $($missing -join ', ')" }
    Write-Host "inbox entries before/after restart: $($before.Count)/$($after.Count)"

    Write-Host '== quit both'
    Stop-Node $a
    Stop-Node $b
    if ((Get-RealState) -ne $realBefore) { throw '%APPDATA%\agentlink or the autostart entry changed' }
    Write-Host 'E2E TRAY PASS'
}
finally {
    foreach ($n in $a, $b) {
        if ($n.ContainsKey('proc') -and -not $n.proc.HasExited) { Stop-Process -Id $n.proc.Id -Force }
    }
    Get-Leftovers $marker | ForEach-Object { Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue }
}
