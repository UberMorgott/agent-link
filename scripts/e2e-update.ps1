#Requires -Version 7
# Self-update end to end, on one machine, without GitHub. Builds agentlink.exe
# as version 0.0.1 into an install folder and as 0.0.2 into a release folder;
# both builds point selfupdate at a local fake releases API (fakerelease, which
# reports each asset's sha256 digest as GitHub does) through -ldflags. Two
# headless apps pair as in e2e-tray.ps1: node-a is a plain build, node-b runs
# from the install folder with a fake agent. The install folder also holds an
# agentlink-tray.exe, the desktop app of older releases.
#
# Checks: `agentlink update --check` sees 0.0.2; node-b's first start removes
# the old agentlink-tray.exe; while node-b's agent runs a slow job, node-b
# checks and installs 0.0.2 through the web UI; the old app quits the normal
# way and the new one starts from the same path, reports 0.0.2 and reattaches
# to the agent, which was never restarted: the job completes once. The one
# executable is replaced, the parked old file is swept, the CLI then reports
# up to date, and a release whose file does not match its digest is refused.
[CmdletBinding()]
param([int]$TimeoutSeconds = 45)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$root = Split-Path -Parent $PSScriptRoot
$bin = Join-Path $root 'bin'
$plain = Join-Path $bin 'agentlink.exe'
$fake = Join-Path $bin 'fakeagent.exe'
$fakeRelease = Join-Path $bin 'fakerelease.exe'
$data = Join-Path $root '.data/e2e-update'
$install = Join-Path $data 'install'
$release = Join-Path $data 'release'
$ghAddr = '127.0.0.1:7549'
$pkg = 'github.com/UberMorgott/agent-link/internal/selfupdate'

function Wait-Until([scriptblock]$Condition, [string]$What) {
    $deadline = (Get-Date).AddSeconds($TimeoutSeconds)
    while ($true) {
        $v = try { & $Condition } catch { $null }
        if ($v) { return $v }
        if ((Get-Date) -gt $deadline) { throw "timed out waiting for $What" }
        Start-Sleep -Milliseconds 250
    }
}

function Build([string]$Version, [string]$Dir) {
    $x = "-X $pkg.Version=$Version -X $pkg.apiBase=http://$ghAddr"
    go -C $root build -ldflags $x -o (Join-Path $Dir 'agentlink.exe') ./cmd/agentlink
    if ($LASTEXITCODE -ne 0) { throw "build agentlink $Version failed" }
}

# Connect-Node reads the page token of the app answering at n.api.
function Connect-Node([hashtable]$n) {
    $base = "http://$($n.api)"
    $page = Wait-Until { Invoke-WebRequest -Uri "$base/ui/settings" -TimeoutSec 2 } "$($n.name) web UI"
    if ($page.Content -notmatch 'name="agentlink-token" content="([0-9a-f]+)"') { throw "$($n.name): no token in page" }
    $n.headers = @{ 'X-Agentlink-Token' = $Matches[1]; 'Origin' = $base; 'Sec-Fetch-Site' = 'same-origin' }
}

function Start-Node([hashtable]$n) {
    $n.proc = Start-Process -FilePath $n.exe -ArgumentList @('-no-tray', '-config', $n.config, '-api', $n.api) -PassThru -WindowStyle Hidden
    Connect-Node $n
}

function Invoke-Ui([hashtable]$n, [string]$Method, [string]$Path, $Body) {
    $p = @{ Method = $Method; Uri = "http://$($n.api)/ui/api/$Path"; Headers = $n.headers; TimeoutSec = 120 }
    if ($null -ne $Body) { $p.Body = ($Body | ConvertTo-Json -Compress); $p.ContentType = 'application/json' }
    Invoke-RestMethod @p
}

function Get-Inbox([hashtable]$n) { @((Invoke-Ui $n GET inbox) | ForEach-Object { $_ }) }

function Get-Out([hashtable]$n, [string]$Id) { Get-Inbox $n | Where-Object { $_.direction -eq 'out' -and $_.id -eq $Id } }

function Get-Trays { @(Get-CimInstance Win32_Process -Filter "Name='agentlink.exe'" | Where-Object { $_.ExecutablePath -like "$install*" }) }

