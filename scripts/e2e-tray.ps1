#Requires -Version 7
# Two desktop apps on one machine as two people. Each agentlink-tray runs
# headless (-no-tray) with its own settings folder and API port, never touching
# %APPDATA%\agentlink or the autostart entry. Setup, sending, reading the inbox
# and quitting all go through the web UI endpoints the way the pages call them
# (per-run token from the page, same-origin headers).
#
# Checks: a->b request answered by b's fake echo agent and shown in a's inbox;
# saving settings while a job runs replies "interrupted" and leaves no agent
# processes behind; a restarted app keeps its inbox history; Quit exits both.
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
        Get-ChildItem -Recurse -Force $dir | ForEach-Object { "$($_.FullName)|$($_.Length)|$($_.LastWriteTimeUtc.Ticks)" }
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
# The settings page's Generate button: 48 random bytes as hex, pasted on both sides.
$secret = -join ((1..48) | ForEach-Object { '{0:x2}' -f (Get-Random -Maximum 256) })
$marker = "agentlink-e2e-$(Get-Random)"

$a = @{ name = 'node-a'; port = 7441; api = '127.0.0.1:7541' }
$b = @{ name = 'node-b'; port = 7442; api = '127.0.0.1:7542' }
foreach ($pair in @(@($a, $b), @($b, $a))) {
    $n, $peer = $pair
    $dir = New-Item -ItemType Directory -Force (Join-Path $data $n.name)
    $n.config = Join-Path $dir 'config.json'
    $n.settings = [ordered]@{
        node = $n.name; listen = "${Address}:$($n.port)"; peer_name = $peer.name; peer_addr = "${Address}:$($peer.port)"
        secret = $secret; areas = @(); handler = 'none'; work_dir = $work.FullName; autostart = $false
    }
}
$b.settings.handler = 'claude'
# The agent command is not editable in the UI; the only pre-seeded field is b's fake agent.
@{ handler_command = @($fake) } | ConvertTo-Json | Set-Content -Path $b.config

try {
    Write-Host "== start two apps (peer address $Address)"
    Start-Node $a
    Start-Node $b
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

    Write-Host '== save settings on node-b while its job runs'
    $job = Invoke-Ui $a POST send @{ to = 'node-b'; body = "tree $marker" }
    Wait-Until { @(Get-Leftovers $marker).Count -ge 2 } 'agent and its child running' | Out-Null
    Save-Settings $b
    $int = Wait-Until { Find-Reply $a $job.id } 'interrupted reply'
    Write-Host "reply: $($int.body)"
    if ($int.body -ne 'agentlink: handler was interrupted (settings changed or the app quit)') { throw 'wrong interrupted reply' }
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