Write-Host '== build 0.0.1 (installed) and 0.0.2 (released)'
if (Test-Path $data) { Remove-Item -Recurse -Force $data }
New-Item -ItemType Directory -Force $install, $release | Out-Null
Build '0.0.1' $install
Build '0.0.2' $release
# The desktop app of older releases, next to the one executable that replaces it.
$legacy = Join-Path $install 'agentlink-tray.exe'
Copy-Item (Join-Path $install 'agentlink.exe') $legacy
go -C $root build -o $plain ./cmd/agentlink
if ($LASTEXITCODE -ne 0) { throw 'build agentlink failed' }
go -C $root build -o $fake ./internal/worker/testdata/fakeagent
if ($LASTEXITCODE -ne 0) { throw 'build fakeagent failed' }
go -C $root build -o $fakeRelease ./internal/selfupdate/testdata/fakerelease
if ($LASTEXITCODE -ne 0) { throw 'build fakerelease failed' }

$work = New-Item -ItemType Directory -Force (Join-Path $data 'work')
$code = -join ((1..6) | ForEach-Object { 'ABCDEFGHJKLMNPQRSTUVWXYZ23456789'[(Get-Random -Maximum 32)] })
$a = @{ name = 'node-a'; port = 7443; api = '127.0.0.1:7543'; exe = $plain }
$b = @{ name = 'node-b'; port = 7444; api = '127.0.0.1:7544'; exe = (Join-Path $install 'agentlink.exe') }
foreach ($pair in @(@($a, $b), @($b, $a))) {
    $n, $peer = $pair
    $dir = New-Item -ItemType Directory -Force (Join-Path $data $n.name)
    $n.config = Join-Path $dir 'config.json'
    $n.settings = [ordered]@{
        node = $n.name; code = $code; peer_addr = "127.0.0.1:$($peer.port)"; handler = 'none'
        work_dir = $work.FullName; listen = "127.0.0.1:$($n.port)"
    }
}
$b.settings.handler = 'claude'
# Updates by hand in this test, so the automatic check cannot race it.
@{ handler_command = @($fake); auto_update = $false } | ConvertTo-Json | Set-Content -Path $b.config

$gh = $null
try {
    $gh = Start-Process -FilePath $fakeRelease -ArgumentList @('-addr', $ghAddr, '-dir', $release, '-tag', 'v0.0.2') -PassThru -WindowStyle Hidden
    Wait-Until { Invoke-RestMethod "http://$ghAddr/repos/UberMorgott/agent-link/releases/latest" -TimeoutSec 2 } 'fake releases API' | Out-Null

    Write-Host '== CLI: version and update --check'
    $cli = Join-Path $install 'agentlink.exe'
    $v = & $cli version
    if ($v -ne '0.0.1') { throw "installed CLI reports $v" }
    $out = & $cli update --check
    Write-Host $out
    if ($LASTEXITCODE -ne 0 -or $out -notmatch 'update available: 0\.0\.2') { throw 'update --check did not see 0.0.2' }

    Write-Host '== start two apps and pair'
    Start-Node $a
    Start-Node $b
    foreach ($n in $a, $b) { Invoke-Ui $n POST settings $n.settings | Out-Null }
    foreach ($n in $a, $b) { Wait-Until { (Invoke-Ui $n GET status).connected } "$($n.name) connected" | Out-Null }
    Wait-Until { -not (Test-Path $legacy) } 'the old agentlink-tray.exe removed' | Out-Null
    $st = Invoke-Ui $b GET update
    if ($st.current -ne '0.0.1' -or -not $st.enabled -or $st.auto) { throw "node-b update state: $($st | ConvertTo-Json -Compress)" }

    Write-Host '== a slow job runs on node-b'
    $runlog = Join-Path $data 'runs.log'
    $job = Invoke-Ui $a POST send @{ to = 'node-b'; body = "slow 8 $runlog job1" }
    Wait-Until { (Test-Path $runlog) -and (Get-Content $runlog) -contains 'job1' } 'job running' | Out-Null
    $agent = @(Get-CimInstance Win32_Process -Filter "Name='fakeagent.exe'" | Where-Object { $_.ExecutablePath -eq $fake })
    if ($agent.Count -ne 1) { throw "expected one agent, found $($agent.Count)" }

    Write-Host '== node-b: check, then install 0.0.2 while the job runs'
    $st = Invoke-Ui $b POST update/check
    Write-Host "check: $($st.text)"
    if (-not $st.available -or $st.latest -ne '0.0.2') { throw "check: $($st | ConvertTo-Json -Compress)" }
    $st = Invoke-Ui $b POST update/apply
    Write-Host "apply: $($st.text)"
    if (-not $st.restarting) { throw "apply: $($st | ConvertTo-Json -Compress)" }
    if (-not $b.proc.WaitForExit(20000)) { throw 'the old node-b did not quit after the update' }
    Write-Host "old node-b exited with code $($b.proc.ExitCode)"
    Connect-Node $b
    $st = Wait-Until { $s = Invoke-Ui $b GET update; if ($s.current -eq '0.0.2') { $s } } 'node-b answering as 0.0.2'
    $trays = Get-Trays
    if ($trays.Count -ne 1) { throw "expected one node-b process after the update, found $($trays.Count)" }
    $b.proc = Get-Process -Id $trays[0].ProcessId
    if (-not (Get-Process -Id $agent[0].ProcessId -ErrorAction SilentlyContinue)) { throw 'the update killed the running agent' }
    Write-Host "node-b now $($st.current), agent $($agent[0].ProcessId) still running"

    Write-Host '== the job completes once, answered by the new app'
    Wait-Until { (Get-Out $a $job.id).PSObject.Properties['job_status']?.Value -eq 'completed' } 'job completed' | Out-Null
    $e = Get-Out $a $job.id
    if ($e.answer -ne "echo: slow 8 $runlog job1") { throw "wrong answer: $($e.answer)" }
    $runs = (Get-Content $runlog) -join ','
    if ($runs -ne 'job1') { throw "agent runs: $runs" }

    Write-Host '== the executable replaced, leftovers swept'
    $v = & $cli version
    if ($v -ne '0.0.2') { throw "CLI after the update reports $v" }
    if ((Get-FileHash $cli).Hash -ne (Get-FileHash (Join-Path $release 'agentlink.exe')).Hash) { throw 'agentlink.exe was not replaced' }
    Wait-Until { -not @(Get-ChildItem -Force $install | Where-Object { $_.Name -like '.*' }).Count } 'old executable removed' | Out-Null
    $files = @(Get-ChildItem -Force $install).Name -join ','
    if ($files -ne 'agentlink.exe') { throw "install folder holds $files" }
    $out = & $cli update --check
    Write-Host $out
    if ($out -notmatch 'up to date') { throw 'CLI does not report up to date' }

    Write-Host '== a tampered release is refused'
    Build '0.0.3' $release
    Stop-Process -Id $gh.Id -Force
    $gh = Start-Process -FilePath $fakeRelease -ArgumentList @('-addr', $ghAddr, '-dir', $release, '-tag', 'v0.0.3') -PassThru -WindowStyle Hidden
    Wait-Until { Invoke-RestMethod "http://$ghAddr/repos/UberMorgott/agent-link/releases/latest" -TimeoutSec 2 } 'fake releases API' | Out-Null
    # fakerelease took the digest at its start; the file it serves now no longer matches.
    Set-Content -Path (Join-Path $release 'agentlink.exe') -Value 'tampered'
    $out = & $cli update 2>&1
    Write-Host "$out"
    if ($LASTEXITCODE -eq 0 -or "$out" -notmatch 'checksum mismatch') { throw 'a tampered release was not refused' }
    if ((& $cli version) -ne '0.0.2') { throw 'a refused update changed the CLI' }

    Write-Host '== quit both'
    foreach ($n in $a, $b) {
        Invoke-Ui $n POST quit | Out-Null
        if (-not $n.proc.WaitForExit(15000)) { throw "$($n.name) did not exit" }
    }
    Write-Host 'E2E UPDATE PASS'
}
finally {
    foreach ($n in $a, $b) {
        if ($n.ContainsKey('proc') -and -not $n.proc.HasExited) { Stop-Process -Id $n.proc.Id -Force }
    }
    Get-Trays | ForEach-Object { Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue }
    if ($gh -and -not $gh.HasExited) { Stop-Process -Id $gh.Id -Force }
    Get-CimInstance Win32_Process -Filter "Name='fakeagent.exe'" | Where-Object { $_.ExecutablePath -eq $fake } |
        ForEach-Object { Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue }
}
